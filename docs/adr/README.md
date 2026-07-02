# Architecture Decision Records

Decisions made while designing and building Umleiter, in the order they were
settled. Format: [Michael Nygard's ADR template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
(Context / Decision / Consequences).

| # | Decision |
|---|---|
| [0001](0001-custom-go-mirror-not-mbsync-or-imapsync.md) | Custom Go mirror instead of mbsync or imapsync |
| [0002](0002-message-id-dedup-destination-is-source-of-truth.md) | Message-ID dedup; the destination is the source of truth |
| [0003](0003-sqlite-state-store-no-in-memory-set.md) | SQLite state store; dedup set never loaded into RAM |
| [0004](0004-dedicated-gmail-folder-not-archive.md) | Dedicated `Gmail` destination folder, not `Archive` |
| [0005](0005-concurrency-single-loop-plus-file-lock.md) | Concurrency: single sync loop + cross-process file lock |
| [0006](0006-windowed-resumable-reconcile.md) | Windowed, resumable reconcile (Gmail quota reality) |
| [0007](0007-synthesized-dedup-keys-for-missing-message-id.md) | Synthesized dedup keys for messages without a Message-ID |
| [0008](0008-static-binary-distroless-image.md) | Static binary, distroless image, all-env configuration |
