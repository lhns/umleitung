# 0009 — Product-agnostic naming: SOURCE/DEST, not Gmail/Stalwart

Status: accepted

## Context

The first implementation named its configuration after the original
deployment: `GMAIL_*` and `STALWART_*` env vars, `Config.Gmail`/`Config.Stalwart`
fields, and product-specific defaults baked into code (`imap.gmail.com`,
`mail.lhns.de`, folder `[Gmail]/All Mail`). Nothing in the code actually
depends on either product — it is plain IMAP on both sides. The maintainer
asked for the whole project to be product-agnostic.

## Decision

The mirror is a generic **one-way IMAP → IMAP** tool; the Gmail → Stalwart
setup is *deployment configuration*, not code:

- Env vars are `SOURCE_*` and `DEST_*` (`SOURCE_HOST`, `SOURCE_PASSWORD[_FILE]`,
  `DEST_FOLDER`, …); config fields are `Source`/`Dest`.
- No product-specific defaults in code: hosts/users/passwords are required;
  folders default to `INBOX`; ports to `993`; TLS on.
- Provider-specific knowledge lives in documentation only — a "Provider
  notes" README section (Gmail: `[Gmail]/All Mail`, app password, ~2.5 GB/day
  download quota, ~29 min IDLE drop; Stalwart: application password) and the
  deployment examples (`docker-compose.yml`, `stack.yml`), which show the
  original Gmail → Stalwart use case with concrete values.
- Code comments cite Gmail behaviors only as *examples* of provider behavior
  (throttling, forced IDLE logout), since the handling is generic either way.

This amends ADR 0004: the dedicated-folder *reasoning* stands, but the choice
of folder name is deployment configuration; the code default is `INBOX` and
the examples use `Mirror`.

## Consequences

- Umleiter is usable for any IMAP→IMAP mirror (provider migrations, backups
  to any self-hosted server) without code changes.
- Breaking config change pre-first-deployment: `GMAIL_*`/`STALWART_*` names
  were never shipped to a running deployment, so no migration path is needed.
- The integration test uses neutral folder names, keeping the test suite free
  of product assumptions too.
