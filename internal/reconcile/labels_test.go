package reconcile

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
)

func TestKeywordForSanitization(t *testing.T) {
	cases := []struct{ label, repl, want string }{
		{"Work", "_", "work"},
		{"[Werbung]", "_", "werbung"},          // brackets trimmed
		{"Werbung", "_", "werbung"},            // collides with the above
		{"Bücher", "_", "b_cher"},              // umlaut
		{"Wichtige Mails", "_", "wichtige_mails"},
		{"Work/Projects", "_", "work_projects"}, // hierarchy delimiter
		{"$Important", "_", "important"},
		{`\Seen`, "_", "seen"},
		{"a  b//c", "_", "a_b_c"},               // runs collapsed
		{"---", "_", "---"},                     // dashes kept literally
		{"...", "_", ""},                        // nothing survives -> skip
		{"", "_", ""},
		// Dash replacement (Bulwark convention).
		{"Wichtige Mails", "-", "wichtige-mails"},
		{"Work/Projects", "-", "work-projects"},
		{"Bücher", "-", "b-cher"},
		{"a  b//c", "-", "a-b-c"},               // runs collapsed to one dash
		{"Work_Sub", "-", "work_sub"},           // underscore kept literally
	}
	for _, c := range cases {
		if got := keywordFor(c.label, c.repl); got != c.want {
			t.Errorf("keywordFor(%q, %q) = %q, want %q", c.label, c.repl, got, c.want)
		}
	}
	if keywordFor("[Werbung]", "_") != keywordFor("Werbung", "_") {
		t.Error("bracket collision case changed — update docs")
	}
}

func TestKeywordFlagsDedupAndSkip(t *testing.T) {
	rec := &Reconciler{} // no prefix
	flags := rec.labelKeywords([]string{"[Werbung]", "Werbung", "...", "Work"})
	want := []imap.Flag{"werbung", "work"}
	if !slices.Equal(flags, want) {
		t.Fatalf("labelKeywords = %v, want %v", flags, want)
	}

	// With a prefix (Bulwark): $label:<slug>, still deduped.
	rec = &Reconciler{opts: Options{KeywordPrefix: "$label:"}}
	pref := rec.labelKeywords([]string{"Work", "work", "..."})
	if !slices.Equal(pref, []imap.Flag{"$label:work"}) {
		t.Fatalf("prefixed = %v, want [$label:work]", pref)
	}
}

func TestLabelFolderExclusion(t *testing.T) {
	exclude := map[string]bool{"Skipped": true}
	cases := []struct {
		f    imapx.FolderInfo
		want bool
	}{
		{imapx.FolderInfo{Name: "Work"}, true},
		{imapx.FolderInfo{Name: "Friends/Close"}, true},
		{imapx.FolderInfo{Name: "AllMail"}, false},  // source folder
		{imapx.FolderInfo{Name: "INBOX"}, false},    // inbox membership is not a label
		{imapx.FolderInfo{Name: "inbox"}, false},    // case-insensitive
		{imapx.FolderInfo{Name: "Skipped"}, false},  // LABEL_EXCLUDE
		{imapx.FolderInfo{Name: "Parent", Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}}, false},
		{imapx.FolderInfo{Name: "Sent", Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}}, false},
		{imapx.FolderInfo{Name: "Trash", Attrs: []imap.MailboxAttr{imap.MailboxAttrTrash}}, false},
		{imapx.FolderInfo{Name: "Spam", Attrs: []imap.MailboxAttr{imap.MailboxAttrJunk}}, false},
		{imapx.FolderInfo{Name: "Everything", Attrs: []imap.MailboxAttr{imap.MailboxAttrAll}}, false},
		{imapx.FolderInfo{Name: "Wichtig", Attrs: []imap.MailboxAttr{imap.MailboxAttrImportant}}, false},
		{imapx.FolderInfo{Name: "Starred", Attrs: []imap.MailboxAttr{imap.MailboxAttrFlagged}}, false},
		{imapx.FolderInfo{Name: "Drafts", Attrs: []imap.MailboxAttr{imap.MailboxAttrDrafts}}, false},
	}
	for _, c := range cases {
		if got := isLabelFolder(c.f, "AllMail", exclude); got != c.want {
			t.Errorf("isLabelFolder(%q %v) = %v, want %v", c.f.Name, c.f.Attrs, got, c.want)
		}
	}
}

func TestMirrorAppendsLabelKeywords(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs: []fakeMsg{
			msg(1, "<a@x>", "raw-a"), // labeled Work + Friends/Close
			msg(2, "<b@x>", "raw-b"), // unlabeled
		},
		labelFolders: map[string]*fakeLabelFolder{
			"Work":          {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
			"Friends/Close": {uidValidity: 72, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
			"Sent":          {uidValidity: 73, msgs: []fakeMsg{msg(1, "<b@x>", "raw-b")}, attrs: []imap.MailboxAttr{imap.MailboxAttrSent}},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 2 {
		t.Fatalf("copied %d, want 2", sum.Copied)
	}
	if want := []imap.Flag{"friends_close", "work"}; !slices.Equal(dst.appendedFlags[0], want) {
		t.Fatalf("labeled message flags = %v, want %v", dst.appendedFlags[0], want)
	}
	if len(dst.appendedFlags[1]) != 0 {
		t.Fatalf("unlabeled message got flags %v (Sent folder must be excluded)", dst.appendedFlags[1])
	}
}

func TestLabelScanResumable(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<a@x>", "raw-a")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, u, _ := store.FolderState("Work"); v != 71 || u != 1 {
		t.Fatalf("folder state = (%d, %d), want (71, 1)", v, u)
	}

	// Second run: label folder unchanged -> no meta fetches for it.
	src.metaCalls = 0
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.metaCalls != 0 {
		t.Fatalf("unchanged folders re-scanned (%d fetches)", src.metaCalls)
	}

	// UIDVALIDITY reset on the label folder -> full folder rescan, labels
	// table stays correct (idempotent).
	src.labelFolders["Work"].uidValidity = 99
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	labels, _ := store.MemberFolders("<a@x>")
	if !slices.Equal(labels, []string{"Work"}) {
		t.Fatalf("labels after rescan = %v, want [Work]", labels)
	}
	if v, _, _ := store.FolderState("Work"); v != 99 {
		t.Fatalf("folder uidvalidity not updated: %d", v)
	}
}

func TestKeywordAppendFallback(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<a@x>", "raw-a")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
		},
	}
	dst := newFakeDest()
	dst.rejectKeywords = true
	dst.noArbitraryKw = true // also exercises the warning path
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The message must be mirrored despite the keyword rejection: retried
	// once without keywords, recorded, no duplicate.
	if sum.Copied != 1 || len(dst.appended) != 1 {
		t.Fatalf("fallback failed: %+v appended=%d", sum, len(dst.appended))
	}
	if len(dst.appendedFlags[0]) != 0 {
		t.Fatalf("fallback append still carried flags: %v", dst.appendedFlags[0])
	}
	if !store.keys["<a@x>"] {
		t.Fatal("message not recorded after fallback append")
	}

	// Re-run: no duplicate append attempts.
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || len(dst.appended) != 1 {
		t.Fatalf("re-run after fallback duplicated: %+v", sum)
	}
}

func TestKeywordAppendNoRetryWhenAccepted(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<a@x>", "raw-a")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(dst.appended) != 1 {
		t.Fatalf("appended %d times, want exactly 1 (no spurious retry)", len(dst.appended))
	}
	if want := []imap.Flag{"work"}; !slices.Equal(dst.appendedFlags[0], want) {
		t.Fatalf("flags = %v, want %v", dst.appendedFlags[0], want)
	}
}

// A CONNECTION-level append failure must NOT trigger the keyword-less retry:
// the original append may have landed server-side, and retrying could
// duplicate. The pass aborts; the destination guard reconciles next run.
func TestKeywordRetryOnlyOnServerReject(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<a@x>", "raw-a")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
		},
	}
	dst := newFakeDest()
	dst.appendErr = fmt.Errorf("connection reset (injected)") // NOT a tagged server response
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want pass abort on connection-level append error")
	}
	if len(dst.appended) != 0 {
		t.Fatalf("retry appended despite ambiguous failure: %d", len(dst.appended))
	}
	if store.keys["<a@x>"] {
		t.Fatal("key recorded despite unconfirmed append")
	}
}

// With the destination guard disabled there is no re-detection layer, so the
// pipeline must degrade to strict per-message record flushing (crash window
// of one message, as before the batching optimizations).
func TestGuardOffDegradesToSynchronousRecords(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{
		msg(1, "<a@x>", "raw-a"), msg(2, "<b@x>", "raw-b"), msg(3, "<c@x>", "raw-c"),
	}}
	dst := newFakeDest()
	dst.failAppendAt = 3
	rec := newRec(store, src, dst, Options{DestGuard: false})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want error from failed append")
	}
	// Both successfully appended messages must ALREADY be recorded — no
	// batched records pending at crash time when the guard is off.
	if !store.keys["<a@x>"] || !store.keys["<b@x>"] {
		t.Fatalf("records not flushed per message with guard off: %v", store.keys)
	}
	if store.keys["<c@x>"] {
		t.Fatal("failed append recorded")
	}
}

// Guard: a failing append with keywords where the retry ALSO fails must
// surface the error and leave the key unrecorded (retryable).
func TestKeywordAppendFallbackBothFail(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<a@x>", "raw-a")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}},
		},
	}
	dst := newFakeDest()
	dst.appendErr = fmt.Errorf("append always fails (injected)")
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want error when both appends fail")
	}
	if store.keys["<a@x>"] {
		t.Fatal("key recorded despite total append failure")
	}
}
