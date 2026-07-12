package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func fetchSession(t *testing.T, policy *Policy) *Session {
	t.Helper()
	return New(nil, nil, Options{Root: t.TempDir(), Out: &strings.Builder{}, Policy: policy})
}

func TestWebFetchHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><html><head><title>T</title><style>.x{color:red}</style></head>
<body><script>var hidden = "nope";</script><h1>Hello &amp; welcome</h1><p>First para</p><p>Second para</p></body></html>`))
	}))
	defer srv.Close()

	// The test server binds 127.0.0.1 — allowlist it so the private-host
	// guard (and the y/n gate) let the fetch through, which also exercises
	// fetch_allow itself.
	s := fetchSession(t, &Policy{FetchAllow: []string{"http://127.0.0.1*"}})
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "web_fetch",
		Args: map[string]any{"url": srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Hello & welcome", "First para", "Second para", "200 OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"<h1>", "<script", "var hidden", "color:red"} {
		if strings.Contains(out, banned) {
			t.Errorf("output must not contain %q:\n%s", banned, out)
		}
	}
}

func TestWebFetchRefusesPrivateWithoutAllowlist(t *testing.T) {
	s := fetchSession(t, &Policy{})
	for _, u := range []string{"http://127.0.0.1:9/x", "http://localhost/x", "http://192.168.1.1/x", "http://10.0.0.8/x"} {
		if _, err := s.fetchWebURL(context.Background(), u); err == nil ||
			!strings.Contains(err.Error(), "private") {
			t.Errorf("%s must be refused as private, got %v", u, err)
		}
	}
}

func TestWebFetchRefusesBadSchemesAndCreds(t *testing.T) {
	s := fetchSession(t, &Policy{})
	if _, err := s.fetchWebURL(context.Background(), "ftp://example.com/x"); err == nil {
		t.Fatal("non-http scheme must be refused")
	}
	if _, err := s.fetchWebURL(context.Background(), "https://user:pass@example.com/x"); err == nil {
		t.Fatal("embedded credentials must be refused")
	}
}

func TestWebFetchPolicyDeny(t *testing.T) {
	s := fetchSession(t, &Policy{FetchDeny: []string{"*internal.corp*"}})
	if _, err := s.fetchWebURL(context.Background(), "https://internal.corp/secrets"); err == nil {
		t.Fatal("fetch_deny must refuse")
	}
}

func TestHTMLToTextEntities(t *testing.T) {
	got := htmlToText("<div>a &lt;b&gt; c</div><br><div>next</div>")
	if !strings.Contains(got, "a <b> c") || !strings.Contains(got, "next") {
		t.Fatalf("entity decode / break handling wrong: %q", got)
	}
}
