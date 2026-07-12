package creds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPKCEPair(t *testing.T) {
	v1, c1, err := pkcePair()
	if err != nil {
		t.Fatal(err)
	}
	v2, c2, _ := pkcePair()
	if v1 == v2 || c1 == c2 {
		t.Fatal("pkce pairs must be unique")
	}
	if len(v1) < 43 { // RFC 7636 minimum verifier length
		t.Fatalf("verifier too short: %d", len(v1))
	}
	if strings.ContainsAny(c1, "+/=") {
		t.Fatal("challenge must be base64url without padding")
	}
}

// Token exchange against a fake auth server: correct form fields in,
// tokens out.
func TestExchangeCode(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-123", "refresh_token": "rt-456", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	cfg := chatgptOAuth
	cfg.TokenURL = srv.URL
	ts, err := exchangeCode(cfg, "code-1", "verifier-1", "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "at-123" || ts.RefreshToken != "rt-456" {
		t.Fatalf("tokens = %+v", ts)
	}
	if time.Until(ts.Expiry) < 50*time.Minute {
		t.Fatal("expiry not set from expires_in")
	}
	for k, want := range map[string]string{
		"grant_type": "authorization_code", "code": "code-1",
		"code_verifier": "verifier-1", "client_id": cfg.ClientID,
	} {
		if gotForm.Get(k) != want {
			t.Fatalf("form %s = %q, want %q", k, gotForm.Get(k), want)
		}
	}
	// error responses surface
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srvErr.Close()
	cfg.TokenURL = srvErr.URL
	if _, err := exchangeCode(cfg, "bad", "v", "r"); err == nil {
		t.Fatal("token error must surface")
	}
}
