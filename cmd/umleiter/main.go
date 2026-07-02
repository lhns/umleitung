// Umleiter — one-way IMAP → IMAP mail mirror.
//
// Single process, single sync loop. Near-real-time via IMAP IDLE with a
// periodic full reconcile as a safety net. Idempotent by Message-ID:
// re-running any number of times never duplicates.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lhns/umleitung/internal/config"
	"github.com/lhns/umleitung/internal/imapx"
	"github.com/lhns/umleitung/internal/lock"
	"github.com/lhns/umleitung/internal/reconcile"
	"github.com/lhns/umleitung/internal/state"
)

func main() {
	// `umleiter -healthcheck` probes the running instance's /healthz and
	// exits 0/1 — used as the container HEALTHCHECK (distroless has no curl).
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("umleiter starting",
		"source", cfg.Source.Addr(), "source_folder", cfg.Source.Folder,
		"dest", cfg.Dest.Addr(), "dest_folder", cfg.Dest.Folder,
		"poll_interval", cfg.PollInterval, "seed_dest", string(cfg.SeedDest),
		"dest_guard", cfg.DestGuard, "uid_batch", cfg.UIDBatch,
		"sync_labels", cfg.SyncLabels)

	// Cross-process guard: refuse to start if another instance holds the
	// state volume. Two syncers would double-append.
	l, err := lock.Acquire(cfg.LockPath)
	if err != nil {
		log.Error("startup lock failed", "err", err)
		os.Exit(1)
	}
	defer l.Release()

	store, err := state.Open(cfg.StatePath)
	if err != nil {
		log.Error("state open failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var healthy atomic.Int64 // unix time of last successful reconcile
	if cfg.HealthAddr != "" {
		go serveHealth(cfg, log, &healthy)
	}

	// Outer supervision loop: (re)connect both sides with exponential
	// backoff, run the sync loop, and on any connection error start over.
	// Provider throttle/quota disconnects (e.g. Gmail's daily IMAP download
	// cap) land here too — they are expected during a large first run and
	// simply back off and resume.
	backoff := time.Second
	const maxBackoff = 5 * time.Minute
	for {
		if ctx.Err() != nil {
			log.Info("shutting down")
			return
		}
		err := runSession(ctx, cfg, store, log, &healthy)
		if err == nil || errors.Is(err, context.Canceled) {
			log.Info("shutting down")
			return
		}
		log.Error("session ended, will reconnect", "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runSession connects both endpoints and runs reconcile+IDLE until an error
// or shutdown. Returning nil means clean shutdown.
func runSession(ctx context.Context, cfg *config.Config, store *state.Store, log *slog.Logger, healthy *atomic.Int64) error {
	src, err := imapx.Dial(cfg.Source)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := imapx.Dial(cfg.Dest)
	if err != nil {
		return err
	}
	defer dst.Close()

	if err := dst.EnsureFolder(); err != nil {
		return err
	}
	// Keep the destination folder selected: the per-append destination guard
	// and seeding search/fetch against it.
	if _, _, _, err := dst.SelectFolder(); err != nil {
		return err
	}

	rec := reconcile.New(store, src, dst, reconcile.Options{
		UIDBatch:     cfg.UIDBatch,
		DestGuard:    cfg.DestGuard,
		CarrySeen:    cfg.CarrySeen,
		SyncLabels:   cfg.SyncLabels,
		SourceFolder: cfg.Source.Folder,
		LabelExclude: cfg.LabelExclude,
	}, log)

	// Destination seeding: bootstrap the dedup set from what the destination
	// already holds, so correctness never depends on local state.
	if err := maybeSeed(ctx, cfg, store, rec, log); err != nil {
		return err
	}

	// Initial catch-up (may be large on first run; windowed + resumable).
	if err := runReconcile(ctx, rec, log, healthy); err != nil {
		return err
	}

	// IDLE/poll loop. go-imap auto-restarts IDLE every ~28min; IDLE_RESET is
	// our own extra cap on one IDLE session, POLL_INTERVAL the reconcile
	// safety net. Any wake → stop IDLE → reconcile → re-IDLE.
	for {
		if ctx.Err() != nil {
			return nil
		}
		idle, err := src.Idle()
		if err != nil {
			return err
		}
		wake := time.NewTimer(minDuration(cfg.PollInterval, cfg.IdleReset))
		var reason string
		select {
		case <-src.Notify():
			reason = "idle-push"
		case <-wake.C:
			reason = "timer"
		case <-ctx.Done():
			reason = "shutdown"
		}
		wake.Stop()
		if err := idle.Close(); err != nil {
			return err
		}
		if err := idle.Wait(); err != nil {
			return err
		}
		if reason == "shutdown" {
			return nil
		}
		log.Debug("reconcile triggered", "reason", reason)
		if err := runReconcile(ctx, rec, log, healthy); err != nil {
			return err
		}
	}
}

func maybeSeed(ctx context.Context, cfg *config.Config, store *state.Store, rec *reconcile.Reconciler, log *slog.Logger) error {
	seed := false
	switch cfg.SeedDest {
	case config.SeedAlways:
		seed = true
	case config.SeedEmpty:
		n, err := store.CopiedCount()
		if err != nil {
			return err
		}
		seed = n == 0
	}
	if !seed {
		return nil
	}
	log.Info("seeding dedup set from destination folder", "folder", cfg.Dest.Folder)
	start := time.Now()
	n, err := rec.SeedFromDest(ctx)
	if err != nil {
		return err
	}
	log.Info("seeding done", "keys", n, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func runReconcile(ctx context.Context, rec *reconcile.Reconciler, log *slog.Logger, healthy *atomic.Int64) error {
	start := time.Now()
	sum, err := rec.Run(ctx)
	if err != nil {
		return err
	}
	healthy.Store(time.Now().Unix())
	log.Info("reconcile done",
		"candidates", sum.Candidates, "copied", sum.Copied, "skipped_dupes", sum.SkippedDup,
		"uidvalidity_changed", sum.UIDValidityChanged,
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// serveHealth exposes /healthz: 200 while reconciles keep succeeding, 503 if
// none succeeded within 3× POLL_INTERVAL (wedged container → Swarm restarts).
func serveHealth(cfg *config.Config, log *slog.Logger, healthy *atomic.Int64) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		last := healthy.Load()
		if last == 0 || time.Since(time.Unix(last, 0)) > 3*cfg.PollInterval {
			http.Error(w, "no successful reconcile recently", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if err := http.ListenAndServe(cfg.HealthAddr, mux); err != nil {
		log.Error("health server failed", "err", err)
	}
}

// healthcheck probes the local /healthz endpoint of the running instance.
func healthcheck() int {
	addr := os.Getenv("HEALTH_ADDR")
	if addr == "" {
		addr = ":8080"
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
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)
	return log
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
