# 0010 — Label sync via folder membership → IMAP keywords; categories out of scope

Status: accepted

## Context

The maintainer asked whether Gmail labels and Gmail inbox categories
(Allgemein/Primary, Werbung/Promotions, Foren/Forums, Benachrichtigungen/
Updates, Soziale Medien/Social) can be mirrored.

Facts established against go-imap v2.0.0-beta.8:

- go-imap v2 has **no Gmail extension support**: no `X-GM-LABELS`, no
  `X-GM-RAW`, no custom FETCH/SEARCH hooks, no public raw-command API. The
  direct label route would mean hand-rolling IMAP wire parsing — rejected in
  ADR 0001.
- **Labels are reachable product-agnostically anyway**: Gmail (like other
  label-based servers) exposes every user label as an IMAP *folder*; a
  message with N labels appears in N folders. Folder membership ⇒ label set.
- **Categories are neither labels nor folders.** They are invisible to
  `X-GM-LABELS` and to LIST; they exist only via `X-GM-RAW category:...`
  search (unsupported extension) or the Gmail REST API's `CATEGORY_*` labels
  (OAuth — excluded by ADR/spec). They also carry no meaning to any client
  outside Gmail.
- Destination equivalent of a label: **IMAP keywords** (custom per-message
  flags). Keywords need **no pre-creation** — there is no server-side tag
  registry; a keyword exists on a message by being set. Stalwart stores them
  as JMAP keywords. Whether a server permits arbitrary keywords is advertised
  via `PERMANENTFLAGS \*` on SELECT.

## Decision

Opt-in `SYNC_LABELS=true` (default off). Each reconcile first runs a **label
scan**: LIST all source folders; exclude the mirror source folder, `INBOX`
(inbox membership is not a label), unselectable and special-use folders
(`\Sent`, `\Trash`, `\Junk`, `\All`, `\Archive`, `\Drafts`, `\Flagged`,
`\Important`), and user-listed `LABEL_EXCLUDE` names. Remaining folders are
scanned with the same windowed, resumable, per-folder
UIDVALIDITY+high-water-mark machinery as the main mirror, recording
`(dedup key, label)` in SQLite. The mirror phase then appends each message
with its labels mapped to keywords.

Keyword mapping: RFC 3501 flag atoms only (printable ASCII minus
``( ) { % * " \ ]`` and space — a protocol limit on every IMAP server).
Disallowed runes → `_`, runs collapsed, trimmed, then **lowercased** (IMAP
flags are case-insensitive and servers canonicalize them; the integration
test against go-imap's own server proved casing is not preserved).
`[Werbung]` → `werbung`, `Work/Projects` → `work_projects`, `Bücher` →
`b_cher`.

Labels must never break the mirror:
- If the destination does not advertise `PERMANENTFLAGS \*`, log a warning
  and attempt anyway (servers often accept more than they advertise).
- If an APPEND **with keywords** fails, retry once **without** keywords
  before treating it as an error. Correctness (the copy) always wins over
  decoration (the tags).

## Consequences

- Labels are captured **as of copy time**. Later label changes in the source
  are not propagated; retroactive backfill (`STORE` keyword updates on
  existing destination messages — the first write-to-existing-message
  operation) is deferred as future work.
- Distinct labels can collide after sanitization (`[Werbung]` and `Werbung`
  → `werbung`); harmless and documented.
- Client visibility varies: Thunderbird shows keywords as tags once a
  matching tag key is defined (one-time, cosmetic); JMAP/webmail clients
  generally show them automatically; most mobile clients ignore them (they
  remain stored server-side).
- **Categories are not implemented** and are documented as infeasible within
  this design (no X-GM support, no OAuth). Anyone needing them must use the
  Gmail REST API — a different tool.
- The label scan adds one LIST plus incremental per-folder scans to each
  reconcile; after the first run this is near-free (high-water marks).
