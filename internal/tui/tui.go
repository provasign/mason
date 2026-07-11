// Package tui is mason's full-screen terminal UI: scrolling transcript,
// input box, spinner, live status bar (model · tokens · cost · savings),
// inline permission prompts, and model switching. The agent runs in a
// goroutine; everything it writes flows through a channel into the view.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/provasign/mason/internal/localmodels"
)

// Config wires the TUI to the session without importing the agent package
// (keeps the dependency one-way and the TUI testable).
type Config struct {
	ModelName   string
	Root        string
	Version     string
	Ask         func(ctx context.Context, task string) (string, error)
	SwitchModel func(spec string) error
	Usage       func() (in, out int, costUSD float64)
	Savings     func() string // one-line ledger summary, "" if none
	Compact     func() (before, after int, err error)
	Clear       func()
	SaveSession func()
}

// UI owns the event channel shared with the agent goroutine.
type UI struct {
	events chan tea.Msg
}

func New() *UI { return &UI{events: make(chan tea.Msg, 256)} }

// Writer returns the io.Writer the agent session should use as Out: every
// chunk the agent renders lands in the transcript.
func (u *UI) Writer() *chanWriter { return &chanWriter{ch: u.events} }

// Permit is the agent's permission gate: it round-trips through the UI so
// the y/N prompt renders inside the TUI instead of corrupting the screen.
func (u *UI) Permit(action string) bool {
	resp := make(chan bool, 1)
	u.events <- permMsg{action: action, resp: resp}
	return <-resp
}

type chanWriter struct{ ch chan tea.Msg }

func (w *chanWriter) Write(b []byte) (int, error) {
	w.ch <- chunkMsg(string(b))
	return len(b), nil
}

type (
	chunkMsg string
	permMsg  struct {
		action string
		resp   chan bool
	}
	doneMsg struct {
		reply string
		err   error
	}
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	statusStyle = lipgloss.NewStyle().Faint(true)
	permStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	youStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
)

type uiModel struct {
	ui     *UI
	cfg    Config
	vp     viewport.Model
	in     textinput.Model
	sp     spinner.Model
	buf    *strings.Builder // pointer: uiModel is copied every Update, and a copied Builder panics
	wide   int
	tall   int
	ready  bool
	busy   bool
	cancel context.CancelFunc
	perm   *permMsg
	model  string
}

func newModel(u *UI, cfg Config) uiModel {
	in := textinput.New()
	in.Placeholder = "type a task — /help for commands"
	in.Focus()
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	return uiModel{ui: u, cfg: cfg, in: in, sp: sp, model: cfg.ModelName, buf: &strings.Builder{}}
}

// Run starts the full-screen UI and blocks until exit.
func (u *UI) Run(cfg Config) error {
	p := tea.NewProgram(newModel(u, cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m uiModel) listen() tea.Cmd {
	return func() tea.Msg { return <-m.ui.events }
}

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(m.listen(), textinput.Blink)
}

func (m *uiModel) append(s string) {
	m.buf.WriteString(s)
	m.vp.SetContent(m.buf.String())
	m.vp.GotoBottom()
}

func (m *uiModel) submit(task string) tea.Cmd {
	m.append("\n" + youStyle.Render("you› ") + task + "\n")
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.busy = true
	events := m.ui.events
	ask := m.cfg.Ask
	go func() {
		reply, err := ask(ctx, task)
		events <- doneMsg{reply: reply, err: err}
	}()
	return m.sp.Tick
}

func (m *uiModel) statusLine() string {
	in, out, cost := m.cfg.Usage()
	state := "idle — enter a task"
	if m.busy {
		state = m.sp.View() + " working (Ctrl+C cancels the task)"
	}
	usage := ""
	if in+out > 0 {
		usage = fmt.Sprintf(" · %d in/%d out", in, out)
		if cost > 0 {
			usage += fmt.Sprintf(" ≈ $%.4f", cost)
		} else {
			usage += " ($0 local)"
		}
	}
	return statusStyle.Render(fmt.Sprintf(" %s%s · %s", state, usage, m.model))
}

func (m uiModel) View() string {
	if !m.ready {
		return "starting…"
	}
	header := headerStyle.Render(fmt.Sprintf(" mason %s ", m.cfg.Version)) +
		statusStyle.Render("· "+m.cfg.Root)
	prompt := m.in.View()
	if m.perm != nil {
		prompt = permStyle.Render(fmt.Sprintf(" allow? %s  [y/n]", m.perm.action))
	}
	return header + "\n" + m.vp.View() + "\n" + prompt + "\n" + m.statusLine()
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.wide, m.tall = msg.Width, msg.Height
		vh := msg.Height - 4 // header, input, status
		if vh < 3 {
			vh = 3
		}
		if !m.ready {
			m.vp = viewport.New(msg.Width, vh)
			m.ready = true
			m.append(fmt.Sprintf("welcome — model %s · /models to switch · /help for commands\n", m.model))
		} else {
			m.vp.Width, m.vp.Height = msg.Width, vh
		}
		m.in.Width = msg.Width - 4
		m.vp.SetContent(m.buf.String())
		return m, nil

	case chunkMsg:
		m.append(string(msg))
		return m, m.listen()

	case permMsg:
		mm := msg
		m.perm = &mm
		return m, m.listen()

	case doneMsg:
		m.busy = false
		m.cancel = nil
		if msg.err != nil {
			m.append("\n" + errStyle.Render("✗ "+msg.err.Error()) + "\n")
		}
		if m.cfg.SaveSession != nil {
			m.cfg.SaveSession()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		if m.busy {
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		// Permission prompt swallows keys until answered.
		if m.perm != nil {
			switch strings.ToLower(msg.String()) {
			case "y":
				m.perm.resp <- true
				m.append(permStyle.Render(" allow? "+m.perm.action+" — YES") + "\n")
				m.perm = nil
			case "n", "esc", "ctrl+c":
				m.perm.resp <- false
				m.append(permStyle.Render(" allow? "+m.perm.action+" — NO") + "\n")
				m.perm = nil
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			if m.busy && m.cancel != nil {
				m.cancel()
				m.append("\n" + errStyle.Render("… cancelling task") + "\n")
				return m, nil
			}
			if m.cfg.SaveSession != nil {
				m.cfg.SaveSession()
			}
			return m, tea.Quit
		case "up", "down", "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "enter":
			line := strings.TrimSpace(m.in.Value())
			m.in.SetValue("")
			if line == "" {
				return m, nil
			}
			if strings.HasPrefix(line, "/") {
				return m.command(line)
			}
			if m.busy {
				m.append(errStyle.Render("a task is already running — Ctrl+C to cancel it first") + "\n")
				return m, nil
			}
			// Mutate BEFORE the return copies m — `return m, m.submit(...)`
			// evaluates the returned m first and would drop busy/cancel.
			cmd := m.submit(line)
			return m, cmd
		}
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	return m, cmd
}

// command handles slash commands inside the TUI.
func (m uiModel) command(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		if m.cfg.SaveSession != nil {
			m.cfg.SaveSession()
		}
		return m, tea.Quit
	case "/help":
		m.append(`
commands:
  /models        list local models — pick by number with /model N
  /model N       switch to installed model number N (from /models)
  /model <spec>  switch to any model, e.g. claude:claude-sonnet-5
  /cost          session token usage and cost
  /savings       graph-read token ledger
  /compact       summarize old history
  /clear         drop the conversation
  /exit          quit (Ctrl+C when idle also quits)
keys: ↑/↓ PgUp/PgDn scroll · Ctrl+C cancels a running task
`)
		return m, nil
	case "/cost":
		in, out, cost := m.cfg.Usage()
		m.append(fmt.Sprintf("usage: %d in / %d out tokens ≈ $%.4f\n", in, out, cost))
		return m, nil
	case "/savings":
		if s := m.cfg.Savings(); s != "" {
			m.append(s + "\n")
		} else {
			m.append("no ledgered reads yet\n")
		}
		return m, nil
	case "/clear":
		m.cfg.Clear()
		m.buf.Reset()
		m.append("conversation cleared\n")
		return m, nil
	case "/compact":
		before, after, err := m.cfg.Compact()
		if err != nil {
			m.append(errStyle.Render("compact: "+err.Error()) + "\n")
		} else {
			m.append(fmt.Sprintf("compacted %d → %d chars\n", before, after))
		}
		return m, nil
	case "/models":
		st := localmodels.Detect()
		ram := localmodels.SystemRAMGB()
		var b strings.Builder
		b.WriteString("\ninstalled — switch with /model N:\n")
		for i, t := range st.Installed {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, t)
		}
		b.WriteString("downloadable (run `mason models` in a terminal for one-keypress install):\n")
		installed := st.InstalledSet()
		for _, c := range localmodels.Catalog {
			if !installed[c.Tag] && c.Fits(ram) {
				fmt.Fprintf(&b, "     %-22s %.1f GB · needs %d GB — %s\n", c.Tag, c.DownloadGB, c.MinRAMGB, c.Note)
			}
		}
		m.append(b.String())
		return m, nil
	case "/model":
		if len(fields) < 2 {
			m.append("current model: " + m.model + "\n")
			return m, nil
		}
		spec := fields[1]
		if n, err := strconv.Atoi(spec); err == nil {
			st := localmodels.Detect()
			if n < 1 || n > len(st.Installed) {
				m.append(errStyle.Render("no installed model #"+spec+" — see /models") + "\n")
				return m, nil
			}
			spec = "ollama:" + st.Installed[n-1]
		}
		if err := m.cfg.SwitchModel(spec); err != nil {
			m.append(errStyle.Render("model: "+err.Error()) + "\n")
			return m, nil
		}
		m.model = spec
		m.append("switched to " + spec + "\n")
		return m, nil
	default:
		m.append("unknown command — /help\n")
		return m, nil
	}
}
