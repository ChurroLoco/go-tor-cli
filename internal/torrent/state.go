package torrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type record struct {
	ID       string `json:"id"`
	Magnet   string `json:"magnet,omitempty"`
	Metainfo []byte `json:"metainfo,omitempty"`
	Paused   bool   `json:"paused"`
}

type state struct {
	Version  int      `json:"version"`
	Torrents []record `json:"torrents"`
}

// A rename commits state even if syncing the containing directory then fails.
type committedError struct{ error }

func (e *committedError) Unwrap() error { return e.error }

func committed(err error) bool {
	var e *committedError
	return errors.As(err, &e)
}

func (c *Client) save(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := state{Version: 1, Torrents: make([]record, 0, len(c.entries))}
	for _, e := range c.entries {
		s.Torrents = append(s.Torrents, e.record)
	}
	sort.Slice(s.Torrents, func(i, j int) bool { return s.Torrents[i].ID < s.Torrents[j].ID })
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode torrent state: %w", err)
	}
	if err := atomicWrite(ctx, c.statePath, data); err != nil {
		return fmt.Errorf("save torrent state: %w", err)
	}
	return nil
}

func atomicWrite(ctx context.Context, path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".torrent-state-*")
	if err != nil {
		return fmt.Errorf("create atomic state file: %w", err)
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write atomic state file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync atomic state file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close atomic state file: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return &committedError{fmt.Errorf("open state directory for sync: %w", err)}
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return &committedError{fmt.Errorf("sync state directory: %w", err)}
	}
	return nil
}
