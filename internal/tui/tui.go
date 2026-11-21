// Package tui provides an asynchronous terminal interface to app.Service.
package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/ChurroLoco/go-tor-cli/internal/app"
	"github.com/dustin/go-humanize"
)

// Run starts the fullscreen interface and blocks until quit or cancellation.
// Every service call receives a context canceled when the interface exits.
func Run(ctx context.Context, svc app.Service) error {
	if svc == nil {
		return errors.New("tui: nil service")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newModel(ctx, svc), tea.WithContext(ctx)).Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type screen uint8

const (
	listScreen screen = iota
	detailsScreen
	addScreen
	removeScreen
)

type statsMsg struct {
	seq      uint64
	torrents []app.Torrent
	err      error
}
type pollMsg struct{ seq uint64 }
type detailMsg struct {
	id      string
	torrent app.Torrent
	err     error
}
type operationMsg struct{ err error }

type bindings []key.Binding

func (b bindings) ShortHelp() []key.Binding { return b }
func (b bindings) FullHelp() [][]key.Binding {
	return [][]key.Binding{b[:3], b[3:6], b[6:]}
}

type model struct {
	ctx          context.Context
	svc          app.Service
	table        table.Model
	input        textinput.Model
	progress     progress.Model
	help         help.Model
	keys         bindings
	torrents     []app.Torrent
	selectedID   string
	detail       *app.Torrent
	detailBusy   bool
	screen       screen
	returnScreen screen
	removeID     string
	removeName   string
	deleteData   bool
	busy         bool
	err          error
	width        int
	height       int
	pollSeq      uint64
	polling      bool
	waiting      bool
	fileOffset   int
}

func newModel(ctx context.Context, svc app.Service) *model {
	m := &model{
		ctx: ctx, svc: svc, width: 80, height: 24,
		table: table.New(table.WithFocused(true)),
		input: textinput.New(), progress: progress.New(), help: help.New(),
		keys: bindings{
			key.NewBinding(key.WithKeys("↑", "↓", "j", "k"), key.WithHelp("↑/↓ j/k", "navigate")),
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details")),
			key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
			key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause/resume")),
			key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back/cancel")),
			key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
			key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "dismiss error")),
			key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q/ctrl+c", "quit")),
		},
	}
	m.input.Placeholder = "magnet:?… or /path/to/file.torrent"
	m.input.CharLimit = 0
	m.input.SetVirtualCursor(true)
	m.resize()
	return m
}

func (m *model) Init() tea.Cmd {
	if m.polling {
		return nil
	}
	return m.fetch()
}

func (m *model) fetch() tea.Cmd {
	m.polling, m.waiting = true, false
	m.pollSeq++
	ctx, svc, seq := m.ctx, m.svc, m.pollSeq
	return func() tea.Msg {
		if ctx.Err() != nil {
			return tea.Quit()
		}
		torrents, err := svc.Torrents(ctx)
		return statsMsg{seq, torrents, err}
	}
}

func (m *model) nextPoll() tea.Cmd {
	ctx, seq := m.ctx, m.pollSeq
	return func() tea.Msg {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return tea.Quit()
		case <-timer.C:
			return pollMsg{seq}
		}
	}
}

func (m *model) fetchDetails() tea.Cmd {
	if m.detailBusy || m.selectedID == "" {
		return nil
	}
	m.detailBusy = true
	ctx, svc, id := m.ctx, m.svc, m.selectedID
	return func() tea.Msg {
		if ctx.Err() != nil {
			return tea.Quit()
		}
		t, err := svc.Torrent(ctx, id)
		return detailMsg{id, t, err}
	}
}

func (m *model) operate(call func(context.Context) error) tea.Cmd {
	m.busy = true
	ctx := m.ctx
	return func() tea.Msg {
		if ctx.Err() != nil {
			return operationMsg{ctx.Err()}
		}
		return operationMsg{call(ctx)}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.ctx.Err() != nil {
		return m, tea.Quit
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.resize()
	case statsMsg:
		if !m.polling || m.waiting || msg.seq != m.pollSeq {
			return m, nil
		}
		m.waiting = true
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.setTorrents(msg.torrents)
		}
		cmd := m.nextPoll()
		if m.screen == detailsScreen && m.selectedID != "" {
			cmd = tea.Batch(cmd, m.fetchDetails())
		}
		return m, cmd
	case pollMsg:
		if m.waiting && msg.seq == m.pollSeq {
			return m, m.fetch()
		}
	case detailMsg:
		m.detailBusy = false
		if m.screen == detailsScreen && msg.id == m.selectedID {
			if msg.err != nil {
				m.err = msg.err
			} else {
				m.detail = &msg.torrent
			}
		}
	case operationMsg:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
		} else if m.screen == addScreen || m.screen == removeScreen {
			m.screen = listScreen
			m.input.Blur()
		}
	case tea.KeyPressMsg:
		k := msg.String()
		if k == "ctrl+c" || (k == "q" && m.screen != addScreen) {
			return m, tea.Quit
		}
		if m.screen == addScreen {
			switch k {
			case "esc":
				if !m.busy {
					m.screen = m.returnScreen
					m.input.Blur()
				}
				return m, nil
			case "enter":
				value := strings.TrimSpace(m.input.Value())
				if m.busy || value == "" {
					return m, nil
				}
				svc := m.svc
				return m, m.operate(func(ctx context.Context) error {
					var err error
					if strings.HasPrefix(strings.ToLower(value), "magnet:") {
						_, err = svc.AddMagnet(ctx, value)
					} else {
						_, err = svc.AddTorrentFile(ctx, value)
					}
					return err
				})
			}
			if !m.busy {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			return m, nil
		}
		if m.screen == removeScreen {
			switch k {
			case "esc", "n":
				if !m.busy {
					m.screen = m.returnScreen
				}
			case "tab", " ":
				if !m.busy {
					m.deleteData = !m.deleteData
				}
			case "y", "enter":
				if !m.busy {
					svc, id, deleteData := m.svc, m.removeID, m.deleteData
					return m, m.operate(func(ctx context.Context) error {
						return svc.Remove(ctx, id, deleteData)
					})
				}
			}
			return m, nil
		}
		switch k {
		case "?":
			m.help.ShowAll = !m.help.ShowAll
			m.resize()
		case "x":
			m.err = nil
		case "esc":
			if m.screen == detailsScreen {
				m.screen = listScreen
			}
		case "enter":
			if m.screen == detailsScreen {
				m.screen = listScreen
			} else if m.selectedID != "" {
				m.screen, m.fileOffset = detailsScreen, 0
				m.detail = nil
				return m, m.fetchDetails()
			}
		case "a":
			if !m.busy {
				m.returnScreen, m.screen = m.screen, addScreen
				m.input.Reset()
				return m, m.input.Focus()
			}
		case "d":
			if t := m.selected(); t != nil && !m.busy {
				m.returnScreen, m.screen = m.screen, removeScreen
				m.removeID, m.removeName, m.deleteData = t.ID, t.Name, false
			}
		case "p":
			if t := m.selected(); t != nil && !m.busy {
				svc, id, paused := m.svc, t.ID, strings.EqualFold(t.Status, "paused") || strings.EqualFold(t.Status, "stopped")
				return m, m.operate(func(ctx context.Context) error {
					if paused {
						return svc.Resume(ctx, id)
					}
					return svc.Pause(ctx, id)
				})
			}
		case "up", "k", "down", "j":
			if m.screen == detailsScreen {
				delta := 1
				if k == "up" || k == "k" {
					delta = -1
				}
				last := 0
				if t := m.selected(); t != nil {
					last = max(0, len(t.Files)-1)
				}
				if m.detail != nil && m.detail.ID == m.selectedID {
					last = max(0, len(m.detail.Files)-1)
				}
				m.fileOffset = max(0, min(last, m.fileOffset+delta))
			} else {
				m.table, _ = m.table.Update(msg)
				m.syncSelection()
			}
		}
	default:
		if m.screen == addScreen && !m.busy {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *model) selected() *app.Torrent {
	for i := range m.torrents {
		if m.torrents[i].ID == m.selectedID {
			return &m.torrents[i]
		}
	}
	return nil
}

func (m *model) syncSelection() {
	m.selectedID = ""
	if i := m.table.Cursor(); i >= 0 && i < len(m.torrents) {
		m.selectedID = m.torrents[i].ID
	}
}

func (m *model) setTorrents(torrents []app.Torrent) {
	cursor := m.table.Cursor()
	m.torrents = torrents
	for i, t := range torrents {
		if t.ID == m.selectedID {
			cursor = i
			break
		}
	}
	rows := make([]table.Row, 0, len(torrents))
	for _, t := range torrents {
		rows = append(rows, table.Row{safe(t.Name), safe(t.Status), fmt.Sprintf("%.1f%%", fraction(t.Progress)*100),
			bytes(t.Size), rate(t.DownloadRate), rate(t.UploadRate), fmt.Sprint(t.Peers), eta(t.ETASeconds)})
	}
	m.table.SetRows(rows)
	m.table.SetCursor(cursor)
	m.syncSelection()
	if m.detail != nil && m.detail.ID != m.selectedID {
		m.detail = nil
	}
	if m.selectedID == "" && m.screen == detailsScreen {
		m.screen = listScreen
	}
}

func (m *model) resize() {
	m.help.SetWidth(m.width)
	m.input.SetWidth(max(1, m.width-4))
	m.progress.SetWidth(max(1, min(48, m.width-2)))
	widths := []int{max(8, m.width-71), 10, 8, 8, 8, 8, 4, 9}
	budget := max(8, m.width-16)
	for {
		total, largest := 0, 0
		for i, width := range widths {
			total += width
			if width > widths[largest] {
				largest = i
			}
		}
		if total <= budget {
			break
		}
		widths[largest]--
	}
	titles := []string{"Name", "Status", "Progress", "Size", "DL", "UL", "Peers", "ETA"}
	columns := make([]table.Column, len(titles))
	for i, title := range titles {
		columns[i] = table.Column{Title: title, Width: widths[i]}
	}
	m.table.SetColumns(columns)
	m.table.SetWidth(m.width)
	m.table.SetHeight(max(2, m.height-5-lipgloss.Height(m.help.View(m.keys))-m.summaryHeight()))
}

func (m *model) View() tea.View {
	if m.width < 12 || m.height < 4 {
		quit := "q"
		if m.screen == addScreen {
			quit = "ctrl+c"
		}
		v := tea.NewView(lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(quit + ": quit • resize terminal"))
		v.AltScreen = true
		return v
	}
	var dl, ul float64
	var peers int
	for _, t := range m.torrents {
		dl += t.DownloadRate
		ul += t.UploadRate
		peers += t.Peers
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(
		fmt.Sprintf("gotor-cli  DL %s  UL %s  Torrents %d  Peers %d", rate(dl), rate(ul), len(m.torrents), peers))
	var body string
	footer := m.help.View(m.keys)
	switch m.screen {
	case addScreen:
		body = "Add magnet URI or torrent file\n" + m.input.View()
		footer = "enter:add esc:cancel\nctrl+c:quit"
	case removeScreen:
		mode := "preserve"
		if m.deleteData {
			mode = "DELETE"
		}
		body = fmt.Sprintf("Remove %s?\nData: %s", safe(m.removeName), mode)
		footer = "y/enter: confirm • n/esc: cancel • q: quit\ntab/space: toggle delete data"
		if m.width < 45 {
			footer = "y:yes n:no q:quit\ntab:delete toggle"
		}
	case detailsScreen:
		body = m.detailsView()
	default:
		if m.width < 45 || m.height < 8 {
			body = "Terminal too small; resize for torrent table."
			if t := m.selected(); t != nil {
				body += "\n" + safe(t.Name) + " " + safe(t.Status)
			}
		} else if len(m.torrents) == 0 {
			body = "No torrents. Press a to add a magnet or torrent file."
		} else {
			body = m.table.View()
			if m.summaryHeight() > 0 {
				body += "\n" + m.summaryView()
			}
		}
	}
	if m.busy {
		footer = "Working…\n" + footer
	}
	if m.err != nil {
		footer = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("Error: "+safe(m.err.Error())+" (x: dismiss)") + "\n" + footer
	}
	// Reserve the footer so errors and confirmation controls remain visible.
	footer = lipgloss.NewStyle().MaxHeight(max(1, m.height-2)).Render(footer)
	if available := m.height - 2 - lipgloss.Height(footer); available > 0 {
		body = lipgloss.NewStyle().MaxHeight(available).Render(body)
	} else {
		body = ""
	}
	content := header + "\n" + body + "\n" + footer
	v := tea.NewView(lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(content))
	v.AltScreen = true
	return v
}

func (m *model) summaryHeight() int {
	if m.height < 16 {
		return 0
	}
	return 8
}

func (m *model) summaryView() string {
	lines := strings.Split(m.detailsView(), "\n")
	if len(lines) > 4 {
		lines[4] = "Files (enter: expanded details):"
	}
	return strings.Join(lines[:min(7, len(lines))], "\n")
}

func (m *model) detailsView() string {
	t := m.selected()
	if m.screen == detailsScreen && m.detail != nil && m.detail.ID == m.selectedID {
		t = m.detail
	}
	if t == nil {
		return "Torrent no longer available."
	}
	lines := []string{
		safe(t.Name) + " • " + safe(t.Status),
		fmt.Sprintf("Size %s • Downloaded %s • Uploaded %s", bytes(t.Size), bytes(t.Downloaded), bytes(t.Uploaded)),
		fmt.Sprintf("Ratio %.2f • Progress %.1f%% • ETA %s • Peers %d", t.Ratio, fraction(t.Progress)*100, eta(t.ETASeconds), t.Peers),
		m.progress.ViewAs(fraction(t.Progress)), "Files (↑/↓ j/k scroll):",
	}
	offset := min(m.fileOffset, max(0, len(t.Files)-1))
	for _, f := range t.Files[offset:] {
		lines = append(lines, fmt.Sprintf("%s  %s / %s", safe(f.Path), bytes(f.Downloaded), bytes(f.Size)))
	}
	if len(t.Files) == 0 {
		lines = append(lines, "No file information yet.")
	}
	return strings.Join(lines, "\n")
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func bytes(n int64) string { return humanize.Bytes(uint64(max(0, n))) }
func rate(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		n = 0
	}
	n = min(n, float64(math.MaxInt64))
	return humanize.Bytes(uint64(n)) + "/s"
}
func fraction(n float64) float64 {
	if math.IsNaN(n) {
		return 0
	}
	return max(0, min(1, n))
}
func eta(seconds int64) string {
	if seconds < 0 {
		return "—"
	}
	if seconds == 0 {
		return "0s"
	}
	if seconds >= 86400 {
		return fmt.Sprintf("%dd %dh", seconds/86400, seconds%86400/3600)
	}
	return (time.Duration(seconds) * time.Second).String()
}
