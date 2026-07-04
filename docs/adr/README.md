# Architecture Decision Records

Decisions made while designing and building Umleiter, in the order they were
settled. Format: [Michael Nygard's ADR template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
(Context / Decision / Consequences).

| # | Decision |
|---|---|
| [0001](0001-custom-go-mirror-not-mbsync-or-imapsync.md) | Custom Go mirror instead of mbsync or imapsync |
| [0002](0002-message-id-dedup-destination-is-source-of-truth.md) | Message-ID dedup; the destination is the source of truth |
| [0003](0003-sqlite-state-store-no-in-memory-set.md) | SQLite state store; dedup set never loaded into RAM |
| [0004](0004-dedicated-gmail-folder-not-archive.md) | Dedicated destination folder, not `Archive` |
| [0005](0005-concurrency-single-loop-plus-file-lock.md) | Concurrency: single sync loop + cross-process file lock |
| [0006](0006-windowed-resumable-reconcile.md) | Windowed, resumable reconcile (Gmail quota reality) |
| [0007](0007-synthesized-dedup-keys-for-missing-message-id.md) | Synthesized dedup keys for messages without a Message-ID |
| [0008](0008-static-binary-distroless-image.md) | Static binary, distroless image, all-env configuration |
| [0009](0009-product-agnostic-source-dest-naming.md) | Product-agnostic naming: SOURCE/DEST, not Gmail/Stalwart |
| [0010](0010-label-sync-via-folder-membership.md) | Label sync via folder membership → IMAP keywords; categories out of scope |
| [0011](0011-membership-propagation-archive-and-labels.md) | Membership propagation: archive routing/moves + label keyword updates; DB migrations |
| [0012](0012-yaml-config-multi-mirror.md) | YAML configuration and multi-mirror instances |
| [0013](0013-sent-routing.md) | Sent routing: a third destination bucket |
| [0014](0014-configurable-keyword-prefix.md) | Configurable label-keyword prefix (client namespaces) |
