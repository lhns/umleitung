# Umleiter

One-way **Gmail → Stalwart** mail mirror. A single small container that
continuously copies mail from Gmail's `[Gmail]/All Mail` into a folder on a
self-hosted Stalwart IMAP server — near-real-time via IMAP IDLE, with a
periodic full reconcile as a safety net.

## Guarantees

- **Strictly one-way.** Never writes to Gmail, never deletes on either side.
  Append-only: the worst any failure can cause is a *temporarily missed*
  message, picked up on the next reconcile — never data loss, never corruption.
- **No duplicates, ever.** Idempotency is keyed on the RFC 5322 `Message-ID`
  header (not on IMAP UIDs, which reset). Three independent layers enforce it:
  1. **Destination seeding** (default on): on first start — or any start where
     local state is empty/lost — the destination folder's existing
     `Message-ID`s are streamed into the dedup set. Correctness never depends
     on local state; Stalwart itself is the source of truth.
  2. **Persisted dedup set** in SQLite: indexed per-message lookup (the set is
     never loaded into RAM, so mailbox size doesn't matter). A key is recorded
     only *after* a confirmed successful APPEND — a crash in between leaves
     the message safely retryable, never duplicated.
  3. **Destination guard** (default on): before appending a message not in
     the set, the destination is searched for its `Message-ID` directly. This
     closes the "appended but crashed before recording" window.
- **No concurrent syncs.** The sync loop is single-goroutine, and a
  cross-process file lock on the state volume makes a second instance refuse
  to start. Run exactly one replica regardless.

## Configuration

Everything is configured via environment variables. Secrets accept `*_FILE`
variants pointing at a file (Docker/Swarm secrets).

| Variable | Default | Description |
|---|---|---|
| `GMAIL_HOST` | `imap.gmail.com` | Source IMAPS host |
| `GMAIL_PORT` | `993` | Source port |
| `GMAIL_USER` | — (required) | Gmail address |
| `GMAIL_APP_PASSWORD` / `_FILE` | — (required) | Gmail app password (account needs 2FA) |
| `GMAIL_FOLDER` | `[Gmail]/All Mail` | Source folder |
| `GMAIL_TLS` | `true` | Implicit TLS (IMAPS); disable only for local testing |
| `STALWART_HOST` | `mail.lhns.de` | Destination IMAPS host |
| `STALWART_PORT` | `993` | Destination port |
| `STALWART_USER` | — (required) | Stalwart account |
| `STALWART_APP_PASSWORD` / `_FILE` | — (required) | **Stalwart application password** (not the LDAP password, not OAuth) |
| `STALWART_FOLDER` | `Gmail` | Destination folder; created if missing |
| `STALWART_TLS` | `true` | Implicit TLS (IMAPS); disable only for local testing |
| `POLL_INTERVAL` | `900` | Seconds between safety-net reconciles |
| `IDLE_RESET` | `1500` | Max seconds for one IDLE session (Gmail force-logs-out idle connections after ~29 min; go-imap also auto-restarts IDLE every ~28 min) |
| `STATE_PATH` | `/state/umleiter.db` | SQLite state database |
| `LOCK_PATH` | `/state/umleiter.lock` | Cross-process startup lock |
| `SEED_DEST` | `empty` | Seed dedup set from destination: `empty` (only when local set is empty), `always`, `never` |
| `DEST_GUARD` | `true` | Per-append `SEARCH HEADER Message-ID` on destination |
| `UID_BATCH` | `2000` | UID window size for the windowed, resumable scan |
| `CARRY_SEEN` | `true` | Propagate `\Seen` from source (no other flags/labels are ever copied) |
| `HEALTH_ADDR` | `:8080` | `/healthz` liveness endpoint; empty disables |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

The destination folder is deliberately **not** `Archive` by default: All Mail
mixes inbox, sent and archived mail, so it lands in a dedicated `Gmail` folder.
Set `STALWART_FOLDER=Archive` (or anything else) if you prefer.

## Deployment

Single container. Compose:

```sh
mkdir -p state && chown -R 65532 state   # distroless runs as nonroot (uid 65532)
GMAIL_USER=... GMAIL_APP_PASSWORD=... STALWART_USER=... STALWART_APP_PASSWORD=... \
  docker compose up -d
```

Docker Swarm (secrets, bind mount, `replicas: 1`): see [`stack.yml`](stack.yml).

**Never run more than one instance.** Two syncers would race to append. The
file lock refuses a second instance on the same state volume, and the Swarm
stack pins `replicas: 1` — keep it that way.

## First run

- **Back up the state volume** once the initial mirror completes (`/state`);
  it is the dedup fast path. (Losing it is *safe* — seeding rebuilds it from
  the destination — but a backup makes restarts instant.)
- A large mailbox will hit **Gmail's IMAP download quota (~2.5 GB/day)**. The
  first full mirror of years of mail may take **days**: Umleiter gets
  throttled/disconnected, backs off, reconnects and resumes from the last
  committed window. This is expected, not a bug. Progress is never lost and
  interrupting/restarting at any point is safe.
- If Stalwart was **pre-populated by a prior bulk import** (e.g. imapsync into
  the same folder), the default `SEED_DEST=empty` handles it: existing
  messages are seeded into the dedup set on first start and never re-copied.

## Edge cases (documented behavior)

- **Missing `Message-ID`** (rare but legal): a stable key is synthesized from
  SHA-256 of (INTERNALDATE, From, Subject, size). Seeding computes the same
  key from destination copies, so dedup still holds. The destination *guard*
  can't search for synthesized keys — layers 1–2 cover those messages.
- **Gmail UIDVALIDITY change:** the UID high-water mark is reset and everything
  is re-scanned; the `Message-ID` set prevents any duplicate appends.
- **Two distinct messages sharing one `Message-ID`** (some spam/forwards): the
  second is treated as already mirrored and skipped — the inherent tradeoff of
  Message-ID dedup (imapsync behaves the same way).

## Implementation notes

Go, single static binary (`CGO_ENABLED=0`), distroless image.
Libraries: [`go-imap/v2`](https://github.com/emersion/go-imap) (IMAP + IDLE),
[`go-message`](https://github.com/emersion/go-message) (header parsing),
[`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) (pure-Go SQLite —
keeps the binary static), [`gofrs/flock`](https://github.com/gofrs/flock)
(cross-process lock).

Layout: `internal/imapx` (IMAP wrapper) · `internal/state` (SQLite store) ·
`internal/reconcile` (core algorithm) · `internal/config` · `internal/lock` ·
`cmd/umleiter`.

```sh
go test ./...
docker build -t umleitung .
```

Tests include a self-contained end-to-end suite (`internal/integration`) that
spins up two in-memory IMAP servers (go-imap's `imapmemserver`) and runs the
full mirror over real IMAP connections: first sync, duplicate Message-ID,
missing Message-ID, state wipe + re-seed, incremental sync, the
append-but-not-recorded crash window, UIDVALIDITY reset, and flag policy.
No Docker or network required.

Design decisions are documented as ADRs in [`docs/adr/`](docs/adr/).

## Alternative: mbsync + goimapnotify (not used)

The battle-tested combination — `mbsync` (isync) driven by `goimapnotify` on
IDLE events, wrapped in `flock -n` for serialization, with `SyncState` on a
persistent bind mount — was considered and documented as a fallback. It was
rejected as the primary approach for one reason: **mbsync keys its sync state
on UID + journal and explicitly does not dedupe by `Message-ID`**, so a Gmail
UIDVALIDITY change or state loss can produce duplicates — the exact failure
this tool exists to rule out. `imapsync` does dedupe by Message-ID but is a
large Perl deployment and not IDLE-native. Umleiter reimplements only the
small useful subset: IDLE→trigger (goimapnotify's job) inline in the sync
loop, and one-way append with Message-ID idempotency.
