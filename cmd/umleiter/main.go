// Umleiter — one-way IMAP → IMAP mail mirror.
//
// One instance runs any number of configured mirrors concurrently, each with
// its own connections, state database and supervision loop. Idempotent by
// Message-ID: re-running any number of times never duplicates.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lhns/umleitung/internal/config"
	"github.com/lhns/umleitung/internal/lock"
	"github.com/lhns/umleitung/internal/mirror"
)

func main() {
	// `umleiter -healthcheck` probes the running instance's /healthz and
	// exits 0/1 — used as the container HEALTHCHECK (distroless has no curl).
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.LoadFile(config.Path())
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("umleiter starting", "config", config.Path(), "mirrors", len(cfg.Mirrors))

	// Cross-process guard: refuse to start if another instance holds the
	// state volume. Two instances would double-append.
	l, err := lock.Acquire(cfg.LockPath)
	if err != nil {
		log.Error("startup lock failed", "err", err)
		os.Exit(1)
	}
	defer l.Release()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Per-mirror liveness heartbeats; /healthz aggregates them.
	type mirrorHealth struct {
		name      string
		staleness time.Duration
		beat      *atomic.Int64
	}
	health := make([]mirrorHealth, len(cfg.Mirrors))
	for i, m := range cfg.Mirrors {
		health[i] = mirrorHealth{name: m.Name, staleness: 3 * m.PollInterval, beat: &atomic.Int64{}}
		health[i].beat.Store(time.Now().Unix())
	}
	if cfg.HealthAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				for _, h := range health {
					if time.Since(time.Unix(h.beat.Load(), 0)) > h.staleness {
						http.Error(w, fmt.Sprintf("mirror %q made no progress recently", h.name), http.StatusServiceUnavailable)
						return
					}
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})
			if err := http.ListenAndServe(cfg.HealthAddr, mux); err != nil {
				log.Error("health server failed", "err", err)
			}
		}()
	}

	var wg sync.WaitGroup
	for i, m := range cfg.Mirrors {
		wg.Add(1)
		go func(m config.Mirror, beat *atomic.Int64) {
			defer wg.Done()
			if err := mirror.Run(ctx, m, log, beat); err != nil {
				log.Error("mirror failed", "mirror", m.Name, "err", err)
				stop() // a mirror that cannot run at all (e.g. state db) takes the instance down for a clean restart
			}
		}(m, health[i].beat)
	}
	wg.Wait()
	log.Info("umleiter stopped")
}

// healthcheck probes the local /healthz endpoint of the running instance.
func healthcheck() int {
	addr := ":8080"
	if cfg, err := config.LoadFile(config.Path()); err == nil {
		if cfg.HealthAddr == "" {
			return 0 // health disabled by config: nothing to probe
		}
		addr = cfg.HealthAddr
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
		// Render durations human-readable ("11ms", "15m0s") instead of raw
		// nanosecond integers.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Value.Kind() == slog.KindDuration {
				a.Value = slog.StringValue(a.Value.Duration().String())
			}
			return a
		},
	}))
	slog.SetDefault(log)
	return log
}
