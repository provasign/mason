package creds

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// EXPERIMENTAL: Sign in with ChatGPT (OpenAI Codex OAuth — permitted in
// third-party tools, unlike Anthropic's, which is banned). This implements
// the standard PKCE flow: browser consent → local callback → token
// exchange → OS keychain. The provider-side endpoint wiring is validated
// against a live ChatGPT account before it is enabled by default.

// oauthConfig captures one vendor's PKCE endpoints.
type oauthConfig struct {
	AuthURL  string
	TokenURL string
	ClientID string
	Scopes   string
	Port     int
}

var chatgptOAuth = oauthConfig{
	AuthURL:  "https://auth.openai.com/oauth/authorize",
	TokenURL: "https://auth.openai.com/oauth/token",
	// The public Codex CLI client id (its OAuth flow is the one OpenAI
	// blesses for third-party use).
	ClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
	Scopes:   "openid profile email offline_access",
	Port:     1455,
}

// TokenSet is what OAuth leaves in the keychain (never on disk).
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 48)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// LoginChatGPT runs the browser PKCE flow and stores the tokens in the OS
// keychain under vendor "chatgpt".
func LoginChatGPT() error {
	return oauthLogin("chatgpt", chatgptOAuth)
}

func oauthLogin(vendor string, cfg oauthConfig) error {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, _, err := pkcePair()
	if err != nil {
		return err
	}
	redirect := fmt.Sprintf("http://localhost:%d/auth/callback", cfg.Port)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("callback port %d busy: %w", cfg.Port, err)
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state[:32] {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth: %s", e)
			return
		}
		fmt.Fprintln(w, "mason: signed in — you can close this tab.")
		codeCh <- q.Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := cfg.AuthURL + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {cfg.Scopes},
		"state":                 {state[:32]},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	fmt.Println("Opening your browser to sign in with ChatGPT…")
	if !openBrowser(authURL) {
		fmt.Println("(open manually:)", authURL)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("sign-in timed out")
	}
	tokens, err := exchangeCode(cfg, code, verifier, redirect)
	if err != nil {
		return err
	}
	return StoreTokens(vendor, tokens)
}

// exchangeCode swaps the authorization code for tokens.
func exchangeCode(cfg oauthConfig, code, verifier, redirect string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {cfg.ClientID},
		"code_verifier": {verifier},
	}
	resp, err := http.Post(cfg.TokenURL, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return nil, fmt.Errorf("token exchange failed: %s", tr.Error)
	}
	return &TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// StoreTokens keeps the token set in the OS keychain only.
func StoreTokens(vendor string, t *TokenSet) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := keyring.Set(service, vendor+"-oauth", string(b)); err != nil {
		return fmt.Errorf("keychain store failed: %w", err)
	}
	fmt.Printf("✓ signed in — tokens stored in the OS keychain (%s)\n", vendor)
	fmt.Println("note: the chatgpt: provider is EXPERIMENTAL and enables after first live validation")
	return nil
}

// LoadTokens retrieves a stored token set, nil if absent.
func LoadTokens(vendor string) *TokenSet {
	raw, err := keyring.Get(service, vendor+"-oauth")
	if err != nil || raw == "" {
		return nil
	}
	var t TokenSet
	if json.Unmarshal([]byte(raw), &t) != nil {
		return nil
	}
	return &t
}
