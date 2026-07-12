package lspclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeServer speaks just enough LSP over pipes: answers initialize and
// shutdown, and publishes scripted diagnostics on didOpen.
func fakeServer(t *testing.T, in io.Reader, out io.Writer, diagsByFile map[string][]map[string]any) {
	t.Helper()
	r := bufio.NewReader(in)
	write := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				length, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var msg struct {
			ID     json.Number    `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{"capabilities": map[string]any{}}})
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": nil})
		case "textDocument/didOpen":
			td, _ := msg.Params["textDocument"].(map[string]any)
			uri, _ := td["uri"].(string)
			ds := diagsByFile[filepath.Base(uri)]
			if ds == nil {
				ds = []map[string]any{}
			}
			write(map[string]any{"jsonrpc": "2.0",
				"method": "textDocument/publishDiagnostics",
				"params": map[string]any{"uri": uri, "diagnostics": ds}})
		}
	}
}

func startFake(t *testing.T, diags map[string][]map[string]any) *Client {
	t.Helper()
	cIn, sOut := io.Pipe() // server → client
	sIn, cOut := io.Pipe() // client → server
	go fakeServer(t, sIn, sOut, diags)
	c := newClient("fake", t.TempDir(), cOut, cIn)
	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cOut.Close(); cIn.Close() })
	return c
}

func TestDiagnosticsRoundtrip(t *testing.T) {
	c := startFake(t, map[string][]map[string]any{
		"bad.go": {{
			"range":    map[string]any{"start": map[string]any{"line": float64(4)}},
			"severity": float64(1),
			"message":  "undefined: frobnicate",
			"source":   "compiler",
		}, {
			"range":    map[string]any{"start": map[string]any{"line": float64(9)}},
			"severity": float64(2),
			"message":  "unused variable x",
		}},
	})
	f := filepath.Join(t.TempDir(), "bad.go")
	if err := os.WriteFile(f, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := c.Diagnostics(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("want 2 diagnostics, got %d", len(ds))
	}
	if ds[0].Line != 5 || ds[0].Severity != 1 || !strings.Contains(ds[0].Message, "frobnicate") {
		t.Fatalf("first diagnostic wrong: %+v", ds[0])
	}
	if got := ds[0].String(); !strings.Contains(got, "line 5: error: undefined: frobnicate [compiler]") {
		t.Fatalf("String() = %q", got)
	}
	if ds[1].Severity != 2 {
		t.Fatalf("second severity: %+v", ds[1])
	}
}

func TestDiagnosticsCleanFile(t *testing.T) {
	c := startFake(t, nil) // publishes empty lists
	f := filepath.Join(t.TempDir(), "ok.go")
	if err := os.WriteFile(f, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := c.Diagnostics(f)
	if err != nil || len(ds) != 0 {
		t.Fatalf("clean file must yield zero diagnostics: %v %v", ds, err)
	}
}

func TestDiagnosticsMissingFile(t *testing.T) {
	c := startFake(t, nil)
	if _, err := c.Diagnostics(filepath.Join(t.TempDir(), "nope.go")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestDiagnosticsTimeoutWaiterCleanup(t *testing.T) {
	// A server that never publishes: Diagnostics must time out (bounded)
	// and deregister its waiter.
	cIn, _ := io.Pipe()
	sIn, cOut := io.Pipe()
	go func() { // answer initialize only
		fakeServer(t, sIn, io.Discard, nil)
	}()
	_ = cIn
	c := newClient("mute", t.TempDir(), cOut, strings.NewReader(""))
	f := filepath.Join(t.TempDir(), "x.go")
	os.WriteFile(f, []byte("package x\n"), 0o644)
	done := make(chan error, 1)
	go func() {
		_, err := c.Diagnostics(f)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("mute server must produce an error")
		}
	case <-time.After(diagTimeout + 5*time.Second):
		t.Fatal("Diagnostics did not respect its timeout")
	}
}

func TestPathToURI(t *testing.T) {
	if got := pathToURI("/Users/x/proj/a.go"); got != "file:///Users/x/proj/a.go" {
		t.Fatalf("posix: %q", got)
	}
	// Windows drive paths gain the leading slash the URI scheme requires.
	if got := pathToURI(`C:\proj\a.go`); got != "file:///C:/proj/a.go" {
		t.Fatalf("windows: %q", got)
	}
}

func TestLanguageID(t *testing.T) {
	if languageID("a/b.go") != "go" || languageID("x.tsx") != "typescriptreact" || languageID("q.weird") != "plaintext" {
		t.Fatal("languageID mapping wrong")
	}
}

func TestDetectNoMarkers(t *testing.T) {
	if name, _, _ := Detect(t.TempDir()); name != "" {
		t.Fatalf("empty dir must detect no server, got %q", name)
	}
}
