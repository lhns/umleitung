# 0005 — Concurrency: single sync loop + cross-process file lock

Status: accepted

## Context

Hard requirement from the maintainer: multiple syncs must never run
concurrently and conflict. The spec's answer — "single goroutine" plus
"`replicas: 1`, never scale >1" — is a *convention*, not a guarantee. A
fat-fingered `replicas: 2`, a manual `docker run` beside the Swarm service,
or an IDLE-triggered reconcile overlapping a poll-triggered one could all
double-append.

## Decision

Two real locks, plus the dedup backstop:

- **In-process:** the sync loop is one goroutine; IDLE wake-ups, poll timers
  and the initial catch-up all funnel through the same sequential loop, so
  two reconciles cannot overlap by construction.
- **Cross-process:** an OS-level advisory lock (`gofrs/flock`) on
  `/state/umleiter.lock`, acquired **non-blocking at startup**. A second
  instance sharing the state volume logs a clear error and exits non-zero
  immediately — it must never wait and then proceed.
- **Backstop:** even if both locks were somehow bypassed (e.g. two replicas
  with *separate* state volumes), Message-ID idempotency (ADR 0002) still
  prevents duplicates — the destination guard and seeding check Stalwart
  itself.

`replicas: 1` stays pinned in `stack.yml` as the first line of defense.

## Consequences

- "Only one sync ever runs" is enforced, not hoped for. Verified by unit
  test: second `Acquire` on a held lock fails.
- The file lock only protects instances sharing the state volume. Two
  instances with *different* volumes are caught by the dedup layers instead —
  slower (both do work) but still duplicate-free.
- Advisory file locks on some network filesystems are unreliable; the state
  volume is a local bind mount on the Swarm node, where flock semantics hold.
