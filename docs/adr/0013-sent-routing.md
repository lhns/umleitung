# 0013 — Sent routing: a third destination bucket

Status: accepted

## Context

With archive routing (ADR 0011), sent mail — present in the source's All
Mail but never in its INBOX — landed in the Archive folder. The maintainer
wants sent mail in a destination **Sent** folder so mail clients display it
as sent.

## Decision

`sent: {enabled, folder, source_folder}` per mirror — the membership engine's
third consumer. The source's sent folder (default: the `\Sent` special-use
selector, resolved at connect time like `\All`; localization-proof) is
watched like the INBOX; routing becomes three buckets with priority
**inbox > sent > archive > primary** (mail-to-self is in inbox AND sent — the
inbox wins).

Generalizations this forced (all mechanical):
- `destFolderFor` implements the priority chain; `destBucketFolders()`
  replaces the two-folder assumptions in the dedup guard, seeding, keyword
  propagation and backfill.
- Move propagation no longer derives direction from the pending op — it
  **recomputes the desired bucket from current membership** and moves the
  copy there from whichever bucket holds it (more robust against stacked or
  stale ops, and N-bucket-proof).
- The placement backfill groups wrong-side messages **by desired bucket**
  (previously: single "other folder"), so enabling sent routing on an
  existing mirror batch-moves sent mail out of Archive automatically.
- Sent/inbox memberships are placement, not labels — excluded from keyword
  computation.

## Consequences

- Same guarantees as archive routing: moves only, content never deleted,
  manual refiling respected, Message-ID-less messages routed at copy time
  but not moved afterwards.
- Config validation: `sent.folder` must differ from `dest.folder` and
  `archive.folder`.
- Future buckets (e.g. drafts) would follow the identical pattern.
