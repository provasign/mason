// Package localmodels makes free local models a one-keypress experience:
// a curated catalog of open-source coding models (all tool-calling capable —
// the measured floor for mason's harness), filtered by this machine's
// memory, with guided Ollama setup and download.
package localmodels

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Model is one curated catalog entry. MinRAMGB is a practical minimum for
// the default quantization (on Apple Silicon unified memory counts as GPU
// memory, which is why RAM is the single gate).
type Model struct {
	Tag        string
	DownloadGB float64
	MinRAMGB   int
	Note       string
	Blessed    bool // measured in the provasign study: sits on the engine ceiling
}

// Catalog is the curated set. Every entry supports tool calling — models
// without it cannot drive mason at all (measured: the interface, not
// reasoning, is the local floor).
var Catalog = []Model{
	{Tag: "qwen3-coder:30b", DownloadGB: 19, MinRAMGB: 24, Blessed: true,
		Note: "best local coder; measured: matches Claude Code + Sonnet on oracle tasks"},
	{Tag: "qwen2.5-coder:14b", DownloadGB: 9.0, MinRAMGB: 16, Blessed: true,
		Note: "measured: graph tasks at the engine ceiling; light general coding"},
	{Tag: "gpt-oss:20b", DownloadGB: 14, MinRAMGB: 16,
		Note: "OpenAI's open model; strong general reasoning"},
	{Tag: "devstral:24b", DownloadGB: 14, MinRAMGB: 24,
		Note: "Mistral's agentic coding model"},
	{Tag: "qwen2.5-coder:7b", DownloadGB: 4.7, MinRAMGB: 8,
		Note: "small machines; simple tasks and graph routing"},
	{Tag: "llama3.1:8b", DownloadGB: 4.9, MinRAMGB: 8,
		Note: "general-purpose fallback with tool support"},
	{Tag: "qwen2.5-coder:3b", DownloadGB: 1.9, MinRAMGB: 4,
		Note: "minimum viable; graph routing only, expect misses on edits"},
}

// SystemRAMGB reports total physical memory, 0 if unknown.
func SystemRAMGB() int {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0
		}
		return int(b >> 30)
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.ParseInt(f[1], 10, 64)
					return int(kb >> 20)
				}
			}
		}
	}
	return 0
}

// State describes the local Ollama installation.
type State struct {
	BinaryInstalled bool
	ServerUp        bool
	Installed       []string // model tags present locally
}

func ollamaURL() string {
	if v := os.Getenv("OLLAMA_HOST_URL"); v != "" {
		return v
	}
	return "http://localhost:11434"
}

// Detect probes the Ollama binary, server, and installed models.
func Detect() State {
	st := State{}
	if _, err := exec.LookPath("ollama"); err == nil {
		st.BinaryInstalled = true
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(ollamaURL() + "/api/tags")
	if err != nil {
		return st
	}
	defer resp.Body.Close()
	st.ServerUp = true
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&tags) == nil {
		for _, m := range tags.Models {
			st.Installed = append(st.Installed, m.Name)
		}
	}
	return st
}

// Fits reports whether the model can run on ramGB of memory. Unknown RAM
// (0) is permissive — the user is warned instead of blocked.
func (m Model) Fits(ramGB int) bool {
	return ramGB == 0 || ramGB >= m.MinRAMGB
}

// InstalledSet returns the installed tags as a set for O(1) lookups.
func (s State) InstalledSet() map[string]bool {
	set := map[string]bool{}
	for _, t := range s.Installed {
		set[t] = true
	}
	return set
}

// Pull downloads a model via `ollama pull`, streaming progress to the
// user's terminal (Ollama renders its own progress bars).
func Pull(tag string) error {
	cmd := exec.Command("ollama", "pull", tag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// InstallOllama installs Ollama itself where a package manager makes that a
// single command; otherwise it returns instructions.
func InstallOllama() error {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("brew"); err == nil {
			fmt.Println("installing Ollama via Homebrew…")
			cmd := exec.Command("brew", "install", "ollama")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	}
	if runtime.GOOS == "linux" {
		return fmt.Errorf("install Ollama with:  curl -fsSL https://ollama.com/install.sh | sh")
	}
	return fmt.Errorf("install Ollama from https://ollama.com/download and re-run mason")
}

// StartServer launches `ollama serve` detached and waits for it to answer.
func StartServer() error {
	cmd := exec.Command("ollama", "serve")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	c := &http.Client{Timeout: 1 * time.Second}
	for i := 0; i < 20; i++ {
		if resp, err := c.Get(ollamaURL() + "/api/tags"); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("ollama server did not come up — try `ollama serve` in another terminal")
}
