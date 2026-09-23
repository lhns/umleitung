# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG GIT_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${GIT_SHA}" -o /umleiter ./cmd/umleiter \
 && mkdir /state

# ---- final stage ----
# distroless/static ships CA certs and a nonroot user; the binary is static.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /umleiter /umleiter
# Pre-owned by nonroot so a fresh named volume is writable (bind mounts
# still need `chown -R 65532`).
COPY --from=build --chown=65532:65532 /state /state

# State volume: SQLite db + startup lock live here. Must be persistent.
VOLUME /state

# /healthz liveness endpoint (health_addr, default :8080)
EXPOSE 8080

ENTRYPOINT ["/umleiter"]
