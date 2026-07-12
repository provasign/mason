package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func TestRedactSecrets(t *testing.T) {
	cases := map[string]string{
		"sk-ant-api03-AbCdEf123456789012345":        "anthropic-key",
		"key = sk-proj-AbCdEfGh1234567890AbCdEfGh":  "openai-key",
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456":      "github-token",
		"AKIAIOSFODNN7EXAMPLE":                      "aws-access-key",
		"AIzaSyA-1234567890abcdefghijklmnopqrstu":   "google-api-key",
		"xoxb-123456789012-abcdefghijk":             "slack-token",
		`api_key = "super-secret-value-12345"`:      "credential",
		`password: 'hunter2hunter2hunter2'`:         "credential",
	}
	for input, kind := range cases {
		out, n := redactSecrets(input)
		if n == 0 || !strings.Contains(out, "[REDACTED:"+kind+"]") {
			t.Errorf("%q not redacted as %s: %q (n=%d)", input, kind, out, n)
		}
	}
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow...\n-----END RSA PRIVATE KEY-----"
	if out, n := redactSecrets(pem); n == 0 || strings.Contains(out, "MIIEow") {
		t.Errorf("private key not redacted: %q", out)
	}
}

// A test file full of dummy credential-shaped assignments should report a
// per-kind breakdown (all "credential"), not an opaque total — that's how the
// user tells fixtures from a real key leak.
func TestRedactByKindBreakdown(t *testing.T) {
	fixture := `password = "hunter2hunter2"
token = "test-token-abcdef"
api_key = "fake-key-abcdefgh"
`
	_, by := redactSecretsByKind(fixture)
	if by["credential"] != 3 {
		t.Fatalf("want 3 credential hits, got %+v", by)
	}
	if len(by) != 1 {
		t.Fatalf("test fixtures should only trip the generic credential pattern: %+v", by)
	}
	got := summarizeKinds(map[string]int{"credential": 6, "anthropic-key": 1})
	if got != "6×credential, 1×anthropic-key" {
		t.Fatalf("breakdown must lead with the heaviest kind, got %q", got)
	}
}

func TestRedactLeavesCleanCodeAlone(t *testing.T) {
	clean := `def summarize(numbers):
    return {"min": min(numbers), "max": max(numbers)}

# a token of appreciation for the api design
config = load_config("config/config.yaml")
`
	out, n := redactSecrets(clean)
	if n != 0 || out != clean {
		t.Fatalf("clean code modified (n=%d): %q", n, out)
	}
}

// The choke point: a tool result carrying a secret must reach the model
// redacted.
func TestModelNeverSeesSecrets(t *testing.T) {
	secret := "sk-ant-api03-SUPERSECRET1234567890123"
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "read_file",
			Args: map[string]any{"path": "config.py"}}}},
		{Role: "assistant", Content: "the config loads a key"},
	}}
	invoke := func(tool string, args map[string]any) (any, error) {
		return map[string]any{"content": "KEY = \"" + secret + "\"\n"}, nil
	}
	s := New(fp, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	if _, err := s.Ask(context.Background(), "what does config.py load"); err != nil {
		t.Fatal(err)
	}
	for _, seen := range fp.seen {
		if strings.Contains(seen, secret) {
			t.Fatalf("secret leaked to the model: %s", seen)
		}
	}
	found := false
	for _, seen := range fp.seen {
		if strings.Contains(seen, "[REDACTED:") {
			found = true
		}
	}
	if !found {
		t.Fatal("redaction marker missing from tool result")
	}
}
