package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/state"
)

// Pending-op kinds (state.pending.kind).
const (
	pendingMove    = "move"
	pendingKeyword = "keyword"
)

// syncMembership scans every watched source folder (label folders and/or the
// routing folders) and records membership changes.
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
			if r.isRoutingFolder(f.Name) || !isLabelFolder(f, r.opts.SourceFolder, exclude) {
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

	// Removals: stored uids missing from the current snapshot.
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
	var gone []string
	for uid, key := range stored {
		if !current[uid] {
			gone = append(gone, key)
		}
	}
	if err := r.recordRemovals(ctx, folder, pendingKind, gone); err != nil {
		return err
	}

	// Additions: windowed scan above the high-water mark.
	return r.scanWindows(ctx, lastUID+1, uidNext, r.src.FetchMetaRange, func(stop uint32, metas []imapx.MsgMeta) error {
		err := r.recordAdditions(folder, metas, func(string) string { return pendingKind })
		if err != nil {
			return err
		}
		if err := r.store.SetFolderState(folder, uidValidity, stop); err != nil {
			return err
		}
		r.opts.progress("membership", item, int(stop))
		return nil
	})
}

// rebuildWatchedFolder handles first-time scans and UIDVALIDITY resets: a
// full scan diffed against stored membership BY KEY (stored uids are
// meaningless). Pending ops are suppressed when nothing was stored (feature
// activation — the placement backfill covers existing mail).
func (r *Reconciler) rebuildWatchedFolder(ctx context.Context, folder, pendingKind, item string, uidValidity, uidNext uint32) error {
	storedKeys, err := r.store.MemberKeys(folder)
	if err != nil {
		return err
	}
	firstScan := len(storedKeys) == 0
	seen := map[string]bool{}
	kindFor := func(key string) string {
		seen[key] = true
		if firstScan || storedKeys[key] {
			return "" // activation or uid refresh, not a membership change
		}
		return pendingKind
	}
	err = r.scanWindows(ctx, 1, uidNext, r.src.FetchMetaRange, func(stop uint32, metas []imapx.MsgMeta) error {
		if err := r.recordAdditions(folder, metas, kindFor); err != nil {
			return err
		}
		r.opts.progress("membership-rebuild", item, int(stop))
		return nil
	})
	if err != nil {
		return err
	}
	var gone []string
	for key := range storedKeys {
		if !seen[key] {
			gone = append(gone, key)
		}
	}
	if err := r.recordRemovals(ctx, folder, pendingKind, gone); err != nil {
		return err
	}
	return r.store.SetFolderState(folder, uidValidity, max(uidNext, 1)-1)
}

// recordAdditions records metas as members of folder in one batch; kindFor
// returns each message's ungated pending-op kind.
func (r *Reconciler) recordAdditions(folder string, metas []imapx.MsgMeta, kindFor func(key string) string) error {
	items := make([]state.MemberChangeItem, 0, len(metas))
	for i := range metas {
		key := DedupKey(&metas[i])
		kind, err := r.gatePending(key, kindFor(key))
		if err != nil {
			return err
		}
		items = append(items, state.MemberChangeItem{Key: key, UID: uint32(metas[i].UID), Add: true, PendingKind: kind})
	}
	return r.store.MemberChangeBatch(folder, items)
}

// recordRemovals records keys as no longer members of folder in one batch.
func (r *Reconciler) recordRemovals(ctx context.Context, folder, pendingKind string, keys []string) error {
	items := make([]state.MemberChangeItem, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, err := r.gatePending(key, pendingKind)
		if err != nil {
			return err
		}
		items = append(items, state.MemberChangeItem{Key: key, PendingKind: kind})
	}
	return r.store.MemberChangeBatch(folder, items)
}

// gatePending returns pendingKind, or "" when no destination op applies: the
// message must already be mirrored and locatable by a real Message-ID.
func (r *Reconciler) gatePending(key, pendingKind string) (string, error) {
	if pendingKind == "" || !IsRealMessageID(key) {
		return "", nil
	}
	copied, err := r.store.HasKey(key)
	if err != nil || !copied {
		return "", err
	}
	return pendingKind, nil
}

// labelsFor returns a message's watched-folder memberships minus the routing
// folders.
func (r *Reconciler) labelsFor(key string) ([]string, error) {
	folders, err := r.store.MemberFolders(key)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(folders, r.isRoutingFolder), nil
}

// isRoutingFolder reports whether a watched source folder drives placement
// (inbox/sent) rather than labels.
func (r *Reconciler) isRoutingFolder(folder string) bool {
	return folder == r.opts.SourceInbox || (r.opts.SentRouting && folder == r.opts.SentSrcFolder)
}

// destFolderFor routes a message by source-folder membership, priority:
// inbox > sent > archive > primary (mail-to-self is in inbox AND sent).
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

// countMove attributes n completed moves to the summary by target folder.
func (r *Reconciler) countMove(sum *Summary, desired string, n int) {
	switch desired {
	case r.opts.ArchiveFolder:
		sum.MovedToArchive += n
	case r.opts.SentFolder:
		sum.MovedToSent += n
	default:
		sum.MovedToInbox += n
	}
}

// propagate drains the pending-operation queue. A row is deleted only after
// its operation is confirmed (or definitively unnecessary); on error it
// survives and is retried next reconcile.
func (r *Reconciler) propagate(ctx context.Context, sum *Summary) error {
	for {
		ops, err := r.store.PendingOps(200)
		if err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		// One delete transaction per page; on failure only the completed
		// prefix is deleted.
		var done []int64
		for _, op := range ops {
			if err := ctx.Err(); err != nil {
				_ = r.store.DeletePendingBatch(done)
				return err
			}
			if err := r.applyPending(op, sum); err != nil {
				_ = r.store.DeletePendingBatch(done)
				return fmt.Errorf("pending op %d (%s %s %q): %w", op.ID, op.Kind, op.Op, op.Folder, err)
			}
			done = append(done, op.ID)
		}
		if err := r.store.DeletePendingBatch(done); err != nil {
			return err
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
		// bucket holds it.
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
				r.countMove(sum, desired, 1)
				break
			}
		}
		return nil
	case pendingKeyword:
		if !r.opts.LabelPropagate {
			return nil
		}
		kw := r.labelKeyword(op.Folder)
		if kw == "" {
			return nil
		}
		for _, folder := range r.destBucketFolders() {
			found, err := r.dst.StoreKeywordByMessageID(folder, op.MessageID, op.Op == "add", kw)
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
		return nil // unknown kind from a future version: drop
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
		"kwprefix=" + r.opts.KeywordPrefix,
		"kwrepl=" + r.opts.KeywordReplacement,
	}, ";")
}

// maybeBackfill auto-corrects mail mirrored before the current routing/label
// configuration was active: moves messages to the right bucket and adds
// missing label keywords (add-only — a stale keyword is indistinguishable
// from a user tag). The fingerprint is stored only after full completion.
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

	err = r.scanWindows(ctx, 1, uidNext, r.dst.FetchMetaRange, func(stop uint32, metas []imapx.MsgMeta) error {
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
				}
			}
			if r.opts.SyncLabels {
				labels, err := r.labelsFor(key)
				if err != nil {
					return err
				}
				if missing := r.missingKeywords(labels, metas[i].Flags); len(missing) > 0 {
					sig := flagSig(missing)
					kwGroups[sig] = append(kwGroups[sig], metas[i].UID)
					kwFlags[sig] = missing
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Keywords first: STOREs address UIDs in this folder, so they must land
	// before those messages move away (keywords travel with the move).
	for sig, uids := range kwGroups {
		for chunk := range slices.Chunk(uids, moveChunk) {
			if err := r.dst.StoreKeywordsUIDs(chunk, kwFlags[sig]); err != nil {
				return err
			}
		}
		sum.KeywordsUpdated += len(uids)
	}
	for want, uids := range wrongByDest {
		for chunk := range slices.Chunk(uids, moveChunk) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.dst.MoveUIDs(folder, chunk, want); err != nil {
				return err
			}
		}
		r.countMove(sum, want, len(uids))
	}
	return nil
}

// missingKeywords returns keyword flags for labels not yet present in flags
// (case-insensitive; never removes existing keywords).
func (r *Reconciler) missingKeywords(labels []string, flags []imap.Flag) []imap.Flag {
	present := map[string]bool{}
	for _, f := range flags {
		present[strings.ToLower(string(f))] = true
	}
	var missing []imap.Flag
	for _, kw := range r.labelKeywords(labels) {
		if !present[strings.ToLower(string(kw))] {
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
	slices.Sort(ss)
	return strings.Join(ss, "\x00")
}
