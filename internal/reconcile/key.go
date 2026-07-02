package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lhns/umleitung/internal/imapx"
)

// synthPrefix marks dedup keys synthesized for messages without a Message-ID.
const synthPrefix = "synth-sha256:"

// DedupKey returns the stable deduplication key for a message.
//
// Normally this is the trimmed raw Message-ID header value. For the rare (but
// legal) messages without one, a stable key is synthesized from a SHA-256 hash
// of (INTERNALDATE, From, Subject, size) — the same inputs are available when
// seeding from the destination, so both sides compute identical keys.
func DedupKey(m *imapx.MsgMeta) string {
	if id := strings.TrimSpace(m.MessageID); id != "" {
		return id
	}
	// Length-prefixed fields so crafted From/Subject values cannot shift
	// content across field boundaries and collide.
	h := sha256.New()
	fmt.Fprintf(h, "%d|%d:%s|%d:%s|%d",
		m.InternalDate.Unix(), len(m.From), m.From, len(m.Subject), m.Subject, m.Size)
	return synthPrefix + hex.EncodeToString(h.Sum(nil))
}

// IsRealMessageID reports whether key is an actual Message-ID (usable in an
// IMAP `SEARCH HEADER Message-ID` destination guard) rather than synthesized.
func IsRealMessageID(key string) bool {
	return !strings.HasPrefix(key, synthPrefix)
}
