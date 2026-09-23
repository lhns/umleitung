package imapx

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestParseMetaHeader(t *testing.T) {
	for _, tc := range []struct {
		name               string
		hdr                string
		mid, from, subject string
	}{
		{"all fields",
			"Message-Id: <abc@example.com>\r\nFrom: Alice <alice@example.com>\r\nSubject: Hello world\r\n\r\n",
			"<abc@example.com>", "Alice <alice@example.com>", "Hello world"},
		{"folded Message-ID",
			"Message-ID:\r\n <folded@example.com>\r\n\r\n",
			"<folded@example.com>", "", ""},
		{"missing Message-ID",
			"From: a@b.c\r\nSubject: no id here\r\n\r\n",
			"", "a@b.c", "no id here"},
		{"empty", "", "", "", ""},
	} {
		mid, from, subject := parseMetaHeader([]byte(tc.hdr))
		if mid != tc.mid || from != tc.from || subject != tc.subject {
			t.Errorf("%s: got (%q, %q, %q), want (%q, %q, %q)", tc.name, mid, from, subject, tc.mid, tc.from, tc.subject)
		}
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

	// A full guard chunk must stay shallow (Stalwart rejected the previous
	// 99-deep linear chain with "BAD Too many nested filters").
	ids := make([]string, defaultGuardChunk)
	for i := range ids {
		ids[i] = fmt.Sprintf("<m%d@x>", i)
	}
	tree := orMessageIDCriteria(ids)
	if d := orDepth(tree); d > 8 {
		t.Fatalf("OR-tree depth = %d for %d ids, want <= 8 (balanced)", d, len(ids))
	}
	leaves := map[string]int{}
	countLeaves(tree, leaves)
	if len(leaves) != len(ids) {
		t.Fatalf("leaves = %d, want %d", len(leaves), len(ids))
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

func selectOK(tag string) []string {
	return []string{"* 0 EXISTS", "* OK [UIDVALIDITY 1] ok", tag + " OK [READ-WRITE] SELECT completed"}
}

// A server that rejects OR-trees with BAD must make the guard fall back to
// smaller chunks (down to single-id searches) and remember that size.
func TestSearchMessageIDsInShrinksChunkOnBad(t *testing.T) {
	const hdr = "Message-Id: <hit@x>\r\n\r\n"
	var orSearches atomic.Int32
	ep := scriptedServer(t, func(tag, cmd string) []string {
		switch {
		case strings.HasPrefix(cmd, "SELECT"):
			return selectOK(tag)
		case strings.HasPrefix(cmd, "UID SEARCH") && strings.Contains(cmd, "OR "):
			orSearches.Add(1)
			return []string{tag + " BAD Too many nested filters"}
		case strings.HasPrefix(cmd, "UID SEARCH") && strings.Contains(cmd, "<hit@x>"):
			return []string{"* SEARCH 7", tag + " OK SEARCH completed"}
		case strings.HasPrefix(cmd, "UID SEARCH"):
			return []string{"* SEARCH", tag + " OK SEARCH completed"}
		case strings.HasPrefix(cmd, "UID FETCH"):
			return []string{
				fmt.Sprintf("* 1 FETCH (UID 7 BODY[HEADER.FIELDS (Message-Id From Subject)] {%d}", len(hdr)),
				hdr + ")",
				tag + " OK FETCH completed",
			}
		}
		return []string{tag + " BAD unexpected"}
	})
	cl, err := Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	ids := []string{"<a@x>", "<hit@x>", "<c@x>"}
	found, err := cl.SearchMessageIDsIn("INBOX", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !found["<hit@x>"] {
		t.Fatalf("found = %v, want only <hit@x>", found)
	}
	if cl.guardChunk != 1 {
		t.Fatalf("guardChunk = %d, want 1", cl.guardChunk)
	}
	// The shrunken chunk size sticks: no further OR searches.
	before := orSearches.Load()
	if _, err := cl.SearchMessageIDsIn("INBOX", ids); err != nil {
		t.Fatal(err)
	}
	if n := orSearches.Load(); n != before {
		t.Fatalf("second call issued %d OR searches, want 0", n-before)
	}
}

// NO is a real failure, not a complexity limit: no retry, chunk unchanged.
func TestSearchMessageIDsInFailsOnNo(t *testing.T) {
	ep := scriptedServer(t, func(tag, cmd string) []string {
		if strings.HasPrefix(cmd, "SELECT") {
			return selectOK(tag)
		}
		return []string{tag + " NO server unavailable"}
	})
	cl, err := Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if _, err := cl.SearchMessageIDsIn("INBOX", []string{"<a@x>", "<b@x>"}); err == nil {
		t.Fatal("want error")
	}
	if cl.guardChunk != defaultGuardChunk {
		t.Fatalf("guardChunk = %d, want %d", cl.guardChunk, defaultGuardChunk)
	}
}

// Regression: Close used to block forever on a half-open connection that
// never answers LOGOUT, wedging the mirror supervisor.
func TestCloseDoesNotHangOnUnansweredLogout(t *testing.T) {
	ep := scriptedServer(t, func(tag, cmd string) []string { return nil })
	cl, err := Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { logoutTimeout = d }(logoutTimeout)
	logoutTimeout = 50 * time.Millisecond

	done := make(chan struct{})
	go func() { cl.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on unanswered LOGOUT")
	}
}
