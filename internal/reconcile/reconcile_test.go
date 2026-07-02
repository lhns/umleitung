package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
)

// ---- fakes ----

type fakeStore struct {
	uidValidity uint32
	lastUID     uint32
	keys        map[string]bool
	failRecord  bool

	labels       map[string]map[string]bool // dedup key -> label set
	folderStates map[string][2]uint32       // folder -> {uidvalidity, last_uid}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		keys:         map[string]bool{},
		labels:       map[string]map[string]bool{},
		folderStates: map[string][2]uint32{},
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
func (s *fakeStore) AddLabel(key, label string) error {
	if s.labels[key] == nil {
		s.labels[key] = map[string]bool{}
	}
	s.labels[key][label] = true
	return nil
}
func (s *fakeStore) LabelsFor(key string) ([]string, error) {
	var out []string
	for l := range s.labels[key] {
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}
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

func (f *fakeSource) FetchFull(uid imap.UID) (*imapx.FullMessage, error) {
	for _, m := range f.selectedMsgs() {
		if m.meta.UID == uid {
			return &imapx.FullMessage{Raw: []byte(m.raw), InternalDate: m.meta.InternalDate}, nil
		}
	}
	return nil, fmt.Errorf("uid %d not found", uid)
}

type fakeDest struct {
	appended       []string     // raw bodies, in append order
	appendedFlags  [][]imap.Flag // flags per append, parallel to appended
	messageIDs     map[string]bool
	appendErr      error
	rejectKeywords bool // reject any APPEND carrying non-\Seen flags
	noArbitraryKw  bool
}

func newFakeDest() *fakeDest { return &fakeDest{messageIDs: map[string]bool{}} }

func (d *fakeDest) SelectFolder() (uint32, uint32, uint32, error) {
	return 1, uint32(len(d.appended)) + 1, uint32(len(d.appended)), nil
}
func (d *fakeDest) FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error) {
	var out []imapx.MsgMeta
	for i := range d.appended {
		uid := imap.UID(i + 1)
		if uid >= start && uid <= stop {
			out = append(out, imapx.MsgMeta{UID: uid, MessageID: fmt.Sprintf("<seed-%d@x>", i+1)})
		}
	}
	return out, nil
}
func (d *fakeDest) HasMessageID(mid string) (bool, error) { return d.messageIDs[mid], nil }
func (d *fakeDest) SupportsArbitraryKeywords() bool       { return !d.noArbitraryKw }
func (d *fakeDest) Append(msg *imapx.FullMessage, flags []imap.Flag) error {
	if d.appendErr != nil {
		return d.appendErr
	}
	if d.rejectKeywords {
		for _, f := range flags {
			if f != imap.FlagSeen {
				return fmt.Errorf("keyword %q not permitted (injected)", f)
			}
		}
	}
	d.appended = append(d.appended, string(msg.Raw))
	d.appendedFlags = append(d.appendedFlags, flags)
	return nil
}

func msg(uid uint32, mid, raw string) fakeMsg {
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
	dst.messageIDs["<a@x>"] = true
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
	dst.appended = []string{"x", "y"} // dest already holds 2 messages (seed-1, seed-2)
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
