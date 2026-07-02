# 0007 — Synthesized dedup keys for messages without a Message-ID

Status: accepted

## Context

A missing or empty `Message-ID` is rare but legal (RFC 5322 makes it a
*should*, not a *must*). Such messages still need a stable dedup key, and —
because of destination seeding (ADR 0002) — the key must be computable
identically from both the Gmail original and the Stalwart copy.

## Decision

For messages without a `Message-ID`, synthesize:

```
synth-sha256:HEX( SHA-256( unix(INTERNALDATE) | len:From | len:Subject | size ) )
```

- All four inputs survive the mirror copy (APPEND preserves INTERNALDATE and
  the raw message bytes), so seeding from the destination recomputes the
  identical key.
- Fields are **length-prefixed** before hashing. A plain separator join is
  collision-prone: `From="a", Subject="b\x00c"` and `From="a\x00b",
  Subject="c"` would hash identically. (This exact flaw existed in the first
  implementation and was caught by a unit test; length prefixes fix it.)
- The `synth-sha256:` prefix marks these keys so the destination guard
  (layer 3) knows it cannot `SEARCH HEADER Message-ID` for them.

## Consequences

- Idempotency holds for Message-ID-less mail across restarts, state loss and
  re-seeding — layers 1–2 cover them; only the per-append guard (layer 3)
  does not apply.
- Two genuinely different messages with identical (INTERNALDATE, From,
  Subject, size) would collide; with second-granularity timestamps this is
  vanishingly rare, and the failure mode is a skipped near-identical copy,
  consistent with the append-only philosophy.
- Considered and deferred: stamping an `X-Umleiter-Key` header on append so
  layer 3 could search synthesized keys too — rejected for now to keep
  mirrored messages byte-identical to the originals.
