package torrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cwd, ".engine-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func testClient(t *testing.T, dataDir, stateDir string) *Client {
	t.Helper()
	cfg := at.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.DisablePEX = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.Seed = true
	c, err := newClient(dataDir, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func fixture(t *testing.T, dir, name string, content []byte) (path, id string) {
	t.Helper()
	source := filepath.Join(dir, name)
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(source); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var out bytes.Buffer
	if err := mi.Write(&out); err != nil {
		t.Fatal(err)
	}
	path = source + ".torrent"
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, mi.HashInfoBytes().HexString()
}

func waitSnapshot(t *testing.T, c *Client, id string, predicate func(app.Torrent) bool) app.Torrent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, err := c.Torrent(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status == "error" {
			c.mu.Lock()
			err := c.entries[s.ID].err
			c.mu.Unlock()
			t.Fatalf("torrent worker failed: %v", err)
		}
		if predicate(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := c.Torrent(context.Background(), id)
	t.Fatalf("timed out waiting for torrent: %#v", s)
	return app.Torrent{}
}

func TestPersistentFileAndMagnetPauseResumeRemove(t *testing.T) {
	dir := testDirectory(t)
	dataDir, stateDir := filepath.Join(dir, "data"), filepath.Join(dir, "state")
	c := testClient(t, dataDir, stateDir)
	path, id := fixture(t, dir, "payload", []byte("local fixture"))
	ctx := context.Background()
	got, err := c.AddTorrentFile(ctx, path)
	if err != nil || got.ID != id {
		t.Fatalf("add = %#v, %v", got, err)
	}
	waitSnapshot(t, c, id, func(s app.Torrent) bool { return len(s.Files) == 1 })
	if err := c.Pause(ctx, id[:8]); err != nil {
		t.Fatal(err)
	}
	magnetID := strings.Repeat("a", 40)
	magnet := "magnet:?xt=urn:btih:" + magnetID + "&dn=unknown"
	start := time.Now()
	got, err = c.AddMagnet(ctx, magnet)
	if err != nil || got.Status != "metadata" || got.Name != "unknown" || time.Since(start) > time.Second {
		t.Fatalf("async magnet add = %#v, %v", got, err)
	}
	if err := c.Pause(ctx, magnetID); err != nil {
		t.Fatal(err)
	}
	// Restore must not depend on the original .torrent path.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = testClient(t, dataDir, stateDir)
	list, err := c.Torrents(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("restored list = %#v, %v", list, err)
	}
	for _, s := range list {
		if s.Status != "paused" || s.DownloadRate != 0 || s.UploadRate != 0 {
			t.Fatalf("restored pause state = %#v", s)
		}
	}
	if err := c.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Torrent(ctx, id)
	if got.Status == "paused" {
		t.Fatal("resume did not clear paused state")
	}
	keep := filepath.Join(dataDir, id, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("remove without delete lost data: %v", err)
	}
	if err := c.Remove(ctx, magnetID[:5], true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, magnetID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove with delete left directory: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = testClient(t, dataDir, stateDir)
	list, err = c.Torrents(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("removed torrents restored: %#v, %v", list, err)
	}
}

func TestPersistenceFailureRollsBackOperations(t *testing.T) {
	dir := testDirectory(t)
	c := testClient(t, filepath.Join(dir, "data"), filepath.Join(dir, "state"))
	id := strings.Repeat("b", 40)
	ctx := context.Background()
	if _, err := c.AddMagnet(ctx, "magnet:?xt=urn:btih:"+id); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.statePath = filepath.Join(dir, "missing", "torrents.json")
	c.mu.Unlock()
	for _, call := range []func() error{
		func() error { return c.Pause(ctx, id) },
		func() error { return c.Remove(ctx, id, true) },
		func() error {
			_, err := c.AddMagnet(ctx, "magnet:?xt=urn:btih:"+strings.Repeat("c", 40))
			return err
		},
	} {
		if err := call(); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("persistence error not wrapped: %v", err)
		}
	}
	list, err := c.Torrents(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Status == "paused" {
		t.Fatalf("failed operation changed state: %#v, %v", list, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", id)); err != nil {
		t.Fatalf("failed removal deleted data: %v", err)
	}
}

func TestContextCancellationAndClose(t *testing.T) {
	dir := testDirectory(t)
	c := testClient(t, filepath.Join(dir, "data"), filepath.Join(dir, "state"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := []func() error{
		func() error { _, err := c.AddMagnet(ctx, "not a magnet"); return err },
		func() error { _, err := c.AddTorrentFile(ctx, "missing"); return err },
		func() error { _, err := c.Torrents(ctx); return err },
		func() error { _, err := c.Torrent(ctx, "missing"); return err },
		func() error { return c.Pause(ctx, "missing") },
		func() error { return c.Resume(ctx, "missing") },
		func() error { return c.Remove(ctx, "missing", true) },
	}
	for _, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Fatalf("context cancellation lost: %v", err)
		}
	}
	for i := 0; i < 4; i++ {
		id := strings.Repeat(string(rune('a'+i)), 40)
		if _, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+id); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				if err := c.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join metadata workers")
	}
	if _, err := c.Torrents(context.Background()); err == nil {
		t.Fatal("closed client accepted a query")
	}
}

func TestCancellationWhileWaitingForLock(t *testing.T) {
	dir := testDirectory(t)
	c := testClient(t, filepath.Join(dir, "data"), filepath.Join(dir, "state"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	c.mu.Lock()
	go func() {
		_, err := c.Torrents(ctx)
		result <- err
	}()
	select {
	case err := <-result:
		c.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended operation ignored context: %v", err)
		}
	case <-time.After(time.Second):
		c.mu.Unlock()
		t.Fatal("cancellation waited for the client mutex")
	}
}

func TestSafeDeletionAndSymlinkRejection(t *testing.T) {
	dir := testDirectory(t)
	dataDir := filepath.Join(dir, "data")
	c := testClient(t, dataDir, filepath.Join(dir, "state"))
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 40)
	if err := os.Symlink(outside, filepath.Join(dataDir, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+id); err == nil {
		t.Fatal("accepted a symlink torrent directory")
	}
	if err := os.Remove(filepath.Join(dataDir, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+id); err != nil {
		t.Fatal(err)
	}
	// RemoveAll must remove links themselves, never their external targets.
	if err := os.Symlink(outside, filepath.Join(dataDir, id, "link")); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), "../../outside", true); err == nil {
		t.Fatal("accepted a traversal ID")
	}
	if err := c.Remove(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "do not remove" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

func TestPrefixesDuplicateAndBadState(t *testing.T) {
	dir := testDirectory(t)
	dataDir, stateDir := filepath.Join(dir, "data"), filepath.Join(dir, "state")
	c := testClient(t, dataDir, stateDir)
	ctx := context.Background()
	for _, id := range []string{strings.Repeat("a", 40), "a" + strings.Repeat("b", 39)} {
		m := "magnet:?xt=urn:btih:" + id
		if _, err := c.AddMagnet(ctx, m); err != nil {
			t.Fatal(err)
		}
		if _, err := c.AddMagnet(ctx, m); err != nil {
			t.Fatalf("duplicate add failed: %v", err)
		}
	}
	if _, err := c.Torrent(ctx, "a"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix accepted: %v", err)
	}
	list, _ := c.Torrents(ctx)
	if len(list) != 2 {
		t.Fatal("duplicate add created entries")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s := state{Version: 1, Torrents: []record{{ID: "../outside", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}}}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(stateDir, "torrents.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := at.NewDefaultClientConfig()
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisableUTP, cfg.DisableIPv6 = true, true, true, true
	cfg.ListenPort = 0
	if c, err := newClient(dataDir, stateDir, nil, cfg); err == nil {
		c.Close()
		t.Fatal("unsafe saved ID accepted")
	}
}

func TestInvalidTorrentInputs(t *testing.T) {
	dir := testDirectory(t)
	c := testClient(t, filepath.Join(dir, "data"), filepath.Join(dir, "state"))
	ctx := context.Background()
	if _, err := c.AddMagnet(ctx, "not a magnet"); err == nil {
		t.Fatal("accepted invalid magnet")
	}
	bad := filepath.Join(dir, "invalid.torrent")
	if err := os.WriteFile(bad, []byte("invalid bencode"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large.torrent")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((16 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, bad, large, filepath.Join(dir, "missing")} {
		if _, err := c.AddTorrentFile(ctx, path); err == nil {
			t.Fatalf("accepted invalid torrent file %q", path)
		}
	}
	list, err := c.Torrents(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("invalid inputs created torrents: %#v, %v", list, err)
	}
}

func TestLocalSeededMagnetDownload(t *testing.T) {
	dir := testDirectory(t)
	payload := bytes.Repeat([]byte("local seed bytes\n"), 8192)
	path, id := fixture(t, dir, "payload", payload)
	seedData := filepath.Join(dir, "seed-data")
	if err := os.MkdirAll(filepath.Join(seedData, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedData, id, "payload"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	seed := testClient(t, seedData, filepath.Join(dir, "seed-state"))
	if _, err := seed.AddTorrentFile(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, seed, id, func(s app.Torrent) bool { return s.Status == "seeding" })
	dataDir, stateDir := filepath.Join(dir, "download-data"), filepath.Join(dir, "download-state")
	download := testClient(t, dataDir, stateDir)
	if _, err := download.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+id); err != nil {
		t.Fatal(err)
	}
	if err := download.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	download.mu.Lock()
	dt := download.entries[id].torrent
	download.mu.Unlock()
	addrs := seed.engine.ListenAddrs()
	if len(addrs) == 0 {
		t.Fatal("seed has no listener")
	}
	dt.AddPeers([]at.PeerInfo{{Addr: addrs[0], Trusted: true}})
	waitSnapshot(t, download, id, func(s app.Torrent) bool { return len(s.Files) == 1 })
	time.Sleep(100 * time.Millisecond)
	got, _ := download.Torrent(context.Background(), id)
	if got.Downloaded != 0 || got.Status != "paused" {
		t.Fatalf("paused downloader transferred data: %#v", got)
	}
	if err := seed.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := download.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	got, _ = download.Torrent(context.Background(), id)
	if got.Downloaded != 0 {
		t.Fatalf("paused seed uploaded data: %#v", got)
	}
	if err := seed.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	got = waitSnapshot(t, download, id, func(s app.Torrent) bool { return s.Status == "seeding" })
	if got.Downloaded != int64(len(payload)) || got.Progress != 1 || len(got.Files) != 1 || got.Files[0].Downloaded != int64(len(payload)) {
		t.Fatalf("download snapshot incorrect: %#v", got)
	}
	waitSnapshot(t, seed, id, func(s app.Torrent) bool { return s.Uploaded > 0 })
	b, err := os.ReadFile(filepath.Join(dataDir, id, "payload"))
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatalf("downloaded bytes do not match: len=%d, %v", len(b), err)
	}
	if err := download.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	download = testClient(t, dataDir, stateDir)
	waitSnapshot(t, download, id, func(s app.Torrent) bool { return s.Downloaded == int64(len(payload)) })
	got, _ = download.Torrent(context.Background(), id)
	if got.Status != "paused" {
		t.Fatalf("download restart lost pause: %#v", got)
	}
}
