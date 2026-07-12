// Package lspclient is a minimal Language Server Protocol client over stdio,
// scoped to exactly one job: after mason edits a file, ask the language
// server what is broken in it (textDocument/publishDiagnostics) and feed
// that back deterministically — the model sees compile/type errors at edit
// time instead of discovering them turns later through bash.
package lspclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Diagnostic is one server finding in a file.
type Diagnostic struct {
	Line     int    // 1-based
	Severity int    // 1=error 2=warning 3=info 4=hint
	Message  string
	Source   string
}

func (d Diagnostic) String() string {
	sev := "error"
	switch d.Severity {
	case 2:
		sev = "warning"
	case 3:
		sev = "info"
	case 4:
		sev = "hint"
	}
	src := ""
	if d.Source != "" {
		src = " [" + d.Source + "]"
	}
	return fmt.Sprintf("line %d: %s: %s%s", d.Line, sev, d.Message, src)
}

// Client is one running language server.
type Client struct {
	name string
	root string
	cmd  *exec.Cmd
	in   io.WriteCloser
	mu   sync.Mutex // serializes writes
	id   atomic.Int64

	pendMu  sync.Mutex
	pending map[int64]chan rpcResult

	diagMu  sync.Mutex
	waiters map[string][]chan []Diagnostic // by URI

	closed chan struct{}
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

const (
	initTimeout = 15 * time.Second
	diagTimeout = 10 * time.Second
)

// Start launches the server and completes the LSP handshake.
func Start(name, command string, args []string, root string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // language servers are chatty on stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp %s: %w", name, err)
	}
	c := newClient(name, root, stdin, stdout)
	c.cmd = cmd
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("lsp %s: %w", name, err)
	}
	return c, nil
}

// newClient wires a client over any transport (tests use pipes).
func newClient(name, root string, in io.WriteCloser, out io.Reader) *Client {
	c := &Client{
		name: name, root: root, in: in,
		pending: map[int64]chan rpcResult{},
		waiters: map[string][]chan []Diagnostic{},
		closed:  make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(out))
	return c
}

func (c *Client) Name() string { return c.name }

// readLoop parses Content-Length framed messages and routes them: responses
// to their pending call, publishDiagnostics to registered waiters,
// everything else dropped.
func (c *Client) readLoop(r *bufio.Reader) {
	defer close(c.closed)
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				c.failPending(fmt.Errorf("lsp %s: connection closed", c.name))
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break // end of headers
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				length, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if length <= 0 || length > 64<<20 {
			c.failPending(fmt.Errorf("lsp %s: bad frame length %d", c.name, length))
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			c.failPending(fmt.Errorf("lsp %s: truncated frame", c.name))
			return
		}
		var msg struct {
			ID     json.Number     `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "textDocument/publishDiagnostics":
			c.deliverDiagnostics(msg.Params)
		case msg.Method != "" && msg.ID.String() != "":
			// Server-to-client request (e.g. workspace/configuration,
			// client/registerCapability) — answer with null so servers
			// that block on it keep going.
			c.respondNull(msg.ID)
		case msg.ID.String() != "":
			id, _ := msg.ID.Int64()
			c.pendMu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.pendMu.Unlock()
			if ch != nil {
				res := rpcResult{result: msg.Result}
				if msg.Error != nil {
					res.err = fmt.Errorf("lsp %s: %s", c.name, msg.Error.Message)
				}
				ch <- res
			}
		}
	}
}

func (c *Client) failPending(err error) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcResult{err: err}
		delete(c.pending, id)
	}
}

func (c *Client) deliverDiagnostics(params json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Message  string `json:"message"`
			Source   string `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	out := make([]Diagnostic, 0, len(p.Diagnostics))
	for _, d := range p.Diagnostics {
		sev := d.Severity
		if sev == 0 {
			sev = 1 // unspecified severity is an error per LSP practice
		}
		out = append(out, Diagnostic{Line: d.Range.Start.Line + 1,
			Severity: sev, Message: d.Message, Source: d.Source})
	}
	c.diagMu.Lock()
	ws := c.waiters[p.URI]
	delete(c.waiters, p.URI)
	c.diagMu.Unlock()
	for _, w := range ws {
		w <- out
	}
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = c.in.Write(b)
	return err
}

func (c *Client) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := c.id.Add(1)
	ch := make(chan rpcResult, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.write(req); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-time.After(timeout):
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, fmt.Errorf("lsp %s: %s timed out", c.name, method)
	}
}

func (c *Client) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) respondNull(id json.Number) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
}

func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // windows drive paths
	}
	return "file://" + p
}

func (c *Client) handshake() error {
	_, err := c.call("initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(c.root),
		"workspaceFolders": []map[string]any{
			{"uri": pathToURI(c.root), "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{},
				"synchronization":    map[string]any{"didSave": true},
			},
			"workspace": map[string]any{"configuration": false},
		},
		"clientInfo": map[string]any{"name": "mason"},
	}, initTimeout)
	if err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

// Diagnostics opens one file (current on-disk content) and returns what the
// server reports for it, waiting up to diagTimeout for the publish. The
// document is closed afterwards so repeat calls always see fresh content.
func (c *Client) Diagnostics(absPath string) ([]Diagnostic, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	uri := pathToURI(absPath)
	ch := make(chan []Diagnostic, 1)
	c.diagMu.Lock()
	c.waiters[uri] = append(c.waiters[uri], ch)
	c.diagMu.Unlock()

	if err := c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": languageID(absPath),
			"version": 1, "text": string(content),
		},
	}); err != nil {
		return nil, err
	}
	defer func() {
		_ = c.notify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
	}()
	select {
	case ds := <-ch:
		return ds, nil
	case <-c.closed:
		return nil, fmt.Errorf("lsp %s: server exited", c.name)
	case <-time.After(diagTimeout):
		c.diagMu.Lock()
		delete(c.waiters, uri)
		c.diagMu.Unlock()
		return nil, fmt.Errorf("lsp %s: no diagnostics within %s", c.name, diagTimeout)
	}
}

func languageID(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".go":
		return "go"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	default:
		return "plaintext"
	}
}

// Close shuts the server down.
func (c *Client) Close() {
	_, _ = c.call("shutdown", nil, 2*time.Second)
	_ = c.notify("exit", nil)
	_ = c.in.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// Detect picks a language server for the project: an explicit config wins,
// then well-known servers matched against the project's ecosystem markers —
// only when the binary is actually installed. Empty name means none.
func Detect(root string) (name, command string, args []string) {
	exists := func(f string) bool {
		_, err := os.Stat(filepath.Join(root, f))
		return err == nil
	}
	inPath := func(bin string) bool {
		_, err := exec.LookPath(bin)
		return err == nil
	}
	switch {
	case exists("go.mod") && inPath("gopls"):
		return "gopls", "gopls", nil
	case (exists("tsconfig.json") || exists("package.json")) && inPath("typescript-language-server"):
		return "tsserver", "typescript-language-server", []string{"--stdio"}
	case (exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt")) && inPath("pyright-langserver"):
		return "pyright", "pyright-langserver", []string{"--stdio"}
	case (exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt")) && inPath("pylsp"):
		return "pylsp", "pylsp", nil
	case exists("Cargo.toml") && inPath("rust-analyzer"):
		return "rust-analyzer", "rust-analyzer", nil
	}
	return "", "", nil
}
