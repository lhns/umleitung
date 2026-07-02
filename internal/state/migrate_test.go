package state

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// buildLegacyDB creates a database exactly as the pre-migration deployed
// version did: schema via CREATE TABLE IF NOT EXISTS, no user_version ever
// set (reads as 0), with realistic data.
func buildLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	legacySchema := `
CREATE TABLE IF NOT EXISTS copied (
	message_id TEXT PRIMARY KEY,
	uid        INTEGER NOT NULL DEFAULT 0,
	copied_at  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS labels (
	message_id TEXT NOT NULL,
	label      TEXT NOT NULL,
	PRIMARY KEY (message_id, label)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS folders (
	name        TEXT PRIMARY KEY,
	uidvalidity INTEGER NOT NULL DEFAULT 0,
	last_uid    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
INSERT INTO copied VALUES ('<a@x>', 10, 1000), ('<b@x>', 20, 2000);
INSERT INTO meta VALUES ('uidvalidity', '42'), ('last_uid', '20');
INSERT INTO labels VALUES ('<a@x>', 'Work'), ('<a@x>', 'Friends/Close'), ('<b@x>', 'Work');
INSERT INTO folders VALUES ('Work', 7, 5), ('Friends/Close', 8, 3);
`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrateLegacyDBWithoutVersioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	buildLegacyDB(t, path)
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("legacy db user_version = %d, want 0 (no migration system)", v)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade from legacy db failed: %v", err)
	}
	defer s.Close()

	// Dedup state preserved.
	if has, _ := s.HasKey("<a@x>"); !has {
		t.Fatal("copied row lost in migration")
	}
	if n, _ := s.CopiedCount(); n != 2 {
		t.Fatalf("copied count = %d, want 2", n)
	}
	if v, _ := s.UIDValidity(); v != 42 {
		t.Fatalf("uidvalidity lost: %d", v)
	}
	if u, _ := s.LastUID(); u != 20 {
		t.Fatalf("last_uid lost: %d", u)
	}

	// Labels migrated into members.
	folders, err := s.MemberFolders("<a@x>")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || folders[0] != "Friends/Close" || folders[1] != "Work" {
		t.Fatalf("migrated members = %v, want [Friends/Close Work]", folders)
	}
	// Migrated rows have uid=0 placeholders — excluded from uid-based diffing.
	if m, _ := s.MemberUIDKeys("Work"); len(m) != 0 {
		t.Fatalf("uid=0 placeholder rows leaked into MemberUIDKeys: %v", m)
	}
	// But they DO count as membership.
	if has, _ := s.MemberHas("Work", "<b@x>"); !has {
		t.Fatal("migrated membership lost")
	}

	// Folder scan state cleared -> forces full rebuild (refreshes real uids).
	if v, u, _ := s.FolderState("Work"); v != 0 || u != 0 {
		t.Fatalf("folders not cleared: (%d, %d)", v, u)
	}
}

func TestMigrateFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if v := userVersion(t, path); v != schemaVersion {
		t.Fatalf("fresh db version = %d, want %d", v, schemaVersion)
	}
}

func TestMigrateIsIdempotentAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	buildLegacyDB(t, path)
	for i := range 3 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		s.Close()
	}
	if v := userVersion(t, path); v != schemaVersion {
		t.Fatalf("version = %d, want %d", v, schemaVersion)
	}
}

func TestRefuseNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+7)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("opened a db from a newer version — downgrade protection missing")
	}
}
