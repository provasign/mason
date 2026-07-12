package mcpclient

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// fakeServer speaks MCP over pipes: handshake, tools/list, tools/call.
func fakeServer(t *testing.T, in io.Reader, out io.WriteCloser) {
	t.Helper()
	sc := bufio.NewScanner(in)
	reply := func(id any, result string) {
		b, _ := json.Marshal(id)
		out.Write([]byte(`{"jsonrpc":"2.0","id":` + string(b) + `,"result":` + result + "}\n"))
	}
	for sc.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake"}}`)
		case "notifications/initialized":
			// notification: no reply; also emit a stray server notification
			out.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n"))
		case "tools/list":
			reply(req.ID, `{"tools":[{"name":"lookup_issue","description":"Fetch an issue","inputSchema":{"type":"object","properties":{"id":{"type":"string"}}}}]}`)
		case "tools/call":
			name, _ := req.Params["name"].(string)
			if name != "lookup_issue" {
				reply(req.ID, `{"content":[{"type":"text","text":"unknown tool"}],"isError":true}`)
				continue
			}
			args, _ := req.Params["arguments"].(map[string]any)
			id, _ := args["id"].(string)
			reply(req.ID, `{"content":[{"type":"text","text":"issue `+id+`: flaky test"},{"type":"text","text":"status: open"}]}`)
		}
	}
}

func TestClientHandshakeListCall(t *testing.T) {
	cIn, sOut, err := os.Pipe() // server writes -> client reads (kernel-buffered)
	if err != nil {
		t.Fatal(err)
	}
	sIn, cOut, err := os.Pipe() // client writes -> server reads
	if err != nil {
		t.Fatal(err)
	}
	go fakeServer(t, sIn, sOut)
	c := newClient("fake", cOut, cIn)
	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "lookup_issue" {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].InputSchema["type"] != "object" {
		t.Fatal("input schema not carried through")
	}
	got, err := c.Call("lookup_issue", map[string]any{"id": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "issue 42: flaky test") || !strings.Contains(got, "status: open") {
		t.Fatalf("call result = %q", got)
	}
	// error results surface as errors
	if _, err := c.Call("nope", nil); err == nil {
		t.Fatal("isError result must surface as an error")
	}
}
