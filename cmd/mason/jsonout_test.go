package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestEmitJSONStdout(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	emitJSON(jsonResult{OK: true, Reply: "a <b> & c", Model: "m"})
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("stdout must carry exactly one JSON object: %v (%q)", err, b)
	}
	if m["changedFiles"] == nil {
		t.Fatal("nil changedFiles must serialize as [], not null")
	}
	if !strings.Contains(string(b), "a <b> & c") {
		t.Fatalf("HTML must not be escaped: %q", b)
	}
}

func TestChangedBetween(t *testing.T) {
	before := map[string]string{"a.go": " M", "b.go": "??"}
	after := map[string]string{"a.go": " M", "c.go": "??", "d.go": " M"}
	got := changedBetween(before, after)
	want := []string{"b.go", "c.go", "d.go"} // b reverted, c+d new changes
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if changedBetween(nil, nil) != nil {
		t.Fatal("empty in, empty out")
	}
}

func TestGitStatusLines(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %s", out)
	}
	if gitStatusLines(root) == nil {
		t.Fatal("clean repo must yield an empty (non-nil) map")
	}
	writeTree2(t, root, "x.txt", "hi")
	m := gitStatusLines(root)
	if m["x.txt"] != "??" {
		t.Fatalf("untracked file must appear, got %v", m)
	}
}

func writeTree2(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := exec.Command("sh", "-c", "printf '%s' '"+content+"' > "+root+"/"+rel).Run(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONResultShape(t *testing.T) {
	b, err := json.Marshal(jsonResult{OK: true, Reply: "r", Model: "m",
		Usage: jsonUsage{InputTokens: 1, OutputTokens: 2, CostUSD: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ok", "reply", "model", "usage", "changedFiles", "durationMs"} {
		if _, present := m[k]; !present {
			t.Errorf("json missing key %q: %s", k, b)
		}
	}
}
