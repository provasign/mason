package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/provasign/mason/internal/provider"
)

// Sessions are stored one file per conversation under a per-root directory:
//
//	<cache>/mason/sessions/<sha1(root)>/<id>.json
//
// (v0.15 and earlier used a single <sha1(root)>.json — still readable; it
// shows up in the list as "legacy" and is superseded on the next save.)

type sessionFile struct {
	Model    string         `json:"model"`
	Name     string         `json:"name,omitempty"`  // user-assigned (/resume name …)
	Title    string         `json:"title,omitempty"` // auto: first task, truncated
	Updated  time.Time      `json:"updated,omitempty"`
	Messages []provider.Msg `json:"messages"`
}

type sessionMeta struct {
	Path    string
	ID      string
	Label   string // name if set, else title, else "(untitled)"
	Model   string
	Updated time.Time
	Msgs    int
}

func rootHash(root string) string {
	sum := sha1.Sum([]byte(root))
	return hex.EncodeToString(sum[:])
}

func cacheBase() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return base
}

// sessionsDir is the per-root directory of session files.
func sessionsDir(root string) string {
	return filepath.Join(cacheBase(), "mason", "sessions", rootHash(root))
}

// legacySessionPath is the pre-v0.16 single-file location.
func legacySessionPath(root string) string {
	return filepath.Join(cacheBase(), "mason", "sessions", rootHash(root)+".json")
}

// newSessionID is sortable-by-time and unique enough for one root.
func newSessionID() string {
	return time.Now().Format("20060102-150405")
}

func sessionPathFor(root, id string) string {
	return filepath.Join(sessionsDir(root), id+".json")
}

// sessionTitle derives the auto-title from the first real user task.
func sessionTitle(msgs []provider.Msg) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		// File-attachment messages are harness-injected file CONTENT —
		// nothing in them is a task.
		if strings.HasPrefix(m.Content, "[mason attached") {
			continue
		}
		// Skip leading harness notes ("[PLAN MODE …]", undo notices) and
		// take the first line the user actually typed.
		for _, line := range strings.Split(m.Content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "[") {
				continue
			}
			if len(line) > 60 {
				line = line[:60] + "…"
			}
			return line
		}
	}
	return ""
}

func saveSession(path string, msgs []provider.Msg, model, name string) {
	// Cap persisted history so the session file cannot grow without bound;
	// the system prompt is regenerated on load, so drop it and keep the tail.
	const keep = 400
	title := sessionTitle(msgs)
	if len(msgs) > keep {
		msgs = msgs[len(msgs)-keep:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(sessionFile{Model: model, Name: name, Title: title,
		Updated: time.Now(), Messages: msgs})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

func loadSessionFile(path string) (sessionFile, error) {
	var sf sessionFile
	b, err := os.ReadFile(path)
	if err != nil {
		return sf, err
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return sf, err
	}
	return sf, nil
}

// loadSession keeps the v0.15 signature used by model auto-detection.
func loadSession(path string) ([]provider.Msg, string, error) {
	sf, err := loadSessionFile(path)
	if err != nil {
		return nil, "", err
	}
	return sf.Messages, sf.Model, nil
}

// listSessions returns every stored conversation for root, newest first.
func listSessions(root string) []sessionMeta {
	var out []sessionMeta
	dir := sessionsDir(root)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		sf, err := loadSessionFile(p)
		if err != nil {
			continue
		}
		out = append(out, metaOf(p, strings.TrimSuffix(e.Name(), ".json"), sf))
	}
	if lp := legacySessionPath(root); len(out) == 0 {
		if sf, err := loadSessionFile(lp); err == nil {
			out = append(out, metaOf(lp, "legacy", sf))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func metaOf(path, id string, sf sessionFile) sessionMeta {
	label := sf.Name
	if label == "" {
		label = sf.Title
	}
	if label == "" {
		label = sessionTitle(sf.Messages) // pre-v0.16 files carry no title
	}
	if label == "" {
		label = "(untitled)"
	}
	upd := sf.Updated
	if upd.IsZero() {
		if fi, err := os.Stat(path); err == nil {
			upd = fi.ModTime()
		}
	}
	n := 0
	for _, m := range sf.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			n++
		}
	}
	return sessionMeta{Path: path, ID: id, Label: label, Model: sf.Model, Updated: upd, Msgs: n}
}

// renderSessions formats the picker list (shared by CLI, REPL, and TUI).
func renderSessions(metas []sessionMeta) string {
	if len(metas) == 0 {
		return "no saved sessions for this directory"
	}
	var b strings.Builder
	for i, m := range metas {
		age := "?"
		if !m.Updated.IsZero() {
			age = compactAge(time.Since(m.Updated))
		}
		fmt.Fprintf(&b, "  %d. %-40s %4d msg · %s · %s\n", i+1, m.Label, m.Msgs, m.Model, age)
	}
	b.WriteString("resume with: mason --resume N   (or /resume N inside mason)")
	return b.String()
}

func compactAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
