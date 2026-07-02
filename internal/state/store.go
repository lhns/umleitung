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

// Store is the persistent sync state. Safe for a single process (Umleiter
// enforces single-process via a file lock); SQLite serializes writers anyway.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the state database at path and migrates it to the
// current schema version (see migrate.go). Databases created by older
// versions — including ones from before the migration system existed — are
// upgraded automatically and losslessly.
func Open(path string) (*Store, error) {
	// WAL for durable, non-blocking commits; busy_timeout as a safety net.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// Single connection: one writer, no lock contention with ourselves.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate state db: %w", err)
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

// PendingOp is a queued destination mutation (move or keyword change) that
// must be applied exactly-once-or-retried: enqueued in the same transaction
// as the membership change that caused it, deleted only after the
// STORE/MOVE is confirmed (or definitively unnecessary).
type PendingOp struct {
	ID        int64
	Kind      string // "move" | "keyword"
	MessageID string // dedup key
	Folder    string // the membership folder that changed
	Op        string // "add" | "remove"
}

// MemberChange atomically updates folder membership and — when pendingKind
// is "move" or "keyword" — enqueues the pending destination operation caused
// by it, in ONE transaction: a delta is either fully recorded or not at all.
func (s *Store) MemberChange(folder, key string, uid uint32, add bool, pendingKind string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if add {
		_, err = tx.Exec(`INSERT INTO members (folder, message_id, uid) VALUES (?, ?, ?)
			ON CONFLICT(folder, message_id) DO UPDATE SET uid = excluded.uid`, folder, key, uid)
	} else {
		_, err = tx.Exec(`DELETE FROM members WHERE folder = ? AND message_id = ?`, folder, key)
	}
	if err != nil {
		return err
	}
	if pendingKind != "" {
		op := "remove"
		if add {
			op = "add"
		}
		if _, err := tx.Exec(`INSERT INTO pending (kind, message_id, folder, op) VALUES (?, ?, ?, ?)`,
			pendingKind, key, folder, op); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MemberHas reports whether the message is a member of the folder.
func (s *Store) MemberHas(folder, key string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM members WHERE folder = ? AND message_id = ?`, folder, key).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MemberFolders returns all folders the message is a member of, sorted.
func (s *Store) MemberFolders(key string) ([]string, error) {
	rows, err := s.db.Query(`SELECT folder FROM members WHERE message_id = ? ORDER BY folder`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

// MemberUIDKeys returns uid -> dedup key for all members of a folder (used
// by the membership diff; uid=0 placeholder rows from migration are skipped —
// they are refreshed by the rebuild path).
func (s *Store) MemberUIDKeys(folder string) (map[uint32]string, error) {
	rows, err := s.db.Query(`SELECT uid, message_id FROM members WHERE folder = ? AND uid > 0`, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var uid uint32
		var key string
		if err := rows.Scan(&uid, &key); err != nil {
			return nil, err
		}
		out[uid] = key
	}
	return out, rows.Err()
}

// MemberKeys returns the dedup keys of all members of a folder.
func (s *Store) MemberKeys(folder string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT message_id FROM members WHERE folder = ?`, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out[key] = true
	}
	return out, rows.Err()
}

// PendingOps returns up to limit queued destination operations, oldest first.
func (s *Store) PendingOps(limit int) ([]PendingOp, error) {
	rows, err := s.db.Query(`SELECT id, kind, message_id, folder, op FROM pending ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []PendingOp
	for rows.Next() {
		var op PendingOp
		if err := rows.Scan(&op.ID, &op.Kind, &op.MessageID, &op.Folder, &op.Op); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// DeletePending removes a confirmed-applied pending operation.
func (s *Store) DeletePending(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE id = ?`, id)
	return err
}

// MetaGet / MetaSet expose the meta table for feature markers (e.g. the
// placement-backfill fingerprint).
func (s *Store) MetaGet(key string) (string, error)   { return s.metaGet(key) }
func (s *Store) MetaSet(key, value string) error      { return s.metaSet(key, value) }

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
