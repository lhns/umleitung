# Umleiter

[![CI](https://github.com/lhns/umleitung/actions/workflows/ci.yml/badge.svg)](https://github.com/lhns/umleitung/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/lhns/umleitung)](https://github.com/lhns/umleitung/releases/latest)

**Keep a live copy of your mailbox on another IMAP server.**

Umleiter continuously mirrors mail from one IMAP account into another. New
mail shows up within seconds, and your labels, archive, sent mail and read
state come along too. It runs as a single small Docker container.

It was built to move off Gmail gradually: point it at your Gmail account and
your own server (e.g. [Stalwart](https://stalw.art)), and your self-hosted
mailbox stays in sync while you keep using Gmail for as long as you like.
It works with any IMAP servers.

## Why Umleiter?

- **Set and forget.** Runs in the background, picks up new mail instantly via
  IMAP IDLE, and survives disconnects, throttling and restarts without losing
  its place.
- **Safe for your data.** It never changes your source account and never
  deletes anything on either side. Stopping it at any point is safe.
- **No duplicates, ever.** Mail is matched by its `Message-ID`, so restarts,
  a lost state file or a mailbox that already contains an earlier import won't
  produce double copies.
- **Keeps your organization.** Gmail labels become tags, archived mail goes to
  your Archive folder, sent mail to Sent, and later changes (archiving,
  relabeling) follow along.
- **Several mailboxes, one container.** Mirror your whole household or team
  from a single instance.

## Quick start

You need Docker and credentials for both accounts. For Gmail, create an
[app password](https://myaccount.google.com/apppasswords) (requires 2-step
verification). For Stalwart, create an application password.

**1. Write a config** as `umleiter.yaml`:

```yaml
mirrors:
  - name: me
    source:
      host: imap.gmail.com
      user: you@gmail.com
      password_file: /run/secrets/source_password
      folder: '\All'          # all of your Gmail mail, in any account language
    dest:
      host: mail.example.org
      user: you@example.org
      password_file: /run/secrets/dest_password
      folder: INBOX
    archive: { enabled: true }                    # archived mail -> Archive
    sent:    { enabled: true }                    # sent mail -> Sent
    labels:  { enabled: true, propagate: true }   # Gmail labels -> tags
```

This mirrors your Gmail inbox to the destination's INBOX and everything you
archived to its Archive folder. Leave out `archive`, `sent` and `labels` to
copy everything into one folder instead.

**2. Add a `docker-compose.yml`:**

```yaml
services:
  umleiter:
    image: ghcr.io/lhns/umleitung:latest
    restart: unless-stopped
    volumes:
      - ./umleiter.yaml:/config/umleiter.yaml:ro
      - ./state:/state
      - ./secrets:/run/secrets:ro
    healthcheck:
      test: ["CMD", "/umleiter", "-healthcheck"]
      interval: 60s
      timeout: 5s
      retries: 3
```

**3. Add your passwords and start it:**

```sh
mkdir -p state secrets
printf '%s' 'gmail-app-password' > secrets/source_password
printf '%s' 'dest-app-password'  > secrets/dest_password
sudo chown -R 65532 state secrets  # the container runs as uid 65532
docker compose up -d
docker compose logs -f
```

The first run copies your whole mailbox and can take a while (see
[First run](#first-run)). After that, new mail arrives within seconds.

For Docker Swarm, [`stack.yml`](stack.yml) uses Swarm configs and secrets
instead. Pin a version with `ghcr.io/lhns/umleitung:0.1.0`.

## Features

### Archive and sent folders

IMAP has no "archived" flag. In Gmail, archiving just removes the Inbox label.
With `archive.enabled`, Umleiter mirrors that: mail in your source inbox goes
to `dest.folder`, everything else goes to `archive.folder` (default
`Archive`).

With `sent.enabled`, mail you sent goes to `sent.folder` (default `Sent`), so
your mail client shows it as sent mail. If a message qualifies for more than
one folder (e.g. mail to yourself), the order is inbox, then sent, then
archive.

Later changes follow: archive a message in Gmail and its copy moves to
Archive; move it back to the inbox and the copy moves back. These moves are
picked up on the next sync, which is triggered by new mail or at the latest
after `poll_interval` (15 minutes by default).

Turning these options on for a mailbox that is already mirrored sorts the
existing copies into the right folders once. If you move a copy somewhere else
yourself, Umleiter leaves it alone.

### Labels as tags

Gmail shows each label as an IMAP folder. With `labels.enabled`, Umleiter
attaches your labels to the copies as IMAP keywords, which mail clients show
as tags. With `labels.propagate`, adding or removing a label later updates the
copy too. Only that label changes, so tags you set yourself are never touched.

Label names are converted to valid keywords: lowercase, with spaces, slashes
and non-ASCII characters replaced, so `Work/Projects` becomes
`work_projects`.

How tags show up depends on your mail client:

| Client | Setup |
|---|---|
| Thunderbird | Define a tag with the matching key once (Settings → Tags) |
| [Bulwark](https://github.com/bulwarkmail/webmail) | Set `keyword_prefix: "$label:"` and `keyword_replacement: "-"` |
| Most webmail / JMAP clients | Shown automatically |
| Most mobile apps | Not shown (the tags stay on the server) |

Your inbox, sent, archive, trash, spam, drafts, starred, important and
all-mail folders are never treated as labels. Exclude others with
`labels.exclude: [Notes]`.

### Multiple mailboxes

Add more entries under `mirrors:`. They run side by side, each with its own
state file.

## Configuration

Umleiter reads one YAML file from `/config/umleiter.yaml` (change it with the
`CONFIG_PATH` environment variable). An annotated example is in
[`config.example.yaml`](config.example.yaml). Unknown keys are rejected, so
typos are caught at startup.

Every optional setting has a default. To turn off something that is on by
default, set it explicitly, e.g. `health_addr: null`.

<details>
<summary><b>All settings</b></summary>

| Key | Default | Description |
|---|---|---|
| `health_addr` | `":8080"` | `/healthz` endpoint; `null` disables it |
| `log_level` | `info` | `debug`, `info`, `warn` or `error` |
| `state_dir` | `/state` | Where state files are kept |
| `lock_path` | `{state_dir}/umleiter.lock` | Lock file that prevents a second instance |
| `mirrors[].name` | required | Unique name, `[a-z0-9_-]+`; used in logs and the state file name |
| `mirrors[].state_path` | `{state_dir}/{name}.db` | State file for this mirror |
| `mirrors[].poll_interval` | `15m` | How often a full sync runs in addition to instant updates |
| `mirrors[].idle_reset` | `25m` | Maximum length of one IDLE session |
| `mirrors[].uid_batch` | `2000` | Messages per batch; progress is saved after each batch |
| `mirrors[].seed` | `empty` | Read the destination's existing mail to avoid duplicates: `empty` (when the state file is new), `always` or `never` |
| `mirrors[].dest_guard` | `true` | Check the destination for each message before copying it |
| `mirrors[].carry_seen` | `true` | Copy the read/unread state |
| `source`/`dest` `.host`, `.user` | required | |
| `source`/`dest` `.password` or `.password_file` | required (one of) | Prefer `password_file` |
| `source`/`dest` `.port` | `993` | |
| `source`/`dest` `.tls` | `true` | Implicit TLS; turn off only for local testing |
| `source`/`dest` `.folder` | `INBOX` | Source also accepts `\All`, `\Sent` etc.; the destination folder is created if missing |
| `source.inbox` | `INBOX` | The source's inbox, for archive and sent routing |
| `archive.enabled` | `false` | [Archive and sent folders](#archive-and-sent-folders) |
| `archive.folder` | `Archive` | Created if missing |
| `sent.enabled` | `false` | [Archive and sent folders](#archive-and-sent-folders) |
| `sent.folder` | `Sent` | Created if missing |
| `sent.source_folder` | `\Sent` | The source's sent folder, as `\Sent` or its exact name |
| `labels.enabled` | `false` | [Labels as tags](#labels-as-tags) |
| `labels.propagate` | `false` | Apply later label changes to copies |
| `labels.exclude` | `[]` | Folders not treated as labels |
| `labels.keyword_prefix` | `""` | Prepended to every tag; `"$label:"` for Bulwark |
| `labels.keyword_replacement` | `"_"` | Character replacing spaces, slashes etc. in tags; `"-"` for Bulwark |

</details>

## Running it

### First run

- **Expect it to take a while.** Gmail limits IMAP downloads to roughly
  2.5 GB per day, so years of mail can take several days. Umleiter backs off
  when throttled and resumes where it stopped; you don't need to do anything.
- **Already imported your mail?** If the destination already contains copies
  (e.g. from imapsync), those are detected and not copied again.
- **Back up `./state`** once the first run is done. Losing it is safe, but
  Umleiter then has to rescan both mailboxes.

### Only one instance

Never run two instances against the same mailboxes. A second instance on the
same state directory refuses to start, but separate state directories can't
detect each other. In Swarm, keep `replicas: 1`.

### Health and logs

`/umleiter -healthcheck` (used by the compose file above) and
`GET :8080/healthz` report unhealthy when a mirror has stopped making
progress. Long runs log `progress` lines with the current phase and position.

### Troubleshooting

- Connection errors are retried automatically with backoff (1 s up to
  5 min). Short outages need no action.
- If a connection never succeeds, test it from the Docker host:
  `openssl s_client -connect host:993`.
- On Docker overlay networks, large TLS handshakes can be dropped when the
  MTU is too large. Try `com.docker.network.driver.mtu: 1400`.

### Upgrading

Pull the new image and restart. The state file is upgraded automatically.

<details>
<summary>Coming from a version configured with environment variables</summary>

| Old env var | YAML path |
|---|---|
| `SOURCE_HOST/PORT/USER/PASSWORD(_FILE)/FOLDER/TLS` | `source.host/port/user/password(_file)/folder/tls` |
| `DEST_*` (same fields) | `dest.*` |
| `SOURCE_INBOX` | `source.inbox` |
| `ARCHIVE_ROUTING` / `DEST_ARCHIVE_FOLDER` | `archive.enabled` / `archive.folder` |
| `SYNC_LABELS` / `LABEL_PROPAGATE` / `LABEL_EXCLUDE` | `labels.enabled` / `labels.propagate` / `labels.exclude` |
| `POLL_INTERVAL` (seconds) / `IDLE_RESET` | `poll_interval: 15m` / `idle_reset: 25m` |
| `SEED_DEST` / `DEST_GUARD` / `UID_BATCH` / `CARRY_SEEN` | `seed` / `dest_guard` / `uid_batch` / `carry_seen` |
| `STATE_PATH` | `mirrors[].state_path`: set it to your old path (`/state/umleiter.db`) to keep your state |
| `LOCK_PATH` / `HEALTH_ADDR` / `LOG_LEVEL` | `lock_path` / `health_addr` / `log_level` |

</details>

## Provider notes

**Gmail (source)**

- Use `folder: '\All'` to mirror everything. Gmail translates folder names
  into your account's language (`[Gmail]/All Mail`, `[Gmail]/Alle
  Nachrichten`, …); `\All` finds the right one in any language. `INBOX` is
  never translated.
- Gmail's **categories** (Primary, Promotions, Social, …) are not available
  over IMAP and can't be mirrored. Labels can.
- Gmail drops idle connections after about 29 minutes; Umleiter reconnects
  automatically.

**Stalwart (destination)**

- Log in with an application password, not your account or LDAP password.
- Tags are stored as JMAP keywords, so they are searchable and visible to JMAP
  clients too.

## How it works

<details>
<summary><b>Guarantees</b></summary>

- **One-way.** Nothing is ever written to the source. On the destination,
  Umleiter only adds messages, moves them between folders (when archive or
  sent routing is on), and adds or removes tags (when labels are on). It
  never deletes. The worst a failure can cause is a message copied late, on
  the next sync.
- **No duplicates.** Messages are identified by their `Message-ID` header, not
  by IMAP UIDs, which servers may reset. Three layers enforce this:
  1. **Seeding:** when the state file is new, the destination's existing
     `Message-ID`s are read first, so the destination itself is the source of
     truth.
  2. **State file:** a message is recorded only after the destination has
     confirmed the copy. A crash in between means a retry, not a duplicate.
  3. **Destination guard:** before copying, the destination is searched for
     the messages' `Message-ID`s. This covers a crash right after a copy.
- **One sync at a time.** Each mirror runs a single sync loop, and a lock file
  stops a second instance.

</details>

<details>
<summary><b>Phases of a sync</b></summary>

Each sync runs these phases in order. They appear as `progress phase=…` in
the logs.

| # | Phase | Reads | Writes | If interrupted |
|---|---|---|---|---|
| 0 | `seed` (startup, per `seed`) | destination | local state | resumes per batch |
| 1 | `membership` / `membership-rebuild` | source label folders, inbox, sent | local state | resumes per batch; a first scan restarts the current folder |
| 2 | `backfill` (only after routing or label settings change) | destination | moves, tags | idempotent; reruns until done |
| 3 | `mirror` | source folder | **copies to the destination** | resumes per batch |
| 4 | propagation | queued changes | moves, tag changes | queued changes are retried |

Nothing is written to the destination before phase 2, and stopping at any
point is safe.

</details>

<details>
<summary><b>Edge cases</b></summary>

- **No `Message-ID`** (rare but allowed): a stable ID is computed from the
  date, sender, subject and size. The destination guard can't search for it,
  but seeding and the state file still prevent duplicates. Such messages are
  routed when copied but not moved afterwards.
- **Server resets its UIDs (UIDVALIDITY change):** the source is rescanned;
  `Message-ID` matching prevents duplicates.
- **Two different messages with the same `Message-ID`** (some spam or
  forwards): the second is treated as already copied. imapsync behaves the
  same way.
- **Mail deleted in the source** leaves the inbox too, so with archive routing
  its copy moves to Archive. Umleiter never deletes.
- **Changing `keyword_prefix`** on an existing mirror adds the new tags to
  mirrored mail. The old tags stay.

</details>

<details>
<summary><b>Why not mbsync or imapsync?</b></summary>

`mbsync` tracks sync state by UID and doesn't deduplicate by `Message-ID`, so
a UIDVALIDITY change or lost state can create duplicates, which is exactly
what this tool must never do. `imapsync` does deduplicate by `Message-ID` but
is a large Perl tool without IDLE support. Umleiter implements the small part
that's needed: IDLE-triggered, one-way copying with `Message-ID`
deduplication. See [ADR 0001](docs/adr/0001-custom-go-mirror-not-mbsync-or-imapsync.md).

</details>

Design decisions are recorded in [`docs/adr/`](docs/adr/).

## Development

```sh
go test ./...
docker build -t umleitung .
```

Go, built as a single static binary in a distroless image. Main libraries:
[go-imap](https://github.com/emersion/go-imap),
[go-message](https://github.com/emersion/go-message),
[modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (pure Go) and
[flock](https://github.com/gofrs/flock).

| Package | Purpose |
|---|---|
| `cmd/umleiter` | Entry point, health check |
| `internal/config` | YAML config and validation |
| `internal/mirror` | Per-mirror runtime: reconnects, seeding, IDLE loop |
| `internal/reconcile` | Sync algorithm |
| `internal/imapx` | IMAP client wrapper |
| `internal/state` | SQLite state store |
| `internal/lock` | Single-instance lock |
| `internal/integration` | End-to-end tests against in-memory IMAP servers; no Docker or network needed |
