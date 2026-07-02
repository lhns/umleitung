# 0003 — SQLite state store; dedup set never loaded into RAM

Status: accepted

## Context

The spec suggested a JSON file, "or SQLite if the set becomes unwieldy". An
intermediate design used bbolt with the key set held in a Go map. The
maintainer then flagged the real constraint: the Gmail mailbox is huge today
and the Stalwart mailbox will be huge later — **loading the whole ID set into
memory is the thing that blows up**, regardless of which file format persists
it. The binding constraint is memory, not disk.

Driver choice matters too: the common `mattn/go-sqlite3` driver needs cgo,
which breaks the `CGO_ENABLED=0` static binary and forces a larger base image
(ADR 0008).

## Decision

SQLite via **`modernc.org/sqlite`** (pure-Go, no cgo), with the design rule
that **dedup is always an indexed per-key lookup, never an in-memory set**:

- `copied(message_id TEXT PRIMARY KEY, uid, copied_at) WITHOUT ROWID` — each
  check is `SELECT 1 WHERE message_id = ?`: O(log n), constant memory.
- `meta(key, value)` for `uidvalidity` and `last_uid`.
- WAL mode + busy_timeout; a single connection (one writer by construction).
  Commits are transactional and fsync'd — no temp-file+rename dance, no
  torn state after a crash.
- Seeding (ADR 0002) streams keys into the table in batched transactions;
  no big list is ever materialized.

## Consequences

- Memory stays flat no matter how many years of mail accumulate.
- Rejected: JSON (full rewrite per persist, full set in RAM), bbolt-with-map
  (RAM), cgo SQLite (breaks static/distroless).
- Pure-Go SQLite is somewhat slower than the cgo driver — irrelevant here:
  the workload is one lookup/insert per message against network-bound IMAP
  operations.
