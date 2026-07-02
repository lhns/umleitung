package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/lhns/umleitung/internal/imapx"
)

func TestDedupKeyUsesMessageID(t *testing.T) {
	m := &imapx.MsgMeta{MessageID: "<abc@example.com>"}
	if got := DedupKey(m); got != "<abc@example.com>" {
		t.Fatalf("DedupKey = %q, want raw Message-ID", got)
	}
	if !IsRealMessageID(DedupKey(m)) {
		t.Fatal("real Message-ID misclassified as synthesized")
	}
}

func TestDedupKeyTrimsWhitespace(t *testing.T) {
	m := &imapx.MsgMeta{MessageID: "  <abc@example.com>\t"}
	if got := DedupKey(m); got != "<abc@example.com>" {
		t.Fatalf("DedupKey = %q, want trimmed", got)
	}
}

func TestDedupKeySynthesisIsStable(t *testing.T) {
	date := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	a := &imapx.MsgMeta{From: "a@b.c", Subject: "hi", InternalDate: date, Size: 123}
	b := &imapx.MsgMeta{From: "a@b.c", Subject: "hi", InternalDate: date, Size: 123}
	ka, kb := DedupKey(a), DedupKey(b)
	if ka != kb {
		t.Fatalf("synthesized keys differ for identical input: %q vs %q", ka, kb)
	}
	if !strings.HasPrefix(ka, synthPrefix) {
		t.Fatalf("synthesized key %q missing prefix", ka)
	}
	if IsRealMessageID(ka) {
		t.Fatal("synthesized key misclassified as real Message-ID")
	}
}

func TestDedupKeySynthesisDistinguishesMessages(t *testing.T) {
	date := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	base := imapx.MsgMeta{From: "a@b.c", Subject: "hi", InternalDate: date, Size: 123}
	variants := []imapx.MsgMeta{base, base, base, base}
	variants[1].From = "x@b.c"
	variants[2].Subject = "hi there"
	variants[3].Size = 124
	seen := map[string]bool{}
	for i := range variants {
		k := DedupKey(&variants[i])
		if i > 0 && seen[k] {
			t.Fatalf("variant %d collided", i)
		}
		seen[k] = true
	}
	// Different timestamp must change the key too.
	shifted := base
	shifted.InternalDate = date.Add(time.Second)
	if seen[DedupKey(&shifted)] {
		t.Fatal("timestamp shift did not change synthesized key")
	}
}

// A crafted From/Subject pair must not collide with a differently-split pair
// (field-separator injection).
func TestDedupKeySynthesisSeparatorSafety(t *testing.T) {
	date := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	a := &imapx.MsgMeta{From: "a", Subject: "b\x00c", InternalDate: date, Size: 1}
	b := &imapx.MsgMeta{From: "a\x00b", Subject: "c", InternalDate: date, Size: 1}
	if DedupKey(a) == DedupKey(b) {
		t.Fatal("separator injection caused key collision")
	}
}
