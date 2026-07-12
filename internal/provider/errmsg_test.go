package provider

import (
	"strings"
	"testing"
)

func TestProviderErrorMessage(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantSubstr string
		wantHint   string
	}{
		{"anthropic billing", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`,
			"credit balance is too low", "billing"},
		{"auth", 401,
			`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			"invalid x-api-key", "mason login"},
		{"rate limit", 429,
			`{"error":{"message":"rate limit exceeded"}}`,
			"rate limit exceeded", "rate limited"},
		{"unknown model", 404,
			`{"error":{"message":"model: nonesuch not found"}}`,
			"nonesuch not found", "/model"},
		{"non-json body falls back to raw", 500, `upstream boom`, "upstream boom", ""},
	}
	for _, c := range cases {
		got := providerErrorMessage(c.status, []byte(c.body))
		if !strings.Contains(got, c.wantSubstr) {
			t.Errorf("%s: message %q missing %q", c.name, got, c.wantSubstr)
		}
		if c.wantHint != "" && !strings.Contains(got, c.wantHint) {
			t.Errorf("%s: message %q missing hint %q", c.name, got, c.wantHint)
		}
		// The whole point: no raw JSON braces leak through for structured errors.
		if c.name != "non-json body falls back to raw" && strings.Contains(got, `{"`) {
			t.Errorf("%s: raw JSON leaked into %q", c.name, got)
		}
	}
}
