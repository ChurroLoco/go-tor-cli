package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	"github.com/ChurroLoco/go-tor-cli/internal/config"
)

type fakeService struct {
	app.Service
	items      []app.Torrent
	err        error
	operation  string
	value      string
	deleteData bool
}

func (s *fakeService) Torrents(context.Context) ([]app.Torrent, error) {
	return s.items, s.err
}
func (s *fakeService) Torrent(_ context.Context, id string) (app.Torrent, error) {
	s.value = id
	return app.Torrent{ID: id, Name: "example"}, s.err
}
func (s *fakeService) AddMagnet(_ context.Context, value string) (app.Torrent, error) {
	s.operation, s.value = "magnet", value
	return app.Torrent{ID: "abc", Name: "example"}, s.err
}
func (s *fakeService) AddTorrentFile(_ context.Context, value string) (app.Torrent, error) {
	s.operation, s.value = "file", value
	return app.Torrent{ID: "abc", Name: "example"}, s.err
}
func (s *fakeService) Pause(_ context.Context, id string) error {
	s.operation, s.value = "pause", id
	return s.err
}
func (s *fakeService) Resume(_ context.Context, id string) error {
	s.operation, s.value = "resume", id
	return s.err
}
func (s *fakeService) Remove(_ context.Context, id string, deleteData bool) error {
	s.operation, s.value, s.deleteData = "remove", id, deleteData
	return s.err
}

func execute(t *testing.T, svc *fakeService, args ...string) (string, error, bool) {
	t.Helper()
	closed := false
	root := newRoot(config.Config{StateDir: t.TempDir(), DataDir: t.TempDir()},
		func(ctx context.Context, cfg config.Config) (app.Service, func(), error) {
			if ctx == nil || cfg.StateDir == "" {
				t.Fatal("missing context or configuration")
			}
			return svc, func() { closed = true }, nil
		},
		func(context.Context, app.Service) error {
			svc.operation = "ui"
			return nil
		})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err, closed
}

func TestJSONList(t *testing.T) {
	for _, items := range [][]app.Torrent{nil, {{ID: "abc", Name: "sample", Progress: .5}}} {
		out, err, closed := execute(t, &fakeService{items: items}, "list", "--json")
		if err != nil || !closed {
			t.Fatalf("error=%v closed=%v", err, closed)
		}
		var got []app.Torrent
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(items) || strings.TrimSpace(out) == "null" {
			t.Fatalf("unexpected list %s", out)
		}
	}
}

func TestCommands(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		operation  string
		value      string
		deleteData bool
	}{
		{[]string{"add", "magnet:?xt=urn:btih:abc", "--json"}, "magnet", "magnet:?xt=urn:btih:abc", false},
		{[]string{"add", "example.torrent"}, "file", "example.torrent", false},
		{[]string{"pause", "abc"}, "pause", "abc", false},
		{[]string{"resume", "abc"}, "resume", "abc", false},
		{[]string{"remove", "abc"}, "remove", "abc", false},
		{[]string{"remove", "abc", "--delete-data", "--json"}, "remove", "abc", true},
		{nil, "ui", "", false},
		{[]string{"ui"}, "ui", "", false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			svc := &fakeService{}
			_, err, closed := execute(t, svc, tc.args...)
			if err != nil || !closed || svc.operation != tc.operation || svc.value != tc.value || svc.deleteData != tc.deleteData {
				t.Fatalf("error=%v closed=%v service=%+v", err, closed, svc)
			}
		})
	}
}

func TestStatusAndErrors(t *testing.T) {
	out, err, _ := execute(t, &fakeService{}, "status", "abc", "--json")
	var got app.Torrent
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.ID != "abc" {
		t.Fatalf("output=%q error=%v", out, err)
	}
	sentinel := errors.New("backend unavailable")
	_, err, closed := execute(t, &fakeService{err: sentinel}, "pause", "abc")
	if !errors.Is(err, sentinel) || !closed {
		t.Fatalf("error=%v closed=%v", err, closed)
	}
	for _, args := range [][]string{{"add"}, {"status"}, {"remove"}, {"list", "extra"}, {"ui", "--json"}} {
		_, err, closed := execute(t, &fakeService{}, args...)
		if err == nil || closed {
			t.Fatalf("args=%v error=%v closed=%v", args, err, closed)
		}
	}
}

func TestPlainOutputSanitizesTorrentNames(t *testing.T) {
	out, err, _ := execute(t, &fakeService{items: []app.Torrent{{Name: "\x1b[31mname\nforged\trow\u009b31m"}}}, "list")
	if err != nil || strings.ContainsAny(out, "\x1b\u009b") || strings.Contains(out, "\nforged") {
		t.Fatalf("output=%q error=%v", out, err)
	}
}
