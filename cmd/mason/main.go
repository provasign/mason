// mason — a model-agnostic coding agent with the prism/grove code graph and
// shale evidence trail baked in. No steering files: tool routing, payload
// relay, and edit application are properties of the harness.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peterh/liner"
	"golang.org/x/term"

	"github.com/provasign/mason/internal/agent"
	"github.com/provasign/mason/internal/creds"
	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/prism/pkg/kit"
)

var version = "dev" // set by -ldflags at release

const usage = `mason — coding agent with the code graph baked in

usage:
  mason [flags] ["task"]        one-shot task, or interactive REPL if omitted
  mason login <anthropic|openai|gemini>    store an API key in the OS keychain
  mason logout <anthropic|openai|gemini>   remove a stored API key
  mason version

flags:
  --model <spec>   ollama:<tag> | claude:<m> | openai:<m> | gemini:<m>  (default: auto-detect)
  --dir <path>     project root (default: current directory)
  --continue       resume the previous conversation for this directory
  --yes            skip permission prompts for bash/edit/write
  --max-turns <n>  per-task turn budget (default 30)

REPL commands: /model <spec>  /cost  /savings  /compact  /clear  /help  /exit

Project instructions: AGENTS.md and MASON.md at the root are loaded into the
system prompt automatically.

credentials: env var (ANTHROPIC_API_KEY / OPENAI_API_KEY / GEMINI_API_KEY) >
OS keychain > interactive prompt. Keys are never written to config files,
sessions, or logs.`

func main() {
	os.Exit(run(os.Args[1:]))
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
		case "--max-turns":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &maxTurns)
				i++
			}
		default:
			taskParts = append(taskParts, a)
		}
	}
	task := strings.TrimSpace(strings.Join(taskParts, " "))
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	if model == "" {
		detected, err := provider.DetectDefaultModel(creds.Has)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		model = detected
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
	k, err := kit.Open(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mason:", err)
		return 1
	}
	defer k.Close()
	// Delta-index so the graph reflects the CURRENT tree — answers from a
	// stale graph are wrong answers. Cheap when nothing changed.
	if _, err := k.Invoke("prism_index", map[string]any{}); err != nil {
		fmt.Fprintln(os.Stderr, "mason: index:", err)
		return 1
	}

	permit := func(action string) bool {
		if yes {
			return true
		}
		if !interactive {
			fmt.Fprintf(os.Stderr, "denied (non-interactive without --yes): %s\n", action)
			return false
		}
		fmt.Printf("  allow? %s [y/N] ", action)
		r := bufio.NewReader(os.Stdin)
		ans, _ := r.ReadString('\n')
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y")
	}

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
	sess := agent.New(p, k.Invoke, agent.Options{
		Root: root, MaxTurns: maxTurns, Permit: permit,
		ProjectNotes: projectNotes(root), CtxChars: ctxChars,
		Stream: true, Color: colorOut, NewProvider: factory,
	})
	sessFile := sessionPath(root)
	if cont {
		if msgs, m, err := loadSession(sessFile); err == nil {
			sess.SetHistory(msgs)
			fmt.Fprintf(os.Stderr, "resumed %d messages (last model %s)\n", len(msgs), m)
		} else {
			fmt.Fprintln(os.Stderr, "no previous session to continue")
		}
	}

	if task != "" {
		_, err := sess.Ask(task)
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
			before, after, err := sess.Compact()
			if err != nil {
				fmt.Fprintln(os.Stderr, "compact:", err)
			} else {
				fmt.Printf("compacted %d → %d chars\n", before, after)
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
		_, err = sess.Ask(line)
		saveSession(sessFile, sess.History(), model)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
		}
	}
	printCost(sess, model)
	printSavings(k)
	return 0
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
	cost := provider.EstimateCost(model, in, out)
	if cost > 0 {
		fmt.Fprintf(os.Stderr, "usage: %d in / %d out tokens ≈ $%.4f (list-price estimate)\n", in, out, cost)
	} else {
		fmt.Fprintf(os.Stderr, "usage: %d in / %d out tokens ($0 local or unpriced model)\n", in, out)
	}
}

func printSavings(k *kit.Kit) {
	s := k.Savings()
	if s.OriginalTokens == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nledger: %d tokens delivered where raw reads would have cost %d (%.1f%% saved)\n",
		s.DeliveredTokens, s.OriginalTokens, s.SavedPercent)
}
