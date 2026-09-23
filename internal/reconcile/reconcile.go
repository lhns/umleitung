// Package reconcile implements the core one-way, append-only, idempotent
// mirror algorithm (spec §3).
//
// Safety-critical invariant: a dedup key is recorded ONLY after a confirmed
// successful APPEND — never before. Combined with destination seeding and the
// destination guard, this makes duplicates impossible even across crashes,
// state loss and UIDVALIDITY changes.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/state"
)

// PendingOp is a queued destination mutation.
type PendingOp = state.PendingOp

// Store is the persistent state needed by the reconciler.
type Store interface {
	UIDValidity() (uint32, error)
	SetUIDValidity(uint32) error
	LastUID() (uint32, error)
	SetLastUID(uint32) error
	HasKey(key string) (bool, error)
	SeedBatch(keys []string) error
	RecordKeys(records []state.KeyRecord) error
	FolderState(name string) (uidValidity, lastUID uint32, err error)
	SetFolderState(name string, uidValidity, lastUID uint32) error
	MemberChangeBatch(folder string, items []state.MemberChangeItem) error
	MemberHas(folder, key string) (bool, error)
	MemberFolders(key string) ([]string, error)
	MemberUIDKeys(folder string) (map[uint32]string, error)
	MemberKeys(folder string) (map[string]bool, error)
	PendingOps(limit int) ([]PendingOp, error)
	DeletePendingBatch(ids []int64) error
	MetaGet(key string) (string, error)
	MetaSet(key, value string) error
}

// Source is the read-only side.
type Source interface {
	SelectFolder() (uidValidity, uidNext, numMessages uint32, err error)
	SelectNamedFolder(name string) (uidValidity, uidNext, numMessages uint32, err error)
	ListFolders() ([]imapx.FolderInfo, error)
	SearchAllUIDs() ([]imap.UID, error)
	FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error)
	FetchFullStream(uids []imap.UID, fn func(*imapx.FullMessage) error) error
}

// Dest is the append-mostly side. The only mutations of existing messages
// are MOVEs and keyword STOREs — content is never deleted.
type Dest interface {
	SelectNamedFolder(name string) (uidValidity, uidNext, numMessages uint32, err error)
	FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error)
	SearchMessageIDsIn(folder string, ids []string) (map[string]bool, error)
	AppendTo(folder string, msg *imapx.FullMessage, flags []imap.Flag) error
	BeginAppend(folder string, msg *imapx.FullMessage, flags []imap.Flag) (imapx.PendingAppend, error)
	MoveMessageID(fromFolder, toFolder, messageID string) (bool, error)
	MoveUIDs(fromFolder string, uids []imap.UID, toFolder string) error
	StoreKeywordByMessageID(folder, messageID string, add bool, kw imap.Flag) (bool, error)
	StoreKeywordsUIDs(uids []imap.UID, kws []imap.Flag) error
	SupportsArbitraryKeywords() bool
}

// Options tune the reconciler.
type Options struct {
	UIDBatch  int  // UID window size for the windowed, resumable scan
	DestGuard bool // Message-ID search on the destination before appending
	CarrySeen bool // propagate \Seen from source

	SyncLabels   bool     // record source label-folder membership -> dest keywords
	SourceFolder string   // the mirror source folder (excluded from label scan)
	LabelExclude []string // additional folder names excluded from the label scan

	DestFolder         string // primary destination folder
	ArchiveRouting     bool   // route by source-INBOX membership; propagate archive moves
	SourceInbox        string // source folder whose membership means "in inbox"
	ArchiveFolder      string // destination folder for archived mail
	SentRouting        bool   // route by source-Sent membership; propagate moves
	SentSrcFolder      string // resolved source folder whose membership means "sent"
	SentFolder         string // destination folder for sent mail
	LabelPropagate     bool   // STORE keyword changes for post-copy label changes
	KeywordPrefix      string // prepended to each label keyword (e.g. "$label:")
	KeywordReplacement string // sanitization replacement char (default "_")

	// OnProgress, if set, is called after committed work in every
	// long-running phase (liveness heartbeat + progress reporting). item
	// names the current work unit (e.g. "Work 3/12"), or is empty.
	OnProgress func(phase, item string, processed int)
}

func (o *Options) progress(phase, item string, processed int) {
	if o.OnProgress != nil {
		o.OnProgress(phase, item, processed)
	}
}

func (o *Options) labelExcludeSet() map[string]bool {
	set := make(map[string]bool, len(o.LabelExclude))
	for _, n := range o.LabelExclude {
		set[n] = true
	}
	return set
}

// Summary reports what one reconcile pass did.
type Summary struct {
	UIDValidityChanged bool
	Candidates         int
	Copied             int
	SkippedDup         int
	MovedToArchive     int
	MovedToInbox       int
	MovedToSent        int
	KeywordsSet        int // copy-time keyword applications (labels on new mail)
	KeywordsUpdated    int // post-copy keyword STOREs (propagation + backfill)
}

// Reconciler mirrors new source messages into the destination.
type Reconciler struct {
	store Store
	src   Source
	dst   Dest
	opts  Options
	log   *slog.Logger
}

// New creates a Reconciler.
func New(store Store, src Source, dst Dest, opts Options, log *slog.Logger) *Reconciler {
	if opts.UIDBatch < 1 {
		opts.UIDBatch = 2000
	}
	return &Reconciler{store: store, src: src, dst: dst, opts: opts, log: log}
}

// Run performs one full reconcile pass. It is safe to call any number of
// times; it never duplicates. ctx is checked between messages, so shutdown
// never tears a message in half.
func (r *Reconciler) Run(ctx context.Context) (*Summary, error) {
	sum := &Summary{}

	// Membership first, so routing and copy-time keywords see current state.
	if r.opts.SyncLabels && !r.dst.SupportsArbitraryKeywords() {
		r.log.Warn("destination does not advertise arbitrary keyword support (PERMANENTFLAGS \\*); labels may be dropped")
	}
	if err := r.syncMembership(ctx); err != nil {
		return sum, fmt.Errorf("membership scan: %w", err)
	}

	uidValidity, uidNext, _, err := r.src.SelectFolder()
	if err != nil {
		return sum, err
	}
	storedValidity, err := r.store.UIDValidity()
	if err != nil {
		return sum, err
	}
	lastUID, err := r.store.LastUID()
	if err != nil {
		return sum, err
	}

	if storedValidity != uidValidity {
		if storedValidity != 0 {
			// UIDs are meaningless now; rescan everything. The dedup set
			// prevents duplicate appends.
			r.log.Warn("UIDVALIDITY changed — resetting high-water mark, dedup set protects against dupes",
				"stored", storedValidity, "current", uidValidity)
			sum.UIDValidityChanged = true
		}
		// Reset the high-water mark BEFORE adopting the new UIDVALIDITY: a
		// crash in between must re-trigger the reset, not leave a stale mark.
		lastUID = 0
		if err := r.store.SetLastUID(0); err != nil {
			return sum, err
		}
		if err := r.store.SetUIDValidity(uidValidity); err != nil {
			return sum, err
		}
	}

	// Backfill before the mirror loop so a config change re-tags existing
	// mail promptly, not only after a days-long first run. It touches only
	// the destination connection; the source stays selected.
	if err := r.maybeBackfill(ctx, sum); err != nil {
		return sum, fmt.Errorf("backfill: %w", err)
	}

	// last_uid is committed per window, so a crash or throttle disconnect
	// resumes from the last committed window.
	err = r.scanWindows(ctx, lastUID+1, uidNext, r.src.FetchMetaRange, func(stop uint32, metas []imapx.MsgMeta) error {
		sum.Candidates += len(metas)
		if err := r.mirrorWindow(ctx, metas, sum); err != nil {
			return err
		}
		if err := r.store.SetLastUID(stop); err != nil {
			return err
		}
		r.opts.progress("mirror", "", sum.Copied)
		return nil
	})
	if err != nil {
		return sum, err
	}

	if r.opts.ArchiveRouting || r.opts.SentRouting || r.opts.LabelPropagate {
		if err := r.propagate(ctx, sum); err != nil {
			return sum, fmt.Errorf("propagate: %w", err)
		}
	}
	return sum, nil
}

// scanWindows fetches UIDs [first, uidNext-1] in UIDBatch windows and passes
// each window's metadata to fn.
func (r *Reconciler) scanWindows(ctx context.Context, first, uidNext uint32,
	fetch func(start, stop imap.UID) ([]imapx.MsgMeta, error),
	fn func(stop uint32, metas []imapx.MsgMeta) error,
) error {
	batch := uint32(r.opts.UIDBatch)
	for start := first; start < uidNext; start += batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := min(start+batch-1, uidNext-1)
		metas, err := fetch(imap.UID(start), imap.UID(stop))
		if err == nil {
			err = fn(stop, metas)
		}
		if err != nil {
			return fmt.Errorf("window %d:%d: %w", start, stop, err)
		}
	}
	return nil
}

// pendingCopy is a classified candidate awaiting its body copy.
type pendingCopy struct {
	uid        imap.UID
	key        string
	destFolder string
}

// pipelineDepth bounds bodies buffered between the source fetch stream and
// the destination append consumer (memory: depth × message size).
const pipelineDepth = 8

// mirrorWindow mirrors one window of candidates:
//  1. classify locally: dedup lookup + routing
//  2. destination guard: one batched Message-ID search per dest folder
//  3. copy: one streamed FETCH feeding a bounded append pipeline
func (r *Reconciler) mirrorWindow(ctx context.Context, metas []imapx.MsgMeta, sum *Summary) error {
	var pend []pendingCopy
	pendKeys := map[string]bool{} // same key twice in one window: copy once
	for i := range metas {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := DedupKey(&metas[i])
		seen, err := r.store.HasKey(key)
		if err != nil {
			return err
		}
		if seen || pendKeys[key] {
			sum.SkippedDup++
			continue
		}
		pendKeys[key] = true
		destFolder, err := r.destFolderFor(key)
		if err != nil {
			return err
		}
		pend = append(pend, pendingCopy{uid: metas[i].UID, key: key, destFolder: destFolder})
	}

	if r.opts.DestGuard && len(pend) > 0 {
		var err error
		if pend, err = r.guard(pend, sum); err != nil {
			return err
		}
	}
	if len(pend) == 0 {
		return nil
	}
	return r.copyPipeline(ctx, pend, sum)
}

// guard drops candidates already present in any destination bucket (the
// copy may have been moved) and records them — the self-heal for the
// appended-but-unrecorded crash window. Synthesized keys are unsearchable.
func (r *Reconciler) guard(pend []pendingCopy, sum *Summary) ([]pendingCopy, error) {
	var ids []string
	for _, p := range pend {
		if IsRealMessageID(p.key) {
			ids = append(ids, p.key)
		}
	}
	if len(ids) == 0 {
		return pend, nil
	}
	found := map[string]bool{}
	for _, folder := range r.destBucketFolders() {
		f, err := r.dst.SearchMessageIDsIn(folder, ids)
		if err != nil {
			return nil, err
		}
		for id := range f {
			found[id] = true
		}
	}
	var records []state.KeyRecord
	kept := pend[:0]
	for _, p := range pend {
		if found[p.key] {
			sum.SkippedDup++
			records = append(records, state.KeyRecord{Key: p.key, UID: uint32(p.uid), CopiedAtUnix: time.Now().Unix()})
			continue
		}
		kept = append(kept, p)
	}
	return kept, r.store.RecordKeys(records)
}

type copyItem struct {
	full *imapx.FullMessage
	pc   pendingCopy
}

// inflightAppend is an APPEND awaiting server confirmation in the ring.
type inflightAppend struct {
	pa          imapx.PendingAppend
	pc          pendingCopy
	full        *imapx.FullMessage
	baseFlags   []imap.Flag
	hadKeywords bool
}

// appendRing bounds issued-but-unconfirmed APPENDs; together with
// recordFlushSize it is the crash window the destination guard covers.
const (
	appendRing      = 4
	recordFlushSize = 50
)

// copyPipeline overlaps the source body stream (producer) with destination
// appends (consumer). Single producer + single consumer = FIFO order; only
// the consumer touches the store. Dedup records are flushed in batches, at
// stream end, and on any error — state stays behind reality, never ahead.
func (r *Reconciler) copyPipeline(ctx context.Context, pend []pendingCopy, sum *Summary) error {
	byUID := make(map[imap.UID]pendingCopy, len(pend))
	uids := make([]imap.UID, 0, len(pend))
	for _, p := range pend {
		byUID[p.uid] = p
		uids = append(uids, p.uid)
	}

	// Without the guard nothing re-detects appended-but-unrecorded messages
	// after a crash: degrade to one in-flight append, recorded immediately.
	ringLimit, flushLimit := appendRing, recordFlushSize
	if !r.opts.DestGuard {
		ringLimit, flushLimit = 1, 1
	}

	ch := make(chan copyItem, pipelineDepth)
	failed := make(chan struct{}) // closed by consumer on first error
	done := make(chan struct{})
	var consErr error

	go func() {
		defer close(done)
		var ring []inflightAppend
		var records []state.KeyRecord

		fail := func(err error) {
			if consErr == nil {
				consErr = err
				close(failed)
			}
		}
		flushRecords := func() {
			if len(records) == 0 {
				return
			}
			if err := r.store.RecordKeys(records); err != nil {
				fail(err)
				return
			}
			records = records[:0]
		}
		// settleOldest confirms the oldest in-flight append, then buffers
		// its record.
		settleOldest := func() {
			it := ring[0]
			ring = ring[1:]
			keywordsLanded := it.hadKeywords
			if err := it.pa.Wait(); err != nil {
				// Retrying is only safe on a tagged NO/BAD (definitely not
				// stored). A connection error may have stored it — abort and
				// let the guard reconcile next pass.
				if !it.hadKeywords || !isServerReject(err) {
					fail(fmt.Errorf("uid %d: %w", it.pc.uid, err))
					return
				}
				// The mirror wins over label decoration: retry without keywords.
				r.log.Warn("append with label keywords rejected; retrying without keywords",
					"uid", it.pc.uid, "err", err)
				if err := r.dst.AppendTo(it.pc.destFolder, it.full, it.baseFlags); err != nil {
					fail(fmt.Errorf("uid %d: %w", it.pc.uid, err))
					return
				}
				keywordsLanded = false
			}
			if keywordsLanded {
				sum.KeywordsSet++
			}
			records = append(records, state.KeyRecord{
				Key: it.pc.key, UID: uint32(it.pc.uid), CopiedAtUnix: time.Now().Unix(),
			})
			sum.Copied++
			// Per copy: keeps the heartbeat fresh even when the provider
			// bandwidth-shapes the stream.
			r.opts.progress("mirror", "", sum.Copied)
			if len(records) >= flushLimit {
				flushRecords()
			}
		}

		for it := range ch {
			if consErr != nil {
				continue // drain so the producer never blocks
			}
			baseFlags := safeFlags(it.full.Flags, r.opts.CarrySeen)
			flags := baseFlags
			var keywords []imap.Flag
			if r.opts.SyncLabels {
				labels, err := r.labelsFor(it.pc.key)
				if err != nil {
					fail(err)
					continue
				}
				keywords = r.labelKeywords(labels)
				flags = append(slices.Clone(baseFlags), keywords...)
			}
			pa, err := r.dst.BeginAppend(it.pc.destFolder, it.full, flags)
			if err != nil {
				fail(fmt.Errorf("uid %d: %w", it.pc.uid, err))
				continue
			}
			ring = append(ring, inflightAppend{
				pa: pa, pc: it.pc, full: it.full,
				baseFlags: baseFlags, hadKeywords: len(keywords) > 0,
			})
			if len(ring) >= ringLimit {
				settleOldest()
			}
		}
		for consErr == nil && len(ring) > 0 {
			settleOldest()
		}
		flushRecords()
	}()

	prodErr := r.src.FetchFullStream(uids, func(full *imapx.FullMessage) error {
		pc, ok := byUID[full.UID]
		if !ok {
			return nil // unexpected UID in response; ignore
		}
		select {
		case ch <- copyItem{full: full, pc: pc}:
			return nil
		case <-failed:
			return errors.New("append side failed")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(ch)
	<-done

	if consErr != nil {
		return consErr
	}
	if prodErr != nil {
		return prodErr
	}
	return ctx.Err()
}

// isServerReject reports whether err is a tagged NO/BAD: the server
// definitively did NOT store the message, so a retry is safe.
func isServerReject(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr)
}

// safeFlags returns the APPEND flags: \Seen if carried, nothing else (no
// \Deleted, \Recent or provider-specific keywords).
func safeFlags(src []imap.Flag, carrySeen bool) []imap.Flag {
	if carrySeen && slices.Contains(src, imap.FlagSeen) {
		return []imap.Flag{imap.FlagSeen}
	}
	return nil
}

// SeedFromDest streams the dedup keys of every destination bucket into the
// store, bootstrapping idempotency against a pre-populated destination or
// after local state loss. Memory stays bounded to one UID window.
func (r *Reconciler) SeedFromDest(ctx context.Context) (int64, error) {
	folders := r.destBucketFolders()
	var seeded int64
	for i, folder := range folders {
		n, err := r.seedFromDestFolder(ctx, folder, fmt.Sprintf("%s %d/%d", folder, i+1, len(folders)))
		seeded += n
		if err != nil {
			return seeded, fmt.Errorf("seed %q: %w", folder, err)
		}
	}
	return seeded, nil
}

func (r *Reconciler) seedFromDestFolder(ctx context.Context, folder, item string) (int64, error) {
	_, uidNext, numMessages, err := r.dst.SelectNamedFolder(folder)
	if err != nil || numMessages == 0 {
		return 0, err
	}
	var seeded int64
	err = r.scanWindows(ctx, 1, uidNext, r.dst.FetchMetaRange, func(_ uint32, metas []imapx.MsgMeta) error {
		keys := make([]string, 0, len(metas))
		for i := range metas {
			keys = append(keys, DedupKey(&metas[i]))
		}
		if err := r.store.SeedBatch(keys); err != nil {
			return err
		}
		seeded += int64(len(keys))
		r.opts.progress("seed", item, int(seeded))
		return nil
	})
	return seeded, err
}
