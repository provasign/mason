package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/provasign/mason/internal/provider"
)

// isolate the session store under a throwaway HOME so tests never touch the
// user's real cache.
func isolateCache(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("LocalAppData", filepath.Join(tmp, "lad")) // Windows os.UserCacheDir
}

func TestSessionSaveListLoadRoundtrip(t *testing.T) {
	isolateCache(t)
	root := "/fake/project"
	msgs := []provider.Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "rename Foo.Bar to Baz across the repo"},
		{Role: "assistant", Content: "done"},
	}
	p1 := sessionPathFor(root, "20260101-000001")
	saveSession(p1, msgs, "ollama:qwen3:30b", "")
	time.Sleep(5 * time.Millisecond)
	p2 := sessionPathFor(root, "20260101-000002")
	saveSession(p2, msgs, "claude:sonnet", "release prep")

	metas := listSessions(root)
	if len(metas) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(metas))
	}
	if metas[0].Path != p2 {
		t.Fatal("newest session must sort first")
	}
	if metas[0].Label != "release prep" {
		t.Fatalf("name must win as label, got %q", metas[0].Label)
	}
	if !strings.HasPrefix(metas[1].Label, "rename Foo.Bar") {
		t.Fatalf("auto-title must come from the first user task, got %q", metas[1].Label)
	}
	if metas[0].Msgs != 2 {
		t.Fatalf("system prompt must not count as a message, got %d", metas[0].Msgs)
	}

	got, model, err := loadSession(p2)
	if err != nil || model != "claude:sonnet" || len(got) != 3 {
		t.Fatalf("roundtrip failed: %v %q %d", err, model, len(got))
	}
}

func TestSessionLegacyFileListed(t *testing.T) {
	isolateCache(t)
	root := "/fake/legacy-project"
	legacy := legacySessionPath(root)
	os.MkdirAll(filepath.Dir(legacy), 0o755)
	os.WriteFile(legacy, []byte(`{"model":"ollama:x","messages":[{"role":"user","content":"old task"}]}`), 0o600)

	metas := listSessions(root)
	if len(metas) != 1 || metas[0].ID != "legacy" {
		t.Fatalf("legacy session must be listed, got %+v", metas)
	}
	if metas[0].Label != "old task" {
		t.Fatalf("legacy title, got %q", metas[0].Label)
	}
	// Once a new-scheme session exists, the legacy file is superseded.
	saveSession(sessionPathFor(root, "20260101-000003"),
		[]provider.Msg{{Role: "user", Content: "new"}}, "m", "")
	metas = listSessions(root)
	if len(metas) != 1 || metas[0].ID == "legacy" {
		t.Fatalf("new-scheme sessions must supersede legacy, got %+v", metas)
	}
}

func TestSessionTitleSkipsHarnessNotes(t *testing.T) {
	title := sessionTitle([]provider.Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "[mason attached main.go, which the task mentions]\ncontent"},
		{Role: "user", Content: "explain the build\nsecond line"},
	})
	if title != "explain the build" {
		t.Fatalf("got %q", title)
	}
}

func TestSessionPathsAreRootScoped(t *testing.T) {
	isolateCache(t)
	a, b := sessionsDir("/proj/a"), sessionsDir("/proj/b")
	if a == b {
		t.Fatal("different roots must map to different session dirs")
	}
	if filepath.Dir(a) != filepath.Dir(legacySessionPath("/proj/a")) {
		t.Fatal("legacy file must live beside the per-root directory")
	}
	id := newSessionID()
	if len(id) != len("20060102-150405") {
		t.Fatalf("session id shape changed: %q", id)
	}
	if got := sessionPathFor("/proj/a", id); !strings.HasSuffix(got, id+".json") || !strings.HasPrefix(got, a) {
		t.Fatalf("sessionPathFor wrong: %q", got)
	}
}

func TestSaveSessionCapsHistory(t *testing.T) {
	isolateCache(t)
	msgs := make([]provider.Msg, 450)
	for i := range msgs {
		msgs[i] = provider.Msg{Role: "user", Content: "m"}
	}
	p := sessionPathFor("/fake/cap", "20260101-000004")
	saveSession(p, msgs, "m", "")
	got, _, err := loadSession(p)
	if err != nil || len(got) != 400 {
		t.Fatalf("history must be capped at 400, got %d (%v)", len(got), err)
	}
}
