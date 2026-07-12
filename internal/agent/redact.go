package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Secret redaction: tool output (file reads, grep hits, bash output) is
// scanned for credential material BEFORE it reaches the model or the
// screen. Sending a repo's secrets to a model API is exfiltration — the
// harness, not the model, is responsible for preventing it. Targeted
// patterns over entropy heuristics: a false "[REDACTED]" in the middle of
// real code misleads the model, so precision wins.

type secretPattern struct {
	kind string
	re   *regexp.Regexp
	// group, when >0, redacts only that capture group (keeps the key name
	// visible so the model still understands the code's shape)
	group int
}

var secretPatterns = []secretPattern{
	{kind: "private-key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{kind: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{16,}`)},
	{kind: "openai-key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_\-]{20,}`)},
	{kind: "github-token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{kind: "aws-access-key", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{kind: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{kind: "slack-token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`)},
	// generic assignment: api_key = "….", password: '…' — redact the VALUE
	// only, so the code's structure stays readable
	{kind: "credential", group: 2,
		re: regexp.MustCompile(`(?i)\b((?:api[_-]?key|secret[_-]?key|secret|access[_-]?token|auth[_-]?token|token|passwd|password|client[_-]?secret)\s*["']?\s*[:=]\s*["'])([^"'\n]{8,})(["'])`)},
}

// redactSecrets replaces detected credential material with [REDACTED:kind]
// markers and reports how many replacements were made.
func redactSecrets(s string) (string, int) {
	out, by := redactSecretsByKind(s)
	n := 0
	for _, c := range by {
		n += c
	}
	return out, n
}

// redactSecretsByKind is redactSecrets with a per-kind tally, so the user can
// tell 6×credential (almost always test fixtures) from 1×anthropic-key (a real
// leak) instead of an opaque total.
func redactSecretsByKind(s string) (string, map[string]int) {
	by := map[string]int{}
	for _, p := range secretPatterns {
		if p.group == 0 {
			s = p.re.ReplaceAllStringFunc(s, func(string) string {
				by[p.kind]++
				return "[REDACTED:" + p.kind + "]"
			})
			continue
		}
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			sub := p.re.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			by[p.kind]++
			return sub[1] + "[REDACTED:" + p.kind + "]" + sub[3]
		})
	}
	return s, by
}

// summarizeKinds renders a per-kind breakdown like "6×credential, 1×anthropic-key",
// heaviest kind first, so the redaction notice is self-explaining.
func summarizeKinds(by map[string]int) string {
	kinds := make([]string, 0, len(by))
	for k := range by {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if by[kinds[i]] != by[kinds[j]] {
			return by[kinds[i]] > by[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = fmt.Sprintf("%d×%s", by[k], k)
	}
	return strings.Join(parts, ", ")
}

// redact applies secret redaction when enabled (default), notifying the
// user once per burst so the protection is visible, never silent.
func (s *Session) redact(content string) string {
	if s.opts.NoRedact {
		return content
	}
	out, by := redactSecretsByKind(content)
	n := 0
	for _, c := range by {
		n += c
	}
	if n > 0 {
		fmt.Fprintf(s.out, "  %s\n", s.st.yellow(fmt.Sprintf("🔒 %d secret(s) redacted before reaching the model (%s)", n, summarizeKinds(by))))
	}
	return out
}
