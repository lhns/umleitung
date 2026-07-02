package state

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTemp(t)

	if v, err := s.UIDValidity(); err != nil || v != 0 {
		t.Fatalf("fresh uidvalidity = %d, %v; want 0, nil", v, err)
	}
	if err := s.SetUIDValidity(424242); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastUID(987654); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.UIDValidity(); v != 424242 {
		t.Fatalf("uidvalidity = %d", v)
	}
	if u, _ := s.LastUID(); u != 987654 {
		t.Fatalf("last_uid = %d", u)
	}
	// Overwrite works.
	if err := s.SetLastUID(1); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.LastUID(); u != 1 {
		t.Fatalf("last_uid after overwrite = %d", u)
	}
}

func TestKeysPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordKey("<a@x>", 1, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIDValidity(7); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if has, _ := s2.HasKey("<a@x>"); !has {
		t.Fatal("key lost across reopen")
	}
	if has, _ := s2.HasKey("<b@x>"); has {
		t.Fatal("phantom key")
	}
	if v, _ := s2.UIDValidity(); v != 7 {
		t.Fatalf("uidvalidity lost: %d", v)
	}
}

func TestRecordKeyIsIdempotent(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordKey("<a@x>", 1, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordKey("<a@x>", 2, 2000); err != nil {
		t.Fatalf("re-record errored: %v", err)
	}
	n, err := s.CopiedCount()
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.AddLabel("<a@x>", "Work"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLabel("<a@x>", "Work"); err != nil {
		t.Fatalf("re-add errored: %v", err)
	}
	if err := s.AddLabel("<a@x>", "Friends/Close"); err != nil {
		t.Fatal(err)
	}
	labels, err := s.LabelsFor("<a@x>")
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0] != "Friends/Close" || labels[1] != "Work" {
		t.Fatalf("labels = %v, want sorted [Friends/Close Work]", labels)
	}
	if labels, _ := s.LabelsFor("<unknown@x>"); len(labels) != 0 {
		t.Fatalf("unknown key returned labels: %v", labels)
	}
}

func TestFolderStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, u, err := s.FolderState("Work"); err != nil || v != 0 || u != 0 {
		t.Fatalf("fresh folder state = (%d, %d, %v), want zeros", v, u, err)
	}
	if err := s.SetFolderState("Work", 42, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFolderState("Work", 42, 200); err != nil { // overwrite
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if v, u, _ := s2.FolderState("Work"); v != 42 || u != 200 {
		t.Fatalf("folder state lost across reopen: (%d, %d)", v, u)
	}
}

func TestSeedBatch(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordKey("<pre@x>", 1, 1); err != nil {
		t.Fatal(err)
	}
	// Batch containing a pre-existing key and a duplicate within the batch.
	if err := s.SeedBatch([]string{"<pre@x>", "<a@x>", "<b@x>", "<a@x>"}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.CopiedCount()
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
	if err := s.SeedBatch(nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}
