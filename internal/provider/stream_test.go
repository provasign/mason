package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
}

func TestAnthropicStream(t *testing.T) {
	srv := sseServer(t, []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"grep"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"patt"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ern\":\"x\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
	})
	defer srv.Close()
	p := &anthropicProvider{model: "m", key: "k", url: srv.URL}
	var streamed strings.Builder
	msg, err := p.ChatStream(nil, nil, false, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello" || msg.Content != "Hello" {
		t.Fatalf("text = %q / %q", streamed.String(), msg.Content)
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "grep" || msg.Calls[0].Args["pattern"] != "x" {
		t.Fatalf("calls = %+v", msg.Calls)
	}
	if msg.Usage.In != 42 || msg.Usage.Out != 7 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestOpenAIStream(t *testing.T) {
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{"content":"Hi "}}]}`,
		`data: {"choices":[{"delta":{"content":"there"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`,
	})
	defer srv.Close()
	p := &openaiProvider{model: "m", key: "k", url: srv.URL}
	var streamed strings.Builder
	msg, err := p.ChatStream(nil, nil, false, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hi there" {
		t.Fatalf("streamed = %q", streamed.String())
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "bash" || msg.Calls[0].Args["command"] != "ls" {
		t.Fatalf("calls = %+v", msg.Calls)
	}
	if msg.Usage.In != 10 || msg.Usage.Out != 5 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestOllamaStreamHoldsBackToolCallJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, l := range []string{
			`{"message":{"content":"{\"name\": \"dead_code\","},"done":false}`,
			`{"message":{"content":" \"arguments\": {}}"},"done":false}`,
			`{"message":{"content":""},"done":true,"prompt_eval_count":9,"eval_count":3}`,
		} {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
	defer srv.Close()
	p := &ollamaProvider{model: "m", url: srv.URL}
	var streamed strings.Builder
	msg, err := p.ChatStream(nil, []ToolDef{{Name: "dead_code"}}, false, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.Len() != 0 {
		t.Fatalf("tool-call JSON leaked to the stream: %q", streamed.String())
	}
	if len(msg.Calls) != 1 || msg.Calls[0].Name != "dead_code" {
		t.Fatalf("fallback not applied: %+v", msg.Calls)
	}
}

func TestOllamaStreamPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, l := range []string{
			`{"message":{"content":"All"},"done":false}`,
			`{"message":{"content":" done."},"done":false}`,
			`{"message":{"content":""},"done":true,"prompt_eval_count":4,"eval_count":2}`,
		} {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
	defer srv.Close()
	p := &ollamaProvider{model: "m", url: srv.URL}
	var streamed strings.Builder
	msg, err := p.ChatStream(nil, nil, false, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "All done." || msg.Content != "All done." {
		t.Fatalf("streamed = %q content = %q", streamed.String(), msg.Content)
	}
}
