package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	"github.com/ChurroLoco/go-tor-cli/internal/config"
)

func TestServeShutdownAndExclusiveState(t *testing.T) {
	dir, err := os.MkdirTemp("", "gotor-serve-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.Config{StateDir: filepath.Join(dir, "state"), DataDir: filepath.Join(dir, "data")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, logger) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	client := NewClient(cfg.StateDir)
	defer client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		err = client.Ping(probe)
		stop()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Serve(ctx, cfg, logger); err == nil {
		t.Fatal("second daemon acquired the same state directory")
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{cfg.StateDir, 0700}, {filepath.Join(cfg.StateDir, "daemon.sock"), 0600}} {
		fi, err := os.Stat(tc.path)
		if err != nil || fi.Mode().Perm() != tc.mode {
			t.Fatalf("permissions %s: info=%v error=%v", tc.path, fi, err)
		}
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		// Leave the cleanup callback a completion token.
		done <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not close the engine")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "daemon.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
}

type blockingService struct {
	app.Service
	entered chan struct{}
	release chan struct{}
}

func (s blockingService) Torrents(ctx context.Context) ([]app.Torrent, error) {
	close(s.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return []app.Torrent{}, nil
	}
}

func TestRPCShutdownDrainsInflightRequests(t *testing.T) {
	dir, err := os.MkdirTemp("", "gotor-drain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	svc := blockingService{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveRPC(ctx, listener, svc) }()
	client := NewClient(dir)
	defer client.Close()
	listDone := make(chan error, 1)
	go func() {
		_, err := client.Torrents(ctx)
		listDone <- err
	}()
	select {
	case <-svc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("list request did not reach service")
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-listDone:
		t.Fatalf("shutdown canceled an in-flight request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(svc.release)
	select {
	case err := <-listDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("list request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish draining")
	}
}
