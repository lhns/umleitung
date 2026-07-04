package reconcile

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// routingSetup builds a source with an All-Mail-like main folder + INBOX,
// where m1 is in the inbox, m2 is archived (only in main), m3 is sent-only.
func routingSetup() (*fakeStore, *fakeSource, *fakeDest) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs: []fakeMsg{
			msg(1, "<m1@x>", "raw-1"), // in inbox
			msg(2, "<m2@x>", "raw-2"), // archived
			msg(3, "<m3@x>", "raw-3"), // sent-only
		},
		labelFolders: map[string]*fakeLabelFolder{
			"INBOX": {uidValidity: 70, msgs: []fakeMsg{msg(1, "<m1@x>", "raw-1")}},
		},
	}
	return store, src, newFakeDest()
}

func routingOpts() Options {
	return Options{ArchiveRouting: true, SyncLabels: false}
}

func TestArchiveRoutingAtCopyTime(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 3 {
		t.Fatalf("copied %d, want 3", sum.Copied)
	}
	if n := len(dst.inFolder(fakeDestFolder)); n != 1 {
		t.Fatalf("dest folder has %d, want 1 (inbox mail only)", n)
	}
	if n := len(dst.inFolder(fakeArchiveFolder)); n != 2 {
		t.Fatalf("archive has %d, want 2 (archived + sent-only)", n)
	}
	if has, _ := dst.HasMessageIDIn(fakeDestFolder, "<m1@x>"); !has {
		t.Fatal("inbox mail not in dest folder")
	}
	if has, _ := dst.HasMessageIDIn(fakeArchiveFolder, "<m2@x>"); !has {
		t.Fatal("archived mail not in archive folder")
	}
}

func TestArchivePropagationOut(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// User archives m1: it leaves the source INBOX.
	src.labelFolders["INBOX"].msgs = nil

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 1 {
		t.Fatalf("moved_to_archive = %d, want 1", sum.MovedToArchive)
	}
	if has, _ := dst.HasMessageIDIn(fakeArchiveFolder, "<m1@x>"); !has {
		t.Fatal("archived mail not moved to archive folder")
	}
	if n := len(dst.inFolder(fakeDestFolder)); n != 0 {
		t.Fatalf("dest folder still has %d", n)
	}

	// Re-run: stable, no further moves.
	movesBefore := dst.moves
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dst.moves != movesBefore || sum.MovedToArchive != 0 {
		t.Fatalf("re-run performed moves: %+v", sum)
	}
	if dst.total() != 3 {
		t.Fatalf("total dest messages = %d, want 3 (no dupes)", dst.total())
	}
}

func TestArchivePropagationBackToInbox(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// User moves archived m2 back to the inbox.
	src.labelFolders["INBOX"].msgs = append(src.labelFolders["INBOX"].msgs, msg(2, "<m2@x>", "raw-2"))

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToInbox != 1 {
		t.Fatalf("moved_to_inbox = %d, want 1", sum.MovedToInbox)
	}
	if has, _ := dst.HasMessageIDIn(fakeDestFolder, "<m2@x>"); !has {
		t.Fatal("un-archived mail not moved back to dest folder")
	}
	if dst.total() != 3 {
		t.Fatalf("total = %d, want 3", dst.total())
	}
}

func TestManualRefileRespected(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// User manually moved the dest copy of m1 somewhere else entirely.
	moved, err := dst.MoveMessageID(fakeDestFolder, "Custom/Elsewhere", "<m1@x>")
	if err != nil || !moved {
		t.Fatal("test setup: manual refile failed")
	}
	movesBefore := dst.moves

	// m1 is archived at the source; propagation cannot find it -> skip, no error.
	src.labelFolders["INBOX"].msgs = nil
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 0 || dst.moves != movesBefore {
		t.Fatalf("propagation chased a manually refiled message: %+v", sum)
	}
	// The pending op must be consumed (not retried forever).
	if ops, _ := store.PendingOps(10); len(ops) != 0 {
		t.Fatalf("pending not drained after not-found: %+v", ops)
	}
}

func TestSynthesizedKeySkipsPropagation(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "", "raw-nomid")}, // no Message-ID
		labelFolders: map[string]*fakeLabelFolder{
			"INBOX": {uidValidity: 70, msgs: []fakeMsg{msg(1, "", "raw-nomid")}},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Copy-time routing applies (inbox member -> dest folder).
	if n := len(dst.inFolder(fakeDestFolder)); n != 1 {
		t.Fatalf("dest folder has %d, want 1", n)
	}

	// Archive it: no pending op may be enqueued (unlocatable on dest).
	src.labelFolders["INBOX"].msgs = nil
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 0 {
		t.Fatal("synthesized-key message was propagated")
	}
	if ops, _ := store.PendingOps(10); len(ops) != 0 {
		t.Fatalf("pending enqueued for synthesized key: %+v", ops)
	}
}

func TestLabelPropagation(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<m1@x>", "raw-1")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: nil},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{SyncLabels: true, LabelPropagate: true, SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if flags := dst.inFolder(fakeDestFolder)[0].flags; len(flags) != 0 {
		t.Fatalf("unlabeled message has flags %v", flags)
	}

	// Label added AFTER the copy -> +FLAGS on dest.
	src.labelFolders["Work"].msgs = []fakeMsg{msg(1, "<m1@x>", "raw-1")}
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 1 {
		t.Fatalf("keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	if flags := dst.inFolder(fakeDestFolder)[0].flags; !slices.Contains(flags, imap.Flag("work")) {
		t.Fatalf("keyword not stored: %v", flags)
	}

	// Manually-set dest keyword must never be touched.
	dst.inFolder(fakeDestFolder)[0].flags = append(dst.inFolder(fakeDestFolder)[0].flags, "mytag")

	// Label removed -> -FLAGS, and only the changed keyword.
	src.labelFolders["Work"].msgs = nil
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 1 {
		t.Fatalf("keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	flags := dst.inFolder(fakeDestFolder)[0].flags
	if slices.Contains(flags, imap.Flag("work")) {
		t.Fatalf("removed label's keyword still present: %v", flags)
	}
	if !slices.Contains(flags, imap.Flag("mytag")) {
		t.Fatalf("manually-set keyword was removed: %v", flags)
	}
}

// With a keyword prefix (Bulwark), copy-time labels, propagation, and the
// backfill-driven re-tag of already-mirrored mail all use $label:<slug>.
func TestKeywordPrefixEndToEnd(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<m1@x>", "raw-1"), msg(2, "<m2@x>", "raw-2")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<m1@x>", "raw-1")}},
		},
	}
	dst := newFakeDest()
	opts := Options{SyncLabels: true, LabelPropagate: true, SourceFolder: fakeMainFolder, KeywordPrefix: "$label:"}
	rec := newRec(store, src, dst, opts)

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Copy-time: m1 labeled Work -> $label:work; counted in KeywordsSet.
	if sum.KeywordsSet != 1 {
		t.Fatalf("keywords_set = %d, want 1", sum.KeywordsSet)
	}
	m1 := findDstMsg(dst, "<m1@x>")
	if !slices.Contains(m1.flags, imap.Flag("$label:work")) {
		t.Fatalf("copy-time keyword = %v, want $label:work", m1.flags)
	}
	if bare := findDstMsg(dst, "<m1@x>"); slices.Contains(bare.flags, imap.Flag("work")) {
		t.Fatalf("bare keyword leaked: %v", bare.flags)
	}

	// Propagation: add label to m2 after copy -> $label:work via STORE.
	src.labelFolders["Work"].msgs = append(src.labelFolders["Work"].msgs, msg(2, "<m2@x>", "raw-2"))
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m2 := findDstMsg(dst, "<m2@x>"); !slices.Contains(m2.flags, imap.Flag("$label:work")) {
		t.Fatalf("propagated keyword = %v, want $label:work", m2.flags)
	}
}

// Switching the keyword prefix on an existing mirror re-tags already-mirrored
// mail via the backfill (fingerprint change), add-only (old keyword kept).
func TestKeywordPrefixBackfillRetagsExisting(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<m1@x>", "raw-1")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<m1@x>", "raw-1")}},
		},
	}
	dst := newFakeDest()

	// Phase 1: bare keyword.
	rec := newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := findDstMsg(dst, "<m1@x>"); !slices.Contains(m.flags, imap.Flag("work")) {
		t.Fatalf("phase 1 keyword = %v, want work", m.flags)
	}

	// Phase 2: add prefix -> fingerprint changes -> backfill adds $label:work.
	rec = newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder, KeywordPrefix: "$label:"})
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 1 {
		t.Fatalf("backfill keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	m := findDstMsg(dst, "<m1@x>")
	if !slices.Contains(m.flags, imap.Flag("$label:work")) {
		t.Fatalf("post-backfill keyword missing: %v", m.flags)
	}
	if !slices.Contains(m.flags, imap.Flag("work")) {
		t.Fatalf("add-only violated, old keyword removed: %v", m.flags)
	}

	// Phase 3: same config -> no more backfill work.
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 0 {
		t.Fatalf("backfill re-ran: keywords_updated = %d", sum.KeywordsUpdated)
	}
}

func TestLabelPropagationDisabledMeansNoStores(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<m1@x>", "raw-1")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: nil},
		},
	}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{SyncLabels: true, LabelPropagate: false, SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.labelFolders["Work"].msgs = []fakeMsg{msg(1, "<m1@x>", "raw-1")}
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 0 {
		t.Fatal("keywords updated despite LABEL_PROPAGATE=false")
	}
	if ops, _ := store.PendingOps(10); len(ops) != 0 {
		t.Fatalf("pending enqueued despite LABEL_PROPAGATE=false: %+v", ops)
	}
}

// The user requirement: deltas are tracked against the stored previous state
// and survive destination failures — applied on a later run, never lost,
// never re-applied after success.
func TestDeltaDurabilityAcrossFailures(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// m1 leaves the inbox, but the destination is down for the propagation
	// phase: simulate by breaking Move via a poisoned dest.
	src.labelFolders["INBOX"].msgs = nil
	poisoned := &failingMoveDest{fakeDest: dst}
	recPoisoned := newRec(store, src, poisoned, routingOpts())
	if _, err := recPoisoned.Run(context.Background()); err == nil {
		t.Fatal("want error from failed move")
	}
	// The delta survives as a pending op.
	ops, _ := store.PendingOps(10)
	if len(ops) != 1 || ops[0].Kind != "move" || ops[0].Op != "remove" {
		t.Fatalf("pending after failure = %+v, want one move-remove", ops)
	}

	// Next run with a healthy dest: delta applied, queue drained.
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 1 {
		t.Fatalf("delta not applied after recovery: %+v", sum)
	}
	if ops, _ := store.PendingOps(10); len(ops) != 0 {
		t.Fatalf("queue not drained: %+v", ops)
	}
}

type failingMoveDest struct{ *fakeDest }

func (d *failingMoveDest) MoveMessageID(from, to, mid string) (bool, error) {
	return false, fmt.Errorf("dest down (injected)")
}

func TestWatchedFolderUIDValidityReset(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	movesBefore := dst.moves

	// INBOX UIDVALIDITY reset, same membership (fresh UIDs).
	src.labelFolders["INBOX"].uidValidity = 999
	src.labelFolders["INBOX"].msgs = []fakeMsg{msg(41, "<m1@x>", "raw-1")}
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dst.moves != movesBefore || sum.MovedToArchive != 0 || sum.MovedToInbox != 0 {
		t.Fatalf("spurious moves after UIDVALIDITY reset: %+v", sum)
	}

	// Reset again, this time m1 really left the inbox -> detected by key diff.
	src.labelFolders["INBOX"].uidValidity = 1000
	src.labelFolders["INBOX"].msgs = nil
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 1 {
		t.Fatalf("removal across UIDVALIDITY reset missed: %+v", sum)
	}
}

// sentSetup: m1 in inbox, m2 sent-only, m3 neither (archived), m4 in inbox
// AND sent (mail to self).
func sentSetup() (*fakeStore, *fakeSource, *fakeDest) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs: []fakeMsg{
			msg(1, "<m1@x>", "raw-1"),
			msg(2, "<m2@x>", "raw-2"),
			msg(3, "<m3@x>", "raw-3"),
			msg(4, "<m4@x>", "raw-4"),
		},
		labelFolders: map[string]*fakeLabelFolder{
			"INBOX":   {uidValidity: 70, msgs: []fakeMsg{msg(1, "<m1@x>", "raw-1"), msg(4, "<m4@x>", "raw-4")}},
			"SENTSRC": {uidValidity: 71, msgs: []fakeMsg{msg(2, "<m2@x>", "raw-2"), msg(4, "<m4@x>", "raw-4")}},
		},
	}
	return store, src, newFakeDest()
}

func sentOpts() Options {
	return Options{ArchiveRouting: true, SentRouting: true}
}

func TestSentRoutingAtCopyTimeWithPriority(t *testing.T) {
	store, src, dst := sentSetup()
	rec := newRec(store, src, dst, sentOpts())
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 4 {
		t.Fatalf("copied %d, want 4", sum.Copied)
	}
	// m1 inbox -> DEST; m2 sent-only -> SENT; m3 -> ARCHIVE;
	// m4 inbox+sent -> DEST (inbox wins).
	for mid, folder := range map[string]string{
		"<m1@x>": fakeDestFolder, "<m2@x>": fakeSentFolder,
		"<m3@x>": fakeArchiveFolder, "<m4@x>": fakeDestFolder,
	} {
		if has, _ := dst.HasMessageIDIn(folder, mid); !has {
			t.Errorf("%s not in %s", mid, folder)
		}
	}
	if dst.total() != 4 {
		t.Fatalf("total = %d, want 4", dst.total())
	}
}

func TestSentPropagationOnArchiveOfSelfMail(t *testing.T) {
	store, src, dst := sentSetup()
	rec := newRec(store, src, dst, sentOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// User archives the mail-to-self m4: it leaves INBOX but stays in Sent
	// -> desired bucket becomes the Sent folder.
	src.labelFolders["INBOX"].msgs = src.labelFolders["INBOX"].msgs[:1] // keep only m1
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToSent != 1 {
		t.Fatalf("moved_to_sent = %d, want 1 (%+v)", sum.MovedToSent, sum)
	}
	if has, _ := dst.HasMessageIDIn(fakeSentFolder, "<m4@x>"); !has {
		t.Fatal("m4 not moved to sent folder")
	}
	if dst.total() != 4 {
		t.Fatalf("total = %d, want 4", dst.total())
	}
}

// Backfill: sent mail that landed in Archive under archive-only routing moves
// to the Sent folder when sent routing is enabled later.
func TestBackfillMovesSentOutOfArchive(t *testing.T) {
	store, src, dst := sentSetup()

	// Phase 1: archive routing only — sent-only m2 lands in ARCHIVE.
	rec := newRec(store, src, dst, Options{ArchiveRouting: true, SourceInbox: "INBOX"})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if has, _ := dst.HasMessageIDIn(fakeArchiveFolder, "<m2@x>"); !has {
		t.Fatal("setup: m2 not in archive")
	}

	// Phase 2: sent routing enabled — backfill sorts m2 (and m4 stays DEST).
	rec = newRec(store, src, dst, sentOpts())
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToSent != 1 {
		t.Fatalf("backfill moved_to_sent = %d, want 1 (%+v)", sum.MovedToSent, sum)
	}
	if has, _ := dst.HasMessageIDIn(fakeSentFolder, "<m2@x>"); !has {
		t.Fatal("m2 not in sent folder after backfill")
	}
	if dst.total() != 4 {
		t.Fatalf("total = %d, want 4 (no dupes)", dst.total())
	}
}

// Sent-source membership must never leak into label keywords.
func TestSentFolderIsNotALabel(t *testing.T) {
	store, src, dst := sentSetup()
	opts := sentOpts()
	opts.SyncLabels = true
	opts.SourceFolder = fakeMainFolder
	rec := newRec(store, src, dst, opts)
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, m := range dst.inFolder(fakeSentFolder) {
		for _, f := range m.flags {
			if f == "sentsrc" || f == "inbox" {
				t.Fatalf("routing folder leaked as keyword: %v", m.flags)
			}
		}
	}
}

// Upgrade auto-correction: mail mirrored WITHOUT routing gets sorted into the
// right folders when routing is enabled (placement backfill).
func TestBackfillAfterEnablingRouting(t *testing.T) {
	store, src, dst := routingSetup()

	// Phase 1: old config — no routing, everything lands in the dest folder.
	rec := newRec(store, src, dst, Options{})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(dst.inFolder(fakeDestFolder)); n != 3 {
		t.Fatalf("pre-upgrade: dest folder has %d, want 3", n)
	}

	// Phase 2: upgrade — routing enabled. Backfill must move the two
	// non-inbox messages to the archive.
	rec = newRec(store, src, dst, routingOpts())
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.MovedToArchive != 2 {
		t.Fatalf("backfill moved %d, want 2", sum.MovedToArchive)
	}
	if n := len(dst.inFolder(fakeDestFolder)); n != 1 {
		t.Fatalf("post-backfill: dest folder has %d, want 1", n)
	}
	if n := len(dst.inFolder(fakeArchiveFolder)); n != 2 {
		t.Fatalf("post-backfill: archive has %d, want 2", n)
	}
	if dst.total() != 3 {
		t.Fatalf("total = %d, want 3 (no dupes)", dst.total())
	}

	// Phase 3: same config again — fingerprint stored, backfill is a no-op.
	movesBefore := dst.moves
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dst.moves != movesBefore || sum.MovedToArchive != 0 {
		t.Fatalf("backfill re-ran on unchanged config: %+v", sum)
	}
}

// Keyword backfill is add-only: missing label keywords are added, but
// keywords without a matching label (user tags / stale labels) are kept.
func TestBackfillKeywordsAddOnly(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{
		uidValidity: 7,
		msgs:        []fakeMsg{msg(1, "<m1@x>", "raw-1")},
		labelFolders: map[string]*fakeLabelFolder{
			"Work": {uidValidity: 71, msgs: []fakeMsg{msg(1, "<m1@x>", "raw-1")}},
		},
	}
	dst := newFakeDest()

	// Phase 1: mirrored without label sync.
	rec := newRec(store, src, dst, Options{SourceFolder: fakeMainFolder})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// User sets a manual tag on the dest copy in the meantime.
	dst.inFolder(fakeDestFolder)[0].flags = append(dst.inFolder(fakeDestFolder)[0].flags, "mytag")

	// Phase 2: label sync enabled -> backfill adds the missing keyword.
	rec = newRec(store, src, dst, Options{SyncLabels: true, SourceFolder: fakeMainFolder})
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeywordsUpdated != 1 {
		t.Fatalf("keywords_updated = %d, want 1", sum.KeywordsUpdated)
	}
	flags := dst.inFolder(fakeDestFolder)[0].flags
	if !slices.Contains(flags, imap.Flag("work")) {
		t.Fatalf("missing keyword not backfilled: %v", flags)
	}
	if !slices.Contains(flags, imap.Flag("mytag")) {
		t.Fatalf("user tag removed by backfill: %v", flags)
	}
}

// Routing never creates a second copy even when the dest copy moved folders:
// the guard checks both folders.
func TestGuardChecksBothFoldersUnderRouting(t *testing.T) {
	store, src, dst := routingSetup()
	rec := newRec(store, src, dst, routingOpts())
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Simulate state loss (keys wiped) but keep members knowledge minimal:
	// m1 is in the inbox, its dest copy sits in the DEST folder; m2's copy
	// sits in ARCHIVE. Wipe the copied set — the guard must find both.
	store.keys = map[string]bool{}
	store.meta = map[string]string{} // fingerprint too: backfill idempotent anyway
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 {
		t.Fatalf("guard failed, copied %d duplicates", sum.Copied)
	}
	if dst.total() != 3 {
		t.Fatalf("total = %d, want 3", dst.total())
	}
}
