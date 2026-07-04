# 0014 — Configurable label-keyword prefix (client namespaces)

Status: accepted, extends [ADR 0010](0010-label-sync-via-folder-membership.md)

## Context

Labels are mirrored as IMAP keywords (ADR 0010), emitted bare: `GitHub` →
`github`. A user viewing Stalwart through the **Bulwark** JMAP webmail saw no
labels. Root cause, confirmed from Bulwark's source (not the server):
Bulwark's label system is scoped to a keyword namespace — `lib/thread-utils.ts`
`KEYWORD_PREFIX = "$label:"` (legacy `"$color:"`); `getEmailColorTags` only
collects `$label:`/`$color:`-prefixed keywords; `email-list-item.tsx` renders
a pill for any `$label:<id>` (unknown → gray, labelled `<id>`). A bare
`github` keyword lands on the message (Stalwart accepts arbitrary keywords)
but Bulwark surfaces it nowhere. No open Bulwark issue requests bare-keyword
display, and `$label:` is its intended convention.

## Decision

The keyword namespace is client-specific, so make it configuration rather
than hardcode one client: `labels.keyword_prefix` (default `""` = standard
bare keyword). The prefix is prepended OUTSIDE sanitization (`$` and `:` are
valid IMAP-atom and JMAP-keyword characters; the slug stays `[a-z0-9_-]`).
Bulwark users set `"$label:"`. Applied uniformly at the three keyword sites:
copy-time append, post-copy STORE propagation, and the placement/keyword
backfill.

The prefix is part of the backfill fingerprint, so changing it on an existing
mirror re-runs the backfill and add-only-STOREs the new-form keyword onto all
already-mirrored labeled mail (the old bare keyword remains, inert). The
backfill was additionally moved to run BEFORE the mirror loop so this
correction happens promptly on the next start rather than only after a
days-long first run completes.

## Consequences

- Stays product-agnostic (ADR 0009): no client hardcoded; one config line
  switches namespaces, and a future client (or a Bulwark change) is a config
  edit, not a code change.
- Bulwark: `$label:<slug>` shows as a per-message pill immediately;
  sidebar-list membership + color still require the user to define a matching
  label id in Bulwark settings (client-side state Umleiter cannot set).
- Switching the prefix leaves a redundant bare keyword on old mail (add-only
  backfill never removes; harmless). A full-rewrite (remove old, add new)
  was rejected to preserve the "never strip user keywords" guarantee.
- New `Summary.KeywordsSet` counter surfaces copy-time keyword application in
  the `reconcile done` log for at-a-glance confirmation.
