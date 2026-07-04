package reconcile

import (
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

// labelKeyword maps one label to its destination keyword flag, applying the
// configured prefix (e.g. "$label:" for Bulwark) outside sanitization. Returns
// "" when the sanitized slug is empty.
func (r *Reconciler) labelKeyword(label string) imap.Flag {
	slug := keywordFor(label)
	if slug == "" {
		return ""
	}
	return imap.Flag(r.opts.KeywordPrefix + slug)
}

// labelKeywords converts recorded labels to keyword flags (deduplicated,
// empty results skipped).
func (r *Reconciler) labelKeywords(labels []string) []imap.Flag {
	var flags []imap.Flag
	seen := map[imap.Flag]bool{}
	for _, l := range labels {
		kw := r.labelKeyword(l)
		if kw == "" || seen[kw] {
			continue
		}
		seen[kw] = true
		flags = append(flags, kw)
	}
	return flags
}

// The membership scan itself lives in membership.go — label folders and the
// source INBOX share one generalized, diff-based engine.
