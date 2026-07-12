// Package mcpclient is a minimal MCP (Model Context Protocol) client over
// stdio: initialize → tools/list → tools/call. It lets mason consume the
// ecosystem's servers (GitHub, databases, browsers, …) while mason's own
// harness rules — permission gates and secret redaction — still apply to
// every call result.
package mcpclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Tool is one tool exposed by an MCP server.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client is one connected MCP server.
type Client struct {
	name  string
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Scanner
	mu    sync.Mutex
	nexID atomic.Int64
	tools []Tool
}

// ServerConfig is one entry under "mcp" in .mason/config.json.
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

const callTimeout = 60 * time.Second

// Connect starts the server process and completes the MCP handshake.
func Connect(name string, cfg ServerConfig) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := newClient(name, stdin, stdout)
	c.cmd = cmd
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	return c, nil
}

// newClient wires a client over any transport (tests use pipes).
func newClient(name string, in io.WriteCloser, out io.Reader) *Client {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	return &Client{name: name, in: in, out: sc}
}

func (c *Client) Name() string  { return c.name }
func (c *Client) Tools() []Tool { return c.tools }

// rpc sends one request and reads frames until its response id arrives
// (server-initiated notifications are skipped). Guarded by a deadline.
func (c *Client) rpc(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nexID.Add(1)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	type frame struct {
		ID     json.Number     `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	deadline := time.Now().Add(callTimeout)
	for time.Now().Before(deadline) {
		if !c.out.Scan() {
			if err := c.out.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("mcp %s: connection closed", c.name)
		}
		line := c.out.Bytes()
		if len(line) == 0 {
			continue
		}
		var f frame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.ID.String() != fmt.Sprint(id) {
			continue // notification or unrelated frame
		}
		if f.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s", c.name, f.Error.Message)
		}
		return f.Result, nil
	}
	return nil, fmt.Errorf("mcp %s: %s timed out", c.name, method)
}

// notify sends a JSON-RPC notification (no response expected).
func (c *Client) notify(method string) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := c.in.Write(append(b, '\n'))
	return err
}

func (c *Client) handshake() error {
	if _, err := c.rpc("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mason", "version": "1"},
	}); err != nil {
		return err
	}
	if err := c.notify("notifications/initialized"); err != nil {
		return err
	}
	res, err := c.rpc("tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var lst struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &lst); err != nil {
		return err
	}
	c.tools = lst.Tools
	return nil
}

// Call invokes one tool and returns its text content, concatenated.
func (c *Client) Call(tool string, args map[string]any) (string, error) {
	res, err := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	var b []byte
	for _, blk := range out.Content {
		if blk.Type == "text" {
			if len(b) > 0 {
				b = append(b, '\n')
			}
			b = append(b, blk.Text...)
		}
	}
	if out.IsError {
		return "", fmt.Errorf("mcp %s/%s: %s", c.name, tool, string(b))
	}
	return string(b), nil
}

// Close terminates the server process.
func (c *Client) Close() {
	_ = c.in.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}
