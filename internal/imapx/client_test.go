package imapx

import "testing"

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

func TestParseMetaHeaderEmpty(t *testing.T) {
	if mid, from, subject := parseMetaHeader(nil); mid != "" || from != "" || subject != "" {
		t.Fatal("non-empty result for empty header")
	}
}
