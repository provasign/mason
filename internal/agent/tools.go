package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/provasign/mason/internal/provider"
)

// Invoker runs one prism operation by MCP name — satisfied by (*kit.Kit).Invoke.
type Invoker func(tool string, args map[string]any) (any, error)

// Two tool classes, two delivery contracts:
//
//   - GRAPH ops (change_impact, rename_plan, …): the payload renders to the
//     user; the model sees counts/flags only. Relay fidelity by construction.
//   - CODING tools (read_file, grep, edit_file, …): the model needs the
//     content to write code, so it gets it — but read_file is prism_read,
//     so repeat reads of unchanged files cost a ~10-token SHA pointer and
//     every read is ledgered.
func toolDefs() []provider.ToolDef {
	obj := func(props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []provider.ToolDef{
		// --- graph operations (payload-isolated) ---
		{Name: "search_symbols", Description: "Find symbols by name fragment to locate or disambiguate a target.",
			Parameters: obj(map[string]any{"query": str("name fragment")}, "query")},
		{Name: "change_impact", Description: "COMPLETE change-set for a method/interface signature change or deprecation: declaration, override family, callers, declaring types — one call, type-resolved. Always use this instead of grep to find 'every site that must change' or 'all callers'.",
			Parameters: obj(map[string]any{"symbol": str("Type.method, e.g. JsonSerializer.serialize")}, "symbol")},
		{Name: "rename_plan", Description: "Rename a method/member: every concrete edit line with before/after. The harness applies the edits — never edit rename sites by hand.",
			Parameters: obj(map[string]any{"symbol": str("Type.method"), "newName": str("new member name")}, "symbol", "newName")},
		{Name: "apply_rename_plan", Description: "Apply the most recent rename_plan to the working tree. Set includeAmbiguous=true only when a verify command will run.",
			Parameters: obj(map[string]any{"includeAmbiguous": map[string]any{"type": "boolean"}})},
		{Name: "missing_implementations", Description: "Every type in a contract's closure lacking an implementation ('who breaks if X becomes required').",
			Parameters: obj(map[string]any{"symbol": str("Type.method")}, "symbol")},
		{Name: "untested_surface", Description: "The change-set for a symbol split into test-covered and untested sites.",
			Parameters: obj(map[string]any{"symbol": str("Type.method")}, "symbol")},
		{Name: "dead_code", Description: "Unreachable production symbols: safe-to-delete list with caveats.",
			Parameters: obj(map[string]any{})},

		// --- coding tools (content delivered to the model) ---
		{Name: "read_file", Description: "Read a file (graph-aware, session-compressed: a repeat read of an unchanged file returns a short cached-pointer line meaning you ALREADY have the content earlier in this conversation — use that copy, do not re-request).",
			Parameters: obj(map[string]any{"path": str("path relative to project root")}, "path")},
		{Name: "grep", Description: "Search file CONTENTS for a pattern (regex). Use for strings/config/docs. For callers, overrides, or change-sets of a known symbol, use change_impact instead — grep misses type-resolved sites.",
			Parameters: obj(map[string]any{"pattern": str("regex"), "path": str("optional subdirectory or file")}, "pattern")},
		{Name: "list_files", Description: "List files under a directory (recursive, name filter optional).",
			Parameters: obj(map[string]any{"path": str("directory, default root"), "filter": str("optional substring")})},
		{Name: "edit_file", Description: "Replace an exact text snippet in a file. old_text must match exactly once.",
			Parameters: obj(map[string]any{"path": str("file path"), "old_text": str("exact existing text"), "new_text": str("replacement")}, "path", "old_text", "new_text")},
		{Name: "write_file", Description: "Create or overwrite a whole file.",
			Parameters: obj(map[string]any{"path": str("file path"), "content": str("full file content")}, "path", "content")},
		{Name: "bash", Description: "Run a shell command in the project root (build, test, git). Output is truncated.",
			Parameters: obj(map[string]any{"command": str("shell command")}, "command")},
		{Name: "subagent", Description: "Delegate a self-contained subtask (broad exploration, multi-file survey, isolated analysis or change) to a fresh agent with its own empty context and the same tools. Only its final summary returns to you — its intermediate reads never consume your context. Use for work whose raw output would be large.",
			Parameters: obj(map[string]any{
				"task":  str("complete, self-contained instructions for the subagent"),
				"model": str("optional model override, e.g. ollama:qwen2.5-coder:14b for cheap exploration")}, "task")},
	}
}

// graphOps maps model-facing graph tools to prism MCP names. Presence in
// this map is what routes a call through the payload-isolation path.
var graphOps = map[string]string{
	"search_symbols":          "prism_search",
	"change_impact":           "prism_change_impact",
	"rename_plan":             "prism_rename_plan",
	"missing_implementations": "prism_missing_implementations",
	"untested_surface":        "prism_untested_surface",
	"dead_code":               "prism_dead_code",
}

// runGraphOp invokes the prism operation behind a graph tool call and
// returns (compact metadata for the model, full result for rendering).
func runGraphOp(call provider.ToolCall, invoke Invoker) (string, map[string]any, error) {
	tool := graphOps[call.Name]
	args := map[string]any{}
	switch call.Name {
	case "search_symbols":
		args["query"], _ = call.Args["query"].(string)
		args["limit"] = 10
	case "rename_plan":
		args["query"], _ = call.Args["symbol"].(string)
		args["newName"], _ = call.Args["newName"].(string)
	case "dead_code":
		// no args
	default:
		args["query"], _ = call.Args["symbol"].(string)
	}
	res, err := invoke(tool, args)
	if err != nil {
		return "", nil, err
	}
	full, ok := res.(map[string]any)
	if !ok {
		full = map[string]any{}
	}
	return compactMeta(call.Name, full), full, nil
}

const maxToolOutput = 24_000 // chars of tool output delivered to the model

// bashTimeout bounds a single bash tool call so one hung command cannot hang
// the whole session. MASON_BASH_TIMEOUT (seconds) overrides.
func bashTimeout() time.Duration {
	if v := os.Getenv("MASON_BASH_TIMEOUT"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

// colorDiff indents and colors -/+ lines for the transcript.
func (s *Session) colorDiff(d string) string {
	lines := strings.Split(d, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "- "):
			lines[i] = "    " + s.st.red(l)
		case strings.HasPrefix(l, "+ "):
			lines[i] = "    " + s.st.green(l)
		default:
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n")
}

// diffSnippet renders a -/+ preview of a text replacement, capped so huge
// edits do not flood the permission prompt.
func diffSnippet(oldText, newText string) string {
	const capLines = 40
	var b strings.Builder
	oldLines := strings.Split(strings.TrimRight(oldText, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(newText, "\n"), "\n")
	n := 0
	for _, l := range oldLines {
		if n >= capLines {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("- " + l + "\n")
		n++
	}
	for _, l := range newLines {
		if n >= capLines*2 {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("+ " + l + "\n")
		n++
	}
	return strings.TrimRight(b.String(), "\n")
}

// inRoot confines a model-supplied path to the project root. Symlinks and
// ../ traversal cannot escape: the CLEANED absolute path must sit under root.
func (s *Session) inRoot(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	abs := rel
	// Rooted-but-driveless paths ("/etc/x") are not IsAbs on Windows yet are
	// clearly absolute intent — never join them under root.
	rooted := filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\")
	if !rooted {
		abs = filepath.Join(s.root, rel)
	}
	clean := filepath.Clean(abs)
	root := filepath.Clean(s.root)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the project root", rel)
	}
	return clean, nil
}

// runCodingTool executes a content-bearing tool and returns the model-facing
// result string.
func (s *Session) runCodingTool(ctx context.Context, call provider.ToolCall) (string, error) {
	switch call.Name {
	case "read_file":
		path, _ := call.Args["path"].(string)
		s.setStatus("reading %s", path)
		if s.invoke == nil {
			// Engine unavailable: plain read, still root-confined.
			abs, err := s.inRoot(path)
			if err != nil {
				return "", err
			}
			b, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			return truncate(string(b), maxToolOutput), nil
		}
		res, err := s.invoke("prism_read", map[string]any{"file": path})
		if err != nil {
			return "", err
		}
		m, _ := res.(map[string]any)
		content, _ := m["content"].(string)
		return truncate(content, maxToolOutput), nil

	case "lookup":
		name, _ := call.Args["name"].(string)
		s.setStatus("looking up %s", name)
		if s.invoke == nil {
			return "", fmt.Errorf("code graph unavailable — use read_file")
		}
		res, err := s.invoke("prism_lookup", map[string]any{"name": name})
		if err != nil {
			return "", err
		}
		m, _ := res.(map[string]any)
		content, _ := m["content"].(string)
		if content == "" {
			return "", fmt.Errorf("no symbol named %q in the graph — try search_symbols", name)
		}
		return truncate(content, maxToolOutput), nil

	case "grep":
		pattern, _ := call.Args["pattern"].(string)
		sub, _ := call.Args["path"].(string)
		s.setStatus("searching for %s", truncate(pattern, 40))
		target := s.root
		if sub != "" {
			t, err := s.inRoot(sub)
			if err != nil {
				return "", err
			}
			target = t
		}
		out, _ := exec.Command("grep", "-rn", "-E", "--exclude-dir=.git",
			"--exclude-dir=.grove", "-I", pattern, target).CombinedOutput()
		res := strings.ReplaceAll(string(out), s.root+string(filepath.Separator), "")
		if strings.TrimSpace(res) == "" {
			return "(no matches)", nil
		}
		return truncate(res, maxToolOutput), nil

	case "list_files":
		sub, _ := call.Args["path"].(string)
		filter, _ := call.Args["filter"].(string)
		s.setStatus("listing files…")
		target := s.root
		if sub != "" {
			t, err := s.inRoot(sub)
			if err != nil {
				return "", err
			}
			target = t
		}
		var files []string
		_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() && (name == ".git" || name == ".grove" || name == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && (filter == "" || strings.Contains(name, filter)) {
				rel, _ := filepath.Rel(s.root, p)
				files = append(files, rel)
			}
			return nil
		})
		if len(files) == 0 {
			return "(no files)", nil
		}
		return truncate(strings.Join(files, "\n"), maxToolOutput), nil

	case "edit_file":
		path, _ := call.Args["path"].(string)
		s.setStatus("editing %s", path)
		oldText, _ := call.Args["old_text"].(string)
		newText, _ := call.Args["new_text"].(string)
		v := VerdictAsk
		if s.opts.Policy != nil {
			v = s.opts.Policy.PathVerdict("edit", path)
		}
		if ok, why := s.gate(v, "edit "+path, diffSnippet(oldText, newText)); !ok {
			return "", fmt.Errorf("%s: edit of %s", why, path)
		}
		abs, aerr := s.inRoot(path)
		if aerr != nil {
			return "", aerr
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", err
		}
		n := strings.Count(string(data), oldText)
		if n == 0 {
			return "", fmt.Errorf("old_text not found in %s", path)
		}
		if n > 1 {
			return "", fmt.Errorf("old_text matches %d times in %s — provide a larger unique snippet", n, path)
		}
		if err := os.WriteFile(abs, []byte(strings.Replace(string(data), oldText, newText, 1)), 0o644); err != nil {
			return "", err
		}
		s.mutated = true
		fmt.Fprintf(s.out, "  ✎ %s\n%s\n", path, s.colorDiff(diffSnippet(oldText, newText)))
		return "edit applied", nil

	case "write_file":
		path, _ := call.Args["path"].(string)
		s.setStatus("writing %s", path)
		content, _ := call.Args["content"].(string)
		abs, aerr := s.inRoot(path)
		if aerr != nil {
			return "", aerr
		}
		detail := fmt.Sprintf("(new file, %d bytes)", len(content))
		if prev, err := os.ReadFile(abs); err == nil {
			detail = fmt.Sprintf("(OVERWRITES existing file, %d → %d bytes)", len(prev), len(content))
		}
		head := content
		if lines := strings.SplitN(head, "\n", 21); len(lines) > 20 {
			head = strings.Join(lines[:20], "\n") + "\n  …"
		}
		v := VerdictAsk
		if s.opts.Policy != nil {
			v = s.opts.Policy.PathVerdict("write", path)
		}
		if ok, why := s.gate(v, "write "+path, detail+"\n"+head); !ok {
			return "", fmt.Errorf("%s: write of %s", why, path)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return "", err
		}
		s.mutated = true
		head = content
		if lines := strings.SplitN(head, "\n", 13); len(lines) > 12 {
			head = strings.Join(lines[:12], "\n") + "\n  …"
		}
		var hb strings.Builder
		for _, l := range strings.Split(head, "\n") {
			hb.WriteString("+ " + l + "\n")
		}
		fmt.Fprintf(s.out, "  ✎ %s (%d bytes)\n%s", path, len(content), s.colorDiff(strings.TrimRight(hb.String(), "\n"))+"\n")
		return "file written", nil

	case "bash":
		command, _ := call.Args["command"].(string)
		v := VerdictAsk
		if s.opts.Policy != nil {
			v = s.opts.Policy.BashVerdict(command)
		}
		if ok, why := s.gate(v, "run: "+command, ""); !ok {
			return "", fmt.Errorf("%s: command", why)
		}
		s.setStatus("running: %s", truncate(command, 50))
		fmt.Fprintf(s.out, "  $ %s\n", command)
		// bash can mutate the tree (sed, git, generators) — count it, so the
		// honesty guard never false-fires on legitimate shell-made changes.
		s.mutated = true
		cctx, cancel := context.WithTimeout(ctx, bashTimeout())
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", command)
		cmd.Dir = s.root
		// Kill the whole process GROUP on cancel (platform-specific), and
		// force Wait to return even if grandchildren keep the output pipe
		// open — otherwise cancelling during `go build`/`go test` hangs on
		// compiler children that inherited the pipe, and Ctrl+C appears dead.
		configureBashCancel(cmd)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		res := truncate(string(out), maxToolOutput)
		if cctx.Err() == context.DeadlineExceeded {
			return res + "\n(command TIMED OUT after " + bashTimeout().String() + ")", nil
		}
		if err != nil {
			return res + "\n(exit error: " + err.Error() + ")", nil
		}
		if strings.TrimSpace(res) == "" {
			return "(no output, exit 0)", nil
		}
		return res, nil
	}
	return "", fmt.Errorf("unknown tool %q", call.Name)
}
