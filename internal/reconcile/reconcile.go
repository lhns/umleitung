// Package reconcile implements the core one-way, append-only, idempotent
// mirror algorithm (spec §3).
//
// Safety-critical invariant: a dedup key is recorded ONLY after a confirmed
// successful APPEND — never before. Combined with destination seeding and the
// per-append destination guard, this makes duplicates impossible even across
// crashes, state loss and UIDVALIDITY changes.
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

// PendingOp is a queued destination mutation; aliased from the state package
// so store implementations and the reconciler share one type.
type PendingOp = state.PendingOp

// Store is the persistent state needed by the reconciler.
type Store interface {
	UIDValidity() (uint32, error)
	SetUIDValidity(uint32) error
	LastUID() (uint32, error)
	SetLastUID(uint32) error
	HasKey(key string) (bool, error)
	RecordKey(key string, uid uint32, copiedAtUnix int64) error
	CopiedCount() (int64, error)
	SeedBatch(keys []string) error
	RecordKeys(records []state.KeyRecord) error
	FolderState(name string) (uidValidity, lastUID uint32, err error)
	SetFolderState(name string, uidValidity, lastUID uint32) error
	MemberChange(folder, key string, uid uint32, add bool, pendingKind string) error
	MemberChangeBatch(folder string, items []state.MemberChangeItem) error
	MemberHas(folder, key string) (bool, error)
	MemberFolders(key string) ([]string, error)
	MemberUIDKeys(folder string) (map[uint32]string, error)
	MemberKeys(folder string) (map[string]bool, error)
	PendingOps(limit int) ([]PendingOp, error)
	DeletePending(id int64) error
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
	DestGuard bool // per-append `SEARCH HEADER Message-ID` on the destination
	CarrySeen bool // propagate \Seen from source

	SyncLabels   bool     // record source label-folder membership -> dest keywords
	SourceFolder string   // the mirror source folder (excluded from label scan)
	LabelExclude []string // additional folder names excluded from the label scan

	DestFolder     string // primary destination folder
	ArchiveRouting bool   // route by source-INBOX membership; propagate archive moves
	SourceInbox    string // source folder whose membership means "in inbox"
	ArchiveFolder  string // destination folder for archived mail
	SentRouting    bool   // route by source-Sent membership; propagate moves
	SentSrcFolder  string // resolved source folder whose membership means "sent"
	SentFolder     string // destination folder for sent mail
	LabelPropagate bool   // STORE keyword changes for post-copy label changes
	KeywordPrefix  string // prepended to each label keyword (e.g. "$label:")

	// OnProgress, if set, is called after every committed work window in any
	// long-running phase (seeding, membership scan, mirror, backfill). Used
	// as a liveness heartbeat and for progress reporting during large runs.
	// item names the current work unit (e.g. "Work 3/12" for folder 3 of 12);
	// empty when the phase has a single implicit unit.
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
	now   func() time.Time
}

// New creates a Reconciler.
func New(store Store, src Source, dst Dest, opts Options, log *slog.Logger) *Reconciler {
	if opts.UIDBatch < 1 {
		opts.UIDBatch = 2000
	}
	return &Reconciler{store: store, src: src, dst: dst, opts: opts, log: log, now: time.Now}
}

// Run performs one full reconcile pass. It is safe to call any number of
// times; it never duplicates. Respects ctx between messages so shutdown can
// interrupt a large catch-up without tearing a message in half.
func (r *Reconciler) Run(ctx context.Context) (*Summary, error) {
	sum := &Summary{}

	// Membership phase first (label folders + source INBOX), so routing and
	// copy-time keywords see current state. (Leaves some watched folder
	// selected; the SelectFolder below re-selects the mirror source folder.)
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
			// UIDs are meaningless now; rescan everything. The dedup-key set
			// still prevents any duplicate appends.
			r.log.Warn("UIDVALIDITY changed — resetting high-water mark, dedup set protects against dupes",
				"stored", storedValidity, "current", uidValidity)
			sum.UIDValidityChanged = true
		}
		lastUID = 0
		if err := r.store.SetUIDValidity(uidValidity); err != nil {
			return sum, err
		}
		if err := r.store.SetLastUID(0); err != nil {
			return sum, err
		}
	}

	// Placement/keyword backfill: auto-corrects mail mirrored before the
	// current routing/label config was active (config fingerprint change).
	// Runs BEFORE the mirror loop so a config change (e.g. new keyword prefix)
	// re-tags already-mirrored mail promptly, not only after a days-long first
	// run finishes. Touches only the destination connection; the source stays
	// selected for the loop below.
	if err := r.maybeBackfill(ctx, sum); err != nil {
		return sum, fmt.Errorf("backfill: %w", err)
	}

	// Windowed, resumable scan: [lastUID+1 .. uidNext-1] in UIDBatch windows.
	// last_uid is committed once per window, so a crash or a provider-throttle
	// disconnect resumes from the last committed window.
	for start := uint32(lastUID) + 1; start < uidNext; start += uint32(r.opts.UIDBatch) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		stop := min(start+uint32(r.opts.UIDBatch)-1, uidNext-1)

		metas, err := r.src.FetchMetaRange(imap.UID(start), imap.UID(stop))
		if err != nil {
			return sum, fmt.Errorf("window %d:%d: %w", start, stop, err)
		}
		sum.Candidates += len(metas)

		if err := r.mirrorWindow(ctx, metas, sum); err != nil {
			return sum, fmt.Errorf("window %d:%d: %w", start, stop, err)
		}

		// Per-window high-water-mark commit (resumable first run).
		if err := r.store.SetLastUID(stop); err != nil {
			return sum, err
		}
		r.opts.progress("mirror", "", sum.Copied)
	}

	// Apply queued destination mutations (routing moves, keyword updates).
	if r.opts.ArchiveRouting || r.opts.SentRouting || r.opts.LabelPropagate {
		if err := r.propagate(ctx, sum); err != nil {
			return sum, fmt.Errorf("propagate: %w", err)
		}
	}

	return sum, nil
}

// pendingCopy is a classified candidate awaiting its body copy.
type pendingCopy struct {
	uid        imap.UID
	key        string
	destFolder string
}

// pipelineDepth bounds in-flight messages between the Gmail fetch stream and
// the Stalwart append consumer (memory bound: depth × message size).
const pipelineDepth = 8

// mirrorWindow mirrors one window of candidates in three passes:
//  1. classify (local): dedup lookup + routing per candidate
//  2. batched destination guard: ONE Message-ID batch search per dest folder
//     (every candidate is still guarded — only the transport is batched)
//  3. copy: one streaming FETCH for all pending bodies, appends running
//     concurrently in a bounded FIFO pipeline (append-then-record per
//     message, exactly as before)
func (r *Reconciler) mirrorWindow(ctx context.Context, metas []imapx.MsgMeta, sum *Summary) error {
	// Pass 1: classification.
	var pend []pendingCopy
	for i := range metas {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := DedupKey(&metas[i])
		seen, err := r.store.HasKey(key)
		if err != nil {
			return err
		}
		if seen {
			sum.SkippedDup++
			continue
		}
		destFolder, err := r.destFolderFor(key)
		if err != nil {
			return err
		}
		pend = append(pend, pendingCopy{uid: metas[i].UID, key: key, destFolder: destFolder})
	}
	if len(pend) == 0 {
		return nil
	}

	// Pass 2: batched destination guard (both folders under routing — the
	// copy may have been moved).
	if r.opts.DestGuard {
		var ids []string
		for _, p := range pend {
			if IsRealMessageID(p.key) {
				ids = append(ids, p.key)
			}
		}
		found := map[string]bool{}
		if len(ids) > 0 {
			for _, folder := range r.destBucketFolders() {
				f, err := r.dst.SearchMessageIDsIn(folder, ids)
				if err != nil {
					return err
				}
				for id := range f {
					found[id] = true
				}
			}
		}
		kept := pend[:0]
		for _, p := range pend {
			if found[p.key] {
				sum.SkippedDup++
				if err := r.store.RecordKey(p.key, uint32(p.uid), r.now().Unix()); err != nil {
					return err
				}
				continue
			}
			kept = append(kept, p)
		}
		pend = kept
		if len(pend) == 0 {
			return nil
		}
	}

	// Pass 3: streamed copy with a bounded fetch/append pipeline.
	return r.copyPipeline(ctx, pend, sum)
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

// appendRing bounds pipelined (issued-but-unconfirmed) APPENDs; together
// with recordFlushSize this is the crash window the batched destination
// guard covers (appended-but-unrecorded messages are re-detected next run).
const (
	appendRing      = 4
	recordFlushSize = 50
)

// copyPipeline overlaps the source body stream (producer) with destination
// appends (consumer). Single producer + single consumer = FIFO order; the
// consumer alone touches the store. Appends are pipelined (ring of
// appendRing in flight — no per-message dest round trip) and dedup records
// are flushed in batched transactions (recordFlushSize, at stream end, and
// on any error — state stays behind reality, never ahead).
func (r *Reconciler) copyPipeline(ctx context.Context, pend []pendingCopy, sum *Summary) error {
	byUID := make(map[imap.UID]pendingCopy, len(pend))
	uids := make([]imap.UID, 0, len(pend))
	for _, p := range pend {
		byUID[p.uid] = p
		uids = append(uids, p.uid)
	}

	// With the destination guard disabled there is no layer that re-detects
	// appended-but-unrecorded messages after a crash — degrade to the strict
	// synchronous mode (1 in-flight append, record flushed per message) so
	// the crash window stays at a single message.
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
		// settleOldest confirms the oldest in-flight append and buffers its
		// record — APPEND confirmed first, record after, never the reverse.
		settleOldest := func() {
			it := ring[0]
			ring = ring[1:]
			keywordsLanded := it.hadKeywords
			if err := it.pa.Wait(); err != nil {
				// The keyword-less retry is only safe when the SERVER
				// rejected the append (tagged NO/BAD = definitely not
				// stored). On a connection-level error the append may have
				// landed — retrying could duplicate; abort instead and let
				// the destination guard reconcile on the next pass.
				if !it.hadKeywords || !isServerReject(err) {
					fail(fmt.Errorf("uid %d: %w", it.pc.uid, err))
					return
				}
				// The mirror always wins over label decoration: retry once
				// without keywords before treating this as an error.
				r.log.Warn("append with label keywords rejected; retrying without keywords",
					"uid", it.pc.uid, "err", err)
				if err := r.dst.AppendTo(it.pc.destFolder, it.full, it.baseFlags); err != nil {
					fail(fmt.Errorf("uid %d: %w", it.pc.uid, err))
					return
				}
				keywordsLanded = false // fallback stripped them
			}
			if keywordsLanded {
				sum.KeywordsSet++
			}
			records = append(records, state.KeyRecord{
				Key: it.pc.key, UID: uint32(it.pc.uid), CopiedAtUnix: r.now().Unix(),
			})
			sum.Copied++
			// Every copy: keeps the heartbeat fresh and progress lines
			// flowing even when the provider bandwidth-shapes the stream.
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
				flags = append(append([]imap.Flag{}, baseFlags...), keywords...)
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
			return fmt.Errorf("append side failed")
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

// isServerReject reports whether err is a tagged server response (NO/BAD):
// the server processed the command and definitively did NOT store the
// message, making a retry safe. Connection-level errors return false.
func isServerReject(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr)
}

// safeFlags builds the flag set for the destination APPEND: optionally carry
// \Seen; never propagate anything else (no \Deleted, no \Recent, no provider-specific
// labels/keywords).
func safeFlags(src []imap.Flag, carrySeen bool) []imap.Flag {
	if carrySeen && slices.Contains(src, imap.FlagSeen) {
		return []imap.Flag{imap.FlagSeen}
	}
	return nil
}

// SeedFromDest streams the destination folders' dedup keys into the store in
// batches. This bootstraps idempotency against a pre-populated destination and
// re-derives the truth after local state loss. With archive routing enabled,
// BOTH destination folders are scanned. Memory stays bounded: one UID window
// of header metadata at a time.
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
	if err != nil {
		return 0, err
	}
	if numMessages == 0 {
		return 0, nil
	}
	var seeded int64
	for start := uint32(1); start < uidNext; start += uint32(r.opts.UIDBatch) {
		if err := ctx.Err(); err != nil {
			return seeded, err
		}
		stop := min(start+uint32(r.opts.UIDBatch)-1, uidNext-1)
		metas, err := r.dst.FetchMetaRange(imap.UID(start), imap.UID(stop))
		if err != nil {
			return seeded, fmt.Errorf("seed window %d:%d: %w", start, stop, err)
		}
		keys := make([]string, 0, len(metas))
		for i := range metas {
			keys = append(keys, DedupKey(&metas[i]))
		}
		if err := r.store.SeedBatch(keys); err != nil {
			return seeded, err
		}
		seeded += int64(len(keys))
		r.opts.progress("seed", item, int(seeded))
	}
	return seeded, nil
}
