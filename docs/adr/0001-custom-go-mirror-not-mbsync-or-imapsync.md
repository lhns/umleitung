# 0001 — Custom Go mirror instead of mbsync or imapsync

Status: accepted

## Context

The requirement is a one-way, one-folder, append-only Gmail → Stalwart mirror
whose hard constraint is **no duplicate messages in Stalwart, ever** — across
restarts, state loss, and Gmail UIDVALIDITY changes. The obvious question was
whether battle-tested tools could do this instead of custom code; the concern
"are you sure you can reimplement the important parts correctly?" was raised
explicitly and deserved a real answer.

Candidates:

- **mbsync (isync) + goimapnotify + flock** — most battle-tested, IDLE-driven.
  But mbsync keys its sync state on UID + journal and explicitly does **not**
  dedupe by `Message-ID`. A Gmail UIDVALIDITY reset or sync-state loss can
  therefore produce duplicates — the exact failure the hard requirement
  forbids. Also drags in bidirectional/flag/deletion machinery this use case
  doesn't need.
- **imapsync** — purpose-built one-way IMAP copy that *does* dedupe by
  `Message-ID` natively. Solves the correctness requirement off-the-shelf, but
  is Perl with a large dependency tree (big image, against the
  single-small-container requirement) and is not IDLE-native (needs an
  external trigger or polling).
- **Custom Go binary** — smallest image, IDLE-native, and lets "no duplicates"
  be a construction property rather than a tool configuration.

## Decision

Build a small custom Go mirror. Critically, scope what is and is not
reimplemented:

- **Not reimplemented:** the IMAP wire protocol (go-imap/v2 handles literals,
  continuations, UTF-7 mailbox names, IDLE) and mbsync's genuinely hard parts
  (bidirectional sync, flag reconciliation, deletion propagation, Maildir,
  journal) — none of which this use case needs.
- **Reimplemented:** only goimapnotify's useful half — the IDLE→trigger watch —
  folded *inline* into the sync goroutine. This is simpler than
  goimapnotify+mbsync+flock: notify and sync run in one process, so there is
  no external-command handoff and no cross-process serialization between them.

What remains to own is five IMAP verbs (SELECT, SEARCH, FETCH, APPEND, IDLE)
and a loop.

## Consequences

- We own long-tail robustness (reconnects, throttling, partial fetches). This
  is acceptable because the failure envelope is forgiving: append-only +
  never-delete means the worst bug outcome is a *temporarily missed* message,
  picked up by the next reconcile — never loss, corruption, or (thanks to
  ADR 0002) a duplicate.
- The mbsync fallback is documented in the README for a future maintainer who
  prefers off-the-shelf tools, with its Message-ID caveat stated.
- imapsync remains the best off-the-shelf answer if the custom binary is ever
  abandoned; its image-size and IDLE tradeoffs are recorded here.
