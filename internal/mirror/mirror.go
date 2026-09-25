// Package mirror runs one source→destination mail mirror: connection
// supervision with exponential backoff, seeding, the reconcile/IDLE loop and
// progress/heartbeat reporting. One goroutine per configured mirror.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/lhns/umleiter/internal/config"
	"github.com/lhns/umleiter/internal/imapx"
	"github.com/lhns/umleiter/internal/reconcile"
	"github.com/lhns/umleiter/internal/state"
)

// Run operates the mirror until ctx is cancelled. It owns its state store
// and connections; heartbeat is bumped on every unit of progress so the
// health endpoint can detect a wedged mirror.
func Run(ctx context.Context, m config.Mirror, log *slog.Logger, heartbeat *atomic.Int64) error {
	log = log.With("mirror", m.Name)
	log.Info("mirror starting",
		"source", m.Source.Addr(), "source_folder", m.Source.Folder,
		"dest", m.Dest.Addr(), "dest_folder", m.Dest.Folder,
		"poll_interval", m.PollInterval, "seed", string(m.Seed),
		"dest_guard", m.DestGuard, "uid_batch", m.UIDBatch,
		"labels", m.Labels.Enabled, "label_propagate", m.Labels.Propagate,
		"archive", m.Archive.Enabled, "sent", m.Sent.Enabled)

	store, err := state.Open(m.StatePath)
	if err != nil {
		return fmt.Errorf("mirror %s: state open: %w", m.Name, err)
	}
	defer store.Close()

	heartbeat.Store(time.Now().Unix())

	// Supervision loop: (re)connect with exponential backoff; provider
	// throttle/quota disconnects land here and simply back off and resume.
	var wait time.Duration
	for ctx.Err() == nil {
		heartbeat.Store(time.Now().Unix())
		start := time.Now()
		err := runSession(ctx, m, store, log, heartbeat)
		if err == nil || errors.Is(err, context.Canceled) {
			break
		}
		heartbeat.Store(time.Now().Unix())
		wait = backoff(wait, time.Since(start))
		log.Error("session ended, will reconnect", "err", err, "backoff", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	log.Info("mirror shutting down")
	return nil
}

const (
	initialBackoff = time.Second
	maxBackoff     = 5 * time.Minute
)

// backoff returns the reconnect delay after a session that lasted uptime,
// given the previous delay (0 = none yet). A session that stayed up for
// maxBackoff counts as healthy and restarts the sequence.
func backoff(prev, uptime time.Duration) time.Duration {
	if prev == 0 || uptime >= maxBackoff {
		return initialBackoff
	}
	return min(prev*2, maxBackoff)
}

// runSession connects both endpoints and runs reconcile+IDLE until an error
// or shutdown (nil).
func runSession(ctx context.Context, m config.Mirror, store *state.Store, log *slog.Logger, heartbeat *atomic.Int64) error {
	src, err := imapx.Dial(m.Source)
	if err != nil {
		return err
	}
	defer src.Close()

	// Resolve special-use selectors (e.g. \All) to the localized folder name.
	srcFolder, err := src.ResolveSpecialUse()
	if err != nil {
		return err
	}
	if srcFolder != m.Source.Folder {
		log.Info("resolved special-use source folder", "selector", m.Source.Folder, "folder", srcFolder)
	}
	var sentSrcFolder string
	if m.Sent.Enabled {
		if sentSrcFolder, err = src.ResolveFolder(m.Sent.SourceFolder); err != nil {
			return err
		}
		if sentSrcFolder != m.Sent.SourceFolder {
			log.Info("resolved special-use sent folder", "selector", m.Sent.SourceFolder, "folder", sentSrcFolder)
		}
	}

	dst, err := imapx.Dial(m.Dest)
	if err != nil {
		return err
	}
	defer dst.Close()

	destFolders := []string{m.Dest.Folder}
	if m.Archive.Enabled {
		destFolders = append(destFolders, m.Archive.Folder)
	}
	if m.Sent.Enabled {
		destFolders = append(destFolders, m.Sent.Folder)
	}
	for _, f := range destFolders {
		if err := dst.EnsureNamedFolder(f); err != nil {
			return err
		}
	}
	// Start with the destination folder selected (re-selected on demand later).
	if _, _, _, err := dst.SelectFolder(); err != nil {
		return err
	}

	// Heartbeat + throttled progress logging for long-running phases.
	var lastProgressLog atomic.Int64
	onProgress := func(phase, item string, processed int) {
		now := time.Now().Unix()
		heartbeat.Store(now)
		if last := lastProgressLog.Load(); now-last >= 30 && lastProgressLog.CompareAndSwap(last, now) {
			args := []any{"phase", phase, "processed", processed}
			if item != "" {
				args = append(args, "folder", item)
			}
			log.Info("progress", args...)
		}
	}

	rec := reconcile.New(store, src, dst, reconcile.Options{
		UIDBatch:           m.UIDBatch,
		DestGuard:          m.DestGuard,
		CarrySeen:          m.CarrySeen,
		SyncLabels:         m.Labels.Enabled,
		SourceFolder:       srcFolder,
		LabelExclude:       m.Labels.Exclude,
		DestFolder:         m.Dest.Folder,
		ArchiveRouting:     m.Archive.Enabled,
		SourceInbox:        m.Source.Inbox,
		ArchiveFolder:      m.Archive.Folder,
		SentRouting:        m.Sent.Enabled,
		SentSrcFolder:      sentSrcFolder,
		SentFolder:         m.Sent.Folder,
		LabelPropagate:     m.Labels.Propagate,
		KeywordPrefix:      m.Labels.KeywordPrefix,
		KeywordReplacement: m.Labels.KeywordReplacement,
		OnProgress:         onProgress,
	}, log)

	// Bootstrap the dedup set from the destination, so correctness never
	// depends on local state.
	if err := maybeSeed(ctx, m, store, rec, log); err != nil {
		return err
	}

	// Initial catch-up (may be large on first run; windowed + resumable).
	if err := runReconcile(ctx, rec, log, heartbeat); err != nil {
		return err
	}

	// IDLE/poll loop: idle_reset caps one IDLE session, poll_interval is the
	// reconcile safety net. Any wake: stop IDLE, reconcile, re-IDLE.
	for ctx.Err() == nil {
		idle, err := src.Idle()
		if err != nil {
			return err
		}
		wake := time.NewTimer(min(m.PollInterval, m.IdleReset))
		reason := "timer"
		select {
		case <-src.Notify():
			reason = "idle-push"
		case <-wake.C:
		case <-ctx.Done():
		}
		wake.Stop()
		heartbeat.Store(time.Now().Unix())
		if err := idle.Close(); err != nil {
			return err
		}
		if err := idle.Wait(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			break
		}
		log.Debug("reconcile triggered", "reason", reason)
		if err := runReconcile(ctx, rec, log, heartbeat); err != nil {
			return err
		}
	}
	return nil
}

func maybeSeed(ctx context.Context, m config.Mirror, store *state.Store, rec *reconcile.Reconciler, log *slog.Logger) error {
	seed := false
	switch m.Seed {
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
	log.Info("seeding dedup set from destination", "folder", m.Dest.Folder)
	start := time.Now()
	n, err := rec.SeedFromDest(ctx)
	if err != nil {
		return err
	}
	log.Info("seeding done", "keys", n, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func runReconcile(ctx context.Context, rec *reconcile.Reconciler, log *slog.Logger, heartbeat *atomic.Int64) error {
	start := time.Now()
	sum, err := rec.Run(ctx)
	if err != nil {
		return err
	}
	heartbeat.Store(time.Now().Unix())
	log.Info("reconcile done",
		"candidates", sum.Candidates, "copied", sum.Copied, "skipped_dupes", sum.SkippedDup,
		"moved_to_archive", sum.MovedToArchive, "moved_to_inbox", sum.MovedToInbox,
		"moved_to_sent", sum.MovedToSent,
		"keywords_set", sum.KeywordsSet, "keywords_updated", sum.KeywordsUpdated,
		"uidvalidity_changed", sum.UIDValidityChanged,
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}
