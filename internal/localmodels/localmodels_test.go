package localmodels

import "testing"

// Every catalog entry must be coherent: nonzero sizes and a note.
func TestCatalogSane(t *testing.T) {
	if len(Catalog) < 5 {
		t.Fatal("catalog too small to be useful")
	}
	seen := map[string]bool{}
	for _, m := range Catalog {
		if m.Tag == "" || m.DownloadGB <= 0 || m.MinRAMGB <= 0 || m.Note == "" {
			t.Fatalf("incoherent entry: %+v", m)
		}
		if seen[m.Tag] {
			t.Fatalf("duplicate tag %s", m.Tag)
		}
		seen[m.Tag] = true
	}
	if !seen["qwen3-coder:30b"] || !seen["qwen2.5-coder:14b"] {
		t.Fatal("blessed measured models missing from catalog")
	}
}

// Resource filtering: small machines must not be offered big models, and
// unknown RAM must be permissive (warn, don't block).
func TestFits(t *testing.T) {
	big := Model{Tag: "x", MinRAMGB: 24}
	if big.Fits(16) {
		t.Fatal("24GB model must not fit a 16GB machine")
	}
	if !big.Fits(24) || !big.Fits(64) {
		t.Fatal("fitting machines rejected")
	}
	if !big.Fits(0) {
		t.Fatal("unknown RAM must be permissive")
	}
}
