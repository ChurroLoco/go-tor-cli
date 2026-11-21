// Package torrent provides the persistent BitTorrent backend.
package torrent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var _ app.Service = (*Client)(nil)

type entry struct {
	record
	torrent *at.Torrent
	storage storage.ClientImplCloser
	cancel  context.CancelFunc
	err     error
	ready   bool
	last    time.Time
	read    int64
	written int64
	down    float64
	up      float64
}

// Client owns its engine and metadata workers. Close must be called on shutdown.
type Client struct {
	mu        sync.Mutex
	engine    *at.Client
	entries   map[string]*entry
	dataDir   string
	dataRoot  *os.Root
	statePath string
	logger    *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

// New restores saved torrents. Metadata acquisition never blocks construction.
func New(dataDir, stateDir string, logger *slog.Logger) (*Client, error) {
	cfg := at.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.Seed = true
	return newClient(dataDir, stateDir, logger, cfg)
}

func newClient(dataDir, stateDir string, logger *slog.Logger, cfg *at.ClientConfig) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var err error
	for _, dir := range []string{dataDir, stateDir} {
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("data and state directories must not be empty")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	dataDir, err = filepath.EvalSymlinks(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute data directory: %w", err)
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open data directory: %w", err)
	}
	cfg.DataDir = dataDir
	cfg.Slogger = logger
	cfg.NoDefaultPortForwarding = true
	engine, err := at.NewClient(cfg)
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("create BitTorrent engine: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		engine: engine, entries: make(map[string]*entry), dataDir: dataDir, dataRoot: root,
		statePath: filepath.Join(stateDir, "torrents.json"), logger: logger,
		ctx: ctx, cancel: cancel, closeDone: make(chan struct{}),
	}
	if err := c.restore(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func specID(spec *at.TorrentSpec) string {
	if spec.InfoHash != (metainfo.Hash{}) {
		return spec.InfoHash.HexString()
	}
	if spec.InfoHashV2.Ok {
		h := spec.InfoHashV2.Value
		return h.ToShort().HexString()
	}
	return ""
}

func parseRecord(r record) (*at.TorrentSpec, error) {
	if len(r.Metainfo) != 0 {
		mi, err := metainfo.Load(bytes.NewReader(r.Metainfo))
		if err != nil {
			return nil, fmt.Errorf("decode metainfo: %w", err)
		}
		return at.TorrentSpecFromMetaInfoErr(mi)
	}
	return at.TorrentSpecFromMagnetUri(r.Magnet)
}

func (c *Client) restore() error {
	data, err := os.ReadFile(c.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read torrent state: %w", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("decode torrent state: %w", err)
	}
	if s.Version != 1 {
		return fmt.Errorf("unsupported torrent state version %d", s.Version)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range s.Torrents {
		spec, err := parseRecord(r)
		if err != nil {
			return fmt.Errorf("restore torrent %q: %w", r.ID, err)
		}
		if !validID(r.ID) || specID(spec) == "" {
			return fmt.Errorf("invalid saved torrent ID %q", r.ID)
		}
		// Hybrid torrents added by v2 retain their original, stable ID.
		if r.ID != specID(spec) && (!spec.InfoHashV2.Ok || r.ID != spec.InfoHashV2.Value.ToShort().HexString()) {
			return fmt.Errorf("saved torrent %q does not match its metainfo", r.ID)
		}
		if _, ok := c.entries[r.ID]; ok {
			return fmt.Errorf("duplicate saved torrent %q", r.ID)
		}
		if _, err := c.addLocked(c.ctx, r, spec, false); err != nil {
			return fmt.Errorf("restore torrent %q: %w", r.ID, err)
		}
	}
	return nil
}

func (c *Client) AddMagnet(ctx context.Context, magnet string) (app.Torrent, error) {
	if err := ctx.Err(); err != nil {
		return app.Torrent{}, err
	}
	spec, err := at.TorrentSpecFromMagnetUri(strings.TrimSpace(magnet))
	if err != nil {
		return app.Torrent{}, fmt.Errorf("parse magnet: %w", err)
	}
	return c.add(ctx, record{ID: specID(spec), Magnet: strings.TrimSpace(magnet)}, spec)
}

func (c *Client) AddTorrentFile(ctx context.Context, path string) (app.Torrent, error) {
	if err := ctx.Err(); err != nil {
		return app.Torrent{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return app.Torrent{}, fmt.Errorf("inspect torrent file %q: %w", path, err)
	}
	const maxMetainfoSize = 16 << 20
	if !fi.Mode().IsRegular() || fi.Size() > maxMetainfoSize {
		return app.Torrent{}, fmt.Errorf("torrent file must be a regular file of at most %d bytes", maxMetainfoSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return app.Torrent{}, fmt.Errorf("open torrent file %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: f}, maxMetainfoSize+1))
	if err != nil {
		return app.Torrent{}, fmt.Errorf("read torrent file %q: %w", path, err)
	}
	if len(data) > maxMetainfoSize {
		return app.Torrent{}, fmt.Errorf("torrent file exceeds %d bytes", maxMetainfoSize)
	}
	if err := ctx.Err(); err != nil {
		return app.Torrent{}, err
	}
	r := record{Metainfo: data}
	spec, err := parseRecord(r)
	if err != nil {
		return app.Torrent{}, fmt.Errorf("parse torrent file %q: %w", path, err)
	}
	r.ID = specID(spec)
	return c.add(ctx, r, spec)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (c *Client) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for !c.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("torrent client is closed")
	}
	return nil
}

func (c *Client) add(ctx context.Context, r record, spec *at.TorrentSpec) (app.Torrent, error) {
	if err := c.lock(ctx); err != nil {
		return app.Torrent{}, err
	}
	defer c.mu.Unlock()
	if !validID(r.ID) {
		return app.Torrent{}, fmt.Errorf("torrent requires a valid infohash")
	}
	if e, ok := c.entries[r.ID]; ok {
		return c.snapshot(e), nil
	}
	e, err := c.addLocked(ctx, r, spec, true)
	if err != nil {
		if e != nil {
			return c.snapshot(e), err
		}
		return app.Torrent{}, err
	}
	return c.snapshot(e), nil
}

func (c *Client) ensureDirectory(id string) error {
	if err := c.dataRoot.Mkdir(id, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create torrent data directory: %w", err)
	}
	fi, err := c.dataRoot.Lstat(id)
	if err != nil {
		return fmt.Errorf("inspect torrent data directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("torrent data directory %q is not a real directory", id)
	}
	return nil
}

func (c *Client) addLocked(ctx context.Context, r record, spec *at.TorrentSpec, persist bool) (*entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.ensureDirectory(r.ID); err != nil {
		return nil, err
	}
	store := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: c.dataDir, DisableMmap: true, Logger: c.logger,
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string {
			return filepath.Join(base, r.ID)
		},
	})
	spec.Storage = store
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
	spec.DeferStorageOpen = true
	t, fresh, err := c.engine.AddTorrentSpec(spec)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("add torrent to engine: %w", err)
	}
	if !fresh {
		_ = store.Close()
		for _, e := range c.entries {
			if e.torrent == t {
				return e, nil
			}
		}
		return nil, fmt.Errorf("torrent already exists in engine")
	}
	workerCtx, cancel := context.WithCancel(c.ctx)
	e := &entry{record: r, torrent: t, storage: store, cancel: cancel}
	c.entries[r.ID] = e
	var saveErr error
	if persist {
		saveErr = c.save(ctx)
		if saveErr != nil && !committed(saveErr) {
			delete(c.entries, r.ID)
			cancel()
			t.Drop()
			_ = store.Close()
			return nil, saveErr
		}
	}
	c.workers.Add(1)
	go c.metadata(workerCtx, e)
	return e, saveErr
}

func (c *Client) metadata(ctx context.Context, e *entry) {
	defer c.workers.Done()
	select {
	case <-ctx.Done():
		return
	case <-e.torrent.GotInfo():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || c.closed || c.entries[e.ID] != e {
		return
	}
	if len(e.Metainfo) == 0 {
		mi := e.torrent.Metainfo()
		var data bytes.Buffer
		if err := mi.Write(&data); err != nil {
			c.workerError(e, fmt.Errorf("encode acquired metainfo: %w", err))
			return
		}
		e.Metainfo = data.Bytes()
		if err := c.save(ctx); err != nil {
			if !committed(err) {
				e.Metainfo = nil
			}
			c.workerError(e, err)
			return
		}
	}
	if err := c.ensureDirectory(e.ID); err != nil {
		c.workerError(e, err)
		return
	}
	if err := e.torrent.OpenStorage(ctx, e.storage); err != nil {
		if ctx.Err() == nil {
			c.workerError(e, fmt.Errorf("open torrent storage: %w", err))
		}
		return
	}
	e.ready = true
	e.torrent.DownloadAll()
	c.setTransfers(e)
}

func (c *Client) workerError(e *entry, err error) {
	e.err = err
	c.logger.Error("torrent metadata/storage worker failed", "id", e.ID, "error", err)
}

func (c *Client) setTransfers(e *entry) {
	if e.Paused || !e.ready || e.err != nil {
		e.torrent.DisallowDataDownload()
		e.torrent.DisallowDataUpload()
		e.down, e.up = 0, 0
	} else {
		e.torrent.AllowDataDownload()
		e.torrent.AllowDataUpload()
	}
}

func validID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 40 && err == nil && strings.ToLower(id) == id
}

func (c *Client) find(id string) (*entry, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) == 0 || len(id) > 40 || strings.ContainsAny(id, "/\\.") {
		return nil, fmt.Errorf("invalid torrent ID %q", id)
	}
	var found *entry
	for key, e := range c.entries {
		if strings.HasPrefix(key, id) {
			if found != nil {
				return nil, fmt.Errorf("torrent ID prefix %q is ambiguous", id)
			}
			found = e
		}
	}
	if found == nil {
		return nil, fmt.Errorf("torrent %q not found", id)
	}
	return found, nil
}

func (c *Client) Torrents(ctx context.Context) ([]app.Torrent, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	out := make([]app.Torrent, 0, len(c.entries))
	for _, e := range c.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, c.snapshot(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Client) Torrent(ctx context.Context, id string) (app.Torrent, error) {
	if err := c.lock(ctx); err != nil {
		return app.Torrent{}, err
	}
	defer c.mu.Unlock()
	e, err := c.find(id)
	if err != nil {
		return app.Torrent{}, err
	}
	return c.snapshot(e), nil
}

func (c *Client) snapshot(e *entry) app.Torrent {
	t := e.torrent
	stats := t.Stats()
	now := time.Now()
	read, written := stats.BytesReadData.Int64(), stats.BytesWrittenData.Int64()
	if e.last.IsZero() {
		e.last, e.read, e.written = now, read, written
	} else if elapsed := now.Sub(e.last).Seconds(); elapsed >= 1 {
		e.down = math.Max(0, float64(read-e.read)/elapsed)
		e.up = math.Max(0, float64(written-e.written)/elapsed)
		e.last, e.read, e.written = now, read, written
	}
	out := app.Torrent{
		ID: e.ID, Name: t.Name(), Status: "metadata", Uploaded: written,
		DownloadRate: e.down, UploadRate: e.up, Peers: stats.ActivePeers,
		ETASeconds: -1, Files: make([]app.File, 0),
	}
	if t.Info() != nil {
		out.Size = t.Length()
		out.Downloaded = t.BytesCompleted()
		if out.Size > 0 {
			out.Progress = math.Min(1, float64(out.Downloaded)/float64(out.Size))
		}
		out.Status = "downloading"
		if e.ready && out.Downloaded >= out.Size {
			out.Status, out.Progress, out.ETASeconds = "seeding", 1, 0
		} else if out.DownloadRate > 0 {
			out.ETASeconds = int64(math.Ceil(float64(out.Size-out.Downloaded) / out.DownloadRate))
		}
		for _, f := range t.Files() {
			out.Files = append(out.Files, app.File{Path: f.Path(), Size: f.Length(), Downloaded: f.BytesCompleted()})
		}
	}
	if out.Downloaded > 0 {
		out.Ratio = float64(out.Uploaded) / float64(out.Downloaded)
	}
	if e.Paused {
		out.Status, out.DownloadRate, out.UploadRate = "paused", 0, 0
		out.ETASeconds = -1
	}
	if e.err != nil {
		out.Status, out.DownloadRate, out.UploadRate = "error", 0, 0
	}
	return out
}

func (c *Client) Pause(ctx context.Context, id string) error {
	return c.pause(ctx, id, true)
}

func (c *Client) Resume(ctx context.Context, id string) error {
	return c.pause(ctx, id, false)
}

func (c *Client) pause(ctx context.Context, id string, paused bool) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	e, err := c.find(id)
	if err != nil {
		return err
	}
	if e.err != nil && !paused {
		return fmt.Errorf("torrent cannot resume: %w", e.err)
	}
	old := e.Paused
	e.Paused = paused
	saveErr := c.save(ctx)
	if saveErr != nil && !committed(saveErr) {
		e.Paused = old
		return saveErr
	}
	c.setTransfers(e)
	return saveErr
}

func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	e, err := c.find(id)
	if err != nil {
		return err
	}
	delete(c.entries, e.ID)
	saveErr := c.save(ctx)
	if saveErr != nil && !committed(saveErr) {
		c.entries[e.ID] = e
		return saveErr
	}
	e.cancel()
	e.torrent.Drop()
	if err := e.storage.Close(); err != nil {
		return errors.Join(saveErr, fmt.Errorf("close removed torrent storage: %w", err))
	}
	if deleteData {
		if !validID(e.ID) {
			return fmt.Errorf("refusing to delete invalid torrent directory %q", e.ID)
		}
		if err := c.dataRoot.RemoveAll(e.ID); err != nil {
			return errors.Join(saveErr, fmt.Errorf("delete torrent data %q: %w", e.ID, err))
		}
	}
	return saveErr
}

// Close cancels and joins workers, then closes the network engine and storage.
// It is safe to call concurrently or more than once.
func (c *Client) Close() error {
	c.cancel()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	c.mu.Unlock()
	c.workers.Wait()
	errs := c.engine.Close()
	for _, e := range c.entries {
		if err := e.storage.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close torrent %q storage: %w", e.ID, err))
		}
	}
	if err := c.dataRoot.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close data directory: %w", err))
	}
	c.closeErr = errors.Join(errs...)
	close(c.closeDone)
	return c.closeErr
}
