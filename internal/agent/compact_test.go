package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func bigHistory() []provider.Msg {
	msgs := []provider.Msg{
		{Role: "user", Content: "add retry logic to the fetcher\nwith backoff"},
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "read_file",
			Args: map[string]any{"path": "fetch.go"}}}},
		{Role: "tool", Name: "read_file", CallID: "1",
			Content: "package fetch\n" + strings.Repeat("x", 5000)},
		{Role: "assistant", Content: "Added retries with exponential backoff; tests pass."},
		{Role: "user", Content: "now cap it at 3 attempts"},
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "2", Name: "edit_file",
			Args: map[string]any{"path": "fetch.go"}}}},
		{Role: "tool", Name: "edit_file", CallID: "2", Content: "edit applied"},
		{Role: "assistant", Content: "Capped at 3."},
	}
	return msgs
}

func TestCompactIsDeterministicAndModelFree(t *testing.T) {
	fp := &fakeProvider{}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	s.msgs = append(s.msgs, bigHistory()...)

	before, after, err := s.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.turns) != 0 {
		t.Fatal("deterministic compaction must make ZERO model calls")
	}
	if after >= before {
		t.Fatalf("compaction must shrink history: %d → %d", before, after)
	}
	if s.msgs[0].Role != "system" {
		t.Fatal("system prompt must survive")
	}
	digest := s.msgs[1].Content
	// The last 3 messages stay verbatim as the tail — the digest covers
	// everything BEFORE them.
	for _, want := range []string{"TASK: add retry logic", "read_file",
		"reply: Added retries", "TASK: now cap it at 3 attempts"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}
	if strings.Contains(digest, strings.Repeat("x", 500)) {
		t.Fatal("tool payloads must be truncated in the digest")
	}
	// The recent turn survives verbatim in the tail.
	tail := s.msgs[2:]
	if len(tail) == 0 || tail[0].Role == "tool" {
		t.Fatalf("kept tail must not begin with a dangling tool message: %+v", tail)
	}
	found := false
	for _, m := range tail {
		if strings.Contains(m.Content, "Capped at 3.") {
			found = true
		}
	}
	if !found {
		t.Fatal("most recent reply must survive verbatim in the tail")
	}
}

func TestCompactTwiceKeepsPriorLedger(t *testing.T) {
	s := New(&fakeProvider{}, nil, Options{Root: t.TempDir(), Out: io.Discard})
	s.msgs = append(s.msgs, bigHistory()...)
	if _, _, err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.msgs = append(s.msgs, bigHistory()...) // more work after the first compaction
	if _, _, err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest := s.msgs[1].Content
	if strings.Count(digest, "TASK: add retry logic") < 2 {
		t.Fatalf("second compaction must carry the first ledger forward:\n%s", digest)
	}
	// Exactly one header: the fresh wrapper; the embedded prior ledger's
	// header must have been stripped.
	if n := strings.Count(digest, "Ledger of earlier turns"); n != 1 {
		t.Fatalf("want exactly 1 ledger header, got %d:\n%s", n, digest)
	}
}

func TestDigestBudgetDropsOldestFirst(t *testing.T) {
	var msgs []provider.Msg
	for i := 0; i < 400; i++ {
		msgs = append(msgs, provider.Msg{Role: "user",
			Content: "task number " + strings.Repeat("y", 100)})
	}
	msgs = append(msgs, provider.Msg{Role: "user", Content: "THE FINAL TASK"})
	d := digestHistory(msgs)
	if len(d) > 14_000 {
		t.Fatalf("digest must respect its budget, got %d chars", len(d))
	}
	if !strings.Contains(d, "THE FINAL TASK") {
		t.Fatal("newest entries must survive the budget")
	}
	if !strings.Contains(d, "older ledger entries elided") {
		t.Fatal("elision must be marked")
	}
}
