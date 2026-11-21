package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	"github.com/ChurroLoco/go-tor-cli/internal/config"
	"github.com/ChurroLoco/go-tor-cli/internal/torrent"
)

// Serve owns the engine; a file lock serializes startup and protects persisted state.
func Serve(ctx context.Context, cfg config.Config, logger *slog.Logger) (retErr error) {
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(cfg.StateDir, 0700); err != nil {
		return fmt.Errorf("protect state directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open daemon lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("daemon already running or cannot lock state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	socket := filepath.Join(cfg.StateDir, "daemon.sock")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	engine, err := torrent.New(cfg.DataDir, cfg.StateDir, logger)
	if err != nil {
		return fmt.Errorf("start torrent engine: %w", err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close torrent engine: %w", err))
		}
	}()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on local socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("protect local socket: %w", err)
	}
	logger.Info("daemon started", "socket", socket, "downloads", cfg.DataDir)
	if err := serveRPC(ctx, listener, app.NewService(engine)); err != nil {
		return err
	}
	logger.Info("daemon stopped")
	return nil
}

func serveRPC(ctx context.Context, listener net.Listener, svc app.Service) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Handler:           handler(svc, stop),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve daemon: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			cancelRequests()
			_ = server.Close()
			return fmt.Errorf("shutdown daemon: %w", err)
		}
		<-done
	}
	return nil
}

func daemonRunning(stateDir string) (bool, error) {
	lock, err := os.OpenFile(filepath.Join(stateDir, "daemon.lock"), os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open daemon lock: %w", err)
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect daemon lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return false, nil
}

// Connect starts a detached copy of this executable only when no daemon is reachable.
func Connect(ctx context.Context, cfg config.Config) (*Client, error) {
	client := NewClient(cfg.StateDir)
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	err := client.Ping(pingCtx)
	cancel()
	if err == nil {
		return client, nil
	}
	if err := ctx.Err(); err != nil {
		client.Close()
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		client.Close()
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("find executable: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.StateDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	cmd := exec.Command(executable, "--state-dir", cfg.StateDir, "--data-dir", cfg.DataDir, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = logFile.Close()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var startupErr error
	for {
		probeCtx, probeCancel := context.WithTimeout(waitCtx, 500*time.Millisecond)
		err = client.Ping(probeCtx)
		probeCancel()
		if err == nil {
			return client, nil
		}
		if startupErr != nil {
			running, lockErr := daemonRunning(cfg.StateDir)
			if lockErr != nil || !running {
				client.Close()
				return nil, fmt.Errorf("daemon exited before startup (see %s): %w", filepath.Join(cfg.StateDir, "daemon.log"), errors.Join(startupErr, lockErr))
			}
		}
		select {
		case err := <-exited:
			if err == nil {
				err = fmt.Errorf("daemon stopped before becoming ready")
			}
			startupErr = err
		case <-waitCtx.Done():
			_ = cmd.Process.Kill()
			client.Close()
			return nil, fmt.Errorf("wait for daemon (see %s): %w", filepath.Join(cfg.StateDir, "daemon.log"), waitCtx.Err())
		case <-tick.C:
		}
	}
}
