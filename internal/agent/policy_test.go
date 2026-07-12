package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyBashVerdicts(t *testing.T) {
	p := &Policy{
		Bash:      "ask",
		BashAllow: []string{"go test *", "go build *", "git status"},
		BashDeny:  []string{"git push *", "rm -rf *"},
	}
	if p.BashVerdict("go test ./...") != VerdictAllow {
		t.Fatal("allow pattern must allow")
	}
	if p.BashVerdict("git status") != VerdictAllow {
		t.Fatal("exact allow must allow")
	}
	if p.BashVerdict("git push origin main") != VerdictDeny {
		t.Fatal("deny pattern must deny")
	}
	if p.BashVerdict("rm -rf /") != VerdictDeny {
		t.Fatal("rm -rf must deny")
	}
	if p.BashVerdict("cargo build") != VerdictAsk {
		t.Fatal("unlisted must ask")
	}
	// deny wins over allow
	p2 := &Policy{BashAllow: []string{"git *"}, BashDeny: []string{"git push *"}}
	if p2.BashVerdict("git push origin") != VerdictDeny {
		t.Fatal("deny must win over allow")
	}
}

func TestPolicyPathVerdicts(t *testing.T) {
	p := &Policy{Edit: "allow", Write: "ask",
		PathsDeny: []string{".env*", "secrets/**", "*.pem"}}
	if p.PathVerdict("edit", "src/main.go") != VerdictAllow {
		t.Fatal("edit mode allow")
	}
	if p.PathVerdict("write", "src/main.go") != VerdictAsk {
		t.Fatal("write mode ask")
	}
	for _, denied := range []string{".env", ".env.local", "secrets/prod/key.txt", "certs/server.pem"} {
		if p.PathVerdict("edit", denied) != VerdictDeny {
			t.Fatalf("%s must be denied", denied)
		}
	}
}

func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".mason"), 0o755)
	os.WriteFile(filepath.Join(dir, ".mason", "config.json"), []byte(`{
		"permissions": {"bash": "allow", "paths_deny": [".env*"]}
	}`), 0o644)
	p := LoadPolicy(dir)
	if p.Bash != "allow" || len(p.PathsDeny) != 1 {
		t.Fatalf("policy not loaded: %+v", p)
	}
	if LoadPolicy(t.TempDir()).Bash != "" {
		t.Fatal("missing config must load an empty policy")
	}
}
