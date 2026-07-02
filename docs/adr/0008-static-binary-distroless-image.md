# 0008 — Static binary, distroless image, all-env configuration

Status: accepted

## Context

Hard requirement from the maintainer: easy configurability and deployment via
a **single Docker container**. The service also handles mail credentials, so
attack surface matters.

## Decision

- **Go, `CGO_ENABLED=0`** — one static binary; every dependency is pure Go
  (this drove the modernc SQLite choice in ADR 0003 and go-imap/v2 in
  ADR 0001).
- **Multi-stage Dockerfile** → `gcr.io/distroless/static:nonroot`: final
  image contains the binary + CA certs, nothing else — no shell, no package
  manager. Runs as uid 65532; the `/state` bind mount must be owned
  accordingly.
- **All configuration via environment variables** with sane defaults; the
  only required ones are the four credentials. Secrets accept `*_FILE`
  variants for Docker/Swarm secrets. No config files, no flags (one
  exception below).
- **Health without a shell:** distroless has no curl/wget, so the container
  HEALTHCHECK re-invokes the binary itself — `umleiter -healthcheck` probes
  the running instance's `/healthz` (which reports unhealthy if no reconcile
  succeeded within 3× `POLL_INTERVAL`) and exits 0/1. Swarm restarts a
  wedged container.
- **Structured JSON logs to stdout** (never message bodies) — Swarm/`docker
  logs` capture them; no log files in the container.
- **CI** (GitHub Actions) runs vet+tests and publishes
  `ghcr.io/lhns/umleitung:latest`, which `docker-compose.yml` and `stack.yml`
  reference — deployment is `docker stack deploy -c stack.yml umleiter` plus
  two secrets and one directory.

## Consequences

- Tiny image, minimal attack surface, trivially reproducible deploys; the
  same image runs under plain `docker run`, compose, or Swarm.
- No shell in the container means no exec-debugging; diagnosis relies on
  logs and `/healthz` by design.
- The `*_FILE` secret pattern keeps credentials out of `docker inspect`
  output and image layers.
