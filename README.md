# Umleiter

One-way **IMAP → IMAP** mail mirror. A single small container that
continuously copies mail from a folder on one IMAP server (source) into a
folder on another (destination) — near-real-time via IMAP IDLE, with a
periodic full reconcile as a safety net.

Works with any IMAP servers. Its original use case: mirroring a Gmail
account's All Mail into a self-hosted mailbox.

## Guarantees

- **Strictly one-way.** Never writes to the source, never deletes on either
  side. Append-only: the worst any failure can cause is a *temporarily missed*
  message, picked up on the next reconcile — never data loss, never corruption.
- **No duplicates, ever.** Idempotency is keyed on the RFC 5322 `Message-ID`
  header (not on IMAP UIDs, which reset). Three independent layers enforce it:
  1. **Destination seeding** (default on): on first start — or any start where
     local state is empty/lost — the destination folder's existing
     `Message-ID`s are streamed into the dedup set. Correctness never depends
     on local state; the destination itself is the source of truth.
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
| `SOURCE_HOST` | — (required) | Source IMAP host |
| `SOURCE_PORT` | `993` | Source port |
| `SOURCE_USER` | — (required) | Source account |
| `SOURCE_PASSWORD` / `_FILE` | — (required) | Source password (use an app password where available) |
| `SOURCE_FOLDER` | `INBOX` | Source folder |
| `SOURCE_TLS` | `true` | Implicit TLS (IMAPS); disable only for local testing |
| `DEST_HOST` | — (required) | Destination IMAP host |
| `DEST_PORT` | `993` | Destination port |
| `DEST_USER` | — (required) | Destination account |
| `DEST_PASSWORD` / `_FILE` | — (required) | Destination password (use an app password where available) |
| `DEST_FOLDER` | `INBOX` | Destination folder; created if missing |
| `DEST_TLS` | `true` | Implicit TLS (IMAPS); disable only for local testing |
| `POLL_INTERVAL` | `900` | Seconds between safety-net reconciles |
| `IDLE_RESET` | `1500` | Max seconds for one IDLE session (servers force-drop idle connections; go-imap also auto-restarts IDLE every ~28 min) |
| `STATE_PATH` | `/state/umleiter.db` | SQLite state database |
| `LOCK_PATH` | `/state/umleiter.lock` | Cross-process startup lock |
| `SEED_DEST` | `empty` | Seed dedup set from destination: `empty` (only when local set is empty), `always`, `never` |
| `DEST_GUARD` | `true` | Per-append `SEARCH HEADER Message-ID` on destination |
| `UID_BATCH` | `2000` | UID window size for the windowed, resumable scan |
| `CARRY_SEEN` | `true` | Propagate `\Seen` from source (no other flags/keywords are ever copied) |
| `HEALTH_ADDR` | `:8080` | `/healthz` liveness endpoint; empty disables |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Tip: pick a dedicated destination folder (e.g. `Mirror`) rather than `INBOX`
or `Archive` — mirrored mail stays clearly separated from natively-delivered
mail and folder semantics stay clean.

## Deployment

Single container. Example `docker-compose.yml` (mirroring a Gmail account
into a self-hosted server):

```yaml
services:
  umleiter:
    image: ghcr.io/lhns/umleitung:latest
    restart: unless-stopped
    environment:
      SOURCE_HOST: "imap.gmail.com"
      SOURCE_USER: "user@gmail.com"
      SOURCE_PASSWORD: "the-gmail-app-password"
      SOURCE_FOLDER: "[Gmail]/All Mail"
      DEST_HOST: "mail.example.org"
      DEST_USER: "user@example.org"
      DEST_PASSWORD: "the-dest-app-password"
      DEST_FOLDER: "Mirror"
    volumes:
      # Persistent state (dedup fast path). Owner must be uid 65532
      # (distroless nonroot): chown -R 65532 ./state
      - ./state:/state
    healthcheck:
      test: ["CMD", "/umleiter", "-healthcheck"]
      interval: 60s
      timeout: 5s
      retries: 3
```

```sh
mkdir -p state && chown -R 65532 state
docker compose up -d
```

Prefer `SOURCE_PASSWORD_FILE`/`DEST_PASSWORD_FILE` over inline passwords where
possible. A ready-to-edit copy lives at [`docker-compose.yml`](docker-compose.yml);
for Docker Swarm (secrets, bind mount, `replicas: 1`) see [`stack.yml`](stack.yml).

**Never run more than one instance.** Two syncers would race to append. The
file lock refuses a second instance on the same state volume, and the Swarm
stack pins `replicas: 1` — keep it that way.

## First run

- **Back up the state volume** once the initial mirror completes (`/state`);
  it is the dedup fast path. (Losing it is *safe* — seeding rebuilds it from
  the destination — but a backup makes restarts instant.)
- Large mailboxes may hit **provider download quotas** and get throttled
  (see provider notes below). Umleiter treats throttle disconnects as
  expected: it backs off, reconnects and resumes from the last committed
  window. Progress is never lost and interrupting/restarting at any point is
  safe.
- If the destination was **pre-populated by a prior bulk import** (e.g.
  imapsync into the same folder), the default `SEED_DEST=empty` handles it:
  existing messages are seeded into the dedup set on first start and never
  re-copied.

## Provider notes

- **Gmail as source:** use `SOURCE_FOLDER=[Gmail]/All Mail` to mirror
  everything (inbox, sent, archived). Requires an app password (account with
  2FA). Gmail caps IMAP download at roughly ~2.5 GB/day — the first full
  mirror of years of mail may take days; this is a quota, not a bug. Gmail
  also force-drops IDLE connections after ~29 minutes; handled automatically.
- **Stalwart as destination:** use a Stalwart *application password* (not the
  directory/LDAP password, not OAuth).

## Edge cases (documented behavior)

- **Missing `Message-ID`** (rare but legal): a stable key is synthesized from
  SHA-256 of (INTERNALDATE, From, Subject, size). Seeding computes the same
  key from destination copies, so dedup still holds. The destination *guard*
  can't search for synthesized keys — layers 1–2 cover those messages.
- **Source UIDVALIDITY change:** the UID high-water mark is reset and
  everything is re-scanned; the `Message-ID` set prevents any duplicate
  appends.
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
on UID + journal and explicitly does not dedupe by `Message-ID`**, so a
UIDVALIDITY change or state loss can produce duplicates — the exact failure
this tool exists to rule out. `imapsync` does dedupe by Message-ID but is a
large Perl deployment and not IDLE-native. Umleiter reimplements only the
small useful subset: IDLE→trigger (goimapnotify's job) inline in the sync
loop, and one-way append with Message-ID idempotency.
