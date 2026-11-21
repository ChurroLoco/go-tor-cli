package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
)

type request struct {
	Operation  string `json:"operation"`
	Value      string `json:"value,omitempty"`
	DeleteData bool   `json:"delete_data,omitempty"`
}

type response struct {
	Torrent  app.Torrent   `json:"torrent,omitempty"`
	Torrents []app.Torrent `json:"torrents,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type Client struct {
	http     *http.Client
	stateDir string
}

func NewClient(stateDir string) *Client {
	socket := filepath.Join(stateDir, "daemon.sock")
	return &Client{stateDir: stateDir, http: &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) call(ctx context.Context, req request) (response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return response{}, fmt.Errorf("encode daemon request: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/rpc", bytes.NewReader(body))
	if err != nil {
		return response{}, fmt.Errorf("create daemon request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(r)
	if err != nil {
		return response{}, fmt.Errorf("contact daemon: %w", err)
	}
	defer res.Body.Close()
	var result response
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("decode daemon response: %w", err)
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	if res.StatusCode != http.StatusOK {
		return result, fmt.Errorf("daemon returned HTTP %d", res.StatusCode)
	}
	return result, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, request{Operation: "ping"})
	return err
}

func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.call(ctx, request{Operation: "shutdown"})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := daemonRunning(c.stateDir)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for daemon shutdown: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func (c *Client) AddMagnet(ctx context.Context, magnet string) (app.Torrent, error) {
	res, err := c.call(ctx, request{Operation: "add-magnet", Value: magnet})
	return res.Torrent, err
}

func (c *Client) AddTorrentFile(ctx context.Context, path string) (app.Torrent, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return app.Torrent{}, fmt.Errorf("resolve torrent file: %w", err)
	}
	res, err := c.call(ctx, request{Operation: "add-file", Value: path})
	return res.Torrent, err
}

func (c *Client) Torrents(ctx context.Context) ([]app.Torrent, error) {
	res, err := c.call(ctx, request{Operation: "list"})
	if res.Torrents == nil {
		res.Torrents = []app.Torrent{}
	}
	return res.Torrents, err
}

func (c *Client) Torrent(ctx context.Context, id string) (app.Torrent, error) {
	res, err := c.call(ctx, request{Operation: "status", Value: id})
	return res.Torrent, err
}

func (c *Client) Pause(ctx context.Context, id string) error {
	_, err := c.call(ctx, request{Operation: "pause", Value: id})
	return err
}

func (c *Client) Resume(ctx context.Context, id string) error {
	_, err := c.call(ctx, request{Operation: "resume", Value: id})
	return err
}

func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error {
	_, err := c.call(ctx, request{Operation: "remove", Value: id, DeleteData: deleteData})
	return err
}

func handler(svc app.Service, stop func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(response{Error: "use POST /rpc"})
			return
		}
		var req request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(response{Error: fmt.Sprintf("decode request: %v", err)})
			return
		}
		var result response
		var err error
		switch req.Operation {
		case "ping":
		case "shutdown":
			defer stop()
		case "add-magnet":
			result.Torrent, err = svc.AddMagnet(r.Context(), req.Value)
		case "add-file":
			result.Torrent, err = svc.AddTorrentFile(r.Context(), req.Value)
		case "list":
			result.Torrents, err = svc.Torrents(r.Context())
		case "status":
			result.Torrent, err = svc.Torrent(r.Context(), req.Value)
		case "pause":
			err = svc.Pause(r.Context(), req.Value)
		case "resume":
			err = svc.Resume(r.Context(), req.Value)
		case "remove":
			err = svc.Remove(r.Context(), req.Value, req.DeleteData)
		default:
			err = fmt.Errorf("unknown operation %q", req.Operation)
		}
		if err != nil {
			result.Error = err.Error()
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
