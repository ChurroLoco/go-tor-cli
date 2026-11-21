package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeBackend struct {
	call       string
	arg        string
	deleteData bool
	ctx        context.Context
	err        error
	result     Torrent
}

func (b *fakeBackend) called(ctx context.Context, call, arg string) {
	b.ctx, b.call, b.arg = ctx, call, arg
}
func (b *fakeBackend) AddMagnet(ctx context.Context, arg string) (Torrent, error) {
	b.called(ctx, "magnet", arg)
	return b.result, b.err
}
func (b *fakeBackend) AddTorrentFile(ctx context.Context, arg string) (Torrent, error) {
	b.called(ctx, "file", arg)
	return b.result, b.err
}
func (b *fakeBackend) Torrents(ctx context.Context) ([]Torrent, error) {
	b.called(ctx, "list", "")
	return []Torrent{b.result}, b.err
}
func (b *fakeBackend) Torrent(ctx context.Context, arg string) (Torrent, error) {
	b.called(ctx, "get", arg)
	return b.result, b.err
}
func (b *fakeBackend) Pause(ctx context.Context, arg string) error {
	b.called(ctx, "pause", arg)
	return b.err
}
func (b *fakeBackend) Resume(ctx context.Context, arg string) error {
	b.called(ctx, "resume", arg)
	return b.err
}
func (b *fakeBackend) Remove(ctx context.Context, arg string, deleteData bool) error {
	b.called(ctx, "remove", arg)
	b.deleteData = deleteData
	return b.err
}

func serviceCalls() map[string]func(context.Context, Service) error {
	return map[string]func(context.Context, Service) error{
		"magnet": func(ctx context.Context, s Service) error { _, err := s.AddMagnet(ctx, "input"); return err },
		"file":   func(ctx context.Context, s Service) error { _, err := s.AddTorrentFile(ctx, "input"); return err },
		"list":   func(ctx context.Context, s Service) error { _, err := s.Torrents(ctx); return err },
		"get":    func(ctx context.Context, s Service) error { _, err := s.Torrent(ctx, "input"); return err },
		"pause":  func(ctx context.Context, s Service) error { return s.Pause(ctx, "input") },
		"resume": func(ctx context.Context, s Service) error { return s.Resume(ctx, "input") },
		"remove": func(ctx context.Context, s Service) error { return s.Remove(ctx, "input", true) },
	}
}

func TestServiceDelegatesAndWrapsErrors(t *testing.T) {
	sentinel := errors.New("backend failure")
	for name, call := range serviceCalls() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := &fakeBackend{}
			s := NewService(b)
			if err := call(ctx, s); err != nil {
				t.Fatal(err)
			}
			if b.call != name || b.ctx != ctx {
				t.Fatalf("backend got %q, context %v", b.call, b.ctx)
			}
			if name != "list" && b.arg != "input" {
				t.Fatalf("backend argument = %q", b.arg)
			}
			if name == "remove" && !b.deleteData {
				t.Fatal("delete-data flag not delegated")
			}
			b.err = sentinel
			if err := call(ctx, s); !errors.Is(err, sentinel) {
				t.Fatalf("backend error lost: %v", err)
			}
		})
	}
}

func TestServiceCancellationAndMissingBackend(t *testing.T) {
	for name, call := range serviceCalls() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			b := &fakeBackend{}
			if err := call(ctx, NewService(b)); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled operation returned %v", err)
			}
			if b.call != "" {
				t.Fatal("canceled operation reached backend")
			}
			if err := call(context.Background(), NewService(nil)); err == nil {
				t.Fatal("missing backend should fail")
			}
		})
	}
}

func TestServicePreservesSnapshots(t *testing.T) {
	want := Torrent{ID: "id", Name: "name", Files: []File{{Path: "file", Size: 7}}}
	s := NewService(&fakeBackend{result: want})
	ctx := context.Background()
	for _, call := range []func() (Torrent, error){
		func() (Torrent, error) { return s.AddMagnet(ctx, "magnet") },
		func() (Torrent, error) { return s.AddTorrentFile(ctx, "file") },
		func() (Torrent, error) { return s.Torrent(ctx, "id") },
	} {
		got, err := call()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("snapshot = %#v, %v", got, err)
		}
	}
	got, err := s.Torrents(ctx)
	if err != nil || !reflect.DeepEqual(got, []Torrent{want}) {
		t.Fatalf("snapshots = %#v, %v", got, err)
	}
}
