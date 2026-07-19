// mason — a model-agnostic coding agent with the prism/grove code graph and
// optional shale evidence trail when the binary is installed. No steering files: tool routing, payload
// relay, and edit application are properties of the harness.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/peterh/liner"
	"golang.org/x/term"

	"github.com/provasign/mason/internal/agent"
	"github.com/provasign/mason/internal/creds"
	"github.com/provasign/mason/internal/localmodels"
	"github.com/provasign/mason/internal/lspclient"
	"github.com/provasign/mason/internal/mcpclient"
	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/mason/internal/tui"
	"github.com/provasign/prism/pkg/kit"
)

var version = "dev" // set by -ldflags at release

// init resolves a useful version for plain `go build` binaries: the VCS
// revision from Go's build info, marked dirty when the tree was. A bare
// "dev" once overwrote a release binary invisibly — never again.
func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", ""
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 12 {
					rev = s.Value[:12]
				}
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			version = "dev-" + rev + dirty
		}
	}
}

const usage = `mason — coding agent with the code graph baked in

usage:
  mason [flags] ["task"]        one-shot task, or interactive REPL if omitted
  mason review [--base <ref>] [--strict]   engine-verified diff review (no model; --strict fails CI on warnings)
  mason init [--force]          generate MASON.md (project map) from the tree + code graph
  mason sessions                list saved conversations for this directory
  mason models                  browse, download, and pick free local models
  mason login <anthropic|openai>    store an API key in the OS keychain
  mason logout <anthropic|openai>   remove a stored API key
  mason version

flags:
  --model <name>   friendly names: sonnet | haiku | opus | gpt | gpt-mini — or full specs:
                   ollama:<tag> | claude:<m> | openai:<m> | openrouter:<m> |
                   lmstudio:<m> | vllm:<m> | oai:<url>#<m> | auto  (default: auto-detect)
                   explicit auto = measured local routing: graph tasks → small, coding → best
                   omitted --model = select one available model; no per-task routing
  --dir <path>     project root (default: current directory)
  --continue       resume the latest conversation for this directory
  --resume [n]     pick a saved conversation to resume (list + prompt, or directly by number)
  --plan           plan mode: read-only session — mutating tools are refused by the harness
  --json           one-shot machine output: exactly one JSON object on stdout (CI/SDK)
  --max-cost <usd> hard cost budget: the task stops when the session estimate reaches it
  --image <path>   attach an image to the task (repeatable; vision-capable models)
                   images named in the task text are attached automatically
  --yes            skip permission prompts for bash/edit/write
  --no-tui         plain line-based REPL instead of the full-screen UI
  --max-turns <n>  per-task turn budget (default: 60 local, 30 API; 0 = unlimited)

REPL commands: /model [name]  /plan  /sessions  /resume N  /cost  /savings
               /compact  /clear  /help  /exit  (Tab-completes; the full-screen TUI adds
               a live autocomplete popup and /mouse to toggle native text selection)

Project instructions: AGENTS.md and MASON.md at the root are loaded into the
system prompt automatically.

credentials: env var (ANTHROPIC_API_KEY / OPENAI_API_KEY) > OS keychain >
interactive prompt. Keys are never written to config files, sessions, or
logs. Local models need no credentials at all.`

func main() {
	os.Exit(run(os.Args[1:]))
}

// interruptibleAsk runs one task; the first Ctrl+C cancels the task (the
// session survives, ready for the next prompt), it does not kill mason.
func interruptibleAsk(sess *agent.Session, task string) (string, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return sess.Ask(ctx, task)
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "login":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: mason login <anthropic|openai|openrouter>")
				return 2
			}
			if err := creds.Login(args[1]); err != nil {
				fmt.Fprintln(os.Stderr, "login:", err)
				return 1
			}
			return 0
		case "logout":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: mason logout <anthropic|openai>")
				return 2
			}
			if err := creds.Delete(args[1]); err != nil {
				fmt.Fprintln(os.Stderr, "logout:", err)
				return 1
			}
			fmt.Println("removed", args[1], "credential from the OS keychain")
			return 0
		case "review":
			base, strict := "", false
			for i := 1; i < len(args); i++ {
				switch args[i] {
				case "--base":
					if i+1 < len(args) {
						base = args[i+1]
						i++
					}
				case "--strict":
					strict = true
				}
			}
			return runReview(base, strict)
		case "init":
			force := false
			for _, a := range args[1:] {
				if a == "--force" || a == "-f" {
					force = true
				}
			}
			return runInit(force)
		case "sessions":
			root, err := filepath.Abs(".")
			if err != nil {
				fmt.Fprintln(os.Stderr, "sessions:", err)
				return 1
			}
			fmt.Println(renderSessions(listSessions(root)))
			return 0
		case "models":
			interactive := term.IsTerminal(int(os.Stdin.Fd()))
			spec, err := localmodels.Wizard(interactive)
			if err != nil {
				fmt.Fprintln(os.Stderr, "models:", err)
				return 1
			}
			if spec != "" {
				fmt.Printf("\nready — just run:  mason\n(auto-detect will pick %s)\n", spec)
			}
			return 0
		case "version", "--version", "-v":
			fmt.Println("mason", version)
			return 0
		case "help", "--help", "-h":
			fmt.Println(usage)
			return 0
		}
	}

	dir := "."
	model := ""
	yes := false
	cont := false
	resume := false
	resumeSel := ""
	planMode := false
	noTUI := false
	jsonOut := false
	maxCost := 0.0
	maxTurns := 0
	var images []string
	var taskParts []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--model":
			if i+1 < len(args) {
				model = resolveModelAlias(args[i+1])
				i++
			}
		case "--dir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case "--yes", "-y":
			yes = true
		case "--continue", "-c":
			cont = true
		case "--resume", "-r":
			resume = true
			if i+1 < len(args) {
				if _, err := strconv.Atoi(args[i+1]); err == nil {
					resumeSel = args[i+1]
					i++
				}
			}
		case "--plan":
			planMode = true
		case "--json":
			jsonOut = true
			noTUI = true
		case "--max-cost":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%f", &maxCost)
				i++
			}
		case "--image":
			if i+1 < len(args) {
				images = append(images, args[i+1])
				i++
			}
		case "--no-tui":
			noTUI = true
		case "--max-turns":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &maxTurns)
				if maxTurns == 0 {
					maxTurns = -1 // 0 means unlimited
				}
				i++
			}
		default:
			taskParts = append(taskParts, a)
		}
	}
	task := strings.TrimSpace(strings.Join(taskParts, " "))
	if jsonOut && task == "" {
		fmt.Fprintln(os.Stderr, "mason: --json requires a one-shot task")
		return 2
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	if model == "" {
		model = detectModel(mustAbsDir(dir), interactive)
		if model == "" {
			fmt.Fprintln(os.Stderr, "mason: no model available — run `mason models` to set up a free local model, or `mason login anthropic` / `mason login openai`")
			return 1
		}
	}
	var routerFn func(string, bool) provider.Provider
	if model == "auto" {
		small, big := pickAutoModels()
		if big == "" {
			fmt.Fprintln(os.Stderr, "mason: --model auto needs at least one installed local model (mason models)")
			return 1
		}
		if small == "" {
			small = big
		}
		smallP, err := provider.NewProvider(small, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		bigP, err := provider.NewProvider(big, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		routerFn = func(task string, graphShaped bool) provider.Provider {
			if graphShaped {
				return smallP
			}
			return bigP
		}
		model = big // display/cost baseline
		fmt.Fprintf(os.Stderr, "auto routing: graph tasks → %s · coding → %s\n", small, big)
	}
	p, err := provider.NewProvider(model, func(vendor string) (string, error) {
		return creds.Get(vendor, interactive)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mason:", err)
		return 1
	}

	root, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mason:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "mason %s · %s · %s\n", version, p.Name(), root)
	var invoke agent.Invoker
	k, kerr := kit.Open(root)
	if kerr != nil {
		// Engine failure must not brick the agent: degrade to coding tools.
		fmt.Fprintf(os.Stderr, "⚠ code graph unavailable (%v) — graph operations disabled this session\n", kerr)
	} else {
		defer k.Close()
		// Delta-index so the graph reflects the CURRENT tree — answers from a
		// stale graph are wrong answers. Cheap when nothing changed.
		if _, err := k.Invoke("prism_index", map[string]any{}); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ index failed (%v) — graph operations disabled this session\n", err)
			k.Close()
			k = nil
		} else {
			invoke = k.Invoke
		}
	}

	// Full-screen TUI is the default for an interactive REPL session.
	useTUI := interactive && task == "" && !noTUI && term.IsTerminal(int(os.Stdout.Fd()))
	var ui *tui.UI
	if useTUI {
		ui = tui.New()
	}

	permitDetail := func(action, detail string) bool {
		if yes {
			return true
		}
		if ui != nil {
			return ui.PermitDetail(action, detail)
		}
		if !interactive {
			fmt.Fprintf(os.Stderr, "denied (non-interactive without --yes): %s\n", action)
			return false
		}
		if detail != "" {
			for _, l := range strings.Split(detail, "\n") {
				fmt.Println("    " + l)
			}
		}
		fmt.Printf("  allow? %s [y/N] ", action)
		r := bufio.NewReader(os.Stdin)
		ans, _ := r.ReadString('\n')
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y")
	}
	permit := func(action string) bool { return permitDetail(action, "") }

	ctxChars := 400_000
	if strings.HasPrefix(model, "ollama:") || !strings.Contains(model, ":") {
		ctxChars = 48_000 // local num_ctx is 16k tokens
	}
	factory := func(spec string) (provider.Provider, error) {
		return provider.NewProvider(spec, func(vendor string) (string, error) {
			return creds.Get(vendor, interactive)
		})
	}
	colorOut := term.IsTerminal(int(os.Stdout.Fd()))
	opts := agent.Options{
		Root: root, MaxTurns: maxTurns, Permit: permit, PermitDetail: permitDetail,
		ProjectNotes: projectNotes(root), CtxChars: ctxChars,
		Stream: true, Color: colorOut, NewProvider: factory,
	}
	opts.MaxCostUSD = maxCost
	// model is a captured var — the estimator follows mid-session switches.
	opts.CostFn = func(in, out, cr, cw int) float64 {
		return provider.EstimateCostCached(model, in, out, cr, cw)
	}
	if jsonOut {
		// stdout carries exactly one JSON object; narration goes to stderr.
		opts.Out = os.Stderr
		opts.Stream = false
		opts.Color = false
	}
	if k != nil {
		opts.FileSymbols = func(path string) []agent.SymbolInfo {
			syms, err := k.FileSymbols(context.Background(), path)
			if err != nil {
				return nil
			}
			out := make([]agent.SymbolInfo, 0, len(syms))
			for _, s := range syms {
				out = append(out, agent.SymbolInfo{Name: s.Name,
					QualifiedName: s.QualifiedName, Kind: s.Kind, Line: s.Line})
			}
			return out
		}
	}
	if ui != nil {
		opts.Out = ui.Writer()
		opts.Status = ui.Status
		opts.CompactRender = 6 // TUI default: collapse long result groups
		// In the TUI, complete replies render as markdown (glamour) — worth
		// more than raw token streaming inside a viewport; the spinner and
		// tool lines carry liveness. Line mode keeps true streaming.
		opts.Stream = false
		if r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(96)); err == nil {
			opts.Render = func(text string) string {
				out, rerr := r.Render(text)
				if rerr != nil {
					return text
				}
				return strings.TrimRight(out, "\n")
			}
		}
	}
	opts.Policy = agent.LoadPolicy(root)
	opts.Hooks = agent.LoadHooks(root)
	opts.Router = routerFn
	// LSP diagnostics feed: lazily start the detected language server on
	// the FIRST edit (read-only sessions never pay the boot cost) and pipe
	// errors/warnings for each written file into the tool result.
	var lspOnce sync.Once
	var lspC *lspclient.Client
	opts.Diagnostics = func(abs string) []string {
		lspOnce.Do(func() {
			name, command, argv, disabled := lspConfig(root)
			if disabled || name == "" {
				return
			}
			c, err := lspclient.Start(name, command, argv, root)
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠ lsp %s: %v — edit-time diagnostics off\n", name, err)
				return
			}
			lspC = c
			fmt.Fprintf(os.Stderr, "lsp: %s connected — edit-time diagnostics on\n", name)
		})
		if lspC == nil {
			return nil
		}
		ds, err := lspC.Diagnostics(abs)
		if err != nil {
			return nil
		}
		var out []string
		for _, d := range ds {
			if d.Severity <= 2 { // errors + warnings; info/hints are noise here
				out = append(out, d.String())
			}
		}
		return out
	}
	defer func() {
		if lspC != nil {
			lspC.Close()
		}
	}()
	// MCP servers from .mason/config.json → tools the model can call,
	// gated and redacted like everything else.
	mcpClients := connectMCP(root)
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()
	extraTools, extraInvoke := mcpToolset(mcpClients)
	opts.ExtraTools = extraTools
	opts.ExtraInvoke = extraInvoke
	sess := agent.New(p, invoke, opts)
	if len(images) > 0 {
		if err := sess.AttachImages(images); err != nil {
			fmt.Fprintln(os.Stderr, "mason: --image:", err)
			return 1
		}
	}
	if planMode {
		sess.SetPlan(true)
		fmt.Fprintln(os.Stderr, "plan mode ON — read-only session (mutating tools are refused; /plan off to disable)")
	}
	sessName := ""
	sessFile := sessionPathFor(root, newSessionID())
	if cont || resume {
		metas := listSessions(root)
		var pick *sessionMeta
		switch {
		case len(metas) == 0:
			// nothing to resume
		case cont || (!interactive && resumeSel == ""):
			pick = &metas[0]
		case resumeSel != "":
			if n, err := strconv.Atoi(resumeSel); err == nil && n >= 1 && n <= len(metas) {
				pick = &metas[n-1]
			} else {
				fmt.Fprintf(os.Stderr, "no session #%s — pick from:\n%s\n", resumeSel, renderSessions(metas))
				return 2
			}
		default:
			fmt.Fprintln(os.Stderr, renderSessions(metas))
			fmt.Fprint(os.Stderr, "resume which? [1] ")
			r := bufio.NewReader(os.Stdin)
			ans, _ := r.ReadString('\n')
			n := 1
			if t := strings.TrimSpace(ans); t != "" {
				if v, err := strconv.Atoi(t); err == nil {
					n = v
				}
			}
			if n >= 1 && n <= len(metas) {
				pick = &metas[n-1]
			}
		}
		if pick == nil {
			fmt.Fprintln(os.Stderr, "no previous session to continue")
		} else if sf, err := loadSessionFile(pick.Path); err == nil {
			sess.SetHistory(sf.Messages)
			sessName = sf.Name
			if pick.ID != "legacy" {
				sessFile = pick.Path // keep appending to the resumed conversation
			}
			fmt.Fprintf(os.Stderr, "resumed %q — %d messages (last model %s)\n", pick.Label, len(sf.Messages), sf.Model)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			saveSession(sessFile, sess.History(), model, sessName)
			fmt.Fprintf(os.Stderr, "mason: internal error (session saved): %v\n", r)
			os.Exit(1)
		}
	}()

	if task != "" {
		var before map[string]string
		var start time.Time
		if jsonOut {
			before = gitStatusLines(root)
			start = time.Now()
		}
		reply, err := interruptibleAsk(sess, task)
		saveSession(sessFile, sess.History(), model, sessName)
		if jsonOut {
			in, out := sess.Usage()
			cr, cw := sess.CacheUsage()
			res := jsonResult{
				OK:    err == nil,
				Reply: reply,
				Model: model,
				Usage: jsonUsage{InputTokens: in, OutputTokens: out, CacheRead: cr, CacheWrite: cw,
					CostUSD: provider.EstimateCostCached(model, in, out, cr, cw)},
				ChangedFiles: changedBetween(before, gitStatusLines(root)),
				DurationMS:   time.Since(start).Milliseconds(),
			}
			if err != nil {
				res.Error = err.Error()
			}
			emitJSON(res)
			if err != nil {
				return 1
			}
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		printCost(sess, model)
		printSavings(k)
		return 0
	}

	if !interactive {
		fmt.Fprintln(os.Stderr, "mason: no task given and stdin is not a terminal")
		return 2
	}

	if ui != nil {
		err := ui.Run(tui.Config{
			ModelName: model,
			Root:      root,
			Version:   version,
			Ask:       sess.Ask,
			SwitchModel: func(spec string) error {
				np, err := factory(spec)
				if err != nil {
					return err
				}
				sess.SetProvider(np)
				model = spec
				return nil
			},
			Usage: func() (int, int, float64) {
				in, out := sess.Usage()
				cr, cw := sess.CacheUsage()
				return in, out, provider.EstimateCostCached(model, in, out, cr, cw)
			},
			Savings: func() string {
				if k == nil {
					return ""
				}
				s := k.Savings()
				if s.OriginalTokens == 0 {
					return ""
				}
				return fmt.Sprintf("ledger: %d tokens delivered where raw reads would have cost %d (%.1f%% saved)",
					s.DeliveredTokens, s.OriginalTokens, s.SavedPercent)
			},
			Compact:     sess.Compact,
			Clear:       sess.Clear,
			SetRedact:   sess.SetRedact,
			Undo: sess.Undo,
			Review: func(base string) (int, string, error) {
				rep, err := sess.Review(base)
				if err != nil {
					return 0, "", err
				}
				var b strings.Builder
				warns := rep.Render(func(l string) { b.WriteString(l + "\n") })
				return warns, b.String(), nil
			},
			SetVerbose: func(on bool) {
				if on {
					sess.SetCompactRender(0)
				} else {
					sess.SetCompactRender(6)
				}
			},
			SetPlan:  sess.SetPlan,
			PlanOn:   planMode,
			Sessions: func() string { return renderSessions(listSessions(root)) },
			Resume: func(sel string) (string, error) {
				if rest, ok := strings.CutPrefix(sel, "name "); ok {
					sessName = strings.TrimSpace(rest)
					saveSession(sessFile, sess.History(), model, sessName)
					return "session named " + strconv.Quote(sessName), nil
				}
				metas := listSessions(root)
				n, err := strconv.Atoi(strings.TrimSpace(sel))
				if err != nil || n < 1 || n > len(metas) {
					return "", fmt.Errorf("no session %q — /sessions for the list", sel)
				}
				sf, err := loadSessionFile(metas[n-1].Path)
				if err != nil {
					return "", err
				}
				// Persist the current conversation before switching away.
				saveSession(sessFile, sess.History(), model, sessName)
				sess.SetHistory(sf.Messages)
				sessName = sf.Name
				if metas[n-1].ID != "legacy" {
					sessFile = metas[n-1].Path
				}
				return fmt.Sprintf("resumed %q — %d messages", metas[n-1].Label, len(sf.Messages)), nil
			},
			SaveSession:    func() { saveSession(sessFile, sess.History(), model, sessName) },
			ExpandCommand:  func(line string) (string, bool) { return expandCommand(root, line) },
			ExtraHelp:      commandsHelp(root),
			CustomCommands: customCommandInfos(root),
			APIModels:      apiCatalog,
			HasCred:        creds.Has,
			StoreCred:      creds.Store,
			ResolveAlias:   resolveModelAlias,
			OpenKeyPage: func(vendor string) string {
				url, _ := creds.OpenKeyPage(vendor)
				return url
			},
			ModelSuggestions: modelSuggestions,
			FetchRemoteModels: func(vendor string) ([]tui.RemoteModel, error) {
				models, err := fetchRemoteModels(vendor)
				if err != nil {
					return nil, err
				}
				if len(models) > maxRemoteShown {
					models = models[:maxRemoteShown]
				}
				prefix := vendorSpecPrefix(vendor)
				out := make([]tui.RemoteModel, 0, len(models))
				for _, rm := range models {
					out = append(out, tui.RemoteModel{Spec: prefix + rm.ID, Label: rm.Label()})
				}
				return out, nil
			},
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		printCost(sess, model)
		printSavings(k)
		return 0
	}

	// REPL: one conversation, many tasks. liner gives editing, history, and
	// Ctrl+C-cancels-line without aborting the session.
	var pickInstalled []string
	var pickDownload []localmodels.Model
	var pickRemote []remotePick
	fmt.Println(`type a task — /help for commands, /exit to quit`)
	rl := liner.NewLiner()
	rl.SetCtrlCAborts(true)
	var modelSuggCache []tui.CommandInfo
	rl.SetCompleter(func(line string) []string {
		if !strings.HasPrefix(line, "/") {
			return nil
		}
		// Argument stage: "/model <partial>" completes model choices.
		if arg, ok := strings.CutPrefix(line, "/model "); ok {
			if modelSuggCache == nil {
				modelSuggCache = modelSuggestions()
			}
			q := strings.ToLower(strings.TrimSpace(arg))
			var out []string
			for _, c := range modelSuggCache {
				if q == "" || strings.Contains(strings.ToLower(c.Name), q) {
					out = append(out, "/model "+c.Name)
				}
			}
			return out
		}
		q := strings.ToLower(line[1:])
		var out []string
		for _, name := range append(replCommandNames(), listCommands(root)...) {
			if strings.HasPrefix(name, q) {
				out = append(out, "/"+name)
			}
		}
		return out
	})
	histPath := filepath.Join(cacheBase(), "mason", "history")
	if f, err := os.Open(histPath); err == nil {
		_, _ = rl.ReadHistory(f)
		f.Close()
	}
	saveHist := func() {
		if f, err := os.Create(histPath); err == nil {
			_, _ = rl.WriteHistory(f)
			f.Close()
		}
		rl.Close()
	}
	defer saveHist()
	for {
		line, err := rl.Prompt("\nmason> ")
		if err == liner.ErrPromptAborted {
			continue // Ctrl+C clears the line
		}
		if err != nil {
			break // EOF / Ctrl+D
		}
		line = strings.TrimSpace(line)
		if line != "" {
			rl.AppendHistory(line)
		}
		switch {
		case line == "":
			continue
		case line == "/exit" || line == "/quit":
			printCost(sess, model)
			printSavings(k)
			return 0
		case line == "/help":
			fmt.Println(usage)
			if ch := commandsHelp(root); ch != "" {
				fmt.Print("\n" + ch)
			}
			continue
		case line == "/savings":
			printSavings(k)
			continue
		case line == "/cost":
			printCost(sess, model)
			continue
		case line == "/clear":
			sess.Clear()
			fmt.Println("conversation cleared")
			continue
		case strings.HasPrefix(line, "/plan"):
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/plan"))
			if arg == "off" {
				sess.SetPlan(false)
				fmt.Println("plan mode OFF — edits are enabled again")
			} else {
				sess.SetPlan(true)
				fmt.Println("plan mode ON — read-only: the agent investigates and plans, the harness refuses mutations (/plan off to disable)")
			}
			continue
		case line == "/sessions":
			fmt.Println(renderSessions(listSessions(root)))
			continue
		case strings.HasPrefix(line, "/resume"):
			sel := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
			if rest, ok := strings.CutPrefix(sel, "name "); ok {
				sessName = strings.TrimSpace(rest)
				saveSession(sessFile, sess.History(), model, sessName)
				fmt.Printf("session named %q\n", sessName)
				continue
			}
			metas := listSessions(root)
			n, err := strconv.Atoi(sel)
			if err != nil || n < 1 || n > len(metas) {
				fmt.Println(renderSessions(metas))
				continue
			}
			sf, err := loadSessionFile(metas[n-1].Path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "resume:", err)
				continue
			}
			saveSession(sessFile, sess.History(), model, sessName)
			sess.SetHistory(sf.Messages)
			sessName = sf.Name
			if metas[n-1].ID != "legacy" {
				sessFile = metas[n-1].Path
			}
			fmt.Printf("resumed %q — %d messages\n", metas[n-1].Label, len(sf.Messages))
			continue
		case line == "/compact":
			before, after, err := sess.Compact(context.Background())
			if err != nil {
				fmt.Fprintln(os.Stderr, "compact:", err)
			} else {
				fmt.Printf("compacted %d → %d chars\n", before, after)
			}
			continue
		case line == "/models" || line == "/model": // /models = legacy alias
			var text string
			text, pickInstalled, pickDownload, pickRemote = renderModelList()
			fmt.Printf("\ncurrent model: %s\n%s\n", model, text)
			continue
		case strings.HasPrefix(line, "/model"):
			spec := strings.TrimSpace(strings.TrimPrefix(line, "/models"))
			spec = strings.TrimSpace(strings.TrimPrefix(spec, "/model"))
			if n, err := strconv.Atoi(spec); err == nil {
				if pickInstalled == nil && pickDownload == nil {
					_, pickInstalled, pickDownload, pickRemote = renderModelList()
				}
				apiBase := len(pickInstalled) + len(pickDownload)
				curatedEnd := apiBase + len(apiCatalog)
				switch {
				case n >= 1 && n <= len(pickInstalled):
					spec = "ollama:" + pickInstalled[n-1]
				case n > len(pickInstalled) && n <= apiBase:
					pick := pickDownload[n-1-len(pickInstalled)]
					fmt.Printf("downloading %s (%.1f GB)…\n", pick.Tag, pick.DownloadGB)
					dl := exec.Command("ollama", "pull", pick.Tag)
					dl.Stdout, dl.Stderr = os.Stdout, os.Stderr
					if err := dl.Run(); err != nil {
						fmt.Fprintln(os.Stderr, "download failed:", err)
						continue
					}
					spec = "ollama:" + pick.Tag
				case n > apiBase && n <= curatedEnd:
					spec = apiCatalog[n-1-apiBase].Spec
				case n > curatedEnd && n <= curatedEnd+len(pickRemote):
					spec = pickRemote[n-1-curatedEnd].Spec
				default:
					fmt.Println("no model #" + spec + " — /model lists every choice")
					continue
				}
			} else {
				spec = resolveModelAlias(spec)
			}
			// A picked API model without a stored key gets the guided
			// browser + hidden-paste login before switching.
			for _, am := range apiCatalog {
				if am.Spec == spec && !creds.Has(am.Vendor) {
					fmt.Printf("no %s key yet — let's set one up (stored only in your OS keychain)\n", am.Vendor)
					if err := creds.Login(am.Vendor); err != nil {
						fmt.Fprintln(os.Stderr, "login:", err)
					}
					break
				}
			}
			np, err := provider.NewProvider(spec, func(vendor string) (string, error) {
				return creds.Get(vendor, interactive)
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "model:", err)
				continue
			}
			sess.SetProvider(np)
			model = spec
			fmt.Println("switched to", np.Name())
			continue
		case strings.HasPrefix(line, "/"):
			if task, ok := expandCommand(root, line); ok {
				fmt.Println("· running custom command")
				line = task
				break
			}
			fmt.Println("unknown command — /help for the list")
			continue
		}
		_, err = interruptibleAsk(sess, line)
		saveSession(sessFile, sess.History(), model, sessName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
		}
	}
	printCost(sess, model)
	printSavings(k)
	return 0
}

// lspConfig resolves the language server: an explicit "lsp" entry in
// .mason/config.json wins ({"command": "...", "args": [...]} or
// {"disabled": true}); otherwise auto-detection against installed servers.
func lspConfig(root string) (name, command string, args []string, disabled bool) {
	b, err := os.ReadFile(filepath.Join(root, ".mason", "config.json"))
	if err == nil {
		var cfg struct {
			LSP *struct {
				Command  string   `json:"command"`
				Args     []string `json:"args"`
				Disabled bool     `json:"disabled"`
			} `json:"lsp"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.LSP != nil {
			if cfg.LSP.Disabled {
				return "", "", nil, true
			}
			if cfg.LSP.Command != "" {
				return filepath.Base(cfg.LSP.Command), cfg.LSP.Command, cfg.LSP.Args, false
			}
		}
	}
	name, command, args = lspclient.Detect(root)
	return name, command, args, false
}

// connectMCP starts the servers configured under "mcp" in .mason/config.json.
func connectMCP(root string) map[string]*mcpclient.Client {
	b, err := os.ReadFile(filepath.Join(root, ".mason", "config.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		MCP map[string]mcpclient.ServerConfig `json:"mcp"`
	}
	if json.Unmarshal(b, &cfg) != nil || len(cfg.MCP) == 0 {
		return nil
	}
	out := map[string]*mcpclient.Client{}
	for name, sc := range cfg.MCP {
		c, err := mcpclient.Connect(name, sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠ mcp %s: %v (skipped)\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "mcp %s: %d tool(s)\n", name, len(c.Tools()))
		out[name] = c
	}
	return out
}

// mcpToolset exposes each server tool as mcp_<server>_<tool>.
func mcpToolset(clients map[string]*mcpclient.Client) ([]provider.ToolDef, func(string, map[string]any) (string, error)) {
	if len(clients) == 0 {
		return nil, nil
	}
	var defs []provider.ToolDef
	route := map[string]func(map[string]any) (string, error){}
	for name, c := range clients {
		for _, t := range c.Tools() {
			full := "mcp_" + name + "_" + t.Name
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			defs = append(defs, provider.ToolDef{
				Name: full, Description: "[" + name + " MCP] " + t.Description, Parameters: schema,
			})
			tool := t.Name
			cl := c
			route[full] = func(args map[string]any) (string, error) { return cl.Call(tool, args) }
		}
	}
	invoke := func(name string, args map[string]any) (string, error) {
		fn, ok := route[name]
		if !ok {
			return "", fmt.Errorf("unknown mcp tool %q", name)
		}
		return fn(args)
	}
	return defs, invoke
}

// pickAutoModels chooses the routing pair from installed catalog models:
// small = first installed 14B-class-or-below (graph route-and-summarize is
// tier-invariant down to 14B, measured); big = best installed overall.
func pickAutoModels() (small, big string) {
	st := localmodels.Detect()
	if !st.ServerUp {
		return "", ""
	}
	installed := st.InstalledSet()
	for _, m := range localmodels.Catalog {
		if installed[m.Tag] {
			if big == "" {
				big = "ollama:" + m.Tag
			}
			if small == "" && m.MinRAMGB <= 16 {
				small = "ollama:" + m.Tag
			}
		}
	}
	return small, big
}

// runReview is the headless engine review — no model, deterministic, CI-safe.
func runReview(base string, strict bool) int {
	root, err := filepath.Abs(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "review:", err)
		return 1
	}
	k, err := kit.Open(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review: code graph unavailable:", err)
		return 1
	}
	defer k.Close()
	sess := agent.New(nil, k.Invoke, agent.Options{Root: root,
		FileSymbols: func(path string) []agent.SymbolInfo {
			syms, err := k.FileSymbols(context.Background(), path)
			if err != nil {
				return nil
			}
			out := make([]agent.SymbolInfo, 0, len(syms))
			for _, s := range syms {
				out = append(out, agent.SymbolInfo{Name: s.Name, QualifiedName: s.QualifiedName, Kind: s.Kind, Line: s.Line})
			}
			return out
		}})
	rep, err := sess.Review(base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review:", err)
		return 1
	}
	warns := rep.Render(func(l string) { fmt.Println(l) })
	if strict && warns > 0 {
		fmt.Fprintf(os.Stderr, "review: %d warning(s) — failing (--strict)\n", warns)
		return 1
	}
	return 0
}

// detectModel picks a model automatically: an installed local model from
// the curated catalog (in catalog order — best first), then any other
// installed local model, then stored API credentials. When nothing exists
// and the terminal is interactive, it walks the user through a free local
// setup instead of failing.
func detectModel(root string, interactive bool) string {
	// Sticky per-repo default: reuse the model last used here, if it is
	// still available.
	if metas := listSessions(root); len(metas) > 0 {
		last := metas[0].Model
		if strings.HasPrefix(last, "ollama:") || !strings.Contains(last, ":") {
			tag := strings.TrimPrefix(last, "ollama:")
			for _, t := range localmodels.Detect().Installed {
				if t == tag {
					return last
				}
			}
		} else if vendor, ok := map[string]string{"claude": "anthropic", "anthropic": "anthropic",
			"openai": "openai", "gpt": "openai"}[strings.SplitN(last, ":", 2)[0]]; ok && creds.Has(vendor) {
			return last
		}
	}
	st := localmodels.Detect()
	if st.ServerUp {
		installed := st.InstalledSet()
		for _, m := range localmodels.Catalog {
			if installed[m.Tag] {
				return "ollama:" + m.Tag
			}
		}
		if len(st.Installed) > 0 {
			return "ollama:" + st.Installed[0]
		}
	}
	if creds.Has("anthropic") {
		return "claude:claude-haiku-4-5-20251001"
	}
	if creds.Has("openai") {
		return "openai:gpt-4o-mini"
	}
	if interactive {
		fmt.Println("No model found — mason can set up a FREE local model for you (no account, no API key).")
		if spec, err := localmodels.Wizard(true); err == nil && spec != "" {
			return spec
		}
	}
	return ""
}

// mustAbsDir resolves dir without failing (detection is best-effort).
func mustAbsDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// projectNotes loads AGENTS.md / MASON.md from the root (capped) so repos
// can carry instructions the same way they do for Claude Code / Codex.
func projectNotes(root string) string {
	var parts []string
	for _, name := range []string{"AGENTS.md", "MASON.md"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		txt := string(b)
		if len(txt) > 16_000 {
			txt = txt[:16_000] + "\n…(truncated)"
		}
		parts = append(parts, "## "+name+"\n"+txt)
	}
	return strings.Join(parts, "\n\n")
}

func printCost(sess *agent.Session, model string) {
	in, out := sess.Usage()
	if in+out == 0 {
		return
	}
	cr, cw := sess.CacheUsage()
	cost := provider.EstimateCostCached(model, in, out, cr, cw)
	if cr > 0 {
		fmt.Fprintf(os.Stderr, "cache: %d tokens read from prompt cache, %d written\n", cr, cw)
	}
	if cost > 0 {
		fmt.Fprintf(os.Stderr, "usage: %d in / %d out tokens ≈ $%.4f (list-price estimate)\n", in, out, cost)
	} else {
		fmt.Fprintf(os.Stderr, "usage: %d in / %d out tokens ($0 local or unpriced model)\n", in, out)
	}
}

func printSavings(k *kit.Kit) {
	if k == nil {
		return
	}
	s := k.Savings()
	if s.OriginalTokens == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nledger: %d tokens delivered where raw reads would have cost %d (%.1f%% saved)\n",
		s.DeliveredTokens, s.OriginalTokens, s.SavedPercent)
}
