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
