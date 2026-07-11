package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

// fakeProvider replays scripted replies and records what the model was shown.
type fakeProvider struct {
	replies []provider.Msg
	i       int
	seen    []string // tool-result contents shown to the model
	turns   []struct {
		toolNames []string
		forced    bool
	}
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Chat(_ context.Context, msgs []provider.Msg, tools []provider.ToolDef, force bool) (provider.Msg, error) {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}
	f.turns = append(f.turns, struct {
		toolNames []string
		forced    bool
	}{names, force})
	for _, m := range msgs {
		if m.Role == "tool" {
			f.seen = append(f.seen, m.Content)
		}
	}
	if f.i >= len(f.replies) {
		return provider.Msg{Role: "assistant", Content: "done"}, nil
	}
	r := f.replies[f.i]
	f.i++
	return r, nil
}

// Graph-op payloads must never enter the model's context — only compact
// metadata. This is mason's core structural guarantee.
func TestGraphPayloadIsolation(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "change_impact",
			Args: map[string]any{"symbol": "DataKeyCache.GetById"}}}},
		{Role: "assistant", Content: "11 sites, closed."},
	}}
	secret := "pkg/secret/encryption/manager/oss_dek_cache.go"
	invoke := func(tool string, args map[string]any) (any, error) {
		if tool != "prism_change_impact" {
			t.Fatalf("routed to %q", tool)
		}
		return map[string]any{
			"declarations": []any{map[string]any{"filePath": secret, "name": "GetById"}},
			"callers":      []any{map[string]any{"filePath": secret, "name": "Use"}},
			"totalSites":   float64(11),
			"completeness": "closed",
		}, nil
	}
	s := New(fp, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	reply, err := s.Ask(context.Background(), "find every caller of DataKeyCache.GetById")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "11 sites, closed." {
		t.Fatalf("reply = %q", reply)
	}
	for _, seen := range fp.seen {
		if strings.Contains(seen, secret) {
			t.Fatalf("payload leaked into model context: %s", seen)
		}
	}
	if len(fp.seen) == 0 {
		t.Fatal("model never saw a tool result")
	}
}

// A graph-shaped task's first turn must be walled onto the graph tools with
// forced invocation; later turns get the full set unforced.
func TestInvocationWall(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "dead_code", Args: map[string]any{}}}},
		{Role: "assistant", Content: "done"},
	}}
	invoke := func(tool string, args map[string]any) (any, error) {
		return map[string]any{"dead": []any{}}, nil
	}
	s := New(fp, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	if _, err := s.Ask(context.Background(), "clean up any dead code"); err != nil {
		t.Fatal(err)
	}
	if !fp.turns[0].forced {
		t.Fatal("first turn of a graph-shaped task must force a tool call")
	}
	for _, n := range fp.turns[0].toolNames {
		if n == "bash" || n == "grep" || n == "edit_file" {
			t.Fatalf("wall leaked non-graph tool %q into turn 0", n)
		}
	}
	if len(fp.turns) < 2 || fp.turns[1].forced {
		t.Fatal("turn 1 must be unforced with the full toolset")
	}
	found := false
	for _, n := range fp.turns[1].toolNames {
		if n == "bash" {
			found = true
		}
	}
	if !found {
		t.Fatal("turn 1 must include the full toolset")
	}
}

// A non-graph task must NOT be walled.
func TestNoWallForPlainTasks(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "hi"}}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	if _, err := s.Ask(context.Background(), "explain what this repository does"); err != nil {
		t.Fatal(err)
	}
	if fp.turns[0].forced {
		t.Fatal("plain task must not be forced")
	}
}

// applyRenamePlan: applies confirmed edits, skips drifted lines, never
// corrupts, and only touches ambiguous when opted in.
func TestApplyRenamePlan(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc GetById(id string) string {\n\treturn id\n}\n"
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{
		"edits": []any{
			map[string]any{"filePath": "x.go", "line": float64(3),
				"before": "func GetById(id string) string {",
				"after":  "func GetDataKeyById(id string) string {"},
			map[string]any{"filePath": "x.go", "line": float64(4),
				"before": "\tSOMETHING ELSE", "after": "\tcorrupted"},
		},
		"ambiguous": []any{
			map[string]any{"filePath": "x.go", "line": float64(4),
				"before": "\treturn id", "after": "\treturn id // amb"},
		},
	}
	_, _, ambLeft, err := applyRenamePlan(io.Discard, dir, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if ambLeft != 1 {
		t.Fatalf("ambiguousLeft = %d, want 1", ambLeft)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "GetDataKeyById(id string)") {
		t.Fatal("confirmed edit not applied")
	}
	if strings.Contains(string(got), "corrupted") {
		t.Fatal("drifted line was overwritten")
	}
	if strings.Contains(string(got), "// amb") {
		t.Fatal("ambiguous edit applied without opt-in")
	}
	if _, _, _, err := applyRenamePlan(io.Discard, dir, plan, true); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "// amb") {
		t.Fatal("ambiguous edit not applied under opt-in")
	}
}

// edit_file must refuse zero and multiple matches.
func TestEditFileExactness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "y.go")
	os.WriteFile(path, []byte("a\nb\na\n"), 0o644)
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: io.Discard})
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "y.go", "old_text": "a\n", "new_text": "z\n"}}); err == nil {
		t.Fatal("multi-match edit must fail")
	}
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "y.go", "old_text": "missing", "new_text": "z"}}); err == nil {
		t.Fatal("no-match edit must fail")
	}
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "y.go", "old_text": "b\n", "new_text": "z\n"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nz\na\n" {
		t.Fatalf("edit result = %q", got)
	}
}

// Denied permission must surface as a tool error, not execute.
func TestPermissionGate(t *testing.T) {
	dir := t.TempDir()
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: io.Discard,
		Permit: func(string) bool { return false }})
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "touch " + filepath.Join(dir, "no.txt")}}); err == nil {
		t.Fatal("denied bash must error")
	}
	if _, err := os.Stat(filepath.Join(dir, "no.txt")); err == nil {
		t.Fatal("denied command still ran")
	}
}

// A mutation-intent task whose model never runs a mutating tool must get one
// corrective nudge instead of an accepted fabricated summary.
func TestHonestyGuard(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Content: "I added the constant."}, // fabrication
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Args: map[string]any{"path": "z.txt", "content": "x"}}}},
		{Role: "assistant", Content: "now actually done"},
	}}
	dir := t.TempDir()
	s := New(fp, nil, Options{Root: dir, Out: io.Discard})
	reply, err := s.Ask(context.Background(), "add a constant DemoMarker to version.go")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "now actually done" {
		t.Fatalf("fabricated summary was accepted: %q", reply)
	}
	if _, err := os.Stat(filepath.Join(dir, "z.txt")); err != nil {
		t.Fatal("mutating tool did not run after nudge")
	}
}

// A read-only task must not trigger the guard.
func TestHonestyGuardSkipsReadOnly(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "it does X"}}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	reply, err := s.Ask(context.Background(), "explain what this repository does")
	if err != nil || reply != "it does X" {
		t.Fatalf("read-only reply rejected: %q %v", reply, err)
	}
}

// Compaction must shrink history, keep the system prompt, and never leave a
// dangling tool-result at the head of the kept tail.
func TestCompact(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Content: "SUMMARY OF EVERYTHING"},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	for i := 0; i < 6; i++ {
		s.msgs = append(s.msgs,
			provider.Msg{Role: "user", Content: strings.Repeat("x", 500)},
			provider.Msg{Role: "assistant", Content: strings.Repeat("y", 500)})
	}
	s.msgs = append(s.msgs, provider.Msg{Role: "tool", CallID: "1", Name: "grep", Content: "zzz"})
	before, after, err := s.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Fatalf("no shrink: %d -> %d", before, after)
	}
	if s.msgs[0].Role != "system" {
		t.Fatal("system prompt lost")
	}
	if !strings.Contains(s.msgs[1].Content, "SUMMARY OF EVERYTHING") {
		t.Fatal("summary missing")
	}
	for _, m := range s.msgs {
		if m.Role == "tool" && s.msgs[1].Role == "tool" {
			t.Fatal("dangling tool result at head of tail")
		}
	}
}

// SetHistory keeps the fresh system prompt and restores the rest.
func TestSetHistory(t *testing.T) {
	s := New(&fakeProvider{}, nil, Options{Root: t.TempDir(), Out: io.Discard,
		ProjectNotes: "PROJECT RULES"})
	if !strings.Contains(s.msgs[0].Content, "PROJECT RULES") {
		t.Fatal("project notes not in system prompt")
	}
	s.SetHistory([]provider.Msg{
		{Role: "system", Content: "OLD STALE PROMPT"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	})
	if strings.Contains(s.msgs[0].Content, "OLD STALE PROMPT") {
		t.Fatal("stale system prompt restored")
	}
	if len(s.msgs) != 3 || s.msgs[1].Content != "hello" {
		t.Fatalf("history restore wrong: %+v", s.msgs)
	}
}

// Subagent: isolated context (its reads never enter the parent history),
// only the summary returns; usage rolls up; depth is capped at one level.
func TestSubagent(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		// parent delegates
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "subagent",
			Args: map[string]any{"task": "survey the config loading code"}}}},
		// subagent reads a file, then summarizes
		{Role: "assistant", Usage: &provider.Usage{In: 100, Out: 10},
			Calls: []provider.ToolCall{{ID: "s1", Name: "read_file", Args: map[string]any{"path": "cfg.go"}}}},
		{Role: "assistant", Usage: &provider.Usage{In: 200, Out: 20}, Content: "sub summary DONE"},
		// parent concludes
		{Role: "assistant", Content: "parent final"},
	}}
	invoke := func(tool string, args map[string]any) (any, error) {
		if tool == "prism_read" {
			return map[string]any{"content": "SECRETDATA func LoadConfig() {}"}, nil
		}
		return map[string]any{}, nil
	}
	s := New(fp, invoke, Options{Root: t.TempDir(), Out: io.Discard, MaxTurns: 10})
	reply, err := s.Ask(context.Background(), "survey the repository configuration")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "parent final" {
		t.Fatalf("reply = %q", reply)
	}
	var sawSummary bool
	for _, m := range s.History() {
		if strings.Contains(m.Content, "SECRETDATA") {
			t.Fatal("subagent's raw read leaked into parent context")
		}
		if m.Role == "tool" && strings.Contains(m.Content, "sub summary DONE") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("subagent summary did not return to parent")
	}
	in, out := s.Usage()
	if in != 300 || out != 30 {
		t.Fatalf("subagent usage not rolled up: %d/%d", in, out)
	}
}

func TestSubagentDepthGuard(t *testing.T) {
	s := New(&fakeProvider{}, nil, Options{Root: t.TempDir(), Out: io.Discard, Depth: 1, MaxTurns: 10})
	_, err := s.runSubagent(context.Background(), provider.ToolCall{Name: "subagent",
		Args: map[string]any{"task": "recurse"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot spawn") {
		t.Fatalf("depth guard missing: %v", err)
	}
}

// Model-supplied paths must never escape the project root.
func TestPathJail(t *testing.T) {
	dir := t.TempDir()
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: io.Discard})
	for _, tc := range []provider.ToolCall{
		{Name: "edit_file", Args: map[string]any{"path": "../escape.go", "old_text": "a", "new_text": "b"}},
		{Name: "write_file", Args: map[string]any{"path": "../../pwned.txt", "content": "x"}},
		{Name: "write_file", Args: map[string]any{"path": "/etc/mason-pwned", "content": "x"}},
		{Name: "grep", Args: map[string]any{"pattern": "x", "path": "../.."}},
		{Name: "list_files", Args: map[string]any{"path": "../.."}},
	} {
		if _, err := s.runCodingTool(context.Background(), tc); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("%s with %v must be jailed, got err=%v", tc.Name, tc.Args["path"], err)
		}
	}
	// In-root paths still work.
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "write_file",
		Args: map[string]any{"path": "sub/ok.txt", "content": "fine"}}); err != nil {
		t.Fatal(err)
	}
}

// A hung bash command must be cut off by the timeout, not hang the session.
func TestBashTimeout(t *testing.T) {
	t.Setenv("MASON_BASH_TIMEOUT", "1")
	s := New(&fakeProvider{}, nil, Options{Root: t.TempDir(), Out: io.Discard})
	res, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "TIMED OUT") {
		t.Fatalf("timeout not reported: %q", res)
	}
}

// A cancelled context stops the task cleanly and leaves the session usable.
func TestAskInterrupt(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "later"}}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Ask(ctx, "explain the code"); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("want interrupted, got %v", err)
	}
	// Session must still work afterwards.
	reply, err := s.Ask(context.Background(), "explain the code")
	if err != nil || reply != "later" {
		t.Fatalf("session unusable after interrupt: %q %v", reply, err)
	}
}

// Without the engine (invoke==nil) mason degrades: no graph tools offered,
// no wall, read_file falls back to a plain root-confined read.
func TestEngineUnavailableDegradation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("plain content"), 0o644)
	fp := &fakeProvider{replies: []provider.Msg{{Role: "assistant", Content: "done"}}}
	s := New(fp, nil, Options{Root: dir, Out: io.Discard})
	if _, err := s.Ask(context.Background(), "rename the Status method to StatusCode"); err != nil {
		t.Fatal(err)
	}
	if fp.turns[0].forced {
		t.Fatal("wall must be disabled without the engine")
	}
	for _, n := range fp.turns[0].toolNames {
		if _, isGraph := map[string]bool{"change_impact": true, "rename_plan": true}[n]; isGraph {
			t.Fatalf("graph tool %q offered without an engine", n)
		}
	}
	res, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "read_file",
		Args: map[string]any{"path": "f.txt"}})
	if err != nil || res != "plain content" {
		t.Fatalf("fallback read: %q %v", res, err)
	}
}

// Exhausting the turn budget must end in a forced wrap-up summary, not a
// hard failure — the tree state, not the turn count, is the truth.
func TestTurnExhaustionWrapsUp(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "grep", Args: map[string]any{"pattern": "x"}}}},
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "2", Name: "grep", Args: map[string]any{"pattern": "y"}}}},
		{Role: "assistant", Content: "wrap-up: work was completed"},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard, MaxTurns: 2})
	reply, err := s.Ask(context.Background(), "explain the code")
	if err != nil {
		t.Fatalf("turn exhaustion must not fail when a summary is available: %v", err)
	}
	if !strings.Contains(reply, "wrap-up") {
		t.Fatalf("reply = %q", reply)
	}
}

// The edit permission prompt must carry the -/+ diff of the exact change.
func TestEditPermissionShowsDiff(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("old line\n"), 0o644)
	var gotAction, gotDetail string
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: io.Discard,
		PermitDetail: func(action, detail string) bool {
			gotAction, gotDetail = action, detail
			return false // deny, we only care about the preview
		}})
	_, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "a.go", "old_text": "old line", "new_text": "new line"}})
	if err == nil {
		t.Fatal("denied edit must error")
	}
	if gotAction != "edit a.go" {
		t.Fatalf("action = %q", gotAction)
	}
	if !strings.Contains(gotDetail, "- old line") || !strings.Contains(gotDetail, "+ new line") {
		t.Fatalf("diff preview missing: %q", gotDetail)
	}
}

// write_file must disclose overwrite vs new file in the preview.
func TestWritePermissionDisclosesOverwrite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "exists.txt"), []byte("previous"), 0o644)
	var details []string
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: io.Discard,
		PermitDetail: func(_, detail string) bool {
			details = append(details, detail)
			return true
		}})
	s.runCodingTool(context.Background(), provider.ToolCall{Name: "write_file",
		Args: map[string]any{"path": "exists.txt", "content": "next"}})
	s.runCodingTool(context.Background(), provider.ToolCall{Name: "write_file",
		Args: map[string]any{"path": "fresh.txt", "content": "hello"}})
	if !strings.Contains(details[0], "OVERWRITES") {
		t.Fatalf("overwrite not disclosed: %q", details[0])
	}
	if !strings.Contains(details[1], "new file") {
		t.Fatalf("new file not disclosed: %q", details[1])
	}
}
