package state

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the schema version this binary requires.
const schemaVersion = 2

// migrations[i] migrates from user_version i to i+1. Each runs in one
// transaction; user_version is bumped inside the same transaction.
//
// v0 -> v1: the schema as deployed by versions WITHOUT a migration system
// (those never set user_version, so their databases read as 0 — that is the
// designed legacy starting point). All statements are IF NOT EXISTS: a no-op
// against an existing legacy database (all rows preserved), a fresh create
// otherwise.
//
// v1 -> v2: generalized folder-membership tracking. `labels` rows migrate
// into `members` (a label is just membership in a label folder) with uid=0
// placeholders; `folders` scan state is cleared so the membership engine
// does a full rebuild on the next reconcile, refreshing real UIDs and
// diffing by message_id — the uid=0 placeholders never participate in
// uid-based removal detection. Adds the `pending` operation queue.
var migrations = []string{
	// v0 -> v1
	`
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
`,
	// v1 -> v2
	`
CREATE TABLE members (
	folder     TEXT NOT NULL,
	message_id TEXT NOT NULL,
	uid        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (folder, message_id)
) WITHOUT ROWID;
CREATE INDEX idx_members_message_id ON members (message_id);
CREATE INDEX idx_members_folder_uid ON members (folder, uid);
CREATE TABLE pending (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL, -- 'move' | 'keyword'
	message_id TEXT NOT NULL,
	folder     TEXT NOT NULL, -- the label / membership folder that changed
	op         TEXT NOT NULL  -- 'add' | 'remove'
);
INSERT INTO members (folder, message_id, uid)
	SELECT label, message_id, 0 FROM labels;
DROP TABLE labels;
DELETE FROM folders;
`,
}

// migrate brings the database to schemaVersion. Refuses to open databases
// created by a NEWER binary (downgrade protection).
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("state db has schema version %d, but this binary supports at most %d — refusing to downgrade", version, schemaVersion)
	}
	for v := version; v < schemaVersion; v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate schema v%d -> v%d: %w", v, v+1, err)
		}
		// PRAGMA does not support placeholders; v+1 is a trusted constant.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump schema version to %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration to v%d: %w", v+1, err)
		}
	}
	return nil
}
