// Package state persists sync state in SQLite (pure-Go driver, no cgo).
//
// Dedup keys are looked up per message via an indexed query — the full set is
// never loaded into memory, so state size is bounded by disk, not RAM.
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"
)

const (
	insertCopied = `INSERT INTO copied (message_id, uid, copied_at) VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO NOTHING`
	upsertMember = `INSERT INTO members (folder, message_id, uid) VALUES (?, ?, ?)
		ON CONFLICT(folder, message_id) DO UPDATE SET uid = excluded.uid`
	deleteMember  = `DELETE FROM members WHERE folder = ? AND message_id = ?`
	insertPending = `INSERT INTO pending (kind, message_id, folder, op) VALUES (?, ?, ?, ?)`
	deletePending = `DELETE FROM pending WHERE id = ?`
)

// Store is the persistent sync state. Single-process only (enforced by the
// file lock, see package lock).
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the state database at path and migrates it to the
// current schema version (see migrate.go).
func Open(path string) (*Store, error) {
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

// inTx runs fn in a transaction, committing only if fn succeeds.
func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// execBatch executes query n times (with args(i)) in one transaction.
func (s *Store) execBatch(query string, n int, args func(i int) []any) error {
	if n == 0 {
		return nil
	}
	return s.inTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(query)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range n {
			if _, err := stmt.Exec(args(i)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// exists reports whether query returns at least one row.
func (s *Store) exists(query string, args ...any) (bool, error) {
	var one int
	err := s.db.QueryRow(query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// queryEach calls scan for every row of query.
func (s *Store) queryEach(query string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MetaGet returns a meta value ("" if unset).
func (s *Store) MetaGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// MetaSet stores a meta value.
func (s *Store) MetaSet(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) metaGetUint(key string) (uint32, error) {
	v, err := s.MetaGet(key)
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("meta %s: corrupt value %q: %w", key, v, err)
	}
	return uint32(n), nil
}

func (s *Store) metaSetUint(key string, v uint32) error {
	return s.MetaSet(key, strconv.FormatUint(uint64(v), 10))
}

// UIDValidity returns the last-seen source UIDVALIDITY (0 = never seen).
func (s *Store) UIDValidity() (uint32, error) { return s.metaGetUint("uidvalidity") }

// SetUIDValidity stores the source UIDVALIDITY.
func (s *Store) SetUIDValidity(v uint32) error { return s.metaSetUint("uidvalidity", v) }

// LastUID returns the high-water mark within the current UIDVALIDITY.
func (s *Store) LastUID() (uint32, error) { return s.metaGetUint("last_uid") }

// SetLastUID stores the high-water mark.
func (s *Store) SetLastUID(uid uint32) error { return s.metaSetUint("last_uid", uid) }

// HasKey reports whether a dedup key has been recorded.
func (s *Store) HasKey(key string) (bool, error) {
	return s.exists(`SELECT 1 FROM copied WHERE message_id = ?`, key)
}

// RecordKey records a dedup key after a confirmed append. Idempotent.
func (s *Store) RecordKey(key string, uid uint32, copiedAtUnix int64) error {
	_, err := s.db.Exec(insertCopied, key, uid, copiedAtUnix)
	return err
}

// KeyRecord is one copied-message record for RecordKeys.
type KeyRecord struct {
	Key          string
	UID          uint32
	CopiedAtUnix int64
}

// RecordKeys records a batch of dedup keys in one transaction. Idempotent
// per key.
func (s *Store) RecordKeys(records []KeyRecord) error {
	return s.execBatch(insertCopied, len(records), func(i int) []any {
		r := records[i]
		return []any{r.Key, r.UID, r.CopiedAtUnix}
	})
}

// SeedBatch records dedup keys seeded from the destination (uid and
// copied_at 0) in one transaction. Existing keys are skipped.
func (s *Store) SeedBatch(keys []string) error {
	return s.execBatch(insertCopied, len(keys), func(i int) []any {
		return []any{keys[i], 0, 0}
	})
}

// CopiedCount returns the number of recorded dedup keys.
func (s *Store) CopiedCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM copied`).Scan(&n)
	return n, err
}

// PendingOp is a queued destination mutation (move or keyword change).
// It is enqueued in the same transaction as the membership change that
// caused it and deleted only once applied (or definitively unnecessary).
type PendingOp struct {
	ID        int64
	Kind      string // "move" | "keyword"
	MessageID string // dedup key
	Folder    string // the membership folder that changed
	Op        string // "add" | "remove"
}

// MemberChangeItem is one membership change for MemberChangeBatch.
type MemberChangeItem struct {
	Key         string
	UID         uint32
	Add         bool
	PendingKind string // "" = no pending op
}

// MemberChange is MemberChangeBatch for a single item.
func (s *Store) MemberChange(folder, key string, uid uint32, add bool, pendingKind string) error {
	return s.MemberChangeBatch(folder, []MemberChangeItem{{Key: key, UID: uid, Add: add, PendingKind: pendingKind}})
}

// MemberChangeBatch applies membership changes and enqueues their pending
// ops in ONE transaction: a delta is either fully recorded or not at all.
func (s *Store) MemberChangeBatch(folder string, items []MemberChangeItem) error {
	if len(items) == 0 {
		return nil
	}
	return s.inTx(func(tx *sql.Tx) error {
		add, err := tx.Prepare(upsertMember)
		if err != nil {
			return err
		}
		defer add.Close()
		del, err := tx.Prepare(deleteMember)
		if err != nil {
			return err
		}
		defer del.Close()
		pend, err := tx.Prepare(insertPending)
		if err != nil {
			return err
		}
		defer pend.Close()
		for _, it := range items {
			op := "remove"
			if it.Add {
				op = "add"
				_, err = add.Exec(folder, it.Key, it.UID)
			} else {
				_, err = del.Exec(folder, it.Key)
			}
			if err != nil {
				return err
			}
			if it.PendingKind != "" {
				if _, err := pend.Exec(it.PendingKind, it.Key, folder, op); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// MemberHas reports whether the message is a member of the folder.
func (s *Store) MemberHas(folder, key string) (bool, error) {
	return s.exists(`SELECT 1 FROM members WHERE folder = ? AND message_id = ?`, folder, key)
}

// MemberFolders returns all folders the message is a member of, sorted.
func (s *Store) MemberFolders(key string) ([]string, error) {
	var folders []string
	err := s.queryEach(`SELECT folder FROM members WHERE message_id = ? ORDER BY folder`, func(rows *sql.Rows) error {
		var f string
		if err := rows.Scan(&f); err != nil {
			return err
		}
		folders = append(folders, f)
		return nil
	}, key)
	return folders, err
}

// MemberUIDKeys returns uid -> dedup key for all members of a folder. uid=0
// placeholder rows (from migration) are skipped; the rebuild path refreshes
// them.
func (s *Store) MemberUIDKeys(folder string) (map[uint32]string, error) {
	out := map[uint32]string{}
	err := s.queryEach(`SELECT uid, message_id FROM members WHERE folder = ? AND uid > 0`, func(rows *sql.Rows) error {
		var uid uint32
		var key string
		if err := rows.Scan(&uid, &key); err != nil {
			return err
		}
		out[uid] = key
		return nil
	}, folder)
	return out, err
}

// MemberKeys returns the dedup keys of all members of a folder.
func (s *Store) MemberKeys(folder string) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.queryEach(`SELECT message_id FROM members WHERE folder = ?`, func(rows *sql.Rows) error {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		out[key] = true
		return nil
	}, folder)
	return out, err
}

// PendingOps returns up to limit queued destination operations, oldest first.
func (s *Store) PendingOps(limit int) ([]PendingOp, error) {
	var ops []PendingOp
	err := s.queryEach(`SELECT id, kind, message_id, folder, op FROM pending ORDER BY id LIMIT ?`, func(rows *sql.Rows) error {
		var op PendingOp
		if err := rows.Scan(&op.ID, &op.Kind, &op.MessageID, &op.Folder, &op.Op); err != nil {
			return err
		}
		ops = append(ops, op)
		return nil
	}, limit)
	return ops, err
}

// DeletePending removes an applied pending operation.
func (s *Store) DeletePending(id int64) error {
	_, err := s.db.Exec(deletePending, id)
	return err
}

// DeletePendingBatch removes applied pending operations in one transaction.
func (s *Store) DeletePendingBatch(ids []int64) error {
	return s.execBatch(deletePending, len(ids), func(i int) []any { return []any{ids[i]} })
}

// FolderState returns the per-folder UIDVALIDITY and UID high-water mark
// (0, 0 if the folder was never scanned).
func (s *Store) FolderState(name string) (uidValidity, lastUID uint32, err error) {
	err = s.db.QueryRow(`SELECT uidvalidity, last_uid FROM folders WHERE name = ?`, name).Scan(&uidValidity, &lastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return uidValidity, lastUID, err
}

// SetFolderState stores the per-folder UIDVALIDITY and UID high-water mark.
func (s *Store) SetFolderState(name string, uidValidity, lastUID uint32) error {
	_, err := s.db.Exec(`INSERT INTO folders (name, uidvalidity, last_uid) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid`,
		name, uidValidity, lastUID)
	return err
}
