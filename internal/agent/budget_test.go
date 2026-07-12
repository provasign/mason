package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

// tokenCost prices every token at $0.001 so budgets are easy to hit.
func tokenCost(in, out, _, _ int) float64 { return float64(in+out) / 1000 }

func TestCostBudgetStopsPendingWork(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Usage: &provider.Usage{In: 900, Out: 200},
			Calls: []provider.ToolCall{{ID: "1", Name: "list_files", Args: map[string]any{}}}},
		{Role: "assistant", Content: "should never be reached"},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard,
		MaxCostUSD: 1.00, CostFn: tokenCost})
	_, err := s.Ask(context.Background(), "explore the repo")
	if err == nil || !strings.Contains(err.Error(), "cost budget") {
		t.Fatalf("budget must stop the task, got %v", err)
	}
	if fp.i != 1 {
		t.Fatalf("no further model calls after the budget hit, got %d", fp.i)
	}
	// The conversation must stay coherent for the next Ask.
	if last := s.msgs[len(s.msgs)-1]; last.Role != "assistant" {
		t.Fatalf("dangling %s message after budget stop", last.Role)
	}
}

func TestCostBudgetDeliversFinishedAnswer(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Content: "the answer", Usage: &provider.Usage{In: 5000, Out: 5000}},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard,
		MaxCostUSD: 1.00, CostFn: tokenCost})
	reply, err := s.Ask(context.Background(), "what is this repo")
	if err != nil || reply != "the answer" {
		t.Fatalf("a paid-for final answer must be delivered: %q %v", reply, err)
	}
}

func TestNoBudgetNoStop(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Content: "fine", Usage: &provider.Usage{In: 1 << 20, Out: 1 << 20}},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard, CostFn: tokenCost})
	if _, err := s.Ask(context.Background(), "hi"); err != nil {
		t.Fatalf("no MaxCostUSD means no budget enforcement: %v", err)
	}
}
