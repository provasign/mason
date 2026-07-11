// mason — a model-agnostic coding agent with the prism/grove code graph and
// shale evidence trail baked in. No steering files: tool routing, payload
// relay, and edit application are properties of the harness.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
  mason login <anthropic|openai>   store an API key in the OS keychain
  mason logout <anthropic|openai>  remove a stored API key
  mason version

flags:
  --model <spec>   ollama:<tag> | claude:<model> | openai:<model>  (default: auto-detect)
  --dir <path>     project root (default: current directory)
  --yes            skip permission prompts for bash/edit/write
  --max-turns <n>  per-task turn budget (default 30)

credentials: env var (ANTHROPIC_API_KEY / OPENAI_API_KEY) > OS keychain >
interactive prompt. Keys are never written to config files, sessions, or logs.`

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

	sess := agent.New(p, k.Invoke, agent.Options{
		Root: root, MaxTurns: maxTurns, Permit: permit,
	})

	if task != "" {
		reply, err := sess.Ask(task)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			return 1
		}
		fmt.Printf("\n%s\n", reply)
		printSavings(k)
		return 0
	}

	if !interactive {
		fmt.Fprintln(os.Stderr, "mason: no task given and stdin is not a terminal")
		return 2
	}

	// REPL: one conversation, many tasks.
	fmt.Println(`type a task, "/savings" for the token ledger, "/exit" to quit`)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nmason> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "/exit" || line == "/quit":
			printSavings(k)
			return 0
		case line == "/savings":
			printSavings(k)
			continue
		}
		reply, err := sess.Ask(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mason:", err)
			continue
		}
		fmt.Printf("\n%s\n", reply)
	}
	printSavings(k)
	return 0
}

func printSavings(k *kit.Kit) {
	s := k.Savings()
	if s.OriginalTokens == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nledger: %d tokens delivered where raw reads would have cost %d (%.1f%% saved)\n",
		s.DeliveredTokens, s.OriginalTokens, s.SavedPercent)
}
