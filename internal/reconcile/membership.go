package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// pendingMove/pendingKeyword are the pending-op kinds (state.pending.kind).
const (
	pendingMove    = "move"
	pendingKeyword = "keyword"
)

// syncMembership scans every watched source folder (label folders and/or the
// source INBOX) and records membership changes. Runs before the mirror phase
// so routing and copy-time keywords see current state.
func (r *Reconciler) syncMembership(ctx context.Context) error {
	type watched struct{ name, kind string }
	var list []watched
	if r.opts.SyncLabels {
		folders, err := r.src.ListFolders()
		if err != nil {
			return err
		}
		exclude := r.opts.labelExcludeSet()
		kind := ""
		if r.opts.LabelPropagate {
			kind = pendingKeyword
		}
		for _, f := range folders {
			if f.Name == r.opts.SourceInbox || !isLabelFolder(f, r.opts.SourceFolder, exclude) {
				continue
			}
			list = append(list, watched{f.Name, kind})
		}
	}
	if r.opts.ArchiveRouting || r.opts.SentRouting {
		list = append(list, watched{r.opts.SourceInbox, pendingMove})
	}
	if r.opts.SentRouting {
		list = append(list, watched{r.opts.SentSrcFolder, pendingMove})
	}
	for i, w := range list {
		item := fmt.Sprintf("%s %d/%d", w.name, i+1, len(list))
		if err := r.syncWatchedFolder(ctx, w.name, w.kind, item); err != nil {
			return fmt.Errorf("folder %q: %w", w.name, err)
		}
	}
	return nil
}

// syncWatchedFolder diffs one source folder's membership against the stored
// members and records changes. pendingKind ("" = none) selects the pending
// destination operation enqueued for already-copied messages.
func (r *Reconciler) syncWatchedFolder(ctx context.Context, folder, pendingKind, item string) error {
	uidValidity, uidNext, _, err := r.src.SelectNamedFolder(folder)
	if err != nil {
		return err
	}
	storedValidity, lastUID, err := r.store.FolderState(folder)
	if err != nil {
		return err
	}

	if storedValidity != uidValidity {
		return r.rebuildWatchedFolder(ctx, folder, pendingKind, item, uidValidity, uidNext)
	}

	// Removal detection: uid-set diff against the full current snapshot.
	currentUIDs, err := r.src.SearchAllUIDs()
	if err != nil {
		return err
	}
	current := make(map[uint32]bool, len(currentUIDs))
	for _, u := range currentUIDs {
		current[uint32(u)] = true
	}
	stored, err := r.store.MemberUIDKeys(folder)
	if err != nil {
		return err
	}
	for uid, key := range stored {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !current[uid] {
			if err := r.memberChange(folder, key, 0, false, pendingKind); err != nil {
				return err
			}
		}
	}

	// Addition detection: windowed scan above the high-water mark.
	for start := lastUID + 1; start < uidNext; start += uint32(r.opts.UIDBatch) {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := min(start+uint32(r.opts.UIDBatch)-1, uidNext-1)
		metas, err := r.src.FetchMetaRange(imap.UID(start), imap.UID(stop))
		if err != nil {
			return fmt.Errorf("window %d:%d: %w", start, stop, err)
		}
		for i := range metas {
			key := DedupKey(&metas[i])
			if err := r.memberChange(folder, key, uint32(metas[i].UID), true, pendingKind); err != nil {
				return err
			}
		}
		if err := r.store.SetFolderState(folder, uidValidity, stop); err != nil {
			return err
		}
		r.opts.progress("membership", item, int(stop))
	}
	if uidNext <= 1 || lastUID >= uidNext-1 {
		// Nothing scanned; still keep state current.
		return r.store.SetFolderState(folder, uidValidity, lastUID)
	}
	return nil
}

// rebuildWatchedFolder handles first-time scans and UIDVALIDITY resets: a
// full windowed scan, diffed against stored membership BY KEY (stored uids
// are meaningless). Pending ops are suppressed when the stored set was empty
// (feature activation — the placement backfill covers existing mail).
func (r *Reconciler) rebuildWatchedFolder(ctx context.Context, folder, pendingKind, item string, uidValidity, uidNext uint32) error {
	storedKeys, err := r.store.MemberKeys(folder)
	if err != nil {
		return err
	}
	firstScan := len(storedKeys) == 0
	seen := map[string]bool{}
	for start := uint32(1); start < uidNext; start += uint32(r.opts.UIDBatch) {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := min(start+uint32(r.opts.UIDBatch)-1, uidNext-1)
		metas, err := r.src.FetchMetaRange(imap.UID(start), imap.UID(stop))
		if err != nil {
			return fmt.Errorf("rebuild window %d:%d: %w", start, stop, err)
		}
		for i := range metas {
			key := DedupKey(&metas[i])
			seen[key] = true
			kind := pendingKind
			if firstScan || storedKeys[key] {
				// Already a member (uid refresh) or first activation
				// (backfill handles placement/keywords for existing mail).
				kind = ""
			}
			if err := r.memberChange(folder, key, uint32(metas[i].UID), true, kind); err != nil {
				return err
			}
		}
		r.opts.progress("membership-rebuild", item, int(stop))
	}
	// Stored members no longer present anywhere in the folder -> removals.
	for key := range storedKeys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !seen[key] {
			if err := r.memberChange(folder, key, 0, false, pendingKind); err != nil {
				return err
			}
		}
	}
	return r.store.SetFolderState(folder, uidValidity, uidNext-1)
}

// memberChange records a membership change, enqueueing the pending
// destination op only when it is actually applicable: the message must
// already be mirrored and locatable by a real Message-ID.
func (r *Reconciler) memberChange(folder, key string, uid uint32, add bool, pendingKind string) error {
	if pendingKind != "" {
		copied, err := r.store.HasKey(key)
		if err != nil {
			return err
		}
		if !copied || !IsRealMessageID(key) {
			pendingKind = ""
		}
	}
	return r.store.MemberChange(folder, key, uid, add, pendingKind)
}

// labelsFor returns the labels of a message: its watched-folder memberships
// minus the routing folders (inbox/sent membership is placement, not a label).
func (r *Reconciler) labelsFor(key string) ([]string, error) {
	folders, err := r.store.MemberFolders(key)
	if err != nil {
		return nil, err
	}
	out := folders[:0]
	for _, f := range folders {
		if f == r.opts.SourceInbox || (r.opts.SentRouting && f == r.opts.SentSrcFolder) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// destFolderFor routes a message by source-folder membership, priority:
// inbox > sent > archive > primary. (A mail-to-self is in inbox AND sent —
// the inbox wins.)
func (r *Reconciler) destFolderFor(key string) (string, error) {
	if r.opts.ArchiveRouting || r.opts.SentRouting {
		inInbox, err := r.store.MemberHas(r.opts.SourceInbox, key)
		if err != nil {
			return "", err
		}
		if inInbox {
			return r.opts.DestFolder, nil
		}
	}
	if r.opts.SentRouting {
		inSent, err := r.store.MemberHas(r.opts.SentSrcFolder, key)
		if err != nil {
			return "", err
		}
		if inSent {
			return r.opts.SentFolder, nil
		}
	}
	if r.opts.ArchiveRouting {
		return r.opts.ArchiveFolder, nil
	}
	return r.opts.DestFolder, nil
}

// destBucketFolders lists every destination folder a mirrored message may
// live in under the current routing configuration.
func (r *Reconciler) destBucketFolders() []string {
	folders := []string{r.opts.DestFolder}
	if r.opts.SentRouting {
		folders = append(folders, r.opts.SentFolder)
	}
	if r.opts.ArchiveRouting {
		folders = append(folders, r.opts.ArchiveFolder)
	}
	return folders
}

// countMove attributes a completed move to the summary by target folder.
func (r *Reconciler) countMove(sum *Summary, desired string) {
	switch desired {
	case r.opts.ArchiveFolder:
		sum.MovedToArchive++
	case r.opts.SentFolder:
		sum.MovedToSent++
	default:
		sum.MovedToInbox++
	}
}

// propagate drains the pending-operation queue: moves for inbox-membership
// changes, keyword STOREs for label changes. A pending row is deleted only
// after the operation is confirmed (or definitively unnecessary); on error
// it survives and is retried next reconcile.
func (r *Reconciler) propagate(ctx context.Context, sum *Summary) error {
	for {
		ops, err := r.store.PendingOps(200)
		if err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		for _, op := range ops {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.applyPending(op, sum); err != nil {
				return fmt.Errorf("pending op %d (%s %s %q): %w", op.ID, op.Kind, op.Op, op.Folder, err)
			}
			if err := r.store.DeletePending(op.ID); err != nil {
				return err
			}
		}
	}
}

func (r *Reconciler) applyPending(op PendingOp, sum *Summary) error {
	switch op.Kind {
	case pendingMove:
		if !r.opts.ArchiveRouting && !r.opts.SentRouting {
			return nil // feature disabled since enqueue; drop
		}
		// Recompute the desired bucket from current membership (robust
		// against stacked/stale ops) and move the copy there from whichever
		// bucket it currently sits in.
		desired, err := r.destFolderFor(op.MessageID)
		if err != nil {
			return err
		}
		for _, folder := range r.destBucketFolders() {
			if folder == desired {
				continue
			}
			moved, err := r.dst.MoveMessageID(folder, desired, op.MessageID)
			if err != nil {
				return err
			}
			if moved {
				r.countMove(sum, desired)
				break
			}
		}
		return nil
	case pendingKeyword:
		if !r.opts.LabelPropagate {
			return nil
		}
		kw := keywordFor(op.Folder)
		if kw == "" {
			return nil
		}
		for _, folder := range r.destBucketFolders() {
			found, err := r.dst.StoreKeywordByMessageID(folder, op.MessageID, op.Op == "add", imap.Flag(kw))
			if err != nil {
				return err
			}
			if found {
				sum.KeywordsUpdated++
				break
			}
		}
		return nil
	default:
		return nil // unknown kind from a future version: drop (downgrade protection exists anyway)
	}
}

// backfillFingerprint canonically encodes the placement-relevant config.
func (r *Reconciler) backfillFingerprint() string {
	return strings.Join([]string{
		fmt.Sprintf("routing=%t", r.opts.ArchiveRouting),
		"inbox=" + r.opts.SourceInbox,
		"dest=" + r.opts.DestFolder,
		"archive=" + r.opts.ArchiveFolder,
		fmt.Sprintf("sent=%t", r.opts.SentRouting),
		"sentsrc=" + r.opts.SentSrcFolder,
		"sentdst=" + r.opts.SentFolder,
		fmt.Sprintf("labels=%t", r.opts.SyncLabels),
	}, ";")
}

// maybeBackfill auto-corrects mail mirrored before the current routing/label
// configuration was active: moves messages to the correct dest folder and
// adds missing label keywords (add-only — a stale keyword is
// indistinguishable from a user-set tag, so backfill never removes).
// Idempotent; the fingerprint is stored only after full completion.
func (r *Reconciler) maybeBackfill(ctx context.Context, sum *Summary) error {
	fp := r.backfillFingerprint()
	stored, err := r.store.MetaGet("backfill_fingerprint")
	if err != nil {
		return err
	}
	if stored == fp {
		return nil
	}
	if r.opts.ArchiveRouting || r.opts.SentRouting || r.opts.SyncLabels {
		r.log.Info("running placement/keyword backfill", "fingerprint", fp)
		for _, folder := range r.destBucketFolders() {
			if err := r.backfillDestFolder(ctx, folder, sum); err != nil {
				return fmt.Errorf("backfill %q: %w", folder, err)
			}
		}
	}
	return r.store.MetaSet("backfill_fingerprint", fp)
}

const moveChunk = 500

func (r *Reconciler) backfillDestFolder(ctx context.Context, folder string, sum *Summary) error {
	_, uidNext, numMessages, err := r.dst.SelectNamedFolder(folder)
	if err != nil {
		return err
	}
	if numMessages == 0 {
		return nil
	}

	routing := r.opts.ArchiveRouting || r.opts.SentRouting
	wrongByDest := map[string][]imap.UID{} // desired folder -> uids to move there
	kwGroups := map[string][]imap.UID{}    // sorted missing-keyword signature -> uids
	kwFlags := map[string][]imap.Flag{}

	for start := uint32(1); start < uidNext; start += uint32(r.opts.UIDBatch) {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := min(start+uint32(r.opts.UIDBatch)-1, uidNext-1)
		metas, err := r.dst.FetchMetaRange(imap.UID(start), imap.UID(stop))
		if err != nil {
			return fmt.Errorf("window %d:%d: %w", start, stop, err)
		}
		r.opts.progress("backfill", folder, int(stop))
		for i := range metas {
			key := DedupKey(&metas[i])

			if routing {
				want, err := r.destFolderFor(key)
				if err != nil {
					return err
				}
				if want != folder {
					wrongByDest[want] = append(wrongByDest[want], metas[i].UID)
					continue // keywords follow the message; fixed after the move next backfill-free run
				}
			}

			if r.opts.SyncLabels {
				labels, err := r.labelsFor(key)
				if err != nil {
					return err
				}
				missing := missingKeywords(labels, metas[i].Flags)
				if len(missing) > 0 {
					sig := flagSig(missing)
					kwGroups[sig] = append(kwGroups[sig], metas[i].UID)
					kwFlags[sig] = missing
				}
			}
		}
	}

	// Apply keyword additions first (STOREs reference UIDs in this folder,
	// which must happen before those messages potentially move away).
	for sig, uids := range kwGroups {
		for c := range slicesChunk(len(uids), moveChunk) {
			if err := r.dst.StoreKeywordsUIDs(uids[c[0]:c[1]], kwFlags[sig]); err != nil {
				return err
			}
		}
		sum.KeywordsUpdated += len(uids)
	}

	// Then move wrong-side messages to their desired buckets, in chunks.
	for want, uids := range wrongByDest {
		for c := range slicesChunk(len(uids), moveChunk) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.dst.MoveUIDs(folder, uids[c[0]:c[1]], want); err != nil {
				return err
			}
		}
		for range uids {
			r.countMove(sum, want)
		}
	}
	return nil
}

// missingKeywords returns keyword flags for labels not yet present in flags.
func missingKeywords(labels []string, flags []imap.Flag) []imap.Flag {
	present := map[string]bool{}
	for _, f := range flags {
		present[strings.ToLower(string(f))] = true
	}
	var missing []imap.Flag
	for _, kw := range keywordFlags(labels) {
		if !present[string(kw)] {
			missing = append(missing, kw)
		}
	}
	return missing
}

func flagSig(flags []imap.Flag) string {
	ss := make([]string, len(flags))
	for i, f := range flags {
		ss[i] = string(f)
	}
	sort.Strings(ss)
	return strings.Join(ss, "\x00")
}

// slicesChunk yields [start, end) index pairs over n items in chunks.
func slicesChunk(n, chunk int) func(func([2]int) bool) {
	return func(yield func([2]int) bool) {
		for start := 0; start < n; start += chunk {
			end := min(start+chunk, n)
			if !yield([2]int{start, end}) {
				return
			}
		}
	}
}
