// Package integration spins up two real in-memory IMAP servers (go-imap's
// imapmemserver) and runs the full stack — imapx clients, SQLite state store,
// reconciler — against them over actual IMAP connections. Self-contained: no
// Docker, no network beyond loopback.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lhns/umleitung/internal/config"
	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/reconcile"
	"github.com/lhns/umleitung/internal/state"
)

const (
	srcFolder = "Remote/All Mail" // hierarchical, exercises delimiter handling
	dstFolder = "Mirror"
	password  = "hunter2"
)

// startServer runs an in-memory IMAP server on a loopback port and returns
// its endpoint plus the backing user for server-side manipulation.
func startServer(t *testing.T, username string) (config.Endpoint, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser(username, password)
	mem := imapmemserver.New()
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true, // loopback test server, no TLS
		Logger:       slog.NewLogLogger(slog.New(slog.DiscardHandler).Handler(), slog.LevelError),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	return config.Endpoint{
		Host: "127.0.0.1", Port: port,
		User: username, Password: password,
		TLS: false,
	}, user
}

func rawMessage(messageID, subject string) []byte {
	msg := ""
	if messageID != "" {
		msg += fmt.Sprintf("Message-ID: %s\r\n", messageID)
	}
	msg += fmt.Sprintf("From: sender@example.com\r\n"+
		"To: rcpt@example.com\r\n"+
		"Subject: %s\r\n"+
		"Date: Thu, 02 Jul 2026 12:00:00 +0000\r\n"+
		"\r\n"+
		"body of %s\r\n", subject, subject)
	return []byte(msg)
}

// appendToSource appends a message to the source server over IMAP.
func appendToSource(t *testing.T, ep config.Endpoint, raw []byte, internalDate time.Time) {
	t.Helper()
	cl, err := imapx.Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.Append(&imapx.FullMessage{Raw: raw, InternalDate: internalDate}, nil); err != nil {
		t.Fatal(err)
	}
}

// destMessages lists (Message-ID, Subject) of everything in the destination folder.
func destMessages(t *testing.T, ep config.Endpoint) []imapx.MsgMeta {
	t.Helper()
	cl, err := imapx.Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, uidNext, num, err := cl.SelectFolder()
	if err != nil {
		t.Fatal(err)
	}
	if num == 0 {
		return nil
	}
	metas, err := cl.FetchMetaRange(1, imap.UID(uidNext-1))
	if err != nil {
		t.Fatal(err)
	}
	return metas
}

// newReconciler wires real clients + store into a reconciler, mimicking main's
// session setup (ensure + select destination folder).
func newReconciler(t *testing.T, srcEP, dstEP config.Endpoint, store *state.Store) (*reconcile.Reconciler, func()) {
	t.Helper()
	src, err := imapx.Dial(srcEP)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := imapx.Dial(dstEP)
	if err != nil {
		src.Close()
		t.Fatal(err)
	}
	if err := dst.EnsureFolder(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := dst.SelectFolder(); err != nil {
		t.Fatal(err)
	}
	rec := reconcile.New(store, src, dst, reconcile.Options{
		UIDBatch:  2, // tiny windows so the test exercises windowing
		DestGuard: true,
		CarrySeen: true,
	}, slog.New(slog.DiscardHandler))
	return rec, func() { src.Close(); dst.Close() }
}

func TestEndToEndMirror(t *testing.T) {
	srcEP, srcUser := startServer(t, "source@test")
	dstEP, _ := startServer(t, "dest@test")
	srcEP.Folder = srcFolder
	dstEP.Folder = dstFolder

	if err := srcUser.Create(srcFolder, nil); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()

	baseDate := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Source: two normal messages, one without Message-ID, one duplicate
	// Message-ID (must be mirrored exactly once).
	appendToSource(t, srcEP, rawMessage("<m1@test>", "one"), baseDate)
	appendToSource(t, srcEP, rawMessage("<m2@test>", "two"), baseDate.Add(time.Minute))
	appendToSource(t, srcEP, rawMessage("", "no-message-id"), baseDate.Add(2*time.Minute))
	appendToSource(t, srcEP, rawMessage("<m1@test>", "duplicate of one"), baseDate.Add(3*time.Minute))

	// --- First sync: everything copied once, duplicate skipped. ---
	rec, closeRec := newReconciler(t, srcEP, dstEP, store)
	sum, err := rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Candidates != 4 || sum.Copied != 3 || sum.SkippedDup != 1 {
		t.Fatalf("first sync: %+v, want 4 candidates / 3 copied / 1 skipped", sum)
	}
	if got := destMessages(t, dstEP); len(got) != 3 {
		t.Fatalf("dest has %d messages, want 3", len(got))
	}

	// --- Re-run: nothing new. ---
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 {
		t.Fatalf("re-run copied %d, want 0", sum.Copied)
	}
	closeRec()

	// --- Total state loss + destination seeding: still zero duplicates. ---
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	rec, closeRec = newReconciler(t, srcEP, dstEP, store)
	seeded, err := rec.SeedFromDest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seeded != 3 {
		t.Fatalf("seeded %d keys, want 3", seeded)
	}
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The synthesized-key message must also be recognized via seeding.
	if sum.Copied != 0 {
		t.Fatalf("after state wipe + seed: copied %d, want 0 (dupes!)", sum.Copied)
	}
	if got := destMessages(t, dstEP); len(got) != 3 {
		t.Fatalf("dest has %d messages after re-seeded sync, want 3", len(got))
	}

	// --- Incremental: new mail arrives, only it is copied. ---
	appendToSource(t, srcEP, rawMessage("<m5@test>", "five"), baseDate.Add(4*time.Minute))
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 {
		t.Fatalf("incremental: copied %d, want 1", sum.Copied)
	}
	if got := destMessages(t, dstEP); len(got) != 4 {
		t.Fatalf("dest has %d messages, want 4", len(got))
	}

	// --- Crash window: message landed in dest but was never recorded
	// locally; the destination guard must catch it. ---
	dstDirect, err := imapx.Dial(dstEP)
	if err != nil {
		t.Fatal(err)
	}
	if err := dstDirect.Append(&imapx.FullMessage{
		Raw: rawMessage("<m6@test>", "six"), InternalDate: baseDate.Add(5 * time.Minute),
	}, nil); err != nil {
		t.Fatal(err)
	}
	dstDirect.Close()
	appendToSource(t, srcEP, rawMessage("<m6@test>", "six"), baseDate.Add(5*time.Minute))
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || sum.SkippedDup != 1 {
		t.Fatalf("dest guard: %+v, want 0 copied / 1 skipped", sum)
	}
	if got := destMessages(t, dstEP); len(got) != 5 {
		t.Fatalf("dest has %d messages, want 5 (m6 exactly once)", len(got))
	}
	closeRec()

	// --- UIDVALIDITY change: delete + recreate source folder (memserver
	// bumps UIDVALIDITY), re-append same mail with fresh UIDs. ---
	prevValidity, err := store.UIDValidity()
	if err != nil {
		t.Fatal(err)
	}
	if err := srcUser.Delete(srcFolder); err != nil {
		t.Fatal(err)
	}
	if err := srcUser.Create(srcFolder, nil); err != nil {
		t.Fatal(err)
	}
	appendToSource(t, srcEP, rawMessage("<m1@test>", "one"), baseDate)
	appendToSource(t, srcEP, rawMessage("<m2@test>", "two"), baseDate.Add(time.Minute))
	appendToSource(t, srcEP, rawMessage("<m7@test>", "seven, new after reset"), baseDate.Add(6*time.Minute))

	rec, closeRec = newReconciler(t, srcEP, dstEP, store)
	defer closeRec()
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.UIDValidityChanged {
		t.Fatal("UIDVALIDITY change not detected")
	}
	if sum.Copied != 1 || sum.SkippedDup != 2 {
		t.Fatalf("after UIDVALIDITY reset: %+v, want 1 copied / 2 skipped", sum)
	}
	newValidity, err := store.UIDValidity()
	if err != nil {
		t.Fatal(err)
	}
	if newValidity == prevValidity {
		t.Fatal("stored UIDVALIDITY not updated")
	}
	if got := destMessages(t, dstEP); len(got) != 6 {
		t.Fatalf("dest has %d messages after reset, want 6", len(got))
	}
}

// TestSeenFlagCarriedButNothingElse verifies APPEND flag policy end-to-end.
func TestSeenFlagCarriedButNothingElse(t *testing.T) {
	srcEP, srcUser := startServer(t, "source@test")
	dstEP, _ := startServer(t, "dest@test")
	srcEP.Folder = srcFolder
	dstEP.Folder = dstFolder
	if err := srcUser.Create(srcFolder, nil); err != nil {
		t.Fatal(err)
	}

	// Append a seen+flagged message server-side so it carries flags.
	src, err := imapx.Dial(srcEP)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawMessage("<flagged@test>", "flagged")
	if err := src.Append(&imapx.FullMessage{Raw: raw, InternalDate: time.Now()}, []imap.Flag{imap.FlagSeen, imap.FlagFlagged}); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec, closeRec := newReconciler(t, srcEP, dstEP, store)
	defer closeRec()
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.Close()

	// Inspect dest flags directly.
	dst, err := imapx.Dial(dstEP)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, _, _, err := dst.SelectFolder(); err != nil {
		t.Fatal(err)
	}
	full, err := dst.FetchFull(1)
	if err != nil {
		t.Fatal(err)
	}
	var hasSeen, hasFlagged bool
	for _, f := range full.Flags {
		switch f {
		case imap.FlagSeen:
			hasSeen = true
		case imap.FlagFlagged:
			hasFlagged = true
		}
	}
	if !hasSeen {
		t.Error("\\Seen not carried to destination")
	}
	if hasFlagged {
		t.Error("\\Flagged leaked to destination — only \\Seen may be carried")
	}
}
