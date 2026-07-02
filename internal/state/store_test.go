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

func TestMembersRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.MemberChange("Work", "<a@x>", 5, true, ""); err != nil {
		t.Fatal(err)
	}
	// Re-add updates the uid (rescan path) without error.
	if err := s.MemberChange("Work", "<a@x>", 6, true, ""); err != nil {
		t.Fatalf("re-add errored: %v", err)
	}
	if err := s.MemberChange("Friends/Close", "<a@x>", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	folders, err := s.MemberFolders("<a@x>")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || folders[0] != "Friends/Close" || folders[1] != "Work" {
		t.Fatalf("folders = %v, want sorted [Friends/Close Work]", folders)
	}
	if folders, _ := s.MemberFolders("<unknown@x>"); len(folders) != 0 {
		t.Fatalf("unknown key returned folders: %v", folders)
	}
	uidKeys, err := s.MemberUIDKeys("Work")
	if err != nil {
		t.Fatal(err)
	}
	if len(uidKeys) != 1 || uidKeys[6] != "<a@x>" {
		t.Fatalf("uidKeys = %v, want {6: <a@x>}", uidKeys)
	}
	// Removal.
	if err := s.MemberChange("Work", "<a@x>", 0, false, ""); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.MemberHas("Work", "<a@x>"); has {
		t.Fatal("member not removed")
	}
}

func TestMemberChangeWithPendingIsAtomic(t *testing.T) {
	s := openTemp(t)
	if err := s.MemberChange("Work", "<a@x>", 5, true, "keyword"); err != nil {
		t.Fatal(err)
	}
	ops, err := s.PendingOps(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Kind != "keyword" || ops[0].Op != "add" || ops[0].Folder != "Work" {
		t.Fatalf("pending = %+v, want one keyword-add", ops)
	}
	if err := s.DeletePending(ops[0].ID); err != nil {
		t.Fatal(err)
	}
	if ops, _ := s.PendingOps(10); len(ops) != 0 {
		t.Fatalf("pending not drained: %+v", ops)
	}
}

func TestMemberChangeBatchAtomic(t *testing.T) {
	s := openTemp(t)
	items := []MemberChangeItem{
		{Key: "<a@x>", UID: 1, Add: true, PendingKind: "keyword"},
		{Key: "<b@x>", UID: 2, Add: true},
		{Key: "<a@x>", Add: false, PendingKind: "move"},
	}
	if err := s.MemberChangeBatch("Work", items); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.MemberHas("Work", "<a@x>"); has {
		t.Fatal("removal in same batch not applied")
	}
	if has, _ := s.MemberHas("Work", "<b@x>"); !has {
		t.Fatal("addition not applied")
	}
	ops, _ := s.PendingOps(10)
	if len(ops) != 2 || ops[0].Kind != "keyword" || ops[1].Kind != "move" {
		t.Fatalf("pending = %+v, want keyword-add then move-remove", ops)
	}
	// Batch delete drains both in one call.
	if err := s.DeletePendingBatch([]int64{ops[0].ID, ops[1].ID}); err != nil {
		t.Fatal(err)
	}
	if ops, _ := s.PendingOps(10); len(ops) != 0 {
		t.Fatalf("pending not drained: %+v", ops)
	}
	if err := s.MemberChangeBatch("Work", nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestRecordKeysBatch(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordKeys([]KeyRecord{
		{Key: "<a@x>", UID: 1, CopiedAtUnix: 1},
		{Key: "<b@x>", UID: 2, CopiedAtUnix: 2},
		{Key: "<a@x>", UID: 3, CopiedAtUnix: 3}, // idempotent within batch
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CopiedCount(); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	if err := s.RecordKeys(nil); err != nil {
		t.Fatalf("empty batch: %v", err)
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
