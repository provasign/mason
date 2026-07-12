package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/provasign/mason/internal/tui"
)

// Custom slash commands: every .mason/commands/<name>.md becomes /<name>.
// The file body is a prompt template; $ARGUMENTS is replaced with whatever
// follows the command. Loaded fresh on each use so edits apply instantly.

// loadCommand resolves one custom command by name (without the slash).
func loadCommand(root, name string) (string, bool) {
	if strings.ContainsAny(name, `/\.`) {
		return "", false // path tricks are not command names
	}
	b, err := os.ReadFile(filepath.Join(root, ".mason", "commands", name+".md"))
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return "", false
	}
	return string(b), true
}

// expandCommand turns "/name the args" into the command's task text, or
// ok=false when no such custom command exists.
func expandCommand(root, line string) (task string, ok bool) {
	rest := strings.TrimPrefix(line, "/")
	name, args, _ := strings.Cut(rest, " ")
	tpl, ok := loadCommand(root, name)
	if !ok {
		return "", false
	}
	args = strings.TrimSpace(args)
	if strings.Contains(tpl, "$ARGUMENTS") {
		return strings.ReplaceAll(tpl, "$ARGUMENTS", args), true
	}
	if args != "" {
		return strings.TrimRight(tpl, "\n") + "\n\n" + args, true
	}
	return tpl, true
}

// listCommands names the custom commands available in this repo.
func listCommands(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, ".mason", "commands"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}

// commandsHelp renders the custom-command list for /help ("" when none).
func commandsHelp(root string) string {
	names := listCommands(root)
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("custom commands (.mason/commands/): /%s\n", strings.Join(names, "  /"))
}

// customCommandInfos builds {name, desc} entries for the TUI's slash-command
// autocomplete popup: desc is the template's first line, truncated.
func customCommandInfos(root string) []tui.CommandInfo {
	var out []tui.CommandInfo
	for _, name := range listCommands(root) {
		tpl, _ := loadCommand(root, name)
		desc := strings.TrimSpace(tpl)
		if i := strings.IndexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		if len(desc) > 60 {
			desc = desc[:60] + "…"
		}
		out = append(out, tui.CommandInfo{Name: name, Desc: desc})
	}
	return out
}

// replCommandNames names mason's built-in slash commands for the REPL's
// Tab-completer — sourced from tui.BuiltinCommands so the two surfaces
// (TUI popup, REPL completion) can never drift apart.
func replCommandNames() []string {
	names := make([]string, len(tui.BuiltinCommands))
	for i, c := range tui.BuiltinCommands {
		names[i] = c.Name
	}
	return names
}
