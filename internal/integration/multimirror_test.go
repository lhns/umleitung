package integration

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lhns/umleiter/internal/config"
	"github.com/lhns/umleiter/internal/imapx"
	"github.com/lhns/umleiter/internal/mirror"
)

// TestMultiMirror runs two independent mirrors concurrently through
// mirror.Run, the entry point main uses: separate servers, separate state
// databases. It also covers what only the runtime wires up: startup seeding,
// config -> reconcile option plumbing, and IDLE-pushed incremental sync.
func TestMultiMirror(t *testing.T) {
	t.Parallel()
	aliceSrc, aliceSrcUser := startServer(t, "alice@src")
	aliceDst, aliceDstUser := startServer(t, "alice@dst")
	bobSrc, bobSrcUser := startServer(t, "bob@src")
	bobDst, bobDstUser := startServer(t, "bob@dst")
	aliceSrc.Folder, bobSrc.Folder = srcFolder, srcFolder
	aliceDst.Folder, bobDst.Folder = dstFolder, dstFolder
	// Destination folders are pre-created so polling never races the
	// mirrors' own creation.
	for u, folders := range map[*imapmemserver.User][]string{
		aliceSrcUser: {srcFolder, srcInbox, "Work"},
		bobSrcUser:   {srcFolder, srcInbox, "Work"},
		aliceDstUser: {dstFolder},
		bobDstUser:   {dstFolder, dstArchive},
	} {
		for _, f := range folders {
			if err := u.Create(f, nil); err != nil {
				t.Fatal(err)
			}
		}
	}

	// alice: plain mirror whose destination already holds alice-1. The guard
	// is off, so only startup seeding (seed: empty) prevents a duplicate.
	alice1 := rawMessage("<alice-1@test>", "alice one")
	appendMsg(t, aliceSrc, srcFolder, alice1, at(0))
	appendMsg(t, aliceSrc, srcFolder, rawMessage("<alice-2@test>", "alice two"), at(1))
	appendMsg(t, aliceDst, dstFolder, alice1, at(0))

	// bob: archive routing + labels; bob-1 is archived and labeled Work.
	bob1 := rawMessage("<bob-1@test>", "bob one")
	appendMsg(t, bobSrc, srcFolder, bob1, at(0))
	appendMsg(t, bobSrc, "Work", bob1, at(0))

	stateDir := t.TempDir()
	mkMirror := func(name string, src, dst config.Endpoint) config.Mirror {
		src.Inbox = srcInbox
		return config.Mirror{
			Name:         name,
			StatePath:    filepath.Join(stateDir, name+".db"),
			PollInterval: time.Hour, // new mail must arrive via IDLE push
			IdleReset:    time.Hour,
			UIDBatch:     2,
			Seed:         config.SeedEmpty,
			DestGuard:    true,
			CarrySeen:    true,
			Source:       src,
			Dest:         dst,
			Archive:      config.Archive{Folder: dstArchive},
			Labels:       config.Labels{KeywordReplacement: "_"},
		}
	}
	alice := mkMirror("alice", aliceSrc, aliceDst)
	alice.DestGuard = false
	bob := mkMirror("bob", bobSrc, bobDst)
	bob.Archive.Enabled = true
	bob.Labels = config.Labels{Enabled: true, Propagate: true, KeywordPrefix: "$label:", KeywordReplacement: "-"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.DiscardHandler)
	var wg sync.WaitGroup
	beats := make([]*atomic.Int64, 2)
	for i, m := range []config.Mirror{alice, bob} {
		beats[i] = &atomic.Int64{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mirror.Run(ctx, m, log, beats[i]); err != nil {
				t.Errorf("mirror %s: %v", m.Name, err)
			}
		}()
	}
	stopped := func() {
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(pollTimeout):
			t.Fatal("mirrors did not shut down")
		}
	}
	defer stopped()

	waitMids(t, aliceDst, dstFolder, nil, "<alice-1@test>", "<alice-2@test>")
	waitMids(t, bobDst, dstArchive, nil, "<bob-1@test>")
	wantMids(t, bobDst, map[string][]string{dstFolder: nil})
	wantKeywords(t, keywordsIn(t, bobDst, dstArchive), "<bob-1@test>", "$label:work")

	// New mail after the initial catch-up: poll_interval is an hour, so only
	// the IDLE push can deliver it. imapmemserver holds back updates queued
	// before a session enters IDLE until the next mailbox change, so keep
	// changing a flag in the source folder in case the append raced the
	// mirror's IDLE entry.
	appendMsg(t, aliceSrc, srcFolder, rawMessage("<alice-3@test>", "alice three"), at(2))
	poke := func() {
		cl, err := imapx.Dial(aliceSrc)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		if _, _, _, err := cl.SelectFolder(); err != nil {
			t.Fatal(err)
		}
		if err := cl.StoreKeywordsUIDs([]imap.UID{1}, []imap.Flag{"poke"}); err != nil {
			t.Fatal(err)
		}
	}
	waitMids(t, aliceDst, dstFolder, poke, "<alice-1@test>", "<alice-2@test>", "<alice-3@test>")

	for i, b := range beats {
		if b.Load() == 0 {
			t.Fatalf("mirror %d heartbeat never advanced", i)
		}
	}
}

// waitMids polls until folder holds exactly the given Message-IDs, calling
// tick (if set) between polls.
func waitMids(t *testing.T, ep config.Endpoint, folder string, tick func(), want ...string) {
	t.Helper()
	slices.Sort(want)
	deadline := time.Now().Add(pollTimeout)
	for {
		got := midsIn(t, ep, folder)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %v, want %v", folder, got, want)
		}
		if tick != nil {
			tick()
		}
		time.Sleep(20 * time.Millisecond)
	}
}
