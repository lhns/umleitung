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
	FolderState(name string) (uidValidity, lastUID uint32, err error)
	SetFolderState(name string, uidValidity, lastUID uint32) error
	MemberChange(folder, key string, uid uint32, add bool, pendingKind string) error
	MemberHas(folder, key string) (bool, error)
	MemberFolders(key string) ([]string, error)
	MemberUIDKeys(folder string) (map[uint32]string, error)
	MemberKeys(folder string) (map[string]bool, error)
	PendingOps(limit int) ([]PendingOp, error)
	DeletePending(id int64) error
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
	FetchFull(uid imap.UID) (*imapx.FullMessage, error)
}

// Dest is the append-mostly side. The only mutations of existing messages
// are MOVEs and keyword STOREs — content is never deleted.
type Dest interface {
	SelectNamedFolder(name string) (uidValidity, uidNext, numMessages uint32, err error)
	FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error)
	HasMessageIDIn(folder, messageID string) (bool, error)
	AppendTo(folder string, msg *imapx.FullMessage, flags []imap.Flag) error
	MoveMessageID(fromFolder, toFolder, messageID string) (bool, error)
	MoveUIDs(uids []imap.UID, toFolder string) error
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
	LabelPropagate bool   // STORE keyword changes for post-copy label changes

	// OnProgress, if set, is called after every committed work window in any
	// long-running phase (seeding, membership scan, mirror, backfill). Used
	// as a liveness heartbeat and for progress reporting during large runs.
	OnProgress func(phase string, processed int)
}

func (o *Options) progress(phase string, processed int) {
	if o.OnProgress != nil {
		o.OnProgress(phase, processed)
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
	KeywordsUpdated    int
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

		for i := range metas {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			if err := r.mirrorOne(&metas[i], sum); err != nil {
				return sum, fmt.Errorf("uid %d: %w", metas[i].UID, err)
			}
		}

		// Per-window high-water-mark commit (resumable first run).
		if err := r.store.SetLastUID(stop); err != nil {
			return sum, err
		}
		r.opts.progress("mirror", sum.Copied)
	}

	// Placement/keyword backfill: auto-corrects mail mirrored before the
	// current routing/label config was active (config fingerprint change).
	if err := r.maybeBackfill(ctx, sum); err != nil {
		return sum, fmt.Errorf("backfill: %w", err)
	}

	// Apply queued destination mutations (archive moves, keyword updates).
	if r.opts.ArchiveRouting || r.opts.LabelPropagate {
		if err := r.propagate(ctx, sum); err != nil {
			return sum, fmt.Errorf("propagate: %w", err)
		}
	}

	return sum, nil
}

// mirrorOne applies the three dedup layers to a single candidate and appends
// it — routed to the correct destination folder — if and only if it is
// genuinely new.
func (r *Reconciler) mirrorOne(m *imapx.MsgMeta, sum *Summary) error {
	key := DedupKey(m)

	// Layer 2: persisted dedup set (indexed lookup, fast path).
	seen, err := r.store.HasKey(key)
	if err != nil {
		return err
	}
	if seen {
		sum.SkippedDup++
		return nil
	}

	// Routing: inbox members -> primary folder, everything else -> archive.
	destFolder, err := r.destFolderFor(key)
	if err != nil {
		return err
	}

	// Layer 3: destination guard — closes the "appended but not yet
	// recorded" crash window by asking the destination directly. With
	// routing, the copy may have been moved: check both folders.
	if r.opts.DestGuard && IsRealMessageID(key) {
		has, err := r.dst.HasMessageIDIn(destFolder, key)
		if err != nil {
			return err
		}
		if !has {
			if other := r.otherDestFolder(destFolder); other != "" {
				if has, err = r.dst.HasMessageIDIn(other, key); err != nil {
					return err
				}
			}
		}
		if has {
			sum.SkippedDup++
			return r.store.RecordKey(key, uint32(m.UID), r.now().Unix())
		}
	}

	full, err := r.src.FetchFull(m.UID)
	if err != nil {
		return err
	}

	baseFlags := safeFlags(full.Flags, r.opts.CarrySeen)
	flags := baseFlags
	var keywords []imap.Flag
	if r.opts.SyncLabels {
		labels, err := r.labelsFor(key)
		if err != nil {
			return err
		}
		keywords = keywordFlags(labels)
		flags = append(append([]imap.Flag{}, baseFlags...), keywords...)
	}

	// APPEND first, record after — never the other way around.
	if err := r.dst.AppendTo(destFolder, full, flags); err != nil {
		if len(keywords) == 0 {
			return err
		}
		// The mirror always wins over label decoration: retry once without
		// keywords before treating this as an error.
		r.log.Warn("append with label keywords failed; retrying without keywords",
			"uid", m.UID, "keywords", len(keywords), "err", err)
		if err := r.dst.AppendTo(destFolder, full, baseFlags); err != nil {
			return err
		}
	}
	if err := r.store.RecordKey(key, uint32(m.UID), r.now().Unix()); err != nil {
		return err
	}
	sum.Copied++
	return nil
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
	folders := []string{r.opts.DestFolder}
	if r.opts.ArchiveRouting {
		folders = append(folders, r.opts.ArchiveFolder)
	}
	var seeded int64
	for _, folder := range folders {
		n, err := r.seedFromDestFolder(ctx, folder)
		seeded += n
		if err != nil {
			return seeded, fmt.Errorf("seed %q: %w", folder, err)
		}
	}
	return seeded, nil
}

func (r *Reconciler) seedFromDestFolder(ctx context.Context, folder string) (int64, error) {
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
		r.opts.progress("seed", int(seeded))
	}
	return seeded, nil
}
