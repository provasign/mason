package agent

import (
	"fmt"
	"regexp"
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
	n := 0
	for _, p := range secretPatterns {
		if p.group == 0 {
			s = p.re.ReplaceAllStringFunc(s, func(string) string {
				n++
				return "[REDACTED:" + p.kind + "]"
			})
			continue
		}
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			sub := p.re.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			n++
			return sub[1] + "[REDACTED:" + p.kind + "]" + sub[3]
		})
	}
	return s, n
}

// redact applies secret redaction when enabled (default), notifying the
// user once per burst so the protection is visible, never silent.
func (s *Session) redact(content string) string {
	if s.opts.NoRedact {
		return content
	}
	out, n := redactSecrets(content)
	if n > 0 {
		fmt.Fprintf(s.out, "  %s\n", s.st.yellow(fmt.Sprintf("🔒 %d secret(s) redacted before reaching the model", n)))
	}
	return out
}
