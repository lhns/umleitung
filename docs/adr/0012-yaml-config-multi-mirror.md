# 0012 — YAML configuration and multi-mirror instances

Status: accepted

## Context

The env-var configuration had grown incoherent: settings that belong together
were named unrelatedly (`ARCHIVE_ROUTING` + `DEST_ARCHIVE_FOLDER` +
`SOURCE_INBOX`; `SYNC_LABELS` + `LABEL_PROPAGATE` + `LABEL_EXCLUDE`), and a
flat env namespace cannot express **multiple mirrors** — the maintainer wants
one instance to mirror several users' mailboxes.

## Decision

**One YAML file, and only that** (`CONFIG_PATH`, default
`/config/umleiter.yaml`; the env-var configuration is removed — a breaking
pre-1.0 change accepted by the sole deployer). `gopkg.in/yaml.v3` with
`KnownFields` (unknown keys are rejected — typo protection).

- **Grouping fixes the naming**: `archive: {enabled, folder}` and
  `labels: {enabled, propagate, exclude}` live under each mirror; endpoint
  settings nest under `source:`/`dest:`.
- **Multiple mirrors**: `mirrors:` is a list; each entry runs concurrently in
  its own goroutine with its own connections, supervision/backoff loop, and
  its own SQLite state database (`{state_dir}/{name}.db`, overridable via
  `state_path` — the documented migration hook for keeping a pre-existing
  db). Package `internal/mirror` owns the per-mirror runtime;
  `cmd/umleiter` only loads config, takes the instance-global file lock,
  fans out and aggregates health.
- **Defaults philosophy (explicit user requirement)**: an omitted key always
  gets its default — omission never disables anything (`health_addr` absent
  → `":8080"`). Disabling an on-by-default feature requires an explicit
  `null`/`""`. Implementation note: yaml.v3 skips custom unmarshalers for
  null values, so absent-vs-null is distinguished via a raw `yaml.Node`
  field (Kind==0 = absent, Tag `!!null` = explicit null).
- **Health aggregation**: per-mirror heartbeats; `/healthz` returns 503 if
  ANY mirror's heartbeat is staler than 3× that mirror's `poll_interval`
  (the response names the stale mirror). A mirror that cannot run at all
  (e.g. unopenable state db) takes the instance down for a clean restart.
- **Locking stays instance-global**: one lock file per state volume; mirrors
  within an instance are isolated by separate databases.

## Consequences

- Adding a mailbox = adding a `mirrors:` entry; no second container, shared
  image/volume/lock/health.
- Mirrors fail and back off independently — one user's provider outage does
  not stall the others.
- Secrets flow through `password_file` (Docker/Swarm secrets); the config
  file itself can be a Swarm config object (see stack.yml).
- The env→YAML migration table lives in the README; the old `STATE_PATH`
  must be carried into `state_path` to keep in-flight first-run state
  (otherwise seeding rebuilds — safe but slow).
- Per-mirror databases mean per-mirror migrations and independent
  crash/resume behavior; nothing is shared between mirrors except the
  process.
