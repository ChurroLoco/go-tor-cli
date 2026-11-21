package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/ChurroLoco/go-tor-cli/internal/app"
)

type serviceCall struct {
	method string
	value  string
	delete bool
	ctx    context.Context
}

type fakeService struct {
	mu       sync.Mutex
	calls    []serviceCall
	torrents []app.Torrent
	err      error
}

func (f *fakeService) record(ctx context.Context, method, value string, deleteData bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, serviceCall{method, value, deleteData, ctx})
}
func (f *fakeService) AddMagnet(ctx context.Context, value string) (app.Torrent, error) {
	f.record(ctx, "magnet", value, false)
	return app.Torrent{}, f.err
}
func (f *fakeService) AddTorrentFile(ctx context.Context, value string) (app.Torrent, error) {
	f.record(ctx, "file", value, false)
	return app.Torrent{}, f.err
}
func (f *fakeService) Torrents(ctx context.Context) ([]app.Torrent, error) {
	f.record(ctx, "list", "", false)
	return f.torrents, f.err
}
func (f *fakeService) Torrent(ctx context.Context, id string) (app.Torrent, error) {
	f.record(ctx, "detail", id, false)
	for _, torrent := range f.torrents {
		if torrent.ID == id {
			return torrent, f.err
		}
	}
	return app.Torrent{}, f.err
}
func (f *fakeService) Pause(ctx context.Context, id string) error {
	f.record(ctx, "pause", id, false)
	return f.err
}
func (f *fakeService) Resume(ctx context.Context, id string) error {
	f.record(ctx, "resume", id, false)
	return f.err
}
func (f *fakeService) Remove(ctx context.Context, id string, deleteData bool) error {
	f.record(ctx, "remove", id, deleteData)
	return f.err
}

func press(m *model, key string) tea.Cmd {
	code := rune(0)
	text := ""
	switch key {
	case "enter":
		code = tea.KeyEnter
	case "esc":
		code = tea.KeyEscape
	case "tab":
		code = tea.KeyTab
	case "up":
		code = tea.KeyUp
	case "down":
		code = tea.KeyDown
	default:
		code = []rune(key)[0]
		text = key
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: text})
	return cmd
}

func TestPollingIsAsynchronousAndSingleChain(t *testing.T) {
	svc := &fakeService{torrents: []app.Torrent{{ID: "a", Name: "one"}}}
	m := newModel(context.Background(), svc)
	cmd := m.Init()
	if len(svc.calls) != 0 || cmd == nil || m.Init() != nil {
		t.Fatal("Init must schedule exactly one asynchronous fetch")
	}
	msg := cmd().(statsMsg)
	if len(svc.calls) != 1 {
		t.Fatal("fetch command did not call service")
	}
	_, next := m.Update(msg)
	if next == nil || !m.waiting || len(svc.calls) != 1 {
		t.Fatal("stats must schedule a timer, not call service")
	}
	if _, duplicate := m.Update(msg); duplicate != nil {
		t.Fatal("duplicate stats spawned another polling chain")
	}
	_, fetch := m.Update(pollMsg{msg.seq})
	if fetch == nil || len(svc.calls) != 1 || m.waiting {
		t.Fatal("timer must schedule an asynchronous fetch")
	}
	if _, duplicate := m.Update(pollMsg{msg.seq}); duplicate != nil {
		t.Fatal("duplicate timer spawned another fetch")
	}
	m.Update(fetch())
	if len(svc.calls) != 2 {
		t.Fatal("second fetch did not execute")
	}
}

func TestRefreshKeepsIDAndRetainsErrors(t *testing.T) {
	m := newModel(context.Background(), &fakeService{})
	m.setTorrents([]app.Torrent{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	press(m, "j")
	if m.selectedID != "b" {
		t.Fatal("j did not move selection")
	}
	m.setTorrents([]app.Torrent{{ID: "c"}, {ID: "a"}, {ID: "b"}})
	if m.selectedID != "b" || m.table.Cursor() != 2 {
		t.Fatal("refresh lost ID selection")
	}
	m.setTorrents([]app.Torrent{{ID: "c"}})
	if m.selectedID != "c" {
		t.Fatal("removed selection must fall back to a valid row")
	}
	m.Init()
	failure := errors.New("offline")
	m.Update(statsMsg{seq: m.pollSeq, err: failure})
	if m.err != failure || m.selectedID != "c" {
		t.Fatal("failed refresh must retain last stats and error")
	}
	m.Update(pollMsg{m.pollSeq})
	m.Update(statsMsg{seq: m.pollSeq, torrents: []app.Torrent{{ID: "c"}}})
	if m.err != failure {
		t.Fatal("successful refresh must not erase an unacknowledged error")
	}
	press(m, "x")
	if m.err != nil {
		t.Fatal("error dismissal failed")
	}
	m.setTorrents(nil)
	if m.selectedID != "" {
		t.Fatal("empty list retained selection")
	}
}

func TestAddCommandsAndErrorRecovery(t *testing.T) {
	for _, tc := range []struct{ value, method string }{
		{"magnet:?xt=urn:btih:abc", "magnet"},
		{"/data/test.torrent", "file"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			svc := &fakeService{}
			m := newModel(context.Background(), svc)
			press(m, "a")
			if m.screen != addScreen || !m.input.Focused() {
				t.Fatal("add did not focus input")
			}
			if press(m, "enter") != nil {
				t.Fatal("empty input should not add")
			}
			press(m, "q")
			if m.input.Value() != "q" {
				t.Fatal("q must be usable in a magnet/file path")
			}
			m.input.SetValue("  " + tc.value + "  ")
			cmd := press(m, "enter")
			if cmd == nil || !m.busy || len(svc.calls) != 0 || press(m, "enter") != nil {
				t.Fatal("add should schedule exactly one command")
			}
			failure := errors.New("add failed")
			svc.err = failure
			m.Update(cmd())
			if m.screen != addScreen || m.busy || m.err != failure || m.input.Value() == "" {
				t.Fatal("failed add should retain input for retry")
			}
			svc.err = nil
			m.Update(press(m, "enter")())
			if m.screen != listScreen || m.input.Focused() {
				t.Fatal("successful add should return to list")
			}
			for _, call := range svc.calls {
				if call.method != tc.method || call.value != tc.value {
					t.Fatalf("wrong add service call: %+v", call)
				}
			}
		})
	}
}

func TestPauseResumeAndRemovalCaptureSelection(t *testing.T) {
	for _, status := range []string{"downloading", "paused", "stopped"} {
		t.Run(status, func(t *testing.T) {
			svc := &fakeService{}
			m := newModel(context.Background(), svc)
			m.setTorrents([]app.Torrent{{ID: "a", Status: status}, {ID: "b"}})
			cmd := press(m, "p")
			press(m, "j")
			if len(svc.calls) != 0 {
				t.Fatal("Update performed an engine operation")
			}
			m.Update(cmd())
			want := "resume"
			if status == "downloading" {
				want = "pause"
			}
			if svc.calls[0].method != want || svc.calls[0].value != "a" {
				t.Fatal("pause/resume must capture ID when key was pressed")
			}
		})
	}
	for _, deleteData := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "delete"}[deleteData], func(t *testing.T) {
			svc := &fakeService{}
			m := newModel(context.Background(), svc)
			m.setTorrents([]app.Torrent{{ID: "a"}, {ID: "b"}})
			press(m, "d")
			if m.screen != removeScreen || m.deleteData || len(svc.calls) != 0 {
				t.Fatal("remove must request confirmation and default to preserve")
			}
			if deleteData {
				press(m, "tab")
			}
			m.setTorrents([]app.Torrent{{ID: "b"}})
			m.Update(press(m, "y")())
			call := svc.calls[0]
			if call.method != "remove" || call.value != "a" || call.delete != deleteData {
				t.Fatalf("remove must retain confirmed ID and toggle: %+v", call)
			}
			press(m, "d")
			if m.deleteData {
				t.Fatal("delete toggle must reset each confirmation")
			}
			press(m, "n")
			if m.screen != listScreen || len(svc.calls) != 1 {
				t.Fatal("cancel removal called service")
			}
		})
	}
}

func TestDetailsAreAsyncIgnoreStaleAndDoNotOverlap(t *testing.T) {
	svc := &fakeService{torrents: []app.Torrent{
		{ID: "a", Name: "Alpha", Files: []app.File{{Path: "movie", Size: 100}}},
		{ID: "b", Name: "Beta"},
	}}
	m := newModel(context.Background(), svc)
	m.setTorrents(svc.torrents)
	press(m, "esc")
	if m.screen != listScreen {
		t.Fatal("escape on list must not open details")
	}
	cmd := press(m, "enter")
	if m.screen != detailsScreen || cmd == nil || len(svc.calls) != 0 {
		t.Fatal("details must load asynchronously")
	}
	if m.fetchDetails() != nil {
		t.Fatal("details requests must not overlap")
	}
	msg := cmd()
	m.Update(msg)
	if m.detail == nil || !strings.Contains(m.View().Content, "movie") {
		t.Fatal("detail result did not render files")
	}
	press(m, "esc")
	press(m, "j")
	m.Update(msg)
	if m.selectedID != "b" || (m.detail != nil && m.detail.ID == "b") {
		t.Fatal("stale details changed selection")
	}
	press(m, "enter")
	m.Update(detailMsg{id: "a", torrent: svc.torrents[0]})
	if m.detail != nil {
		t.Fatal("stale detail response applied to another ID")
	}
}

func TestResizeAndViews(t *testing.T) {
	m := newModel(context.Background(), &fakeService{})
	m.setTorrents([]app.Torrent{{ID: "a", Name: strings.Repeat("界", 100), Status: "downloading",
		Size: 1000, Downloaded: 500, Uploaded: 100, Progress: .5, DownloadRate: 12, UploadRate: 3,
		Peers: 4, ETASeconds: 60, Ratio: .2, Files: []app.File{{Path: "first"}, {Path: "second"}}}})
	press(m, "?")
	if !m.help.ShowAll {
		t.Fatal("help toggle failed")
	}
	m.screen = listScreen
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, want := range []string{"gotor-cli", "Downloaded", "Uploaded", "Ratio", "Progress", "ETA", "Peers", "Files", "first", "second"} {
		if !strings.Contains(m.View().Content, want) {
			t.Errorf("missing selected summary %q", want)
		}
	}
	m.detail = &app.Torrent{ID: "a", Name: "old details", Progress: .1}
	if strings.Contains(m.summaryView(), "old details") {
		t.Fatal("list summary used stale expanded detail statistics")
	}
	m.detail = nil
	for _, size := range [][2]int{{120, 40}, {80, 24}, {45, 8}, {20, 6}, {1, 1}, {0, 0}} {
		for _, screen := range []screen{listScreen, detailsScreen, addScreen, removeScreen} {
			m.screen = screen
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			if !view.AltScreen || lipgloss.Width(view.Content) > max(1, size[0]) || lipgloss.Height(view.Content) > max(1, size[1]) {
				t.Fatalf("view exceeded terminal for %v screen %d: width=%d height=%d", size, screen, lipgloss.Width(view.Content), lipgloss.Height(view.Content))
			}
		}
	}
	m.screen = detailsScreen
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := m.View().Content
	for _, want := range []string{"Downloaded", "Uploaded", "Ratio", "Progress", "ETA", "Peers", "Files", "first", "second"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing details %q", want)
		}
	}
	press(m, "j")
	if strings.Contains(m.detailsView(), "first") {
		t.Fatal("details file scrolling failed")
	}
	for range 5 {
		press(m, "j")
	}
	press(m, "k")
	if !strings.Contains(m.detailsView(), "first") {
		t.Fatal("file scrolling must clamp at the last file")
	}
	m.screen = removeScreen
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	for _, want := range []string{"y:yes", "n:no", "tab:delete", "Data: preserve"} {
		if !strings.Contains(m.View().Content, want) {
			t.Errorf("small-screen confirmation missing %q: %s", want, m.View().Content)
		}
	}
}

func TestContextCancellationStopsCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &fakeService{}
	m := newModel(ctx, svc)
	fetch := m.Init()
	wait := m.nextPoll()
	cancel()
	if _, ok := fetch().(tea.QuitMsg); !ok || len(svc.calls) != 0 {
		t.Fatal("canceled fetch should quit without calling service")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- wait() }()
	select {
	case msg := <-done:
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatal("canceled poll timer did not quit")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("poll command failed to observe cancellation promptly")
	}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Update failed to quit on cancellation")
	}
	if err := Run(ctx, svc); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run canceled context: %v", err)
	}
	if err := Run(context.Background(), nil); err == nil {
		t.Fatal("Run accepted nil service")
	}
}

func TestFormattingIsSafe(t *testing.T) {
	if safe("hello\x1b[2J\nworld") != "hello [2J world" {
		t.Fatal("control characters were not sanitized")
	}
	want := []string{"—", "0s", "1m0s", "2d 1h"}
	var got []string
	for _, seconds := range []int64{-1, 0, 60, 176400} {
		got = append(got, eta(seconds))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ETA: %v, want %v", got, want)
	}
}
