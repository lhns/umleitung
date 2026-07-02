package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
)

// excludedAttrs are folder attributes that disqualify a folder from being
// treated as a label: unselectable folders and special-use folders (Sent,
// Trash, Junk, All Mail, Starred, Important, ...). These represent mailbox
// roles, not user labels.
var excludedAttrs = map[imap.MailboxAttr]bool{
	imap.MailboxAttrNoSelect:    true,
	imap.MailboxAttrNonExistent: true,
	imap.MailboxAttrAll:         true,
	imap.MailboxAttrArchive:     true,
	imap.MailboxAttrDrafts:      true,
	imap.MailboxAttrFlagged:     true,
	imap.MailboxAttrJunk:        true,
	imap.MailboxAttrSent:        true,
	imap.MailboxAttrTrash:       true,
	imap.MailboxAttrImportant:   true,
}

// isLabelFolder decides whether a listed folder counts as a label folder.
// Excluded: the mirror source folder itself, INBOX (inbox membership is not
// a label), special-use/unselectable folders, and user-configured exclusions.
func isLabelFolder(f imapx.FolderInfo, sourceFolder string, exclude map[string]bool) bool {
	if f.Name == sourceFolder || strings.EqualFold(f.Name, "INBOX") {
		return false
	}
	if exclude[f.Name] {
		return false
	}
	for _, a := range f.Attrs {
		if excludedAttrs[a] {
			return false
		}
	}
	return true
}

// keywordFor maps a label (folder name) to an IMAP keyword. IMAP flag
// keywords must be RFC 3501 atoms — printable ASCII without ( ) { % * " \ ]
// or spaces — so labels are sanitized: every disallowed rune becomes '_',
// runs are collapsed and trimmed. The result is lowercased because IMAP
// flags are case-insensitive and servers canonicalize them anyway (this also
// matches Thunderbird's lowercase tag-key convention).
// ("[Werbung]" -> "werbung", "Work/Projects" -> "work_projects",
// "Bücher" -> "b_cher".)
// Returns "" (skip) if nothing survives. Distinct labels may collide after
// sanitization; documented and harmless.
func keywordFor(label string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range label {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.ToLower(strings.Trim(b.String(), "_"))
}

// keywordFlags converts recorded labels to IMAP keyword flags (deduplicated,
// empty results skipped).
func keywordFlags(labels []string) []imap.Flag {
	var flags []imap.Flag
	seen := map[string]bool{}
	for _, l := range labels {
		kw := keywordFor(l)
		if kw == "" || seen[kw] {
			continue
		}
		seen[kw] = true
		flags = append(flags, imap.Flag(kw))
	}
	return flags
}

// scanLabelFolders records label-folder membership (dedup key -> label) for
// every message in every label folder, using the same windowed, resumable
// pattern as the main mirror scan: per-folder UIDVALIDITY + last_uid
// high-water marks committed per window.
//
// Must run before the main mirror pass selects the source folder (it leaves
// a label folder selected).
func (r *Reconciler) scanLabelFolders(ctx context.Context) error {
	folders, err := r.src.ListFolders()
	if err != nil {
		return err
	}
	for _, f := range folders {
		if !isLabelFolder(f, r.opts.SourceFolder, r.opts.labelExcludeSet()) {
			continue
		}
		if err := r.scanOneLabelFolder(ctx, f.Name); err != nil {
			return fmt.Errorf("label folder %q: %w", f.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) scanOneLabelFolder(ctx context.Context, name string) error {
	uidValidity, uidNext, _, err := r.src.SelectNamedFolder(name)
	if err != nil {
		return err
	}
	storedValidity, lastUID, err := r.store.FolderState(name)
	if err != nil {
		return err
	}
	if storedValidity != uidValidity {
		// Fresh or reset folder: rescan from the beginning. The labels table
		// PK dedupes, so re-recording is harmless.
		lastUID = 0
	}
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
			if err := r.store.AddLabel(DedupKey(&metas[i]), name); err != nil {
				return err
			}
		}
		if err := r.store.SetFolderState(name, uidValidity, stop); err != nil {
			return err
		}
	}
	// Handle the empty-folder / no-new-mail case: still persist UIDVALIDITY.
	if lastUID == 0 && uidNext <= 1 {
		return r.store.SetFolderState(name, uidValidity, 0)
	}
	return nil
}
