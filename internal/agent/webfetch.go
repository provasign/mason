package agent

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// web_fetch pulls a URL into the conversation as plain text. It is gated
// like bash (the network is outside the trust boundary), redacted like
// everything else, and bounded: 30s, 2 MB, text extracted from HTML.
// Private/loopback addresses are refused unless the project policy
// explicitly allowlists them — a model must not be able to walk the
// user's internal network by default.

const (
	fetchTimeout  = 30 * time.Second
	fetchMaxBytes = 2 << 20
)

// fetchWebURL validates, gates, and executes one web_fetch call.
func (s *Session) fetchWebURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("web_fetch: bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("web_fetch: only http(s) urls are fetched, got %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("web_fetch: urls with embedded credentials are refused")
	}
	allowlisted := false
	v := VerdictAsk
	if s.opts.Policy != nil {
		v = s.opts.Policy.FetchVerdict(u.String())
		allowlisted = v == VerdictAllow
	}
	if isPrivateHost(u.Hostname()) && !allowlisted {
		return "", fmt.Errorf("web_fetch: %s resolves to a private/loopback address — refused (allowlist it under permissions.fetch_allow to permit)", u.Hostname())
	}
	if ok, why := s.gate(v, "fetch "+u.String(), ""); !ok {
		return "", fmt.Errorf("%s: web fetch", why)
	}
	s.setStatus("fetching %s", u.Host)

	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mason (+https://github.com/provasign/mason)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return "", fmt.Errorf("web_fetch: reading body: %w", err)
	}
	text := string(body)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "html") || looksLikeHTML(text) {
		text = htmlToText(text)
	}
	out := fmt.Sprintf("[%s %s · %s · %d bytes]\n%s",
		resp.Proto, resp.Status, ct, len(body), text)
	return truncate(out, maxToolOutput), nil
}

// isPrivateHost reports whether host is a literal loopback/private/link-local
// address or an obvious local name. Non-literal hostnames are resolved
// best-effort; resolution failure falls through to the y/n gate.
func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipIsInternal(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ipIsInternal(ip) {
			return true
		}
	}
	return false
}

func ipIsInternal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

var (
	htmlDocRe    = regexp.MustCompile(`(?is)<!doctype html|<html[\s>]`)
	htmlDropRe   = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)[^>]*>.*?</\s*(script|style|noscript|svg|head)\s*>`)
	htmlBreakRe  = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article|blockquote|pre)>|<br\s*/?>`)
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRunsRe  = regexp.MustCompile(`\n{3,}`)
	spaceRunsRe  = regexp.MustCompile(`[ \t]{2,}`)
)

func looksLikeHTML(s string) bool {
	head := s
	if len(head) > 1024 {
		head = head[:1024]
	}
	return htmlDocRe.MatchString(head)
}

// htmlToText is a bounded, dependency-free extraction: drop non-content
// elements, convert structural closes to newlines, strip tags, decode
// entities, collapse whitespace.
func htmlToText(s string) string {
	s = htmlDropRe.ReplaceAllString(s, " ")
	s = htmlBreakRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimSpace(spaceRunsRe.ReplaceAllString(line, " ")) + "\n")
	}
	return strings.TrimSpace(blankRunsRe.ReplaceAllString(b.String(), "\n\n"))
}
