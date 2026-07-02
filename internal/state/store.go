// Package state persists sync state in SQLite (pure-Go driver, no cgo).
//
// Dedup keys are looked up per message via an indexed query — the full set is
// never loaded into memory, so state size is bounded by disk, not RAM.
package state

import (
	"database/sql"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"
)

const schema = `
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
`

// Store is the persistent sync state. Safe for a single process (Umleiter
// enforces single-process via a file lock); SQLite serializes writers anyway.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the state database at path.
func Open(path string) (*Store, error) {
	// WAL for durable, non-blocking commits; busy_timeout as a safety net.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// Single connection: one writer, no lock contention with ourselves.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init state schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) metaGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) metaSet(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) metaGetUint(key string) (uint32, error) {
	v, err := s.metaGet(key)
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("meta %s: corrupt value %q: %w", key, v, err)
	}
	return uint32(n), nil
}

// UIDValidity returns the last-seen source UIDVALIDITY (0 = never seen).
func (s *Store) UIDValidity() (uint32, error) { return s.metaGetUint("uidvalidity") }

// SetUIDValidity stores the source UIDVALIDITY.
func (s *Store) SetUIDValidity(v uint32) error {
	return s.metaSet("uidvalidity", strconv.FormatUint(uint64(v), 10))
}

// LastUID returns the high-water mark within the current UIDVALIDITY.
func (s *Store) LastUID() (uint32, error) { return s.metaGetUint("last_uid") }

// SetLastUID stores the high-water mark.
func (s *Store) SetLastUID(uid uint32) error {
	return s.metaSet("last_uid", strconv.FormatUint(uint64(uid), 10))
}

// HasKey reports whether a dedup key has already been recorded (indexed lookup).
func (s *Store) HasKey(key string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM copied WHERE message_id = ?`, key).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// RecordKey records a dedup key after a confirmed successful append.
// Idempotent: re-recording an existing key is a no-op.
func (s *Store) RecordKey(key string, uid uint32, copiedAtUnix int64) error {
	_, err := s.db.Exec(`INSERT INTO copied (message_id, uid, copied_at) VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO NOTHING`, key, uid, copiedAtUnix)
	return err
}

// CopiedCount returns the number of recorded dedup keys.
func (s *Store) CopiedCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copied`).Scan(&n)
	return n, err
}

// AddLabel records that the message with the given dedup key carries a label
// (source label-folder membership). Idempotent.
func (s *Store) AddLabel(key, label string) error {
	_, err := s.db.Exec(`INSERT INTO labels (message_id, label) VALUES (?, ?)
		ON CONFLICT(message_id, label) DO NOTHING`, key, label)
	return err
}

// LabelsFor returns the labels recorded for a dedup key, sorted.
func (s *Store) LabelsFor(key string) ([]string, error) {
	rows, err := s.db.Query(`SELECT label FROM labels WHERE message_id = ? ORDER BY label`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		labels = append(labels, l)
	}
	return labels, rows.Err()
}

// FolderState returns the per-folder UIDVALIDITY and UID high-water mark used
// by the label scan (0, 0 if the folder was never scanned).
func (s *Store) FolderState(name string) (uidValidity, lastUID uint32, err error) {
	var v, u uint32
	err = s.db.QueryRow(`SELECT uidvalidity, last_uid FROM folders WHERE name = ?`, name).Scan(&v, &u)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return v, u, err
}

// SetFolderState stores the per-folder UIDVALIDITY and UID high-water mark.
func (s *Store) SetFolderState(name string, uidValidity, lastUID uint32) error {
	_, err := s.db.Exec(`INSERT INTO folders (name, uidvalidity, last_uid) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid`,
		name, uidValidity, lastUID)
	return err
}

// SeedBatch inserts a batch of dedup keys inside one transaction (used when
// seeding from the destination folder). Existing keys are skipped.
func (s *Store) SeedBatch(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO copied (message_id, uid, copied_at) VALUES (?, 0, 0)
		ON CONFLICT(message_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, k := range keys {
		if _, err := stmt.Exec(k); err != nil {
			return err
		}
	}
	return tx.Commit()
}
