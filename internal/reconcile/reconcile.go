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
)

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
	AddLabel(key, label string) error
	LabelsFor(key string) ([]string, error)
	FolderState(name string) (uidValidity, lastUID uint32, err error)
	SetFolderState(name string, uidValidity, lastUID uint32) error
}

// Source is the read-only side.
type Source interface {
	SelectFolder() (uidValidity, uidNext, numMessages uint32, err error)
	SelectNamedFolder(name string) (uidValidity, uidNext, numMessages uint32, err error)
	ListFolders() ([]imapx.FolderInfo, error)
	FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error)
	FetchFull(uid imap.UID) (*imapx.FullMessage, error)
}

// Dest is the append-only side.
type Dest interface {
	SelectFolder() (uidValidity, uidNext, numMessages uint32, err error)
	FetchMetaRange(start, stop imap.UID) ([]imapx.MsgMeta, error)
	HasMessageID(messageID string) (bool, error)
	Append(msg *imapx.FullMessage, flags []imap.Flag) error
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

	// Label phase first, so labels are known by the time messages are
	// appended. (Leaves a label folder selected; the SelectFolder below
	// re-selects the mirror source folder.)
	if r.opts.SyncLabels {
		if !r.dst.SupportsArbitraryKeywords() {
			r.log.Warn("destination does not advertise arbitrary keyword support (PERMANENTFLAGS \\*); labels may be dropped")
		}
		if err := r.scanLabelFolders(ctx); err != nil {
			return sum, fmt.Errorf("label scan: %w", err)
		}
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
	}

	return sum, nil
}

// mirrorOne applies the three dedup layers to a single candidate and appends
// it if — and only if — it is genuinely new.
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

	// Layer 3: destination guard — closes the "appended but not yet
	// recorded" crash window by asking the destination directly.
	if r.opts.DestGuard && IsRealMessageID(key) {
		has, err := r.dst.HasMessageID(key)
		if err != nil {
			return err
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
		labels, err := r.store.LabelsFor(key)
		if err != nil {
			return err
		}
		keywords = keywordFlags(labels)
		flags = append(append([]imap.Flag{}, baseFlags...), keywords...)
	}

	// APPEND first, record after — never the other way around.
	if err := r.dst.Append(full, flags); err != nil {
		if len(keywords) == 0 {
			return err
		}
		// The mirror always wins over label decoration: retry once without
		// keywords before treating this as an error.
		r.log.Warn("append with label keywords failed; retrying without keywords",
			"uid", m.UID, "keywords", len(keywords), "err", err)
		if err := r.dst.Append(full, baseFlags); err != nil {
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

// SeedFromDest streams the destination folder's dedup keys into the store in
// batches. This bootstraps idempotency against a pre-populated destination and
// re-derives the truth after local state loss. Memory stays bounded: one
// UID window of header metadata at a time.
func (r *Reconciler) SeedFromDest(ctx context.Context) (int64, error) {
	_, uidNext, numMessages, err := r.dst.SelectFolder()
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
	}
	return seeded, nil
}
