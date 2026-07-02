# 0004 — Dedicated destination folder, not `Archive`

Status: accepted, amended by [ADR 0009](0009-product-agnostic-source-dest-naming.md)
(the folder choice is now deployment configuration — code default is `INBOX`,
the deployment examples use a dedicated `Mirror` folder)

## Context

The spec named `Archive` as the destination folder. The maintainer was
unsure ("probably not?"). The doubt is well-founded: the source is
`[Gmail]/All Mail`, which mixes inbox, sent and archived mail into one
stream. Candidate destinations carry different semantics:

- `Archive` — usually means "mail I consciously archived"; dumping sent and
  unread mail there is semantically muddy.
- `INBOX` — mirrored mail appears freshly delivered; years of old + sent
  mail would flood it. Clearly wrong for this source.
- A dedicated folder — clearly labels the mail as a mirror, never collides
  with natively-managed Stalwart mail.

## Decision

Mirror into a **dedicated destination folder**, created on startup if
missing. Keep it fully configurable via `DEST_FOLDER`, so switching to
`Archive`, `INBOX` or anything else is a one-line env change with no rebuild.

## Consequences

- Deviates from the spec's stated default; recorded here and in the README.
- The seeding logic (ADR 0002) targets whatever folder is configured — if a
  prior imapsync bulk import populated a different folder, point
  `DEST_FOLDER` at it and seeding prevents re-copying.
- Changing the folder *after* mail has been mirrored starts a fresh mirror
  into the new folder (dedup keys seed from the new folder's contents), so
  pick the folder before the first big run.
