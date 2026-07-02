# 0006 — Windowed, resumable reconcile

Status: accepted

## Context

Two realities of a huge first run:

1. **Memory:** "search everything above the high-water mark, then process"
   materializes the full candidate list. With years of mail that's the same
   RAM problem as ADR 0003, just in a different place.
2. **Gmail quotas:** Gmail caps IMAP download at roughly ~2.5 GB/day. The
   first full mirror of a large All Mail *will* be throttled and disconnected,
   possibly repeatedly over days. If progress isn't durable at fine
   granularity, every disconnect restarts expensive work.

## Decision

Scan `[last_uid+1 .. UIDNEXT-1]` in ascending UID windows of `UID_BATCH`
(default 2000):

- One batched `UID FETCH` per window retrieves header metadata (Message-ID,
  From, Subject, INTERNALDATE, size) for all messages in the window in a
  single round trip. (This also made a separate `UID SEARCH` step
  unnecessary — a UID-range FETCH returns only existing UIDs.)
- Messages are fetched-full and appended **one at a time** — at most one
  message body in memory.
- The dedup key is inserted per successful append; **`last_uid` commits once
  per window**, making the scan resumable at window granularity.
- Gmail throttle/quota errors and disconnects are treated as *expected*: the
  session ends, the supervisor backs off exponentially (capped at 5 min),
  reconnects, and the next reconcile resumes from the last committed window.
  Never a crash-loop, never treated as fatal.

## Consequences

- Memory is bounded by one window of header metadata + one message body,
  independent of mailbox size.
- A multi-day, quota-throttled first run is safe to interrupt at any point;
  re-scanning is bounded to at most one window (whose appends are dedup-
  skipped anyway).
- Verified by unit test: a connection failure mid-scan resumes from the last
  committed window without re-fetching completed windows and without
  duplicating.
- README warns that the first run may take days — a Gmail quota, not a bug.
