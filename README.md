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
| `SOURCE_FOLDER` | `INBOX` | Source folder; accepts a special-use selector like `\All` or `\Sent` (RFC 6154), resolved to the server's actual — possibly localized — folder name at connect time |
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
| `SYNC_LABELS` | `false` | Mirror source label-folder membership as destination keywords (see below) |
| `LABEL_EXCLUDE` | — | Comma-separated folder names to exclude from the label scan |
| `LABEL_PROPAGATE` | `false` | Apply post-copy label changes to already-mirrored mail (±keyword deltas; requires `SYNC_LABELS`) |
| `ARCHIVE_ROUTING` | `false` | Route by source-INBOX membership and propagate archive moves (see below) |
| `SOURCE_INBOX` | `INBOX` | Source folder whose membership means "in inbox" |
| `DEST_ARCHIVE_FOLDER` | `Archive` | Destination folder for archived mail; created if missing |
| `HEALTH_ADDR` | `:8080` | `/healthz` liveness endpoint; empty disables |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Tip: pick a dedicated destination folder (e.g. `Mirror`) rather than `INBOX`
or `Archive` — mirrored mail stays clearly separated from natively-delivered
mail and folder semantics stay clean.

## Label sync (`SYNC_LABELS=true`, optional)

Label-based servers (notably Gmail) expose each user label as an IMAP
folder — a message with N labels appears in N folders. With `SYNC_LABELS=true`
Umleiter scans those folders (incrementally, same resumable machinery as the
mirror itself) and appends each message with its labels attached as **IMAP
keywords** on the destination.

- **Nothing to create on the destination, ever.** IMAP keywords have no
  server-side registry; a keyword exists on a message by being set on it.
  Stalwart stores them as JMAP keywords automatically.
- **Client visibility varies:** Thunderbird shows keywords as tags once you
  define a tag with a matching key (one-time, cosmetic — Settings → Tags);
  JMAP/webmail clients generally show them automatically; most mobile IMAP
  clients ignore them (they stay on the server).
- **Keyword names are sanitized:** IMAP flags must be ASCII atoms, so labels
  are mapped like `[Werbung]` → `werbung`, `Work/Projects` → `work_projects`,
  `Bücher` → `b_cher` (lowercased — IMAP flags are case-insensitive).
  Distinct labels can collide after sanitization; harmless.
- **Post-copy changes propagate with `LABEL_PROPAGATE=true`:** labels
  added/removed in the source after a message was mirrored are applied to the
  destination copy as **deltas** (`STORE ±FLAGS` of just the changed keyword,
  diffed against the previously recorded state) — never a full rewrite, so
  tags you set manually in your mail client are never touched. Deltas are
  queued transactionally and retried until the destination confirms; a crash
  or outage never loses one.
- **Labels never break the mirror:** if the destination rejects an APPEND
  with keywords, the message is retried once without them — the copy always
  wins over the tags.
- **Stalwart note:** IMAP keywords *are* JMAP keywords — one per-message
  store exposed via both protocols. Unlike Gmail labels they are flat strings
  (no colors/hierarchy; that's client-side), don't act as folder views, and
  are searchable (`SEARCH KEYWORD` / JMAP `hasKeyword`).
- Excluded automatically: the source folder, `INBOX`, and special-use folders
  (Sent, Trash, Junk, All Mail, Starred, Important, Drafts, Archive). Add
  more via `LABEL_EXCLUDE=Notes,Some/Other`.

## Archive routing (`ARCHIVE_ROUTING=true`, optional)

There is no "archived" flag in IMAP — archiving in Gmail just removes the
Inbox label. Umleiter derives it from folder membership: a message in the
source folder but **not** in `SOURCE_INBOX` is archived.

- **Routing:** inbox members are mirrored into `DEST_FOLDER`; everything else
  (archived and sent-only mail) goes to `DEST_ARCHIVE_FOLDER`. The initial
  bulk run sorts years of mail correctly from the start.
- **Propagation:** archiving a message in the source later MOVEs its
  destination copy `DEST_FOLDER` → `DEST_ARCHIVE_FOLDER` on the next
  reconcile (seconds via IDLE) — and moving it back to the source inbox moves
  it back. Moves never delete content.
- **Upgrade auto-correction:** enabling the feature (or changing folders) on
  an existing mirror triggers a one-time backfill that sorts already-mirrored
  mail into the right folders (batched moves) and adds missing label
  keywords. Idempotent and crash-safe.
- **Manual refiling respected:** if you moved a destination copy elsewhere,
  propagation skips it silently — the mirror never chases your moves.
- Typical Gmail setup: `SOURCE_FOLDER=[Gmail]/All Mail`, `DEST_FOLDER=INBOX`,
  `ARCHIVE_ROUTING=true` → your Stalwart INBOX mirrors your Gmail inbox, and
  your Stalwart Archive holds everything you archived.
- Caveats: mail *deleted* in the source also leaves its inbox, so its copy
  moves to Archive (Umleiter never deletes). Messages without a `Message-ID`
  are routed at copy time but not moved afterwards (unlocatable; rare).

## State database upgrades

The SQLite state database migrates automatically (`PRAGMA user_version`
chain) — including from versions that predate the migration system. Deploy a
new image and the schema upgrades losslessly on startup; a database created
by a *newer* version is refused with a clear error.

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

## How a sync pass works

Every reconcile runs these phases in order (visible as `progress phase=…`
log lines during long runs; `processed` is the UID watermark or running
count, and progress lines are throttled to ≥30s apart):

| # | Phase (log name) | Reads | Writes | Resumability |
|---|---|---|---|---|
| 0 | `seed` (startup only, per `SEED_DEST`) | destination folders | local state only | per window |
| 1 | `membership` / `membership-rebuild` | source label folders + `SOURCE_INBOX` | local state only | incremental scans: per window; first-time/UIDVALIDITY rebuild: per folder (an interrupted folder rescans from its start) |
| 2 | `mirror` | source folder | **appends to the destination** (routed, with keywords) | per window (`last_uid` commits every `UID_BATCH` messages) |
| 3 | `backfill` (only when placement config changed) | destination folders | moves + keyword additions on the destination | idempotent; reruns until completed once |
| 4 | propagation | pending-op queue | moves + keyword deltas on the destination | queued ops survive failures and retry next pass |

Nothing is written to the destination before phase 2. Cancelling at any
point (SIGTERM, crash, provider disconnect) is safe: every phase either
commits progress in windows or is idempotent, and dedup guarantees no
duplicates regardless of where a run stopped. The membership rebuild in
phase 1 is first-run-only work — later reconciles diff incrementally and
pass through in seconds.

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

- **Gmail as source:** to mirror everything (inbox, sent, archived), set
  `SOURCE_FOLDER` to your account's All-Mail folder. **Gmail localizes its
  special folder names over IMAP per account language**: `[Gmail]/All Mail`
  on English accounts, `[Gmail]/Alle Nachrichten` on German ones, etc. Either
  set the exact localized name, or set the explicit special-use selector
  `SOURCE_FOLDER=\All` (RFC 6154), which resolves to that folder by its
  `\All` attribute regardless of language. `INBOX` is never localized (the
  name is reserved by the IMAP protocol; "Posteingang" is only the UI label),
  so `SOURCE_INBOX=INBOX` always works. Requires an app password (account
  with 2FA). Gmail caps IMAP download at roughly ~2.5 GB/day — the first full
  mirror of years of mail may take days; this is a quota, not a bug. Gmail
  also force-drops IDLE connections after ~29 minutes; handled automatically.
  Gmail **labels** appear as IMAP folders → `SYNC_LABELS=true` mirrors them
  as keywords. Gmail **categories** (Primary/Allgemein, Promotions/Werbung,
  Social, Updates, Forums) are *not* labels and *not* folders — they are not
  exposed over standard IMAP at all (only via Gmail-proprietary extensions or
  the OAuth REST API) and cannot be synced; they also mean nothing to clients
  outside Gmail. See ADR 0010.
- **Stalwart as destination:** use a Stalwart *application password* (not the
  directory/LDAP password, not OAuth).

## Troubleshooting connections

Umleiter retries any connection failure with exponential backoff (1s → 5min)
and resumes exactly where it left off — transient outages need no action.
For persistent failures, sanity-check the endpoint from the same network the
container uses (`openssl s_client -connect host:993`); on container overlay
networks also check the MTU (a mis-sized overlay can drop large TLS
handshakes — set e.g. `com.docker.network.driver.mtu: 1400`).

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
