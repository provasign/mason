// Package tui is mason's full-screen terminal UI: scrolling transcript,
// input box, spinner, live status bar (model · tokens · cost · savings),
// inline permission prompts, and model switching. The agent runs in a
// goroutine; everything it writes flows through a channel into the view.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
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
	Compact     func(ctx context.Context) (before, after int, err error)
	SetRedact   func(on bool)
	SetVerbose  func(on bool) // full tool renders vs compact head + "+N more"
	Undo        func() (string, error)
	Review      func(base string) (warns int, text string, err error)
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
	return u.PermitDetail(action, "")
}

// Status receives the agent's live activity narration (non-blocking: a
// stale activity line is better than a blocked agent).
func (u *UI) Status(activity string) {
	select {
	case u.events <- statusMsg(activity):
	default:
	}
}

// PermitDetail additionally shows WHAT the action will do (a diff, a
// content preview) in the transcript before asking y/n.
func (u *UI) PermitDetail(action, detail string) bool {
	resp := make(chan bool, 1)
	u.events <- permMsg{action: action, detail: detail, resp: resp}
	return <-resp
}

type chanWriter struct{ ch chan tea.Msg }

func (w *chanWriter) Write(b []byte) (int, error) {
	// NEVER block the agent goroutine on UI backpressure: a blocked write
	// here is invisible to the cancel context and makes Ctrl+C appear dead.
	// Under pathological pressure a transcript chunk is dropped instead.
	select {
	case w.ch <- chunkMsg(string(b)):
	default:
	}
	return len(b), nil
}

type (
	chunkMsg  string
	statusMsg string
	permMsg  struct {
		action string
		detail string
		resp   chan bool
	}
	pullDoneMsg struct {
		tag string
		err error
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
	addStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	delStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// colorDiff styles -/+ preview lines for the transcript.
func colorDiff(detail string) string {
	lines := strings.Split(detail, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "- "):
			lines[i] = delStyle.Render(l)
		case strings.HasPrefix(l, "+ "):
			lines[i] = addStyle.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

type uiModel struct {
	ui     *UI
	cfg    Config
	vp     viewport.Model
	in     textarea.Model
	sp     spinner.Model
	buf    *strings.Builder // pointer: uiModel is copied every Update, and a copied Builder panics
	wide   int
	tall   int
	ready  bool
	busy   bool
	cancel context.CancelFunc
	perm   *permMsg
	model     string
	inWidth   int       // textarea width, for wrap-aware height growth
	lastCtrlC time.Time // double-press guard for quit-at-idle
	activity  string    // what the agent is doing right now
	taskStart time.Time // for the elapsed display
	autoApprove bool    // blanket approval (/auto, or 'a' at a prompt)
	redactOff   bool    // /secrets off
	// /models pick lists: 1..len(pickInstalled) switch instantly,
	// continuing numbers download via ExecProcess.
	pickInstalled []string
	pickDownload  []localmodels.Model
}

func newModel(u *UI, cfg Config) uiModel {
	in := textarea.New()
	in.Placeholder = "type a task — Enter sends, Ctrl+J for a new line, /help for commands"
	in.ShowLineNumbers = false
	in.CharLimit = 0
	in.SetHeight(1)
	in.Focus()
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	return uiModel{ui: u, cfg: cfg, in: in, sp: sp, model: cfg.ModelName, buf: &strings.Builder{}}
}

// syncInputHeight grows the textarea with its content, counting soft-wrapped
// rows (LineCount only counts logical lines), so long tasks stay fully
// visible without horizontal scrolling.
func (m *uiModel) syncInputHeight() {
	w := m.inWidth
	if w <= 0 {
		w = 80
	}
	h := 0
	for _, line := range strings.Split(m.in.Value(), "\n") {
		h += 1 + len([]rune(line))/w
	}
	if h < 1 {
		h = 1
	}
	if h > 6 {
		h = 6
	}
	if h != m.in.Height() {
		m.in.SetHeight(h)
		m.layout()
	}
}

// layout recomputes the viewport height from the current terminal size and
// input height.
func (m *uiModel) layout() {
	vh := m.tall - 3 - m.in.Height() // header, status, prompt block
	if vh < 3 {
		vh = 3
	}
	m.vp.Height = vh
	m.vp.Width = m.wide
	m.vp.SetContent(m.buf.String())
	m.vp.GotoBottom()
}

// Run starts the full-screen UI and blocks until exit. Mouse mode is on
// for wheel scrolling (terminal text selection needs Shift+drag while a
// mouse-enabled TUI runs — the standard trade-off).
func (u *UI) Run(cfg Config) error {
	p := tea.NewProgram(newModel(u, cfg), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

func (m uiModel) listen() tea.Cmd {
	return func() tea.Msg { return <-m.ui.events }
}

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(m.listen(), textarea.Blink)
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
	m.activity = "starting…"
	m.taskStart = time.Now()
	events := m.ui.events
	ask := m.cfg.Ask
	go func() {
		reply, err := ask(ctx, task)
		events <- doneMsg{reply: reply, err: err}
	}()
	return m.sp.Tick
}

// kfmt renders a token count compactly (413 → 413, 21948 → 21.9k).
func kfmt(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func (m *uiModel) statusLine() string {
	in, out, cost := m.cfg.Usage()
	state := "ready — waiting for your input"
	switch {
	case m.perm != nil:
		state = "waiting for your approval  [y/n]"
	case m.busy:
		act := m.activity
		if act == "" {
			act = "working…"
		}
		state = fmt.Sprintf("%s %s · %ds", m.sp.View(), act, int(time.Since(m.taskStart).Seconds()))
	}
	usage := ""
	if in+out > 0 {
		usage = fmt.Sprintf(" · %s↑ %s↓", kfmt(in), kfmt(out))
		if cost > 0 {
			usage += fmt.Sprintf(" ≈ $%.4f", cost)
		} else {
			usage += " $0"
		}
	}
	hint := ""
	if m.busy {
		hint = "   (Ctrl+C cancels)"
	}
	if m.autoApprove {
		hint += "  [AUTO]"
	}
	return statusStyle.Render(fmt.Sprintf(" %s%s · %s%s", state, usage, m.model, hint))
}

func (m uiModel) View() string {
	if !m.ready {
		return "starting…"
	}
	header := headerStyle.Render(fmt.Sprintf(" mason %s ", m.cfg.Version)) +
		statusStyle.Render("· "+m.cfg.Root)
	prompt := m.in.View()
	if m.perm != nil {
		prompt = permStyle.Render(fmt.Sprintf(" allow? %s  [y/n/a=always]", m.perm.action))
	}
	return header + "\n" + m.vp.View() + "\n" + prompt + "\n" + m.statusLine()
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Degenerate sizes (0x0 from an unconfigured pty) would collapse
		// the layout to one-char rows — fall back to a sane default.
		if msg.Width < 20 {
			msg.Width = 80
		}
		if msg.Height < 6 {
			msg.Height = 24
		}
		m.wide, m.tall = msg.Width, msg.Height
		if !m.ready {
			m.vp = viewport.New(msg.Width, 3)
			m.ready = true
			m.append(fmt.Sprintf("welcome — model %s · /models to switch · /help for commands\n", m.model))
		}
		m.inWidth = msg.Width - 6 // textarea chrome (prompt gutter) eats columns
		m.in.SetWidth(msg.Width - 2)
		m.layout()
		return m, nil

	case chunkMsg:
		m.append(string(msg))
		return m, m.listen()

	case statusMsg:
		m.activity = string(msg)
		return m, m.listen()

	case tea.MouseMsg:
		// Wheel scrolls the transcript.
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case permMsg:
		mm := msg
		if mm.detail != "" {
			m.append("\n" + permStyle.Render("proposed: "+mm.action) + "\n" + colorDiff(mm.detail) + "\n")
		}
		if m.autoApprove {
			mm.resp <- true
			m.append(statusStyle.Render("auto-approved: "+mm.action) + "\n")
			return m, m.listen()
		}
		m.perm = &mm
		return m, m.listen()

	case pullDoneMsg:
		if msg.err != nil {
			m.append(errStyle.Render("download failed: "+msg.err.Error()) + "\n")
			return m, nil
		}
		m.append("✓ " + msg.tag + " installed\n")
		spec := "ollama:" + msg.tag
		if err := m.cfg.SwitchModel(spec); err != nil {
			m.append(errStyle.Render("model: "+err.Error()) + "\n")
			return m, nil
		}
		m.model = spec
		m.append("switched to " + spec + "\n")
		return m, nil
	// NOTE: pullDoneMsg arrives via ExecProcess (tea-internal), not the
	// events channel, so it does not consume the listener.

	case doneMsg:
		m.busy = false
		m.cancel = nil
		m.activity = ""
		if msg.err != nil {
			m.append("\n" + errStyle.Render("✗ "+msg.err.Error()) + "\n")
		}
		if m.cfg.SaveSession != nil {
			m.cfg.SaveSession()
		}
		// Re-arm the event listener — without this the channel reader dies
		// with the task and the NEXT task's output backs up unseen.
		return m, m.listen()

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
			case "a":
				m.autoApprove = true
				m.perm.resp <- true
				m.append(permStyle.Render(" allow? "+m.perm.action+" — ALWAYS (this session; /auto off to re-enable prompts)") + "\n")
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
			// Idle: require a second press within 2s — a reflexive Ctrl+C
			// must not eat the session.
			if time.Since(m.lastCtrlC) > 2*time.Second {
				m.lastCtrlC = time.Now()
				m.append(statusStyle.Render("press Ctrl+C again to exit (or /exit)") + "\n")
				return m, nil
			}
			if m.cfg.SaveSession != nil {
				m.cfg.SaveSession()
			}
			return m, tea.Quit
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "ctrl+j":
			// Insert a literal newline into the input.
			var cmd tea.Cmd
			m.in, cmd = m.in.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m.syncInputHeight()
			return m, cmd
		case "enter":
			line := strings.TrimSpace(m.in.Value())
			m.in.Reset()
			m.in.SetHeight(1)
			m.layout()
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
	m.syncInputHeight()
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
  /review [base] engine-verified diff review (blast radius, coverage, stubs)
  /undo          revert the file changes of the last task (checkpointed per task)
  /auto [off]    blanket-approve bash/edit/write (or 'a' at any prompt)
  /verbose [off] full tool results (default: collapsed to a head + '+N more')
  /secrets [off] secret redaction (default on)
  /clear         drop the conversation
  /exit          quit (Ctrl+C when idle also quits)
keys: mouse wheel or PgUp/PgDn scroll (Shift+drag to select text) · Ctrl+C cancels a running task
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
	case "/auto":
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		switch arg {
		case "off":
			m.autoApprove = false
			m.append("auto-approve OFF — actions will ask y/n again\n")
		default:
			m.autoApprove = true
			m.append("auto-approve ON — bash/edit/write run without asking (this session; /auto off to revert)\n")
		}
		return m, nil
	case "/review":
		if m.busy {
			m.append(errStyle.Render("a task is running — Ctrl+C first") + "\n")
			return m, nil
		}
		if m.cfg.Review == nil {
			m.append(errStyle.Render("review unavailable (engine off)") + "\n")
			return m, nil
		}
		base := ""
		if len(fields) > 1 {
			base = fields[1]
		}
		m.append("running engine review…\n")
		ctxR, cancel := context.WithCancel(context.Background())
		_ = ctxR
		m.cancel = cancel
		m.busy = true
		events := m.ui.events
		review := m.cfg.Review
		go func() {
			_, text, err := review(base)
			out := text
			if err != nil {
				out = "review: " + err.Error() + "\n"
			}
			select {
			case events <- chunkMsg(out):
			default:
			}
			events <- doneMsg{}
		}()
		return m, m.sp.Tick
	case "/undo":
		if m.busy {
			m.append(errStyle.Render("a task is running — Ctrl+C first") + "\n")
			return m, nil
		}
		if m.cfg.Undo == nil {
			m.append(errStyle.Render("undo unavailable (not a git repository)") + "\n")
			return m, nil
		}
		msg, err := m.cfg.Undo()
		if err != nil {
			m.append(errStyle.Render("undo: "+err.Error()) + "\n")
			return m, nil
		}
		m.append("↩ " + msg + "\n")
		return m, nil
	case "/verbose":
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		if m.cfg.SetVerbose != nil {
			if arg == "off" {
				m.cfg.SetVerbose(false)
				m.append("compact tool output — long result groups collapse to a head + '+N more'\n")
			} else {
				m.cfg.SetVerbose(true)
				m.append("verbose tool output — full result groups will be shown\n")
			}
		}
		return m, nil
	case "/secrets":
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		switch arg {
		case "off":
			m.redactOff = true
			if m.cfg.SetRedact != nil {
				m.cfg.SetRedact(false)
			}
			m.append(errStyle.Render("secret redaction OFF — credentials in files WILL reach the model") + "\n")
		default:
			m.redactOff = false
			if m.cfg.SetRedact != nil {
				m.cfg.SetRedact(true)
			}
			m.append("secret redaction ON — detected credentials are replaced with [REDACTED:kind] before reaching the model\n")
		}
		return m, nil
	case "/clear":
		m.cfg.Clear()
		m.buf.Reset()
		m.append("conversation cleared\n")
		return m, nil
	case "/compact":
		if m.busy {
			m.append(errStyle.Render("a task is running — Ctrl+C first") + "\n")
			return m, nil
		}
		// Compaction is a model call: run it as a cancellable busy op OFF
		// the UI thread — synchronous compaction froze the whole TUI
		// (including Ctrl+C) for the duration.
		m.append("compacting…\n")
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.busy = true
		events := m.ui.events
		compact := m.cfg.Compact
		go func() {
			before, after, err := compact(ctx)
			msg := fmt.Sprintf("compacted %d → %d chars\n", before, after)
			if err != nil {
				msg = "compact: " + err.Error() + "\n"
			}
			select {
			case events <- chunkMsg(msg):
			default:
			}
			events <- doneMsg{}
		}()
		return m, m.sp.Tick
	case "/models":
		st := localmodels.Detect()
		ram := localmodels.SystemRAMGB()
		m.pickInstalled = st.Installed
		m.pickDownload = nil
		installed := st.InstalledSet()
		var b strings.Builder
		b.WriteString("\ninstalled — /model N switches:\n")
		for i, t := range st.Installed {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, t)
		}
		b.WriteString("downloadable — /model N downloads then switches:\n")
		n := len(st.Installed)
		for _, c := range localmodels.Catalog {
			if !installed[c.Tag] && c.Fits(ram) {
				n++
				m.pickDownload = append(m.pickDownload, c)
				fmt.Fprintf(&b, "  %d. %-22s %.1f GB · needs %d GB — %s\n", n, c.Tag, c.DownloadGB, c.MinRAMGB, c.Note)
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
			if len(m.pickInstalled) == 0 && len(m.pickDownload) == 0 {
				st := localmodels.Detect()
				m.pickInstalled = st.Installed
			}
			switch {
			case n >= 1 && n <= len(m.pickInstalled):
				spec = "ollama:" + m.pickInstalled[n-1]
			case n > len(m.pickInstalled) && n <= len(m.pickInstalled)+len(m.pickDownload):
				pick := m.pickDownload[n-1-len(m.pickInstalled)]
				m.append(fmt.Sprintf("downloading %s (%.1f GB) — the screen hands over to ollama…\n", pick.Tag, pick.DownloadGB))
				// Suspend the TUI, run ollama pull with its own progress
				// bars, resume, then switch on success.
				cmd := exec.Command("ollama", "pull", pick.Tag)
				return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
					return pullDoneMsg{tag: pick.Tag, err: err}
				})
			default:
				m.append(errStyle.Render("no model #"+fields[1]+" — see /models") + "\n")
				return m, nil
			}
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
