package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGenerateMasonMDGoProject(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":           "module example.com/demo\n\ngo 1.22\n",
		"cmd/demo/main.go": "package main\n\nfunc main() {}\n",
		"internal/lib/a.go": "package lib\n",
		"internal/lib/b.go": "package lib\n",
	})
	md := generateMasonMD(root, 42, 99)
	for _, want := range []string{
		"# example.com/demo",
		"Go (3)",
		"42 symbols, 99 edges",
		"`internal/` — 2 files",
		"`go build ./...`",
		"`go test ./...`",
		"`cmd/demo/main.go` — `func main`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("MASON.md missing %q:\n%s", want, md)
		}
	}
}

func TestGenerateMasonMDSkipsVendorAndGit(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.py":                 "print('x')\n",
		"vendor/dep/x.go":      "package dep\n",
		"node_modules/m/i.js":  "x\n",
		".git/objects/aa":      "bin\n",
	})
	md := generateMasonMD(root, 0, 0)
	if strings.Contains(md, "vendor") || strings.Contains(md, "node_modules") {
		t.Fatalf("vendored trees must be skipped:\n%s", md)
	}
	if !strings.Contains(md, "Python (1)") {
		t.Fatalf("language count wrong:\n%s", md)
	}
}

func TestRunInitWritesAndGuards(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	t.Chdir(root)
	if code := runInit(false); code != 0 {
		t.Fatalf("first init must succeed, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "MASON.md")); err != nil {
		t.Fatal("MASON.md must exist after init")
	}
	if code := runInit(false); code == 0 {
		t.Fatal("second init without --force must fail")
	}
	if code := runInit(true); code != 0 {
		t.Fatal("init --force must regenerate")
	}
}

func TestPackageJSONName(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json": "{\n  \"name\": \"my-app\",\n  \"version\": \"1.0.0\"\n}\n",
	})
	if got := packageJSONName(root); got != "my-app" {
		t.Fatalf("got %q", got)
	}
	if got := packageJSONName(t.TempDir()); got != "" {
		t.Fatalf("missing package.json must yield empty, got %q", got)
	}
}

func TestIntOf(t *testing.T) {
	if intOf(float64(7)) != 7 || intOf(3) != 3 || intOf("x") != 0 || intOf(nil) != 0 {
		t.Fatal("intOf conversions wrong")
	}
}

func TestBuildCommandsDetection(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json": `{"name":"web","scripts":{"build":"x","test":"y"}}`,
		"Makefile":     "all:\n\ttrue\n",
	})
	cmds := strings.Join(buildCommands(root), "|")
	for _, want := range []string{"npm run build", "npm run test", "make"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in %q", want, cmds)
		}
	}
	if strings.Contains(cmds, "go build") {
		t.Errorf("no go.mod, must not suggest go build: %q", cmds)
	}
}
