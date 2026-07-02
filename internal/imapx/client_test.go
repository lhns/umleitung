package imapx

import (
	"fmt"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestParseMetaHeader(t *testing.T) {
	hdr := []byte("Message-Id: <abc@example.com>\r\n" +
		"From: Alice <alice@example.com>\r\n" +
		"Subject: Hello world\r\n" +
		"\r\n")
	mid, from, subject := parseMetaHeader(hdr)
	if mid != "<abc@example.com>" {
		t.Fatalf("mid = %q", mid)
	}
	if from == "" || subject == "" {
		t.Fatalf("from = %q, subject = %q", from, subject)
	}
}

func TestParseMetaHeaderFoldedMessageID(t *testing.T) {
	// RFC 5322 folded header line.
	hdr := []byte("Message-ID:\r\n <folded@example.com>\r\n\r\n")
	mid, _, _ := parseMetaHeader(hdr)
	if mid != "<folded@example.com>" {
		t.Fatalf("mid = %q, want folded value", mid)
	}
}

func TestParseMetaHeaderMissingMessageID(t *testing.T) {
	hdr := []byte("From: a@b.c\r\nSubject: no id here\r\n\r\n")
	mid, from, subject := parseMetaHeader(hdr)
	if mid != "" {
		t.Fatalf("mid = %q, want empty", mid)
	}
	if from != "a@b.c" || subject != "no id here" {
		t.Fatalf("from = %q, subject = %q", from, subject)
	}
}

func TestOrMessageIDCriteria(t *testing.T) {
	// 1 id: plain header criterion, no OR.
	c1 := orMessageIDCriteria([]string{"<a@x>"})
	if len(c1.Or) != 0 || len(c1.Header) != 1 || c1.Header[0].Value != "<a@x>" {
		t.Fatalf("single: %+v", c1)
	}
	// 2 ids: one OR pair.
	c2 := orMessageIDCriteria([]string{"<a@x>", "<b@x>"})
	if len(c2.Or) != 1 || c2.Or[0][0].Header[0].Value != "<a@x>" || c2.Or[0][1].Header[0].Value != "<b@x>" {
		t.Fatalf("pair: %+v", c2)
	}
	// 3 ids: balanced — left leaf a, right subtree OR(b, c).
	c3 := orMessageIDCriteria([]string{"<a@x>", "<b@x>", "<c@x>"})
	if len(c3.Or) != 1 || c3.Or[0][0].Header[0].Value != "<a@x>" {
		t.Fatalf("tree root: %+v", c3)
	}
	inner := c3.Or[0][1]
	if len(inner.Or) != 1 || inner.Or[0][0].Header[0].Value != "<b@x>" || inner.Or[0][1].Header[0].Value != "<c@x>" {
		t.Fatalf("tree inner: %+v", inner)
	}

	// A full guard chunk must stay shallow: servers cap filter nesting depth
	// (Stalwart: "BAD Too many nested filters" — seen in production with the
	// previous 99-deep linear chain).
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("<m%d@x>", i)
	}
	if d := orDepth(orMessageIDCriteria(ids)); d > 8 {
		t.Fatalf("OR-tree depth = %d for 100 ids, want <= 8 (balanced)", d)
	}
	// All leaves present exactly once.
	leaves := map[string]int{}
	countLeaves(orMessageIDCriteria(ids), leaves)
	if len(leaves) != 100 {
		t.Fatalf("leaves = %d, want 100", len(leaves))
	}
	for id, n := range leaves {
		if n != 1 {
			t.Fatalf("leaf %s appears %d times", id, n)
		}
	}
}

func orDepth(c *imap.SearchCriteria) int {
	if len(c.Or) == 0 {
		return 1
	}
	return 1 + max(orDepth(&c.Or[0][0]), orDepth(&c.Or[0][1]))
}

func countLeaves(c *imap.SearchCriteria, leaves map[string]int) {
	if len(c.Or) == 0 {
		leaves[c.Header[0].Value]++
		return
	}
	countLeaves(&c.Or[0][0], leaves)
	countLeaves(&c.Or[0][1], leaves)
}

func TestParseMetaHeaderEmpty(t *testing.T) {
	if mid, from, subject := parseMetaHeader(nil); mid != "" || from != "" || subject != "" {
		t.Fatal("non-empty result for empty header")
	}
}
