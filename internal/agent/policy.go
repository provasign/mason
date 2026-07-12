package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Policy is the project's standing permission configuration
// (.mason/config.json → "permissions"), so teams stop answering y/n for
// commands they always allow while keeping hard denies that even --yes
// cannot override.
//
//	{
//	  "permissions": {
//	    "bash": "ask",              // allow | ask | deny
//	    "edit": "allow",
//	    "write": "allow",
//	    "bash_allow": ["go test *", "go build *", "git status"],
//	    "bash_deny":  ["git push *", "rm -rf *"],
//	    "paths_deny": [".env*", "secrets/**", "*.pem"],
//	    "fetch": "ask",
//	    "fetch_allow": ["https://pkg.go.dev/*"],
//	    "fetch_deny":  ["*internal.corp*"]
//	  }
//	}
type Policy struct {
	Bash       string   `json:"bash"`
	Edit       string   `json:"edit"`
	Write      string   `json:"write"`
	Fetch      string   `json:"fetch"` // web_fetch: allow | ask | deny (default ask)
	BashAllow  []string `json:"bash_allow"`
	BashDeny   []string `json:"bash_deny"`
	PathsDeny  []string `json:"paths_deny"`
	FetchAllow []string `json:"fetch_allow"` // URL wildcards fetched without asking
	FetchDeny  []string `json:"fetch_deny"`
}

// Verdict is a policy decision for one action.
type Verdict int

const (
	VerdictAsk Verdict = iota
	VerdictAllow
	VerdictDeny
)

// LoadPolicy reads .mason/config.json under root; a missing file is an
// empty policy (everything asks, nothing denied).
func LoadPolicy(root string) *Policy {
	b, err := os.ReadFile(filepath.Join(root, ".mason", "config.json"))
	if err != nil {
		return &Policy{}
	}
	var cfg struct {
		Permissions Policy `json:"permissions"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return &Policy{}
	}
	return &cfg.Permissions
}

func modeVerdict(mode string) Verdict {
	switch strings.ToLower(mode) {
	case "allow":
		return VerdictAllow
	case "deny":
		return VerdictDeny
	default:
		return VerdictAsk
	}
}

// wildMatch matches with '*' as "any run of characters" anywhere in the
// pattern — simple enough to predict, strong enough for command lists.
func wildMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = regexp.QuoteMeta(p)
	}
	re, err := regexp.Compile("^" + strings.Join(quoted, ".*") + "$")
	if err != nil {
		return false
	}
	return re.MatchString(strings.TrimSpace(s))
}

// BashVerdict: deny list wins, then allow list, then the mode.
func (p *Policy) BashVerdict(command string) Verdict {
	for _, pat := range p.BashDeny {
		if wildMatch(pat, command) {
			return VerdictDeny
		}
	}
	for _, pat := range p.BashAllow {
		if wildMatch(pat, command) {
			return VerdictAllow
		}
	}
	return modeVerdict(p.Bash)
}

// FetchVerdict: deny list wins, then allow list, then the fetch mode.
func (p *Policy) FetchVerdict(rawURL string) Verdict {
	for _, pat := range p.FetchDeny {
		if wildMatch(pat, rawURL) {
			return VerdictDeny
		}
	}
	for _, pat := range p.FetchAllow {
		if wildMatch(pat, rawURL) {
			return VerdictAllow
		}
	}
	return modeVerdict(p.Fetch)
}

// PathVerdict gates edit/write against denied paths, then the tool mode.
// Denied paths hold even under --yes: they are the project's hard lines.
func (p *Policy) PathVerdict(tool, relPath string) Verdict {
	rel := filepath.ToSlash(relPath)
	for _, pat := range p.PathsDeny {
		pat = filepath.ToSlash(pat)
		if strings.HasSuffix(pat, "/**") {
			if strings.HasPrefix(rel, strings.TrimSuffix(pat, "/**")+"/") {
				return VerdictDeny
			}
			continue
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return VerdictDeny
		}
		if ok, _ := filepath.Match(pat, filepath.Base(rel)); ok {
			return VerdictDeny
		}
	}
	switch tool {
	case "edit":
		return modeVerdict(p.Edit)
	case "write":
		return modeVerdict(p.Write)
	}
	return VerdictAsk
}
