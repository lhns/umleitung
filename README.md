# Umleiter

One-way **IMAP → IMAP** mail mirror. A single small container that
continuously copies mail from a folder on one IMAP server (source) into a
folder on another (destination) — near-real-time via IMAP IDLE, with a
periodic full reconcile as a safety net. One instance runs **any number of
mirrors** (e.g. several users' mailboxes) concurrently.

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
- **No concurrent syncs.** Each mirror's sync loop is single-goroutine
  (mirrors are isolated from each other), and a cross-process file lock on
  the state volume makes a second instance refuse to start. Run exactly one
  replica regardless.

## Configuration

One YAML file, mounted at `/config/umleiter.yaml` (override with the
`CONFIG_PATH` env var — the only environment variable that exists). Full
annotated schema: [`config.example.yaml`](config.example.yaml). Unknown keys
are rejected (typo protection).

```yaml
mirrors:
  - name: pierre                       # [a-z0-9_-]+; db filename + log field
    source:
      host: imap.gmail.com
      user: user@gmail.com
      password_file: /run/secrets/gmail_password   # or `password:`
      folder: '\All'                   # special-use selector or exact name
    dest:
      host: mail.example.org
      user: user@example.org
      password_file: /run/secrets/dest_password
      folder: INBOX
    archive: { enabled: true }         # archived mail -> Archive folder + moves
    labels:  { enabled: true, propagate: true }   # labels -> keywords + deltas
  - name: otheruser                    # any number of mirrors, run concurrently
    source: { host: imap.gmail.com, user: other@gmail.com, password_file: /run/secrets/o1, folder: '\All' }
    dest:   { host: mail.example.org, user: other@example.org, password_file: /run/secrets/o2, folder: INBOX }
```

**Defaults: omission never disables anything.** Every optional key has a
sensible default that applies when the key is absent (`health_addr` →
`":8080"`, `tls` → `true`, `poll_interval` → `15m`, …). To turn an
on-by-default feature off, set it explicitly: `health_addr: null`.

| Key | Default | Description |
|---|---|---|
| `health_addr` | `":8080"` | `/healthz` liveness endpoint; `null` disables |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `state_dir` | `/state` | Per-mirror db defaults to `{state_dir}/{name}.db` |
| `lock_path` | `{state_dir}/umleiter.lock` | Single-instance lock |
| `mirrors[].name` | — required | Unique, `[a-z0-9_-]+` |
| `mirrors[].state_path` | `{state_dir}/{name}.db` | Override to keep a pre-existing db |
| `mirrors[].poll_interval` | `15m` | Safety-net reconcile cadence |
| `mirrors[].idle_reset` | `25m` | Max length of one IDLE session |
| `mirrors[].uid_batch` | `2000` | Window size of the resumable scan |
| `mirrors[].seed` | `empty` | Seed dedup set from destination: `empty`/`always`/`never` |
| `mirrors[].dest_guard` | `true` | Batched per-window Message-ID check on the destination |
| `mirrors[].carry_seen` | `true` | Propagate `\Seen` (no other flags are ever copied) |
| `source`/`dest`.`host`,`user` | — required | |
| `source`/`dest`.`password` / `password_file` | — required (one of) | `password_file` for Docker/Swarm secrets |
| `source`/`dest`.`port` | `993` | |
| `source`/`dest`.`tls` | `true` | Implicit TLS (IMAPS); `false` only for local testing |
| `source`/`dest`.`folder` | `INBOX` | Source accepts special-use selectors (`\All`, `\Sent`); dest folder is created if missing |
| `source.inbox` | `INBOX` | The "in inbox" folder for archive routing |
| `archive.enabled` | `false` | See Archive routing below |
| `archive.folder` | `Archive` | Destination folder for archived mail; created if missing |
| `sent.enabled` | `false` | Route sent mail to its own destination folder (see Archive routing) |
| `sent.folder` | `Sent` | Destination folder for sent mail; created if missing |
| `sent.source_folder` | `\Sent` | Source sent folder (selector or exact localized name) |
| `labels.enabled` | `false` | See Label sync below |
| `labels.propagate` | `false` | Post-copy label changes as keyword deltas (requires `labels.enabled`) |
| `labels.exclude` | `[]` | Folder names excluded from the label scan |

Tip: pick a dedicated destination folder (e.g. `Mirror`) rather than `INBOX`
or `Archive` when you don't use archive routing — mirrored mail stays clearly
separated from natively-delivered mail.

### Migrating from the env-var configuration (pre-YAML versions)

| Old env var | YAML path |
|---|---|
| `SOURCE_HOST/PORT/USER/PASSWORD(_FILE)/FOLDER/TLS` | `source.host/port/user/password(_file)/folder/tls` |
| `DEST_*` (same fields) | `dest.*` |
| `SOURCE_INBOX` | `source.inbox` |
| `ARCHIVE_ROUTING` / `DEST_ARCHIVE_FOLDER` | `archive.enabled` / `archive.folder` |
| `SYNC_LABELS` / `LABEL_PROPAGATE` / `LABEL_EXCLUDE` | `labels.enabled` / `labels.propagate` / `labels.exclude` |
| `POLL_INTERVAL` (seconds) / `IDLE_RESET` | `poll_interval: 15m` / `idle_reset: 25m` (durations) |
| `SEED_DEST` / `DEST_GUARD` / `UID_BATCH` / `CARRY_SEEN` | `seed` / `dest_guard` / `uid_batch` / `carry_seen` |
| `STATE_PATH` | `mirrors[].state_path` — **set this to your old path** (`/state/umleiter.db`) **to keep existing state**; otherwise the mirror starts a fresh db (safe — seeding prevents duplicates — but re-scans) |
| `LOCK_PATH` / `HEALTH_ADDR` / `LOG_LEVEL` | `lock_path` / `health_addr` / `log_level` (top level) |

## Label sync (`labels.enabled: true`, optional)

Label-based servers (notably Gmail) expose each user label as an IMAP
folder — a message with N labels appears in N folders. With `labels.enabled: true`
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
- **Post-copy changes propagate with `labels.propagate: true`:** labels
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
  more via `labels.exclude: [Notes, Some/Other]`.

## Archive routing (`archive.enabled: true`, optional)

There is no "archived" flag in IMAP — archiving in Gmail just removes the
Inbox label. Umleiter derives it from folder membership: a message in the
source folder but **not** in `source.inbox` is archived.

- **Routing:** inbox members are mirrored into `dest.folder`; everything else
  (archived and sent-only mail) goes to `archive.folder`. The initial
  bulk run sorts years of mail correctly from the start.
- **Propagation:** archiving a message in the source later MOVEs its
  destination copy `dest.folder` → `archive.folder` on the next
  reconcile (seconds via IDLE) — and moving it back to the source inbox moves
  it back. Moves never delete content.
- **Upgrade auto-correction:** enabling the feature (or changing folders) on
  an existing mirror triggers a one-time backfill that sorts already-mirrored
  mail into the right folders (batched moves) and adds missing label
  keywords. Idempotent and crash-safe.
- **Manual refiling respected:** if you moved a destination copy elsewhere,
  propagation skips it silently — the mirror never chases your moves.
- Typical Gmail setup: `source.folder: 'All'`, `dest.folder: INBOX`,
  `archive.enabled: true` → your Stalwart INBOX mirrors your Gmail inbox, and
  your Stalwart Archive holds everything you archived.
- Caveats: mail *deleted* in the source also leaves its inbox, so its copy
  moves to Archive (Umleiter never deletes). Messages without a `Message-ID`
  are routed at copy time but not moved afterwards (unlocatable; rare).

**Sent routing** (`sent.enabled: true`) is the same mechanism for sent mail:
membership in the source's `\Sent` folder (resolved like any special-use
selector; localization-proof) routes the copy to `sent.folder` so mail
clients show it as sent instead of it landing in Archive. Routing priority
when memberships overlap (e.g. mail to yourself is in inbox *and* sent):
**inbox > sent > archive**. Propagation and the placement backfill cover
sent mail exactly like archive mail — enabling it later sorts your
already-mirrored sent messages out of Archive automatically.

## State database upgrades

The SQLite state database migrates automatically (`PRAGMA user_version`
chain) — including from versions that predate the migration system. Deploy a
new image and the schema upgrades losslessly on startup; a database created
by a *newer* version is refused with a clear error.

## Deployment

Single container: mount the config and a persistent state volume.

```yaml
services:
  umleiter:
    image: ghcr.io/lhns/umleitung:latest
    restart: unless-stopped
    volumes:
      - ./umleiter.yaml:/config/umleiter.yaml:ro
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
cp config.example.yaml umleiter.yaml   # edit hosts/users/secrets
mkdir -p state && chown -R 65532 state
docker compose up -d
```

Prefer `password_file` over inline passwords. Ready-to-edit copies live at
[`docker-compose.yml`](docker-compose.yml) and, for Docker Swarm (config +
secrets + `replicas: 1`), [`stack.yml`](stack.yml).

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
| 1 | `membership` / `membership-rebuild` | source label folders + `source.inbox` | local state only | incremental scans: per window; first-time/UIDVALIDITY rebuild: per folder (an interrupted folder rescans from its start) |
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
  imapsync into the same folder), the default `seed: empty` handles it:
  existing messages are seeded into the dedup set on first start and never
  re-copied.

## Provider notes

- **Gmail as source:** to mirror everything (inbox, sent, archived), set
  `source.folder` to your account's All-Mail folder. **Gmail localizes its
  special folder names over IMAP per account language**: `[Gmail]/All Mail`
  on English accounts, `[Gmail]/Alle Nachrichten` on German ones, etc. Either
  set the exact localized name, or set the explicit special-use selector
  `folder: '\All'` (RFC 6154), which resolves to that folder by its
  `\All` attribute regardless of language. `INBOX` is never localized (the
  name is reserved by the IMAP protocol; "Posteingang" is only the UI label),
  so `source.inbox: INBOX` always works. Requires an app password (account
  with 2FA). Gmail caps IMAP download at roughly ~2.5 GB/day — the first full
  mirror of years of mail may take days; this is a quota, not a bug. Gmail
  also force-drops IDLE connections after ~29 minutes; handled automatically.
  Gmail **labels** appear as IMAP folders → `labels.enabled: true` mirrors them
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
