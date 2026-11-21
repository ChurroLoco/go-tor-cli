package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
)

type fakeService struct {
	app.Service
	value      string
	deleteData bool
	err        error
}

func (s *fakeService) Torrents(ctx context.Context) ([]app.Torrent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []app.Torrent{{ID: "abc", Name: "sample"}}, s.err
}
func (s *fakeService) AddMagnet(_ context.Context, value string) (app.Torrent, error) {
	s.value = value
	return app.Torrent{ID: "abc"}, s.err
}
func (s *fakeService) AddTorrentFile(_ context.Context, value string) (app.Torrent, error) {
	s.value = value
	return app.Torrent{ID: "abc"}, s.err
}
func (s *fakeService) Torrent(_ context.Context, value string) (app.Torrent, error) {
	s.value = value
	return app.Torrent{ID: "abc"}, s.err
}
func (s *fakeService) Pause(_ context.Context, value string) error {
	s.value = value
	return s.err
}
func (s *fakeService) Resume(_ context.Context, value string) error {
	s.value = value
	return s.err
}
func (s *fakeService) Remove(_ context.Context, value string, deleteData bool) error {
	s.value, s.deleteData = value, deleteData
	return s.err
}

func TestLocalRPC(t *testing.T) {
	// Unix sockets have a short path limit; avoid the test runner's long temporary paths.
	dir, err := os.MkdirTemp("", "gotor-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{}
	shutdown := make(chan struct{}, 1)
	server := &http.Server{Handler: handler(svc, func() { shutdown <- struct{}{} })}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := NewClient(dir)
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := client.Torrents(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "abc" {
		t.Fatalf("items=%v error=%v", items, err)
	}
	if _, err := client.AddMagnet(ctx, "magnet:example"); err != nil || svc.value != "magnet:example" {
		t.Fatalf("value=%s error=%v", svc.value, err)
	}
	if _, err := client.AddTorrentFile(ctx, "example.torrent"); err != nil || !filepath.IsAbs(svc.value) {
		t.Fatalf("value=%s error=%v", svc.value, err)
	}
	if _, err := client.Torrent(ctx, "abc"); err != nil || svc.value != "abc" {
		t.Fatal(err)
	}
	for _, operation := range []func(context.Context, string) error{client.Pause, client.Resume} {
		if err := operation(ctx, "abc"); err != nil || svc.value != "abc" {
			t.Fatal(err)
		}
	}
	if err := client.Remove(ctx, "abc", true); err != nil || !svc.deleteData {
		t.Fatal(err)
	}
	svc.err = errors.New("not found")
	if _, err := client.Torrent(ctx, "missing"); err == nil || err.Error() != "not found" {
		t.Fatalf("error=%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.Torrents(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-shutdown
}

func TestHandlerRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/rpc", "", http.StatusNotFound},
		{"POST", "/other", `{}`, http.StatusNotFound},
		{"POST", "/rpc", `invalid`, http.StatusBadRequest},
		{"POST", "/rpc", `{"unknown":true}`, http.StatusBadRequest},
		{"POST", "/rpc", `{"operation":"invalid"}`, http.StatusBadRequest},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		handler(&fakeService{}, func() {}).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("request=%+v status=%d", tc, w.Code)
		}
	}
}
