package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func typeString(m uiModel, s string) uiModel {
	for _, r := range s {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(uiModel)
	}
	return m
}

// Typing "/" opens the autocomplete popup with every command that prefixes;
// narrowing further filters it; a full unique match closes it again.
func TestAutocompletePopupOpensAndFilters(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	m = typeString(m, "/")
	if !m.suggestOpen || len(m.suggestions) == 0 {
		t.Fatal("popup must open on bare '/'")
	}
	m = typeString(m, "mod")
	if !m.suggestOpen {
		t.Fatal("popup must stay open while narrowing")
	}
	for _, c := range m.suggestions {
		if !strings.HasPrefix(c.Name, "mod") {
			t.Fatalf("filter leaked a non-matching entry: %+v", c)
		}
	}
	// "/models" is an exact, unique match — nothing left to suggest.
	m = typeString(m, "els")
	if m.suggestOpen {
		t.Fatalf("an exact unique match must close the popup, suggestions=%+v", m.suggestions)
	}
}

// A space closes the popup even mid-typing (args must not re-trigger it).
func TestAutocompletePopupClosesOnSpace(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	m = typeString(m, "/model ")
	if m.suggestOpen {
		t.Fatal("a space must close the popup")
	}
}

// Tab/Enter fills the highlighted command (with a trailing space) but does
// NOT submit — the model text still needs its arguments.
func TestAutocompleteTabFillsWithoutSubmitting(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	m = typeString(m, "/pla")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = mm.(uiModel)
	if m.suggestOpen {
		t.Fatal("Tab must close the popup")
	}
	if m.in.Value() != "/plan " {
		t.Fatalf("Tab must fill the highlighted command, got %q", m.in.Value())
	}
}

// Up/Down move the highlight without touching the input text.
func TestAutocompleteArrowsMoveHighlight(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	m = typeString(m, "/s") // sessions, savings, secrets all prefix-match
	if len(m.suggestions) < 2 {
		t.Fatalf("need >=2 matches to test navigation, got %d", len(m.suggestions))
	}
	start := m.suggestIdx
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(uiModel)
	if m.suggestIdx != start+1 {
		t.Fatalf("Down must advance the highlight: %d -> %d", start, m.suggestIdx)
	}
	if m.in.Value() != "/s" {
		t.Fatal("navigating must not alter the typed text")
	}
}

// Esc dismisses the popup and leaves the typed text untouched.
func TestAutocompleteEscDismisses(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	m = typeString(m, "/mo")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(uiModel)
	if m.suggestOpen {
		t.Fatal("Esc must close the popup")
	}
	if m.in.Value() != "/mo" {
		t.Fatalf("Esc must not change the input, got %q", m.in.Value())
	}
}

// Custom commands join the built-ins in the popup.
func TestAutocompleteIncludesCustomCommands(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 },
		CustomCommands: []CommandInfo{{Name: "fix-issue", Desc: "fix a github issue"}}})
	m = typeString(m, "/fix")
	if !m.suggestOpen || len(m.suggestions) != 1 || m.suggestions[0].Name != "fix-issue" {
		t.Fatalf("custom command must appear in suggestions: %+v", m.suggestions)
	}
}

// /mouse toggles mouse capture and returns the matching bubbletea command.
func TestMouseToggleCommand(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	mm, cmd := m.command("/mouse off")
	m = mm.(uiModel)
	if m.mouseOn {
		t.Fatal("/mouse off must clear mouseOn")
	}
	if cmd == nil || cmd() != tea.DisableMouse() {
		t.Fatal("/mouse off must return tea.DisableMouse")
	}
	mm, cmd = m.command("/mouse")
	m = mm.(uiModel)
	if !m.mouseOn {
		t.Fatal("/mouse (no arg) must re-enable")
	}
	if cmd == nil || cmd() != tea.EnableMouseCellMotion() {
		t.Fatal("/mouse on must return tea.EnableMouseCellMotion")
	}
}

// The unified picker's live-fetched section extends the numbering after
// the curated API section, and switches directly (no key entry — the
// vendor is already keyed by construction).
func TestModelNumberResolvesLiveFetchedEntry(t *testing.T) {
	var switched string
	m, _ := newTestModel(Config{ModelName: "m",
		Usage:       func() (int, int, float64) { return 0, 0, 0 },
		SwitchModel: func(spec string) error { switched = spec; return nil },
		HasCred:     func(vendor string) bool { return vendor == "anthropic" },
		APIModels: []APIModel{
			{Label: "Claude Sonnet", Spec: "claude:claude-sonnet-5", Vendor: "anthropic"},
		},
		FetchRemoteModels: func(vendor string) ([]RemoteModel, error) {
			return []RemoteModel{{Spec: "claude:claude-opus-4-8", Label: "Claude Opus 4.8"}}, nil
		},
	})
	mm, cmd := m.command("/models")
	m = mm.(uiModel)
	if cmd == nil {
		t.Fatal("/models must kick off a live-fetch command when a vendor is keyed")
	}
	msg := cmd()
	mm, _ = m.Update(msg)
	m = mm.(uiModel)
	if len(m.pickRemote) != 1 {
		t.Fatalf("remote model must be recorded: %+v", m.pickRemote)
	}
	// The live-fetched Opus is the LAST entry, after every installed/
	// downloadable local model this machine happens to have plus the
	// curated API section — never assume the local picture is empty.
	lastPick := len(m.pickInstalled) + len(m.pickDownload) + len(m.pickAPI) + len(m.pickRemote)
	mm, _ = m.command(fmt.Sprintf("/model %d", lastPick))
	m = mm.(uiModel)
	if switched != "claude:claude-opus-4-8" {
		t.Fatalf("switch = %q, want the live-fetched spec", switched)
	}
}

// View() must render the popup with the highlighted entry marked, and the
// plain prompt otherwise — a cheap smoke test against a real bubbletea
// render pass (not just the model state assertions above).
func TestViewRendersAutocompletePopup(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m", Usage: func() (int, int, float64) { return 0, 0, 0 }})
	if strings.Contains(m.View(), "▸") {
		t.Fatal("no popup marker before typing anything")
	}
	m = typeString(m, "/mod")
	out := m.View()
	if !strings.Contains(out, "▸") || !strings.Contains(out, "/model") {
		t.Fatalf("popup must render with a highlight marker:\n%s", out)
	}
}

func TestFetchErrorSurfacesAsMessage(t *testing.T) {
	m, _ := newTestModel(Config{ModelName: "m",
		Usage:   func() (int, int, float64) { return 0, 0, 0 },
		HasCred: func(string) bool { return true },
		APIModels: []APIModel{
			{Label: "Claude Sonnet", Spec: "claude:claude-sonnet-5", Vendor: "anthropic"},
		},
		FetchRemoteModels: func(vendor string) ([]RemoteModel, error) {
			return nil, errFake
		},
	})
	_, cmd := m.command("/models")
	msg := cmd()
	mm, _ := m.Update(msg)
	m = mm.(uiModel)
	if !strings.Contains(m.buf.String(), "live anthropic model list") {
		t.Fatalf("fetch error must surface in the transcript: %s", m.buf.String())
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

var errFake = fakeErr("network down")
