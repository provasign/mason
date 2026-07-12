package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// --json: machine-readable one-shot mode for CI and SDK callers. Human/tool
// narration goes to stderr; stdout carries exactly one JSON object.

type jsonUsage struct {
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CacheRead    int     `json:"cacheReadTokens,omitempty"`
	CacheWrite   int     `json:"cacheWriteTokens,omitempty"`
	CostUSD      float64 `json:"costUSD"`
}

type jsonResult struct {
	OK           bool      `json:"ok"`
	Reply        string    `json:"reply,omitempty"`
	Error        string    `json:"error,omitempty"`
	Model        string    `json:"model"`
	Usage        jsonUsage `json:"usage"`
	ChangedFiles []string  `json:"changedFiles"`
	DurationMS   int64     `json:"durationMs"`
}

func emitJSON(r jsonResult) {
	if r.ChangedFiles == nil {
		r.ChangedFiles = []string{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r)
}

// gitStatusLines snapshots `git status --porcelain` for change detection.
// Empty on non-git trees — changedFiles is then best-effort empty.
func gitStatusLines(root string) map[string]string {
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if len(l) > 3 {
			m[strings.TrimSpace(l[3:])] = l[:2]
		}
	}
	return m
}

// changedBetween lists files whose porcelain status moved during the task.
func changedBetween(before, after map[string]string) []string {
	var out []string
	for f, st := range after {
		if before[f] != st {
			out = append(out, f)
		}
	}
	for f := range before {
		if _, ok := after[f]; !ok {
			out = append(out, f) // reverted or deleted entries count too
		}
	}
	sort.Strings(out)
	return out
}
