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
	"slices"
	"sort"
	"strings"
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

// TestLabelSyncEndToEnd verifies label-folder membership -> destination
// keywords over real IMAP connections.
func TestLabelSyncEndToEnd(t *testing.T) {
	srcEP, srcUser := startServer(t, "source@test")
	dstEP, _ := startServer(t, "dest@test")
	srcEP.Folder = srcFolder
	dstEP.Folder = dstFolder

	for _, f := range []string{srcFolder, "Work", "Friends/Close", "Ignored"} {
		if err := srcUser.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}

	baseDate := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// Message m1 carries labels Work + Friends/Close (+ one in an excluded
	// folder), m2 carries Work, m3 carries none.
	appendTo := func(folder string, raw []byte, date time.Time) {
		ep := srcEP
		ep.Folder = folder
		appendToSource(t, ep, raw, date)
	}
	m1 := rawMessage("<m1@test>", "one")
	m2 := rawMessage("<m2@test>", "two")
	m3 := rawMessage("<m3@test>", "three")
	appendTo(srcFolder, m1, baseDate)
	appendTo(srcFolder, m2, baseDate.Add(time.Minute))
	appendTo(srcFolder, m3, baseDate.Add(2*time.Minute))
	appendTo("Work", m1, baseDate)
	appendTo("Friends/Close", m1, baseDate)
	appendTo("Ignored", m1, baseDate)
	appendTo("Work", m2, baseDate.Add(time.Minute))

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	src, err := imapx.Dial(srcEP)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := imapx.Dial(dstEP)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.EnsureFolder(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := dst.SelectFolder(); err != nil {
		t.Fatal(err)
	}
	rec := reconcile.New(store, src, dst, reconcile.Options{
		UIDBatch:     2,
		DestGuard:    true,
		SyncLabels:   true,
		SourceFolder: srcFolder,
		LabelExclude: []string{"Ignored"},
	}, slog.New(slog.DiscardHandler))

	ctx := context.Background()
	sum, err := rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 3 {
		t.Fatalf("copied %d, want 3", sum.Copied)
	}

	// Collect keywords per Message-ID from the destination over IMAP.
	keywordsByMID := func() map[string][]string {
		out := map[string][]string{}
		checker, err := imapx.Dial(dstEP)
		if err != nil {
			t.Fatal(err)
		}
		defer checker.Close()
		_, uidNext, _, err := checker.SelectFolder()
		if err != nil {
			t.Fatal(err)
		}
		metas, err := checker.FetchMetaRange(1, imap.UID(uidNext-1))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range metas {
			full, err := checker.FetchFull(m.UID)
			if err != nil {
				t.Fatal(err)
			}
			var kws []string
			for _, f := range full.Flags {
				if !strings.HasPrefix(string(f), `\`) { // skip system flags, keep keywords
					kws = append(kws, string(f))
				}
			}
			sort.Strings(kws)
			out[m.MessageID] = kws
		}
		return out
	}

	got := keywordsByMID()
	if want := []string{"friends_close", "work"}; !slices.Equal(got["<m1@test>"], want) {
		t.Fatalf("m1 keywords = %v, want %v (Ignored folder must not contribute)", got["<m1@test>"], want)
	}
	if want := []string{"work"}; !slices.Equal(got["<m2@test>"], want) {
		t.Fatalf("m2 keywords = %v, want %v", got["<m2@test>"], want)
	}
	if len(got["<m3@test>"]) != 0 {
		t.Fatalf("m3 keywords = %v, want none", got["<m3@test>"])
	}

	// Re-run: no new appends, keywords unchanged.
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 {
		t.Fatalf("re-run copied %d", sum.Copied)
	}

	// New labeled mail arrives -> mirrored with its keyword.
	m4 := rawMessage("<m4@test>", "four")
	appendTo("Work", m4, baseDate.Add(3*time.Minute))
	appendTo(srcFolder, m4, baseDate.Add(3*time.Minute))
	sum, err = rec.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 {
		t.Fatalf("incremental copied %d, want 1", sum.Copied)
	}
	got = keywordsByMID()
	if want := []string{"work"}; !slices.Equal(got["<m4@test>"], want) {
		t.Fatalf("m4 keywords = %v, want %v", got["<m4@test>"], want)
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
