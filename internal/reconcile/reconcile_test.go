package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/state"
)

// ---- fakes ----

type fakeStore struct {
	uidValidity uint32
	lastUID     uint32
	keys        map[string]bool
	failRecord  bool

	members      map[string]map[string]uint32 // folder -> key -> uid
	folderStates map[string][2]uint32         // folder -> {uidvalidity, last_uid}
	pending      []PendingOp
	nextPending  int64
	meta         map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		keys:         map[string]bool{},
		members:      map[string]map[string]uint32{},
		folderStates: map[string][2]uint32{},
		meta:         map[string]string{},
	}
}

func (s *fakeStore) UIDValidity() (uint32, error)  { return s.uidValidity, nil }
func (s *fakeStore) SetUIDValidity(v uint32) error { s.uidValidity = v; return nil }
func (s *fakeStore) LastUID() (uint32, error)      { return s.lastUID, nil }
func (s *fakeStore) SetLastUID(u uint32) error     { s.lastUID = u; return nil }
func (s *fakeStore) HasKey(k string) (bool, error) { return s.keys[k], nil }
func (s *fakeStore) RecordKey(k string, _ uint32, _ int64) error {
	if s.failRecord {
		return fmt.Errorf("record failed (injected)")
	}
	s.keys[k] = true
	return nil
}
func (s *fakeStore) CopiedCount() (int64, error) { return int64(len(s.keys)), nil }
func (s *fakeStore) SeedBatch(keys []string) error {
	for _, k := range keys {
		s.keys[k] = true
	}
	return nil
}
func (s *fakeStore) RecordKeys(records []state.KeyRecord) error {
	for _, rec := range records {
		if err := s.RecordKey(rec.Key, rec.UID, rec.CopiedAtUnix); err != nil {
			return err
		}
	}
	return nil
}
func (s *fakeStore) MemberChangeBatch(folder string, items []state.MemberChangeItem) error {
	for _, it := range items {
		if err := s.MemberChange(folder, it.Key, it.UID, it.Add, it.PendingKind); err != nil {
			return err
		}
	}
	return nil
}
func (s *fakeStore) DeletePendingBatch(ids []int64) error {
	for _, id := range ids {
		if err := s.DeletePending(id); err != nil {
			return err
		}
	}
	return nil
}
func (s *fakeStore) MemberChange(folder, key string, uid uint32, add bool, pendingKind string) error {
	if add {
		if s.members[folder] == nil {
			s.members[folder] = map[string]uint32{}
		}
		s.members[folder][key] = uid
	} else {
		delete(s.members[folder], key)
	}
	if pendingKind != "" {
		op := "remove"
		if add {
			op = "add"
		}
		s.nextPending++
		s.pending = append(s.pending, PendingOp{
			ID: s.nextPending, Kind: pendingKind, MessageID: key, Folder: folder, Op: op,
		})
	}
	return nil
}
func (s *fakeStore) MemberHas(folder, key string) (bool, error) {
	_, ok := s.members[folder][key]
	return ok, nil
}
func (s *fakeStore) MemberFolders(key string) ([]string, error) {
	var out []string
	for folder, keys := range s.members {
		if _, ok := keys[key]; ok {
			out = append(out, folder)
		}
	}
	sort.Strings(out)
	return out, nil
}
func (s *fakeStore) MemberUIDKeys(folder string) (map[uint32]string, error) {
	out := map[uint32]string{}
	for key, uid := range s.members[folder] {
		if uid > 0 {
			out[uid] = key
		}
	}
	return out, nil
}
func (s *fakeStore) MemberKeys(folder string) (map[string]bool, error) {
	out := map[string]bool{}
	for key := range s.members[folder] {
		out[key] = true
	}
	return out, nil
}
func (s *fakeStore) PendingOps(limit int) ([]PendingOp, error) {
	if len(s.pending) > limit {
		return append([]PendingOp{}, s.pending[:limit]...), nil
	}
	return append([]PendingOp{}, s.pending...), nil
}
func (s *fakeStore) DeletePending(id int64) error {
	for i, op := range s.pending {
		if op.ID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return nil
		}
	}
	return nil
}
func (s *fakeStore) MetaGet(key string) (string, error) { return s.meta[key], nil }
func (s *fakeStore) MetaSet(key, value string) error    { s.meta[key] = value; return nil }
func (s *fakeStore) FolderState(name string) (uint32, uint32, error) {
	st := s.folderStates[name]
	return st[0], st[1], nil
}
func (s *fakeStore) SetFolderState(name string, uidValidity, lastUID uint32) error {
	s.folderStates[name] = [2]uint32{uidValidity, lastUID}
	return nil
}

type fakeMsg struct {
	meta imapx.MsgMeta
	raw  string
}

const fakeMainFolder = "AllMail"

type fakeLabelFolder struct {
	uidValidity uint32
	msgs        []fakeMsg
	attrs       []imap.MailboxAttr
}

type fakeSource struct {
	uidValidity  uint32
	msgs         []fakeMsg // ascending UIDs (main folder)
	metaCalls    int
	failAfter    int                         // fail FetchMetaRange after N calls (0 = never)
	labelFolders map[string]*fakeLabelFolder // extra folders returned by LIST
	selected     string                      // currently selected folder ("" = main)
}

func uidNextOf(msgs []fakeMsg) uint32 {
	var maxUID imap.UID
	for _, m := range msgs {
		maxUID = max(maxUID, m.meta.UID)
	}
	return uint32(maxUID) + 1
}

func (f *fakeSource) uidNext() uint32 { return uidNextOf(f.msgs) }

func (f *fakeSource) SelectFolder() (uint32, uint32, uint32, error) {
	return f.SelectNamedFolder(fakeMainFolder)
}

func (f *fakeSource) SelectNamedFolder(name string) (uint32, uint32, uint32, error) {
	if name == fakeMainFolder {
		f.selected = ""
		return f.uidValidity, f.uidNext(), uint32(len(f.msgs)), nil
	}
	lf, ok := f.labelFolders[name]
	if !ok {
		return 0, 0, 0, fmt.Errorf("no such folder %q", name)
	}
	f.selected = name
	return lf.uidValidity, uidNextOf(lf.msgs), uint32(len(lf.msgs)), nil
}

func (f *fakeSource) ListFolders() ([]imapx.FolderInfo, error) {
	out := []imapx.FolderInfo{{Name: fakeMainFolder}}
	var names []string
	for name := range f.labelFolders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, imapx.FolderInfo{Name: name, Attrs: f.labelFolders[name].attrs})
	}
	return out, nil
}

func (f *fakeSource) selectedMsgs() []fakeMsg {
	if f.selected == "" {
		return f.msgs
	}
	return f.labelFolders[f.selected].msgs
}

func (f *fakeSource) FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error) {
	f.metaCalls++
	if f.failAfter > 0 && f.metaCalls > f.failAfter {
		return nil, fmt.Errorf("connection dropped (injected)")
	}
	var out []imapx.MsgMeta
	for _, m := range f.selectedMsgs() {
		if m.meta.UID >= start && m.meta.UID <= stop {
			out = append(out, m.meta)
		}
	}
	return out, nil
}

func (f *fakeSource) SearchAllUIDs() ([]imap.UID, error) {
	var out []imap.UID
	for _, m := range f.selectedMsgs() {
		out = append(out, m.meta.UID)
	}
	return out, nil
}

func (f *fakeSource) FetchFullStream(uids []imap.UID, fn func(*imapx.FullMessage) error) error {
	want := map[imap.UID]bool{}
	for _, u := range uids {
		want[u] = true
	}
	for _, m := range f.selectedMsgs() {
		if !want[m.meta.UID] {
			continue
		}
		err := fn(&imapx.FullMessage{
			UID: m.meta.UID, Raw: []byte(m.raw),
			Flags: m.meta.Flags, InternalDate: m.meta.InternalDate,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

const (
	fakeDestFolder    = "DEST"
	fakeArchiveFolder = "ARCHIVE"
	fakeSentFolder    = "SENT-DEST"
)

type destMsg struct {
	uid          uint32
	mid          string // Message-ID ("" = none, like the real message)
	raw          string
	flags        []imap.Flag
	internalDate time.Time // preserved by APPEND, like a real server
}

type fakeDest struct {
	folders map[string][]*destMsg
	nextUID map[string]uint32

	appended       []string      // raw bodies, in append order (all folders)
	appendedFlags  [][]imap.Flag // flags per append, parallel to appended
	appendedTo     []string      // folder per append
	moves          int
	appendErr      error
	failAppendAt   int // fail the Nth append (1-based; 0 = never)
	rejectKeywords bool // reject any APPEND carrying non-\Seen flags
	noArbitraryKw  bool
	selected       string
	guardSearches  int // SearchMessageIDsIn call count
}

func newFakeDest() *fakeDest {
	return &fakeDest{folders: map[string][]*destMsg{}, nextUID: map[string]uint32{}}
}

// addExisting places a message directly into a dest folder (simulating prior
// content, e.g. the crash window or a pre-populated mailbox).
func (d *fakeDest) addExisting(folder, mid, raw string) {
	d.nextUID[folder]++
	d.folders[folder] = append(d.folders[folder], &destMsg{uid: d.nextUID[folder], mid: mid, raw: raw})
}

func (d *fakeDest) total() int {
	n := 0
	for _, msgs := range d.folders {
		n += len(msgs)
	}
	return n
}

func (d *fakeDest) inFolder(folder string) []*destMsg { return d.folders[folder] }

// findDstMsg returns the destination message with the given Message-ID from
// whichever folder holds it (test helper).
func findDstMsg(d *fakeDest, mid string) *destMsg {
	for _, msgs := range d.folders {
		for _, m := range msgs {
			if m.mid == mid {
				return m
			}
		}
	}
	return nil
}

func (d *fakeDest) SelectNamedFolder(name string) (uint32, uint32, uint32, error) {
	d.selected = name
	return 1, d.nextUID[name] + 1, uint32(len(d.folders[name])), nil
}
func (d *fakeDest) FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error) {
	var out []imapx.MsgMeta
	for _, m := range d.folders[d.selected] {
		if imap.UID(m.uid) >= start && imap.UID(m.uid) <= stop {
			out = append(out, imapx.MsgMeta{
				UID: imap.UID(m.uid), MessageID: m.mid, Flags: m.flags,
				InternalDate: m.internalDate, Size: int64(len(m.raw)),
			})
		}
	}
	return out, nil
}
func (d *fakeDest) HasMessageIDIn(folder, mid string) (bool, error) {
	for _, m := range d.folders[folder] {
		if m.mid == mid {
			return true, nil
		}
	}
	return false, nil
}
func (d *fakeDest) SearchMessageIDsIn(folder string, ids []string) (map[string]bool, error) {
	d.guardSearches++
	found := map[string]bool{}
	for _, id := range ids {
		if has, _ := d.HasMessageIDIn(folder, id); has {
			found[id] = true
		}
	}
	return found, nil
}
func (d *fakeDest) SupportsArbitraryKeywords() bool { return !d.noArbitraryKw }

// fakePending defers the append outcome to Wait, exercising the pipelined
// Wait-error paths in the consumer ring.
type fakePending struct{ err error }

func (p fakePending) Wait() error { return p.err }

func (d *fakeDest) BeginAppend(folder string, msg *imapx.FullMessage, flags []imap.Flag) (imapx.PendingAppend, error) {
	return fakePending{err: d.AppendTo(folder, msg, flags)}, nil
}

func (d *fakeDest) AppendTo(folder string, msg *imapx.FullMessage, flags []imap.Flag) error {
	if d.appendErr != nil {
		return d.appendErr
	}
	if d.failAppendAt > 0 && len(d.appended)+1 == d.failAppendAt {
		return fmt.Errorf("append %d failed (injected)", d.failAppendAt)
	}
	if d.rejectKeywords {
		for _, f := range flags {
			if f != imap.FlagSeen {
				// A tagged server rejection (definitely not stored).
				return &imap.Error{Type: imap.StatusResponseTypeNo, Text: fmt.Sprintf("keyword %q not permitted (injected)", f)}
			}
		}
	}
	d.nextUID[folder]++
	// Extract Message-ID from the raw body if the test encoded one (tests use
	// raw bodies like "raw-a"; guard/seed tests inject via addExisting).
	d.folders[folder] = append(d.folders[folder], &destMsg{
		uid: d.nextUID[folder], mid: midOfRaw(string(msg.Raw)), raw: string(msg.Raw),
		flags: flags, internalDate: msg.InternalDate,
	})
	d.appended = append(d.appended, string(msg.Raw))
	d.appendedFlags = append(d.appendedFlags, flags)
	d.appendedTo = append(d.appendedTo, folder)
	return nil
}
func (d *fakeDest) MoveMessageID(fromFolder, toFolder, mid string) (bool, error) {
	msgs := d.folders[fromFolder]
	for i, m := range msgs {
		if m.mid == mid {
			d.folders[fromFolder] = append(msgs[:i], msgs[i+1:]...)
			d.nextUID[toFolder]++
			moved := *m
			moved.uid = d.nextUID[toFolder]
			d.folders[toFolder] = append(d.folders[toFolder], &moved)
			d.moves++
			return true, nil
		}
	}
	return false, nil
}
func (d *fakeDest) MoveUIDs(fromFolder string, uids []imap.UID, toFolder string) error {
	want := map[uint32]bool{}
	for _, u := range uids {
		want[uint32(u)] = true
	}
	var keep []*destMsg
	for _, m := range d.folders[fromFolder] {
		if want[m.uid] {
			d.nextUID[toFolder]++
			moved := *m
			moved.uid = d.nextUID[toFolder]
			d.folders[toFolder] = append(d.folders[toFolder], &moved)
			d.moves++
		} else {
			keep = append(keep, m)
		}
	}
	d.folders[fromFolder] = keep
	return nil
}
func (d *fakeDest) StoreKeywordByMessageID(folder, mid string, add bool, kw imap.Flag) (bool, error) {
	for _, m := range d.folders[folder] {
		if m.mid == mid {
			if add {
				if !slices.Contains(m.flags, kw) {
					m.flags = append(m.flags, kw)
				}
			} else {
				m.flags = slices.DeleteFunc(m.flags, func(f imap.Flag) bool { return f == kw })
			}
			return true, nil
		}
	}
	return false, nil
}
func (d *fakeDest) StoreKeywordsUIDs(uids []imap.UID, kws []imap.Flag) error {
	want := map[uint32]bool{}
	for _, u := range uids {
		want[uint32(u)] = true
	}
	for _, m := range d.folders[d.selected] {
		if want[m.uid] {
			for _, kw := range kws {
				if !slices.Contains(m.flags, kw) {
					m.flags = append(m.flags, kw)
				}
			}
		}
	}
	return nil
}

// midOfRaw lets fakes correlate appended raw bodies back to Message-IDs: the
// test messages are built by msg(uid, mid, raw), and the raw body encodes the
// mid via the shared registry below.
var rawToMid = map[string]string{}

func midOfRaw(raw string) string { return rawToMid[raw] }

func msg(uid uint32, mid, raw string) fakeMsg {
	rawToMid[raw] = mid
	return fakeMsg{
		meta: imapx.MsgMeta{
			UID:          imap.UID(uid),
			MessageID:    mid,
			InternalDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(uid) * time.Minute),
			Size:         int64(len(raw)),
		},
		raw: raw,
	}
}

func newRec(store Store, src Source, dst Dest, opts Options) *Reconciler {
	if opts.DestFolder == "" {
		opts.DestFolder = fakeDestFolder
	}
	if opts.ArchiveRouting || opts.SentRouting {
		if opts.SourceInbox == "" {
			opts.SourceInbox = "INBOX"
		}
	}
	if opts.ArchiveRouting && opts.ArchiveFolder == "" {
		opts.ArchiveFolder = fakeArchiveFolder
	}
	if opts.SentRouting {
		if opts.SentSrcFolder == "" {
			opts.SentSrcFolder = "SENTSRC"
		}
		if opts.SentFolder == "" {
			opts.SentFolder = fakeSentFolder
		}
	}
	return New(store, src, dst, opts, slog.New(slog.DiscardHandler))
}

// ---- tests ----

func TestFirstRunCopiesEverythingOnce(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{
		msg(1, "<a@x>", "raw-a"), msg(2, "<b@x>", "raw-b"), msg(3, "<c@x>", "raw-c"),
	}}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 3 || sum.SkippedDup != 0 || len(dst.appended) != 3 {
		t.Fatalf("first run: %+v appended=%d", sum, len(dst.appended))
	}

	// Re-run: zero new copies.
	sum, err = rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || len(dst.appended) != 3 {
		t.Fatalf("re-run duplicated: %+v appended=%d", sum, len(dst.appended))
	}
}

func TestDedupSkipsAlreadyCopied(t *testing.T) {
	store := newFakeStore()
	store.keys["<a@x>"] = true
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{
		msg(1, "<a@x>", "raw-a"), msg(2, "<b@x>", "raw-b"),
	}}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.SkippedDup != 1 {
		t.Fatalf("got %+v, want 1 copied 1 skipped", sum)
	}
	if len(dst.appended) != 1 || dst.appended[0] != "raw-b" {
		t.Fatalf("wrong appends: %v", dst.appended)
	}
}

func TestUIDValidityChangeRescansWithoutDuplicating(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{
		msg(10, "<a@x>", "raw-a"), msg(20, "<b@x>", "raw-b"),
	}}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Simulate UIDVALIDITY reset: same messages, brand-new UIDs.
	src.uidValidity = 8
	src.msgs = []fakeMsg{msg(1, "<a@x>", "raw-a"), msg(2, "<b@x>", "raw-b"), msg(3, "<c@x>", "raw-c")}

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.UIDValidityChanged {
		t.Fatal("UIDVALIDITY change not detected")
	}
	if sum.Copied != 1 || sum.SkippedDup != 2 || len(dst.appended) != 3 {
		t.Fatalf("after reset: %+v appended=%d (want only <c@x> new)", sum, len(dst.appended))
	}
	if v, _ := store.UIDValidity(); v != 8 {
		t.Fatalf("stored uidvalidity = %d, want 8", v)
	}
}

func TestDestGuardCatchesUnrecordedAppend(t *testing.T) {
	// Simulates the "appended but crash before record" window: dest already
	// has the message, local store does not.
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}}
	dst := newFakeDest()
	dst.addExisting(fakeDestFolder, "<a@x>", "raw-a-preexisting")
	rec := newRec(store, src, dst, Options{DestGuard: true})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || sum.SkippedDup != 1 || len(dst.appended) != 0 {
		t.Fatalf("dest guard failed: %+v appended=%d", sum, len(dst.appended))
	}
	// And the key must now be recorded locally (self-heal).
	if !store.keys["<a@x>"] {
		t.Fatal("guard hit not recorded to store")
	}
}

func TestAppendFailureLeavesKeyUnrecorded(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}}
	dst := newFakeDest()
	dst.appendErr = fmt.Errorf("append failed (injected)")
	rec := newRec(store, src, dst, Options{})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want error from failed append")
	}
	if store.keys["<a@x>"] {
		t.Fatal("key recorded despite failed append — would lose the message forever")
	}
	if store.lastUID != 0 {
		t.Fatalf("last_uid advanced past unappended message: %d", store.lastUID)
	}

	// Recovery: append works now; message is retried, nothing duplicated.
	dst.appendErr = nil
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || len(dst.appended) != 1 {
		t.Fatalf("retry after failure: %+v appended=%d", sum, len(dst.appended))
	}
}

func TestWindowedResumeAfterMidRunFailure(t *testing.T) {
	// 6 messages, window size 2 → 3 windows; connection dies after window 2.
	store := newFakeStore()
	var msgs []fakeMsg
	for i := uint32(1); i <= 6; i++ {
		msgs = append(msgs, msg(i, fmt.Sprintf("<m%d@x>", i), fmt.Sprintf("raw-%d", i)))
	}
	src := &fakeSource{uidValidity: 7, msgs: msgs, failAfter: 2}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{UIDBatch: 2})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want mid-run failure")
	}
	if len(dst.appended) != 4 {
		t.Fatalf("appended %d before failure, want 4", len(dst.appended))
	}
	if store.lastUID != 4 {
		t.Fatalf("last_uid = %d after two windows, want 4", store.lastUID)
	}

	// Reconnect: resume from last committed window; no duplicates, no misses.
	src.failAfter = 0
	src.metaCalls = 0
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 2 || len(dst.appended) != 6 {
		t.Fatalf("resume: %+v appended=%d, want 2 copied / 6 total", sum, len(dst.appended))
	}
	// Resume must not re-fetch already-committed windows.
	if src.metaCalls != 1 {
		t.Fatalf("resume re-scanned %d windows, want 1", src.metaCalls)
	}
}

func TestSeedFromDestPreventsRecopy(t *testing.T) {
	store := newFakeStore()
	dst := newFakeDest()
	// Dest already holds 2 messages (e.g. from a prior bulk import).
	dst.addExisting(fakeDestFolder, "<seed-1@x>", "x")
	dst.addExisting(fakeDestFolder, "<seed-2@x>", "y")
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{
		msg(1, "<seed-1@x>", "raw-1"), msg(2, "<seed-2@x>", "raw-2"), msg(3, "<new@x>", "raw-3"),
	}}
	rec := newRec(store, src, dst, Options{UIDBatch: 1})

	n, err := rec.SeedFromDest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded %d, want 2", n)
	}

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.SkippedDup != 2 {
		t.Fatalf("after seed: %+v, want 1 copied 2 skipped", sum)
	}
}

// The guard must issue ONE batched search per dest folder per window — not
// per message — while still catching every pre-existing message.
func TestGuardIsBatchedPerWindow(t *testing.T) {
	store := newFakeStore()
	var msgs []fakeMsg
	for i := uint32(1); i <= 5; i++ {
		msgs = append(msgs, msg(i, fmt.Sprintf("<g%d@x>", i), fmt.Sprintf("raw-g%d", i)))
	}
	src := &fakeSource{uidValidity: 7, msgs: msgs}
	dst := newFakeDest()
	dst.addExisting(fakeDestFolder, "<g3@x>", "raw-g3-preexisting")
	rec := newRec(store, src, dst, Options{DestGuard: true, UIDBatch: 100})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 4 || sum.SkippedDup != 1 {
		t.Fatalf("got %+v, want 4 copied / 1 skipped (g3 pre-existing)", sum)
	}
	if dst.guardSearches != 1 {
		t.Fatalf("guard searches = %d, want 1 (one batch per folder per window)", dst.guardSearches)
	}
	if dst.total() != 5 {
		t.Fatalf("dest total = %d, want 5 (no duplicate of g3)", dst.total())
	}
	// The pre-existing message must be recorded (self-heal), not re-appended.
	if !store.keys["<g3@x>"] {
		t.Fatal("guard hit not recorded")
	}
}

// Synthesized keys are unsearchable and must never reach the guard query.
func TestGuardExcludesSynthesizedKeys(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{msg(1, "", "raw-nomid")}}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{DestGuard: true})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dst.guardSearches != 0 {
		t.Fatalf("guard searched for synthesized keys (%d calls)", dst.guardSearches)
	}
	if dst.total() != 1 {
		t.Fatalf("message not mirrored: %d", dst.total())
	}
}

// Pipeline: appends preserve source order, and a mid-stream append failure
// aborts the pass with already-appended messages recorded (resume-safe).
func TestPipelineOrderAndMidStreamFailure(t *testing.T) {
	store := newFakeStore()
	var msgs []fakeMsg
	for i := uint32(1); i <= 6; i++ {
		msgs = append(msgs, msg(i, fmt.Sprintf("<s%d@x>", i), fmt.Sprintf("raw-s%d", i)))
	}
	src := &fakeSource{uidValidity: 7, msgs: msgs}
	dst := newFakeDest()
	dst.failAppendAt = 4 // 4th append errors
	rec := newRec(store, src, dst, Options{UIDBatch: 100})

	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("want error from mid-stream append failure")
	}
	// Exactly 3 appended, in source order, all recorded.
	if len(dst.appended) != 3 {
		t.Fatalf("appended %d, want 3", len(dst.appended))
	}
	for i, raw := range []string{"raw-s1", "raw-s2", "raw-s3"} {
		if dst.appended[i] != raw {
			t.Fatalf("append order broken: %v", dst.appended)
		}
		if !store.keys[fmt.Sprintf("<s%d@x>", i+1)] {
			t.Fatalf("appended message s%d not recorded", i+1)
		}
	}
	// last_uid must NOT have advanced past the failed window.
	if store.lastUID != 0 {
		t.Fatalf("last_uid advanced to %d despite failed window", store.lastUID)
	}

	// Recovery: the failed and remaining messages copy exactly once.
	dst.failAppendAt = 0
	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 3 || dst.total() != 6 {
		t.Fatalf("resume: %+v total=%d, want 3 copied / 6 total", sum, dst.total())
	}
}

// More copies than one record-flush batch: every key must still be recorded
// (multiple flushes + final flush), order preserved through the append ring.
func TestRecordBatchingAcrossFlushes(t *testing.T) {
	store := newFakeStore()
	var msgs []fakeMsg
	for i := uint32(1); i <= 120; i++ { // > 2x recordFlushSize
		msgs = append(msgs, msg(i, fmt.Sprintf("<b%d@x>", i), fmt.Sprintf("raw-b%d", i)))
	}
	src := &fakeSource{uidValidity: 7, msgs: msgs}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{UIDBatch: 1000})

	sum, err := rec.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 120 || dst.total() != 120 {
		t.Fatalf("copied=%d total=%d, want 120/120", sum.Copied, dst.total())
	}
	for i := 1; i <= 120; i++ {
		if !store.keys[fmt.Sprintf("<b%d@x>", i)] {
			t.Fatalf("key b%d not recorded", i)
		}
	}
	// FIFO order through the ring.
	for i, raw := range dst.appended {
		if raw != fmt.Sprintf("raw-b%d", i+1) {
			t.Fatalf("append order broken at %d: %s", i, raw)
		}
	}
}

func TestOnProgressCalledPerWindow(t *testing.T) {
	store := newFakeStore()
	var msgs []fakeMsg
	for i := uint32(1); i <= 6; i++ {
		msgs = append(msgs, msg(i, fmt.Sprintf("<p%d@x>", i), fmt.Sprintf("raw-p%d", i)))
	}
	src := &fakeSource{uidValidity: 7, msgs: msgs}
	dst := newFakeDest()
	var calls int
	rec := newRec(store, src, dst, Options{
		UIDBatch:   2, // 6 messages -> 3 mirror windows
		OnProgress: func(phase, item string, processed int) { calls++ },
	})
	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Fatalf("OnProgress called %d times, want >= 3 (one per window)", calls)
	}
}

func TestContextCancelStopsBetweenMessages(t *testing.T) {
	store := newFakeStore()
	src := &fakeSource{uidValidity: 7, msgs: []fakeMsg{msg(1, "<a@x>", "raw-a")}}
	dst := newFakeDest()
	rec := newRec(store, src, dst, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rec.Run(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(dst.appended) != 0 {
		t.Fatal("appended despite cancelled context")
	}
}
