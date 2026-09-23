// Package integration runs the full stack — imapx clients, SQLite state
// store, reconciler, mirror runtime — against two in-memory IMAP servers
// (go-imap's imapmemserver) over real loopback connections. No Docker.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lhns/umleitung/internal/config"
	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/reconcile"
	"github.com/lhns/umleitung/internal/state"
)

const (
	srcFolder   = "Remote/All Mail" // hierarchical, exercises delimiter handling
	dstFolder   = "Mirror"
	dstArchive  = "MirrorArchive"
	dstSent     = "MirrorSent"
	srcInbox    = "INBOX"
	srcSent     = "Sent"
	password    = "hunter2"
	pollTimeout = 10 * time.Second
)

var baseDate = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

// at returns baseDate plus n minutes.
func at(n int) time.Time { return baseDate.Add(time.Duration(n) * time.Minute) }

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
		Logger:       slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	return config.Endpoint{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		User: username, Password: password,
	}, user
}

// setup starts a source and a destination server, creates srcFolder plus
// extra source folders, and returns endpoints aimed at srcFolder/dstFolder.
func setup(t *testing.T, extraSrcFolders ...string) (srcEP, dstEP config.Endpoint, srcUser *imapmemserver.User) {
	t.Helper()
	srcEP, srcUser = startServer(t, "source@test")
	dstEP, _ = startServer(t, "dest@test")
	srcEP.Folder = srcFolder
	dstEP.Folder = dstFolder
	for _, f := range append([]string{srcFolder}, extraSrcFolders...) {
		if err := srcUser.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	return srcEP, dstEP, srcUser
}

func openStore(t *testing.T, path string) *state.Store {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
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

// appendMsg appends a message to a folder over IMAP.
func appendMsg(t *testing.T, ep config.Endpoint, folder string, raw []byte, date time.Time, flags ...imap.Flag) {
	t.Helper()
	cl, err := imapx.Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.AppendTo(folder, &imapx.FullMessage{Raw: raw, InternalDate: date}, flags); err != nil {
		t.Fatal(err)
	}
}

// expunge removes a message from a folder over raw IMAP (the user archiving
// or unlabeling in their mail client).
func expunge(t *testing.T, ep config.Endpoint, folder, mid string) {
	t.Helper()
	c, err := imapclient.DialInsecure(ep.Addr(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(ep.User, ep.Password).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: mid}},
	}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		t.Fatalf("expunge setup: %q not found in %q", mid, folder)
	}
	if err := c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
}

// metasIn fetches the metadata of every message in a folder.
func metasIn(t *testing.T, ep config.Endpoint, folder string) []imapx.MsgMeta {
	t.Helper()
	cl, err := imapx.Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, uidNext, num, err := cl.SelectNamedFolder(folder)
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

// midsIn returns the sorted Message-IDs in a folder; duplicates are kept so
// callers comparing against an expected list also catch double appends.
func midsIn(t *testing.T, ep config.Endpoint, folder string) []string {
	t.Helper()
	var mids []string
	for _, m := range metasIn(t, ep, folder) {
		mids = append(mids, m.MessageID)
	}
	slices.Sort(mids)
	return mids
}

// wantMids asserts each folder holds exactly the given Message-IDs.
func wantMids(t *testing.T, ep config.Endpoint, want map[string][]string) {
	t.Helper()
	for folder, mids := range want {
		mids = slices.Sorted(slices.Values(mids))
		if got := midsIn(t, ep, folder); !slices.Equal(got, mids) {
			t.Fatalf("%s = %v, want %v", folder, got, mids)
		}
	}
}

// keywordsIn returns each message's sorted keywords (system flags dropped),
// keyed by Message-ID.
func keywordsIn(t *testing.T, ep config.Endpoint, folder string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range metasIn(t, ep, folder) {
		var kws []string
		for _, f := range m.Flags {
			if !strings.HasPrefix(string(f), `\`) {
				kws = append(kws, string(f))
			}
		}
		slices.Sort(kws)
		out[m.MessageID] = kws
	}
	return out
}

func wantKeywords(t *testing.T, got map[string][]string, mid string, want ...string) {
	t.Helper()
	if !slices.Equal(got[mid], want) {
		t.Fatalf("%s keywords = %v, want %v", mid, got[mid], want)
	}
}

// newReconciler wires real clients + store into a reconciler, mimicking the
// mirror session setup (ensure destination folders, select dstEP.Folder).
// DestFolder defaults to dstEP.Folder, UIDBatch to 2 so windowing is
// exercised.
func newReconciler(t *testing.T, srcEP, dstEP config.Endpoint, store *state.Store, opts reconcile.Options) *reconcile.Reconciler {
	t.Helper()
	if opts.DestFolder == "" {
		opts.DestFolder = dstEP.Folder
	}
	if opts.UIDBatch == 0 {
		opts.UIDBatch = 2
	}
	src, err := imapx.Dial(srcEP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	dst, err := imapx.Dial(dstEP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dst.Close)
	folders := []string{opts.DestFolder}
	if opts.ArchiveRouting {
		folders = append(folders, opts.ArchiveFolder)
	}
	if opts.SentRouting {
		folders = append(folders, opts.SentFolder)
	}
	for _, f := range folders {
		if err := dst.EnsureNamedFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := dst.SelectFolder(); err != nil {
		t.Fatal(err)
	}
	return reconcile.New(store, src, dst, opts, slog.New(slog.DiscardHandler))
}

func run(t *testing.T, rec *reconcile.Reconciler) *reconcile.Summary {
	t.Helper()
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func routingOpts() reconcile.Options {
	return reconcile.Options{
		DestGuard:      true,
		ArchiveRouting: true,
		SourceInbox:    srcInbox,
		ArchiveFolder:  dstArchive,
	}
}

func TestEndToEndMirror(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, srcUser := setup(t)
	statePath := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, statePath)
	opts := reconcile.Options{DestGuard: true, CarrySeen: true}

	// Two normal messages, one without Message-ID, one duplicate Message-ID
	// (must be mirrored exactly once).
	appendMsg(t, srcEP, srcFolder, rawMessage("<m1@test>", "one"), at(0))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m2@test>", "two"), at(1))
	appendMsg(t, srcEP, srcFolder, rawMessage("", "no-message-id"), at(2))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m1@test>", "duplicate of one"), at(3))

	// First sync: everything copied once, duplicate skipped.
	rec := newReconciler(t, srcEP, dstEP, store, opts)
	if sum := run(t, rec); sum.Candidates != 4 || sum.Copied != 3 || sum.SkippedDup != 1 {
		t.Fatalf("first sync: %+v, want 4 candidates / 3 copied / 1 skipped", sum)
	}
	wantMids(t, dstEP, map[string][]string{dstFolder: {"", "<m1@test>", "<m2@test>"}})
	if sum := run(t, rec); sum.Copied != 0 {
		t.Fatalf("re-run copied %d, want 0", sum.Copied)
	}

	// Total state loss + destination seeding: still zero duplicates, the
	// synthesized-key message included.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, statePath)
	rec = newReconciler(t, srcEP, dstEP, store, opts)
	seeded, err := rec.SeedFromDest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if seeded != 3 {
		t.Fatalf("seeded %d keys, want 3", seeded)
	}
	if sum := run(t, rec); sum.Copied != 0 {
		t.Fatalf("after state wipe + seed: copied %d, want 0", sum.Copied)
	}

	// Incremental: only new mail is copied.
	appendMsg(t, srcEP, srcFolder, rawMessage("<m5@test>", "five"), at(4))
	if sum := run(t, rec); sum.Copied != 1 {
		t.Fatalf("incremental: copied %d, want 1", sum.Copied)
	}

	// Crash window: message landed in dest but was never recorded locally;
	// the destination guard must catch it.
	m6 := rawMessage("<m6@test>", "six")
	appendMsg(t, dstEP, dstFolder, m6, at(5))
	appendMsg(t, srcEP, srcFolder, m6, at(5))
	if sum := run(t, rec); sum.Copied != 0 || sum.SkippedDup != 1 {
		t.Fatalf("dest guard: %+v, want 0 copied / 1 skipped", sum)
	}
	wantMids(t, dstEP, map[string][]string{dstFolder: {"", "<m1@test>", "<m2@test>", "<m5@test>", "<m6@test>"}})

	// UIDVALIDITY change: recreating the folder bumps UIDVALIDITY; the same
	// mail reappears under fresh UIDs next to one new message.
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
	appendMsg(t, srcEP, srcFolder, rawMessage("<m1@test>", "one"), at(0))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m2@test>", "two"), at(1))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m7@test>", "seven"), at(6))

	rec = newReconciler(t, srcEP, dstEP, store, opts)
	sum := run(t, rec)
	if !sum.UIDValidityChanged {
		t.Fatal("UIDVALIDITY change not detected")
	}
	if sum.Copied != 1 || sum.SkippedDup != 2 {
		t.Fatalf("after UIDVALIDITY reset: %+v, want 1 copied / 2 skipped", sum)
	}
	if v, err := store.UIDValidity(); err != nil || v == prevValidity {
		t.Fatalf("stored UIDVALIDITY = %d (%v), want updated from %d", v, err, prevValidity)
	}
	wantMids(t, dstEP, map[string][]string{dstFolder: {"", "<m1@test>", "<m2@test>", "<m5@test>", "<m6@test>", "<m7@test>"}})
}

// TestFlagPolicy: only \Seen is carried to the destination.
func TestFlagPolicy(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t)
	appendMsg(t, srcEP, srcFolder, rawMessage("<seen@test>", "seen"), at(0), imap.FlagSeen, imap.FlagFlagged, "custom")
	appendMsg(t, srcEP, srcFolder, rawMessage("<unseen@test>", "unseen"), at(1), imap.FlagFlagged)

	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	run(t, newReconciler(t, srcEP, dstEP, store, reconcile.Options{CarrySeen: true}))

	for _, m := range metasIn(t, dstEP, dstFolder) {
		wantSeen := m.MessageID == "<seen@test>"
		for _, f := range m.Flags {
			if f != imap.FlagSeen {
				t.Errorf("%s: flag %s leaked to destination", m.MessageID, f)
			}
		}
		if slices.Contains(m.Flags, imap.FlagSeen) != wantSeen {
			t.Errorf("%s: flags %v, want \\Seen=%t", m.MessageID, m.Flags, wantSeen)
		}
	}
}

// TestLabelSyncEndToEnd: label-folder membership -> destination keywords.
func TestLabelSyncEndToEnd(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, "Work", "Friends/Close", "Ignored")

	// m1: Work + Friends/Close (+ an excluded folder); m2: Work; m3: none.
	m1 := rawMessage("<m1@test>", "one")
	m2 := rawMessage("<m2@test>", "two")
	appendMsg(t, srcEP, srcFolder, m1, at(0))
	appendMsg(t, srcEP, srcFolder, m2, at(1))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m3@test>", "three"), at(2))
	appendMsg(t, srcEP, "Work", m1, at(0))
	appendMsg(t, srcEP, "Friends/Close", m1, at(0))
	appendMsg(t, srcEP, "Ignored", m1, at(0))
	appendMsg(t, srcEP, "Work", m2, at(1))

	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	rec := newReconciler(t, srcEP, dstEP, store, reconcile.Options{
		DestGuard:     true,
		SyncLabels:    true,
		SourceFolder:  srcFolder,
		LabelExclude:  []string{"Ignored"},
		KeywordPrefix: "$label:",
	})
	if sum := run(t, rec); sum.Copied != 3 || sum.KeywordsSet != 2 {
		t.Fatalf("first sync: %+v, want 3 copied / 2 keywords set", sum)
	}
	got := keywordsIn(t, dstEP, dstFolder)
	wantKeywords(t, got, "<m1@test>", "$label:friends_close", "$label:work")
	wantKeywords(t, got, "<m2@test>", "$label:work")
	wantKeywords(t, got, "<m3@test>")

	if sum := run(t, rec); sum.Copied != 0 {
		t.Fatalf("re-run copied %d", sum.Copied)
	}

	// New labeled mail is mirrored with its keyword.
	m4 := rawMessage("<m4@test>", "four")
	appendMsg(t, srcEP, "Work", m4, at(3))
	appendMsg(t, srcEP, srcFolder, m4, at(3))
	if sum := run(t, rec); sum.Copied != 1 {
		t.Fatalf("incremental copied %d, want 1", sum.Copied)
	}
	wantKeywords(t, keywordsIn(t, dstEP, dstFolder), "<m4@test>", "$label:work")
}

// TestLabelPropagationEndToEnd: post-copy label changes become keyword
// deltas; manually set keywords are never touched.
func TestLabelPropagationEndToEnd(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, "Work")
	m1 := rawMessage("<m1@test>", "one")
	appendMsg(t, srcEP, srcFolder, m1, at(0))

	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	rec := newReconciler(t, srcEP, dstEP, store, reconcile.Options{
		DestGuard:      true,
		SyncLabels:     true,
		LabelPropagate: true,
		SourceFolder:   srcFolder,
	})
	run(t, rec)
	wantKeywords(t, keywordsIn(t, dstEP, dstFolder), "<m1@test>")

	// A manual tag on the destination copy.
	dst, err := imapx.Dial(dstEP)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if found, err := dst.StoreKeywordByMessageID(dstFolder, "<m1@test>", true, "mytag"); err != nil || !found {
		t.Fatalf("manual tag setup: %v %v", found, err)
	}

	// Label added at the source after mirroring -> keyword appears.
	appendMsg(t, srcEP, "Work", m1, at(0))
	if sum := run(t, rec); sum.KeywordsUpdated != 1 {
		t.Fatalf("keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	wantKeywords(t, keywordsIn(t, dstEP, dstFolder), "<m1@test>", "mytag", "work")

	// Label removed -> keyword removed, manual tag kept.
	expunge(t, srcEP, "Work", "<m1@test>")
	if sum := run(t, rec); sum.KeywordsUpdated != 1 {
		t.Fatalf("keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	wantKeywords(t, keywordsIn(t, dstEP, dstFolder), "<m1@test>", "mytag")
}

func TestArchiveRoutingEndToEnd(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, srcInbox)
	m1 := rawMessage("<m1@test>", "in inbox")
	m2 := rawMessage("<m2@test>", "archived")
	appendMsg(t, srcEP, srcFolder, m1, at(0))
	appendMsg(t, srcEP, srcFolder, m2, at(1))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m3@test>", "sent-only"), at(2))
	appendMsg(t, srcEP, srcInbox, m1, at(0))

	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	rec := newReconciler(t, srcEP, dstEP, store, routingOpts())

	// Initial routing: inbox mail -> Mirror, everything else -> archive.
	if sum := run(t, rec); sum.Copied != 3 {
		t.Fatalf("copied %d, want 3", sum.Copied)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  {"<m1@test>"},
		dstArchive: {"<m2@test>", "<m3@test>"},
	})

	// Archived in the source -> destination copy moves to archive.
	expunge(t, srcEP, srcInbox, "<m1@test>")
	if sum := run(t, rec); sum.MovedToArchive != 1 {
		t.Fatalf("moved_to_archive = %d, want 1 (%+v)", sum.MovedToArchive, sum)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  nil,
		dstArchive: {"<m1@test>", "<m2@test>", "<m3@test>"},
	})

	// Moved back to the source inbox -> destination copy moves back.
	appendMsg(t, srcEP, srcInbox, m2, at(1))
	if sum := run(t, rec); sum.MovedToInbox != 1 {
		t.Fatalf("moved_to_inbox = %d, want 1 (%+v)", sum.MovedToInbox, sum)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  {"<m2@test>"},
		dstArchive: {"<m1@test>", "<m3@test>"},
	})

	if sum := run(t, rec); *sum != (reconcile.Summary{}) {
		t.Fatalf("re-run not a no-op: %+v", sum)
	}
}

// TestSentRoutingEndToEnd: three buckets with inbox > sent > archive
// priority, including a mail-to-self that is later archived.
func TestSentRoutingEndToEnd(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, srcInbox, srcSent)
	m1 := rawMessage("<m1@test>", "in inbox")
	m2 := rawMessage("<m2@test>", "sent")
	m4 := rawMessage("<m4@test>", "to self")
	appendMsg(t, srcEP, srcFolder, m1, at(0))
	appendMsg(t, srcEP, srcFolder, m2, at(1))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m3@test>", "archived"), at(2))
	appendMsg(t, srcEP, srcFolder, m4, at(3))
	appendMsg(t, srcEP, srcInbox, m1, at(0))
	appendMsg(t, srcEP, srcInbox, m4, at(3))
	appendMsg(t, srcEP, srcSent, m2, at(1))
	appendMsg(t, srcEP, srcSent, m4, at(3))

	opts := routingOpts()
	opts.SentRouting = true
	opts.SentSrcFolder = srcSent
	opts.SentFolder = dstSent
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	rec := newReconciler(t, srcEP, dstEP, store, opts)

	if sum := run(t, rec); sum.Copied != 4 {
		t.Fatalf("copied %d, want 4", sum.Copied)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  {"<m1@test>", "<m4@test>"},
		dstSent:    {"<m2@test>"},
		dstArchive: {"<m3@test>"},
	})

	// Mail-to-self archived: leaves the inbox, stays in Sent.
	expunge(t, srcEP, srcInbox, "<m4@test>")
	if sum := run(t, rec); sum.MovedToSent != 1 {
		t.Fatalf("moved_to_sent = %d, want 1 (%+v)", sum.MovedToSent, sum)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  {"<m1@test>"},
		dstSent:    {"<m2@test>", "<m4@test>"},
		dstArchive: {"<m3@test>"},
	})
}

// TestSentFolderWithLabels: a sent folder named explicitly (no \Sent
// attribute — imapmemserver has no SPECIAL-USE) must stay a routing folder
// when label sync is on, not double as a label.
func TestSentFolderWithLabels(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, srcInbox, srcSent)
	m1 := rawMessage("<m1@test>", "sent")
	appendMsg(t, srcEP, srcFolder, m1, at(0))
	appendMsg(t, srcEP, srcSent, m1, at(0))

	opts := routingOpts()
	opts.SentRouting = true
	opts.SentSrcFolder = srcSent
	opts.SentFolder = dstSent
	opts.SyncLabels = true
	opts.LabelPropagate = true
	opts.SourceFolder = srcFolder
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	rec := newReconciler(t, srcEP, dstEP, store, opts)
	run(t, rec)
	wantMids(t, dstEP, map[string][]string{dstSent: {"<m1@test>"}})

	// Removed from Sent (kept in All Mail) -> moves to archive.
	expunge(t, srcEP, srcSent, "<m1@test>")
	if sum := run(t, rec); sum.MovedToArchive != 1 || sum.KeywordsUpdated != 0 {
		t.Fatalf("%+v, want 1 moved to archive / 0 keyword updates", sum)
	}

	// Back in Sent -> moves back, without a "sent" label keyword.
	appendMsg(t, srcEP, srcSent, m1, at(0))
	if sum := run(t, rec); sum.MovedToSent != 1 || sum.KeywordsUpdated != 0 {
		t.Fatalf("%+v, want 1 moved to sent / 0 keyword updates", sum)
	}
	wantKeywords(t, keywordsIn(t, dstEP, dstSent), "<m1@test>")
}

// TestBackfillAfterUpgrade: mail mirrored without routing is sorted into the
// right folders once routing is enabled on the same state db.
func TestBackfillAfterUpgrade(t *testing.T) {
	t.Parallel()
	srcEP, dstEP, _ := setup(t, srcInbox)
	m1 := rawMessage("<m1@test>", "in inbox")
	appendMsg(t, srcEP, srcFolder, m1, at(0))
	appendMsg(t, srcEP, srcFolder, rawMessage("<m2@test>", "archived"), at(1))
	appendMsg(t, srcEP, srcInbox, m1, at(0))

	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	run(t, newReconciler(t, srcEP, dstEP, store, reconcile.Options{DestGuard: true}))
	wantMids(t, dstEP, map[string][]string{dstFolder: {"<m1@test>", "<m2@test>"}})

	rec := newReconciler(t, srcEP, dstEP, store, routingOpts())
	sum := run(t, rec)
	if sum.Copied != 0 || sum.MovedToArchive != 1 {
		t.Fatalf("backfill: %+v, want 0 copied / 1 moved to archive", sum)
	}
	wantMids(t, dstEP, map[string][]string{
		dstFolder:  {"<m1@test>"},
		dstArchive: {"<m2@test>"},
	})

	if sum := run(t, rec); *sum != (reconcile.Summary{}) {
		t.Fatalf("third run not a no-op: %+v", sum)
	}
}
