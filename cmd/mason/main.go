// mason — a model-agnostic coding agent with the prism/grove code graph and
// shale evidence trail baked in. No steering files: tool routing, payload
// relay, and edit application are properties of the harness.
package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/glamour"
	"github.com/peterh/liner"
	"golang.org/x/term"

	"github.com/provasign/mason/internal/agent"
	"github.com/provasign/mason/internal/creds"
	"github.com/provasign/mason/internal/localmodels"
	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/mason/internal/tui"
	"github.com/provasign/prism/pkg/kit"
)

var version = "dev" // set by -ldflags at release

const usage = `mason — coding agent with the code graph baked in

usage:
  mason [flags] ["task"]        one-shot task, or interactive REPL if omitted
  mason models                  browse, download, and pick free local models
  mason login <anthropic|openai>    store an API key in the OS keychain
  mason logout <anthropic|openai>   remove a stored API key
  mason version

flags:
  --model <spec>   ollama:<tag> | claude:<m> | openai:<m>  (default: auto-detect)
  --dir <path>     project root (default: current directory)
  --continue       resume the previous conversation for this directory
  --yes            skip permission prompts for bash/edit/write
  --no-tui         plain line-based REPL instead of the full-screen UI
  --max-turns <n>  per-task turn budget (default: 60 local, 30 API; 0 = unlimited)

REPL commands: /models  /model <spec>  /cost  /savings  /compact  /clear  /help  /exit

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
				fmt.Fprintln(os.Stderr, "usage: mason login <anthropic|openai>")
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
	noTUI := false
	maxTurns := 0
	var taskParts []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--model":
			if i+1 < len(args) {
				model = args[i+1]
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
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	if model == "" {
		model = detectModel(mustAbsDir(dir), interactive)
		if model == "" {
			fmt.Fprintln(os.Stderr, "mason: no model available — run `mason models` to set up a free local model, or `mason login anthropic` / `mason login openai`")
			return 1
		}
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
	sess := agent.New(p, invoke, opts)
	sessFile := sessionPath(root)
	if cont {
		if msgs, m, err := loadSession(sessFile); err == nil {
			sess.SetHistory(msgs)
			fmt.Fprintf(os.Stderr, "resumed %d messages (last model %s)\n", len(msgs), m)
		} else {
			fmt.Fprintln(os.Stderr, "no previous session to continue")
		}
	}

	defer func() {
		if r := recover(); r != nil {
			saveSession(sessFile, sess.History(), model)
			fmt.Fprintf(os.Stderr, "mason: internal error (session saved): %v\n", r)
			os.Exit(1)
		}
	}()

	if task != "" {
		_, err := interruptibleAsk(sess, task)
		saveSession(sessFile, sess.History(), model)
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
			SaveSession: func() { saveSession(sessFile, sess.History(), model) },
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
	fmt.Println(`type a task — /help for commands, /exit to quit`)
	rl := liner.NewLiner()
	rl.SetCtrlCAborts(true)
	histPath := filepath.Join(filepath.Dir(sessionPath(root)), "..", "history")
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
		case line == "/compact":
			before, after, err := sess.Compact(context.Background())
			if err != nil {
				fmt.Fprintln(os.Stderr, "compact:", err)
			} else {
				fmt.Printf("compacted %d → %d chars\n", before, after)
			}
			continue
		case line == "/models":
			spec, err := localmodels.Wizard(true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "models:", err)
				continue
			}
			if spec != "" {
				np, err := factory(spec)
				if err != nil {
					fmt.Fprintln(os.Stderr, "model:", err)
					continue
				}
				sess.SetProvider(np)
				model = spec
				fmt.Println("switched to", np.Name())
			}
			continue
		case strings.HasPrefix(line, "/model"):
			spec := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if spec == "" {
				fmt.Println("current model:", model)
				continue
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
			fmt.Println("unknown command — /help for the list")
			continue
		}
		_, err = interruptibleAsk(sess, line)
		saveSession(sessFile, sess.History(), model)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
		}
	}
	printCost(sess, model)
	printSavings(k)
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
	if _, last, err := loadSession(sessionPath(root)); err == nil && last != "" {
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

// sessionPath is the per-root conversation store (no credentials ever).
func sessionPath(root string) string {
	sum := sha1.Sum([]byte(root))
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "mason", "sessions", hex.EncodeToString(sum[:])+".json")
}

type sessionFile struct {
	Model    string         `json:"model"`
	Messages []provider.Msg `json:"messages"`
}

func saveSession(path string, msgs []provider.Msg, model string) {
	// Cap persisted history so the session file cannot grow without bound;
	// the system prompt is regenerated on load, so drop it and keep the tail.
	const keep = 400
	if len(msgs) > keep {
		msgs = msgs[len(msgs)-keep:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(sessionFile{Model: model, Messages: msgs})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

func loadSession(path string) ([]provider.Msg, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var sf sessionFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, "", err
	}
	return sf.Messages, sf.Model, nil
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
