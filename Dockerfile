# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /umleiter ./cmd/umleiter

# ---- final stage ----
# distroless/static ships CA certs and a nonroot user; the binary is static.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /umleiter /umleiter

# State volume: SQLite db + startup lock live here. Must be persistent.
VOLUME /state

# /healthz liveness endpoint (HEALTH_ADDR, default :8080)
EXPOSE 8080

USER nonroot
ENTRYPOINT ["/umleiter"]
