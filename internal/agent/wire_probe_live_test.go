package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/prism/pkg/kit"
)

// Live probe of the v0.55.7 wiring: ranged read_file and graph-aware grep,
// through the real dispatch path against a real indexed repo.
func TestLiveWireProbe(t *testing.T) {
	dir := os.Getenv("MASON_PROBE_DIR")
	if dir == "" {
		t.Skip("set MASON_PROBE_DIR")
	}
	k, err := kit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	s := New(&fakeProvider{}, k.Invoke, Options{Root: dir, Out: os.Stderr})

	out, err := s.runCodingTool(context.Background(), provider.ToolCall{
		Name: "read_file", Args: map[string]any{"path": "kinto/core/cache/memory.py", "offset": float64(1), "limit": float64(40)}})
	if err != nil {
		t.Fatalf("ranged read: %v", err)
	}
	if !strings.Contains(out, "lines 1-40 of") {
		t.Errorf("ranged header missing:\n%.200s", out)
	}
	t.Logf("ranged read: %d bytes", len(out))

	out, err = s.runCodingTool(context.Background(), provider.ToolCall{
		Name: "grep", Args: map[string]any{"pattern": "CacheBase.get"}})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "implementation") {
		t.Errorf("structural note missing from grep result:\n%.400s", out)
	}
	t.Logf("graph-aware grep head:\n%.500s", out)

	out, err = s.runCodingTool(context.Background(), provider.ToolCall{
		Name: "grep", Args: map[string]any{"pattern": "cache"}})
	if err != nil {
		t.Fatalf("big grep: %v", err)
	}
	if !strings.Contains(out, "graph rollup") {
		t.Errorf("hit rollup missing from truncated grep:\n%.400s", out)
	}
	t.Logf("rollup lines present: %v", strings.Contains(out, "graph rollup"))
}
