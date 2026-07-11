package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/provasign/prism/pkg/kit"
)

// Manual live probe: MASON_PROBE_DIR=<repo> go test -run LiveEngineProbe
func TestLiveEngineProbe(t *testing.T) {
	dir := os.Getenv("MASON_PROBE_DIR")
	if dir == "" {
		t.Skip("set MASON_PROBE_DIR to run")
	}
	k, err := kit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	s := New(&fakeProvider{}, k.Invoke, Options{Root: dir, Out: io.Discard,
		FileSymbols: func(path string) []SymbolInfo {
			syms, err := k.FileSymbols(context.Background(), path)
			if err != nil {
				fmt.Println("filesymbols err:", err)
				return nil
			}
			out := make([]SymbolInfo, 0, len(syms))
			for _, x := range syms {
				out = append(out, SymbolInfo{Name: x.Name, QualifiedName: x.QualifiedName, Kind: x.Kind, Line: x.Line})
			}
			return out
		}})
	untested, dead := s.engineChecks([]string{"src/stats.py"})
	fmt.Println("UNTESTED:")
	for _, u := range untested {
		fmt.Println("  ", u.String())
	}
	fmt.Println("DEAD:")
	for _, d := range dead {
		fmt.Println("  ", d.String())
	}
}
