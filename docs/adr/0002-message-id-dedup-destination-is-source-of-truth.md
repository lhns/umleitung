# 0002 — Message-ID dedup; the destination is the source of truth

Status: accepted

## Context

"How can we make sure that the sync state is persisted — or that we otherwise
don't duplicate messages into Stalwart?" The naive design trusts a local
state file; but volumes get wiped, hosts change, databases corrupt. IMAP
`APPEND` is not idempotent (every append creates a new message), so retries
cannot be absorbed by the server — something must check *before* appending.

Key insight: every copied message lands in Stalwart with its `Message-ID`
header preserved verbatim, so **the destination folder itself is the
authoritative record of what has been mirrored**. Local state can lie or
vanish; the destination's contents cannot.

## Decision

Dedup is keyed on the RFC 5322 `Message-ID` (a property of message *content*),
never on IMAP UIDs (a property of the *mailbox*, reset by UIDVALIDITY
changes). Correctness never depends on local state. Three layers, each
checked before any append:

1. **Destination seeding (default on, `SEED_DEST=empty`)** — on any start
   where the local dedup set is empty/missing, stream the destination
   folder's `Message-ID`s into it. Wipe the volume, move hosts, corrupt the
   db: the next start re-derives the truth from Stalwart and copies zero
   duplicates. (The spec had this as "optional hardening"; it was promoted to
   the default.)
2. **Persisted local set (fast path)** — SQLite table, indexed lookup.
   **Ordering is safety-critical: APPEND first, record the key only after
   the server confirms.** Never record-then-append. The only crash window is
   "appended but not yet recorded".
3. **Destination guard (default on, `DEST_GUARD`)** — for any candidate not
   in the set, `UID SEARCH HEADER Message-ID <id>` against the destination
   before appending. This closes the layer-2 crash window. One extra round
   trip per *new* message; negligible in steady state.

Either layer 1 or layer 3 alone already guarantees no duplicates; both is
belt-and-suspenders. On UIDVALIDITY change, only the UID high-water mark is
reset — the key set is untouched, so a full rescan appends nothing twice.

## Consequences

- `rm -rf /state` is safe. Backup of the state volume is a performance
  optimization (instant restart), not a correctness requirement.
- Two *distinct* messages sharing one `Message-ID` (some spam/forwards): the
  second is skipped. Inherent tradeoff of Message-ID dedup (imapsync behaves
  identically); documented in the README.
- Seeding a huge destination folder costs one streamed scan on first start —
  a one-time cost unless state is lost.
- Messages without a `Message-ID` need a synthesized key → ADR 0007.
