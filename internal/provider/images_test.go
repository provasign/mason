package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var imgMsg = []Msg{
	{Role: "system", Content: "sys"},
	{Role: "user", Content: "what is in this screenshot?",
		Images: []Image{{MediaType: "image/png", DataB64: "AAAA"}}},
}

func TestOllamaImagePayload(t *testing.T) {
	p := &ollamaProvider{model: "llava"}
	pl := p.payload(imgMsg, nil, false)
	msgs := pl["messages"].([]map[string]any)
	user := msgs[1]
	imgs, ok := user["images"].([]string)
	if !ok || len(imgs) != 1 || imgs[0] != "AAAA" {
		t.Fatalf("ollama wants bare base64 in images: %v", user)
	}
	if msgs[0]["images"] != nil {
		t.Fatal("system message must carry no images")
	}
}

func TestAnthropicImagePayload(t *testing.T) {
	p := &anthropicProvider{model: "m", key: "k"}
	pl := p.payload(imgMsg, nil, false)
	msgs := pl["messages"].([]map[string]any)
	blocks, ok := msgs[0]["content"].([]map[string]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("anthropic wants [image, text] blocks: %v", msgs[0])
	}
	if blocks[0]["type"] != "image" {
		t.Fatalf("first block must be the image: %v", blocks[0])
	}
	src := blocks[0]["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != "AAAA" || src["type"] != "base64" {
		t.Fatalf("image source wrong: %v", src)
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "what is in this screenshot?" {
		t.Fatalf("text block wrong: %v", blocks[1])
	}
	// The rolling cache breakpoint must still land on the LAST block.
	if blocks[1]["cache_control"] == nil {
		t.Fatal("cache_control must survive on the trailing text block")
	}
}

func TestOpenAIImagePayload(t *testing.T) {
	p := &openaiProvider{model: "gpt-4o"}
	pl := p.payload(imgMsg, nil, false)
	msgs := pl["messages"].([]map[string]any)
	parts, ok := msgs[1]["content"].([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("openai wants [text, image_url] parts: %v", msgs[1])
	}
	iu := parts[1]["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,AAAA") {
		t.Fatalf("data URI wrong: %q", url)
	}
}

func TestOllamaNoToolsFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools"`) {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"registry.ollama.ai/library/x does not support tools"}`))
			return
		}
		w.Write([]byte(`{"message":{"content":"a red square"},"prompt_eval_count":10,"eval_count":5}`))
	}))
	defer srv.Close()
	p := &ollamaProvider{model: "x", url: srv.URL}
	tools := []ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}
	msg, err := p.Chat(context.Background(), imgMsg, tools, false)
	if err != nil || msg.Content != "a red square" {
		t.Fatalf("fallback must succeed tool-less: %q %v", msg.Content, err)
	}
	if calls != 2 {
		t.Fatalf("want exactly one retry, got %d calls", calls)
	}
	// The latch must hold: the next turn goes straight to tool-less.
	if _, err := p.Chat(context.Background(), imgMsg, tools, false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("latched provider must not resend tools, got %d calls", calls)
	}
}

func TestTextOnlyPayloadUnchanged(t *testing.T) {
	plain := []Msg{{Role: "user", Content: "hi"}, {Role: "user", Content: "again"}}
	if _, ok := (&openaiProvider{model: "m"}).payload(plain, nil, false)["messages"].([]map[string]any)[0]["content"].(string); !ok {
		t.Fatal("text-only openai message must stay a plain string")
	}
	// anthropic: only the LAST message is converted to blocks (rolling
	// cache breakpoint); earlier text-only messages stay plain strings.
	if _, ok := (&anthropicProvider{model: "m"}).payload(plain, nil, false)["messages"].([]map[string]any)[0]["content"].(string); !ok {
		t.Fatal("non-terminal text-only anthropic message must stay a plain string")
	}
}
