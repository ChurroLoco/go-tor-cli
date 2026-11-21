package app

import (
	"context"
	"fmt"
)

// NewService separates the application's service boundary from its backend.
func NewService(backend Service) Service {
	return &service{backend: backend}
}

type service struct {
	backend Service
}

func (s *service) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.backend == nil {
		return fmt.Errorf("torrent backend is unavailable")
	}
	return nil
}

func (s *service) AddMagnet(ctx context.Context, magnet string) (Torrent, error) {
	if err := s.ready(ctx); err != nil {
		return Torrent{}, err
	}
	t, err := s.backend.AddMagnet(ctx, magnet)
	if err != nil {
		return t, fmt.Errorf("add magnet: %w", err)
	}
	return t, nil
}

func (s *service) AddTorrentFile(ctx context.Context, path string) (Torrent, error) {
	if err := s.ready(ctx); err != nil {
		return Torrent{}, err
	}
	t, err := s.backend.AddTorrentFile(ctx, path)
	if err != nil {
		return t, fmt.Errorf("add torrent file %q: %w", path, err)
	}
	return t, nil
}

func (s *service) Torrents(ctx context.Context) ([]Torrent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	torrents, err := s.backend.Torrents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list torrents: %w", err)
	}
	return torrents, nil
}

func (s *service) Torrent(ctx context.Context, id string) (Torrent, error) {
	if err := s.ready(ctx); err != nil {
		return Torrent{}, err
	}
	t, err := s.backend.Torrent(ctx, id)
	if err != nil {
		return t, fmt.Errorf("get torrent %q: %w", id, err)
	}
	return t, nil
}

func (s *service) Pause(ctx context.Context, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := s.backend.Pause(ctx, id); err != nil {
		return fmt.Errorf("pause torrent %q: %w", id, err)
	}
	return nil
}

func (s *service) Resume(ctx context.Context, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := s.backend.Resume(ctx, id); err != nil {
		return fmt.Errorf("resume torrent %q: %w", id, err)
	}
	return nil
}

func (s *service) Remove(ctx context.Context, id string, deleteData bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := s.backend.Remove(ctx, id, deleteData); err != nil {
		return fmt.Errorf("remove torrent %q: %w", id, err)
	}
	return nil
}
