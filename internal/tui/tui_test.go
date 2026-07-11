package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel(cfg Config) (uiModel, *UI) {
	u := New()
	m := newModel(u, cfg)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return mm.(uiModel), u
}

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// Submitting a task runs Ask in a goroutine, transcript gets the reply
// chunks, and doneMsg clears busy + saves the session.
func TestTaskFlow(t *testing.T) {
	saved := false
	u := New()
	cfg := Config{
		ModelName: "ollama:test",
		Ask: func(ctx context.Context, task string) (string, error) {
			u.Writer().Write([]byte("tool output line\n"))
			return "done", nil
		},
		Usage:       func() (int, int, float64) { return 10, 2, 0 },
		SaveSession: func() { saved = true },
	}
	m := newModel(u, cfg)
	mm0, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm0.(uiModel)
	mm, _ := m.Update(key("fix the bug"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	if !m.busy {
		t.Fatal("submit must set busy")
	}
	// Drain: chunk then done arrive on the events channel.
	for i := 0; i < 4 && m.busy; i++ {
		select {
		case ev := <-u.events:
			mm, _ = m.Update(ev)
			m = mm.(uiModel)
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
		}
	}
	if m.busy {
		t.Fatal("doneMsg must clear busy")
	}
	if !strings.Contains(m.buf.String(), "tool output line") {
		t.Fatal("agent output missing from transcript")
	}
	if !saved {
		t.Fatal("session not saved on done")
	}
}

// The permission round-trip: agent blocks until y/n lands in the TUI.
func TestPermissionRoundTrip(t *testing.T) {
	m, u := newTestModel(Config{ModelName: "m",
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	got := make(chan bool, 1)
	go func() { got <- u.Permit("run: rm -rf build") }()
	ev := <-u.events
	mm, _ := m.Update(ev)
	m = mm.(uiModel)
	if m.perm == nil {
		t.Fatal("permission prompt not shown")
	}
	mm, _ = m.Update(key("y"))
	m = mm.(uiModel)
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("y must grant")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent still blocked after answer")
	}
	if m.perm != nil {
		t.Fatal("prompt must clear")
	}
}

// /model N resolves against installed models; bad numbers error cleanly.
func TestModelSwitchCommand(t *testing.T) {
	var switched string
	m, _ := newTestModel(Config{ModelName: "m",
		Usage:       func() (int, int, float64) { return 0, 0, 0 },
		SwitchModel: func(spec string) error { switched = spec; return nil }})
	mm, _ := m.command("/model claude:claude-sonnet-5")
	m = mm.(uiModel)
	if switched != "claude:claude-sonnet-5" || m.model != switched {
		t.Fatalf("switch failed: %q", switched)
	}
}

// A busy session must reject a second task instead of interleaving.
func TestNoConcurrentTasks(t *testing.T) {
	block := make(chan struct{})
	m, _ := newTestModel(Config{ModelName: "m",
		Ask:   func(ctx context.Context, task string) (string, error) { <-block; return "", nil },
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	mm, _ := m.Update(key("first"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("second"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	if !strings.Contains(m.buf.String(), "already running") {
		t.Fatal("second task must be rejected while busy")
	}
	close(block)
}

// A permission request with a detail block must render the preview in the
// transcript (diff lines styled) before the y/n prompt.
func TestPermissionDetailPreview(t *testing.T) {
	m, u := newTestModel(Config{ModelName: "m",
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	got := make(chan bool, 1)
	go func() { got <- u.PermitDetail("edit a.go", "- old line\n+ new line") }()
	ev := <-u.events
	mm, _ := m.Update(ev)
	m = mm.(uiModel)
	if !strings.Contains(m.buf.String(), "proposed: edit a.go") {
		t.Fatal("preview header missing")
	}
	if !strings.Contains(m.buf.String(), "old line") || !strings.Contains(m.buf.String(), "new line") {
		t.Fatal("diff body missing from transcript")
	}
	mm, _ = m.Update(key("n"))
	m = mm.(uiModel)
	if ok := <-got; ok {
		t.Fatal("n must deny")
	}
}

// Ctrl+C while busy must cancel the running task (ctx fires), keep the
// program alive, and return to idle when doneMsg lands.
func TestCtrlCCancelsTask(t *testing.T) {
	started := make(chan struct{})
	m, u := newTestModel(Config{ModelName: "m",
		Ask: func(ctx context.Context, task string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	mm, _ := m.Update(key("long task"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	<-started
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = mm.(uiModel)
	select {
	case ev := <-u.events:
		mm, _ = m.Update(ev)
		m = mm.(uiModel)
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock the task")
	}
	if m.busy {
		t.Fatal("busy must clear after cancelled task completes")
	}
}

// A single idle Ctrl+C must warn, not quit; the second within the window quits.
func TestIdleCtrlCNeedsDoublePress(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m",
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = mm.(uiModel)
	if cmd != nil {
		t.Fatal("first idle Ctrl+C must not quit")
	}
	if !strings.Contains(m.buf.String(), "again to exit") {
		t.Fatal("no double-press hint shown")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("second Ctrl+C must quit")
	}
}

// Long input must wrap: the textarea grows (up to a cap) instead of
// scrolling horizontally, and resets to one line after submit.
func TestInputWrapsAndGrows(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m",
		Ask:   func(ctx context.Context, task string) (string, error) { return "ok", nil },
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	long := strings.Repeat("refactor the data collection layer and add tests ", 8)
	mm, _ := m.Update(key(long))
	m = mm.(uiModel)
	if m.in.Height() < 2 {
		t.Fatalf("input did not grow for wrapped text: height=%d", m.in.Height())
	}
	if m.in.Height() > 6 {
		t.Fatalf("input grew past cap: %d", m.in.Height())
	}
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	if m.in.Height() != 1 {
		t.Fatalf("input not reset after submit: height=%d", m.in.Height())
	}
	if m.in.Value() != "" {
		t.Fatal("input not cleared after submit")
	}
}

// Ctrl+J inserts a newline instead of submitting.
func TestCtrlJNewline(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m",
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	mm, _ := m.Update(key("first"))
	m = mm.(uiModel)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m = mm.(uiModel)
	mm, _ = m.Update(key("second"))
	m = mm.(uiModel)
	if !strings.Contains(m.in.Value(), "first\nsecond") {
		t.Fatalf("ctrl+j did not insert newline: %q", m.in.Value())
	}
	if m.busy {
		t.Fatal("ctrl+j must not submit")
	}
}

// Activity narration: agent status updates land in the status line while
// busy and clear when the task completes.
func TestActivityNarration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	u := New()
	cfg := Config{ModelName: "m",
		Ask: func(ctx context.Context, task string) (string, error) {
			u.Status("running: go test ./...")
			close(started)
			<-release
			return "ok", nil
		},
		Usage: func() (int, int, float64) { return 21948, 418, 0 }}
	m := newModel(u, cfg)
	mm0, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm0.(uiModel)
	mm, _ := m.Update(key("do it"))
	m = mm.(uiModel)
	mm, _ = m.Update(key("enter"))
	m = mm.(uiModel)
	<-started
	// drain the status event
	ev := <-u.events
	mm, _ = m.Update(ev)
	m = mm.(uiModel)
	line := m.statusLine()
	if !strings.Contains(line, "running: go test") {
		t.Fatalf("activity missing from status line: %q", line)
	}
	if !strings.Contains(line, "21.9k↑") || !strings.Contains(line, "418↓") {
		t.Fatalf("compact token counts missing: %q", line)
	}
	close(release)
	ev = <-u.events // doneMsg
	mm, _ = m.Update(ev)
	m = mm.(uiModel)
	if m.activity != "" {
		t.Fatal("activity not cleared on done")
	}
	if !strings.Contains(m.statusLine(), "ready") {
		t.Fatalf("status not idle after done: %q", m.statusLine())
	}
}

// 'a' at a prompt grants and switches to blanket approval; subsequent
// permission requests auto-approve without a prompt.
func TestAlwaysApprove(t *testing.T) {
	m, u := newTestModel(Config{ModelName: "m",
		Usage: func() (int, int, float64) { return 0, 0, 0 }})
	got := make(chan bool, 1)
	go func() { got <- u.Permit("run: make build") }()
	ev := <-u.events
	mm, _ := m.Update(ev)
	m = mm.(uiModel)
	mm, _ = m.Update(key("a"))
	m = mm.(uiModel)
	if ok := <-got; !ok {
		t.Fatal("'a' must grant")
	}
	if !m.autoApprove {
		t.Fatal("'a' must enable blanket approval")
	}
	// second request: no prompt, auto-granted
	go func() { got <- u.Permit("run: make test") }()
	ev = <-u.events
	mm, _ = m.Update(ev)
	m = mm.(uiModel)
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("auto-approve must grant")
		}
	default:
		t.Fatal("auto-approve did not answer immediately")
	}
	if m.perm != nil {
		t.Fatal("no prompt should be shown under auto-approve")
	}
}
