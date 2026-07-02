# 0011 — Membership propagation: archive routing/moves + label keyword updates

Status: accepted

## Context

The maintainer archives mail in Gmail and wants archived mail in a Stalwart
`Archive` folder instead of the inbox — including mail archived *after* it was
mirrored, and including mail mirrored *before* the feature existed. Also
requested: label changes after mirroring should propagate (diff-based, never
overwriting, never losing a delta), and everything must upgrade automatically
from a deployed version without migrations.

Facts:
- **There is no "archived" attribute in IMAP.** Archiving in Gmail = removing
  the Inbox label. Derivable as: in the source folder (All Mail) but not in
  the source INBOX. Folder membership is the only product-agnostic signal.
- ADR 0010's label sync captured labels at copy time only.
- **Stalwart keywords ARE JMAP keywords**: IMAP keywords and JMAP keywords
  (RFC 8621) are one per-message store exposed via both protocols. Unlike
  Gmail labels they are flat strings (no registry, color, hierarchy — that is
  client-side), do not act as folder views, are case-insensitive, and are
  searchable (IMAP `SEARCH KEYWORD` / JMAP `hasKeyword`).

## Decision

**One generalized membership-diff engine** (`internal/reconcile/membership.go`)
per watched source folder: snapshot the folder's UID set (`UID SEARCH ALL`),
diff against stored members (SQLite `members` table), record
additions/removals with per-folder UIDVALIDITY + high-water marks (windowed,
resumable). On UIDVALIDITY change: full rescan, diffed **by Message-ID** so
unchanged membership produces zero spurious changes.

Consumers of the engine:
- **`ARCHIVE_ROUTING`** (opt-in): the source INBOX's membership routes each
  copy (member → `DEST_FOLDER`, else → `DEST_ARCHIVE_FOLDER`) and membership
  *changes* MOVE the destination copy between those two folders — both
  directions (archive and move-back-to-inbox).
- **`LABEL_PROPAGATE`** (opt-in, requires `SYNC_LABELS`): label-folder
  membership changes become `UID STORE ±FLAGS.SILENT` of the single changed
  keyword on the destination copy.

**Crash-safe delta application (pending queue):** a membership change and its
resulting destination operation are recorded in ONE SQLite transaction
(`pending` table). Propagation applies queued ops and deletes each row only
after the server confirms (or a definitive not-found). A crash or destination
failure leaves the delta queued for retry; MOVE and ±FLAGS are idempotent.
Deltas are computed against *stored previous state* — Stalwart's keyword set
is never read-and-rewritten, so manually-set tags are untouched.

**Placement backfill (upgrade auto-correction):** a config fingerprint
(routing + folder names + label settings) is stored in `meta`. On mismatch —
e.g. mail mirrored before the feature was enabled — both destination folders
are scanned (windowed): wrong-side messages are batch-MOVEd by UID set
(chunks of 500), and missing label keywords are batch-added. Keyword backfill
is **add-only**: a keyword whose label vanished long ago is indistinguishable
from a user tag, so backfill never removes; removals flow only through the
delta queue, which knows the exact label that changed. The fingerprint is
written only after completion; the pass is idempotent.

**Safety boundaries:**
- These are Umleiter's first mutations of existing destination messages —
  strictly MOVE and flag STOREs. **Content is never deleted**; ADR 0002's
  no-duplicates guarantee is unchanged (the destination guard now checks both
  folders). This amends the pure "append-only" claim: append-mostly, with
  content-preserving moves/flag-edits.
- Destination copies are located by `Message-ID` search in the folder they
  are expected in; not found → skipped silently (**manual refiling is
  respected**, the mirror never chases user-moved messages).
- Messages without a `Message-ID` (synthesized keys) get copy-time routing
  but no propagation (unlocatable); rare and documented.
- Mail *deleted* at the source also leaves its INBOX → its destination copy
  moves to Archive (we never delete).

**DB migrations:** `PRAGMA user_version` chain (`internal/state/migrate.go`).
v1 = the schema of deployed versions without a migration system (their dbs
read as user_version=0 — the designed legacy start; IF-NOT-EXISTS, lossless).
v2 = `members` + `pending`; `labels` rows migrate into `members` with uid=0
placeholders and folder scan state is cleared, forcing a rebuild that
refreshes real UIDs. Downgrade protection: refuse dbs with a newer version.

## Consequences

- Gmail-archiving behavior now mirrors naturally: seconds-latency moves via
  the IDLE-driven reconcile; the initial bulk run routes years of archived
  mail directly to Archive; sent-only mail lands in Archive, not the inbox.
- Fully automatic upgrade from the previously deployed version: open db →
  migrate → first reconcile rebuilds membership → fingerprint mismatch →
  backfill sorts existing mail. No manual steps.
- Per-reconcile cost: one `UID SEARCH ALL` per watched folder (uids only)
  plus windowed header fetches for new mail; near-free in steady state.
- A message moved to Archive in Stalwart *by the user* while still in the
  source INBOX will be moved back by a subsequent inbox-membership change
  only if one occurs; propagation acts on source-side changes, not dest-side
  ones (one-way mirror philosophy).
