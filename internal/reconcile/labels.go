package reconcile

import (
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/lhns/umleitung/internal/imapx"
)

// excludedAttrs disqualify a folder from being a label: unselectable and
// special-use folders represent mailbox roles, not user labels.
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

// isLabelFolder reports whether a listed folder counts as a label: not the
// mirror source folder, INBOX, a special-use/unselectable folder or excluded.
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

// keywordFor maps a label (folder name) to an IMAP keyword slug. Keywords
// must be RFC 3501 atoms, so every rune other than ASCII alphanumerics, '-'
// and '_' becomes repl's first byte (default '_'); runs are collapsed and
// trimmed, and the result is lowercased (IMAP flags are case-insensitive).
// With repl "-": "[Werbung]" -> "werbung", "Work/Projects" -> "work-projects",
// "Bücher" -> "b-cher". Returns "" if nothing survives. Distinct labels may
// collide; documented and harmless.
func keywordFor(label, repl string) string {
	rc := byte('_')
	if len(repl) > 0 {
		rc = repl[0]
	}
	var b strings.Builder
	lastRepl := false
	for _, r := range label {
		keep := r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_')
		if keep {
			b.WriteByte(byte(r))
			lastRepl = false
		} else if !lastRepl {
			b.WriteByte(rc)
			lastRepl = true
		}
	}
	return strings.ToLower(strings.Trim(b.String(), string(rc)))
}

// labelKeyword maps one label to its keyword flag; the configured prefix is
// applied outside sanitization. Returns "" when the slug is empty.
func (r *Reconciler) labelKeyword(label string) imap.Flag {
	slug := keywordFor(label, r.opts.KeywordReplacement)
	if slug == "" {
		return ""
	}
	return imap.Flag(r.opts.KeywordPrefix + slug)
}

// labelKeywords converts labels to deduplicated, non-empty keyword flags.
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
