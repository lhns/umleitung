package integration

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhns/umleitung/internal/config"
	"github.com/lhns/umleitung/internal/mirror"
)

// TestMultiMirror runs two independent mirrors concurrently in one process —
// separate source servers, separate destination servers, separate state
// databases — via mirror.Run, the same entry point main uses.
func TestMultiMirror(t *testing.T) {
	src1EP, src1 := startServer(t, "alice@src")
	src2EP, src2 := startServer(t, "bob@src")
	dst1EP, dst1 := startServer(t, "alice@dst")
	dst2EP, dst2 := startServer(t, "bob@dst")

	if err := src1.Create(srcFolder, nil); err != nil {
		t.Fatal(err)
	}
	if err := src2.Create(srcFolder, nil); err != nil {
		t.Fatal(err)
	}
	// Pre-create dest folders so the test's polling checker never races the
	// mirrors' own EnsureFolder.
	if err := dst1.Create(dstFolder, nil); err != nil {
		t.Fatal(err)
	}
	if err := dst2.Create(dstFolder, nil); err != nil {
		t.Fatal(err)
	}

	baseDate := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	{
		ep := src1EP
		ep.Folder = srcFolder
		appendToSource(t, ep, rawMessage("<alice-1@test>", "alice one"), baseDate)
		appendToSource(t, ep, rawMessage("<alice-2@test>", "alice two"), baseDate.Add(time.Minute))
	}
	{
		ep := src2EP
		ep.Folder = srcFolder
		appendToSource(t, ep, rawMessage("<bob-1@test>", "bob one"), baseDate)
	}

	stateDir := t.TempDir()
	mkMirror := func(name string, srcEP, dstEP config.Endpoint) config.Mirror {
		srcEP.Folder = srcFolder
		dstEP.Folder = dstFolder
		return config.Mirror{
			Name:         name,
			StatePath:    filepath.Join(stateDir, name+".db"),
			PollInterval: time.Second, // fast reconcile cadence for the test
			IdleReset:    time.Minute,
			UIDBatch:     2,
			Seed:         config.SeedEmpty,
			DestGuard:    true,
			CarrySeen:    true,
			Source:       srcEP,
			Dest:         dstEP,
			Archive:      config.Archive{Enabled: false, Folder: "Archive"},
			Labels:       config.Labels{},
		}
	}
	mirrors := []config.Mirror{
		mkMirror("alice", src1EP, dst1EP),
		mkMirror("bob", src2EP, dst2EP),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.DiscardHandler)
	beats := []*atomic.Int64{{}, {}}

	var wg sync.WaitGroup
	for i, m := range mirrors {
		wg.Add(1)
		go func(m config.Mirror, beat *atomic.Int64) {
			defer wg.Done()
			if err := mirror.Run(ctx, m, log, beat); err != nil {
				t.Errorf("mirror %s: %v", m.Name, err)
			}
		}(m, beats[i])
	}

	// Wait until both destinations hold exactly their own mail.
	deadline := time.Now().Add(20 * time.Second)
	for {
		alice := midsIn(t, dst1EP, dstFolder)
		bob := midsIn(t, dst2EP, dstFolder)
		if len(alice) == 2 && alice["<alice-1@test>"] && alice["<alice-2@test>"] &&
			len(bob) == 1 && bob["<bob-1@test>"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirrors incomplete: alice=%v bob=%v", alice, bob)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Cross-contamination check: no bob mail at alice's dest and vice versa.
	if midsIn(t, dst1EP, dstFolder)["<bob-1@test>"] {
		t.Fatal("bob's mail leaked into alice's destination")
	}

	// Both heartbeats advanced.
	for i, b := range beats {
		if b.Load() == 0 {
			t.Fatalf("mirror %d heartbeat never advanced", i)
		}
	}

	// Clean shutdown of both.
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("mirrors did not shut down")
	}
}
