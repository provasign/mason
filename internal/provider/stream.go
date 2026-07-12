package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Streamer is implemented by providers that can deliver assistant text
// incrementally. onText receives each text delta as it arrives; the returned
// Msg is complete and identical to what Chat would have returned (tool calls
// assembled, usage filled). Providers without streaming just don't implement
// this — callers fall back to Chat.
type Streamer interface {
	ChatStream(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error)
}

// postStream POSTs payload and hands the response body lines to handle.
func postStream(ctx context.Context, url string, headers map[string]string, payload any, handle func(line []byte) error) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return &httpError{code: resp.StatusCode,
			msg: fmt.Sprintf("%s: HTTP %d: %s", url, resp.StatusCode, truncate(b.String(), 300))}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		if err := handle(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// streamWithRetry retries a streaming call only while nothing has reached
// the screen yet — once bytes are shown, a retry would duplicate output, so
// the error is surfaced instead.
func streamWithRetry(ctx context.Context, onText func(string), fn func(cb func(string)) (Msg, error)) (Msg, error) {
	streamed := false
	cb := func(s string) { streamed = true; onText(s) }
	var msg Msg
	var err error
	for attempt, delay := 0, time.Second; attempt < 3; attempt, delay = attempt+1, delay*4 {
		msg, err = fn(cb)
		if err == nil || streamed || !retryable(err) {
			return msg, err
		}
		select {
		case <-ctx.Done():
			return Msg{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return msg, err
}

// sseData strips an SSE "data:" prefix; ok is false for non-data lines.
func sseData(line []byte) (payload []byte, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:"))), true
}

// --- Ollama streaming --------------------------------------------------------

func (p *ollamaProvider) ChatStream(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	return streamWithRetry(ctx, onText, func(cb func(string)) (Msg, error) {
		return p.chatStreamOnce(ctx, msgs, tools, forceTools, cb)
	})
}

func (p *ollamaProvider) chatStreamOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	payload := p.payload(msgs, tools, forceTools)
	payload["stream"] = true
	out := Msg{Role: "assistant", Usage: &Usage{}}
	// Local model templates sometimes serialize the tool call as content JSON
	// (possibly fenced). Streaming that to the screen would print raw JSON, so
	// text is held back while it still looks like a possible tool-call blob.
	var buf strings.Builder
	flushed := false
	looksLikeCall := func(s string) bool {
		t := strings.TrimSpace(s)
		return t == "" || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "`") || strings.HasPrefix(t, "<")
	}
	err := postStream(ctx, p.url+"/api/chat", nil, payload, func(line []byte) error {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		var chunk struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done            bool `json:"done"`
			PromptEvalCount int  `json:"prompt_eval_count"`
			EvalCount       int  `json:"eval_count"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			return nil // tolerate non-JSON keepalives
		}
		if c := chunk.Message.Content; c != "" {
			buf.WriteString(c)
			if flushed {
				onText(c)
			} else if !looksLikeCall(buf.String()) {
				onText(buf.String())
				flushed = true
			}
		}
		for _, tc := range chunk.Message.ToolCalls {
			args := map[string]any{}
			_ = json.Unmarshal(tc.Function.Arguments, &args)
			out.Calls = append(out.Calls, ToolCall{
				ID: fmt.Sprintf("call_%d", len(out.Calls)), Name: tc.Function.Name, Args: args,
			})
		}
		if chunk.Done {
			out.Usage.In = chunk.PromptEvalCount
			out.Usage.Out = chunk.EvalCount
		}
		return nil
	})
	if err != nil && isNoToolsErr(err) && !flushed {
		// Tool-less models (most vision models) reject the definitions
		// with HTTP 400 before any bytes stream — degrade and retry once.
		p.noTools.Store(true)
		return p.chatStreamOnce(ctx, msgs, tools, false, onText)
	}
	if err != nil {
		return Msg{}, err
	}
	out.Content = buf.String()
	if len(out.Calls) == 0 {
		if calls := parseContentToolCalls(out.Content, tools); len(calls) > 0 {
			out.Calls = calls
			out.Content = ""
		} else if !flushed && out.Content != "" {
			onText(out.Content) // held back but turned out to be real text
		}
	}
	return out, nil
}

// --- Anthropic streaming -----------------------------------------------------

func (p *anthropicProvider) ChatStream(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	return streamWithRetry(ctx, onText, func(cb func(string)) (Msg, error) {
		return p.chatStreamOnce(ctx, msgs, tools, forceTools, cb)
	})
}

func (p *anthropicProvider) chatStreamOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	payload := p.payload(msgs, tools, forceTools)
	payload["stream"] = true
	out := Msg{Role: "assistant", Usage: &Usage{}}
	type block struct {
		kind string // "text" | "tool_use"
		id   string
		name string
		json strings.Builder
	}
	blocks := map[int]*block{}
	err := postStream(ctx, p.base()+"/v1/messages", map[string]string{
		"x-api-key": p.key, "anthropic-version": "2023-06-01",
	}, payload, func(line []byte) error {
		data, ok := sseData(line)
		if !ok {
			return nil
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil
		}
		switch ev.Type {
		case "message_start":
			out.Usage.In = ev.Message.Usage.InputTokens
		case "content_block_start":
			blocks[ev.Index] = &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return nil
			}
			if ev.Delta.Type == "text_delta" {
				out.Content += ev.Delta.Text
				onText(ev.Delta.Text)
			}
			if ev.Delta.Type == "input_json_delta" {
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			b := blocks[ev.Index]
			if b != nil && b.kind == "tool_use" {
				args := map[string]any{}
				if s := b.json.String(); s != "" {
					_ = json.Unmarshal([]byte(s), &args)
				}
				out.Calls = append(out.Calls, ToolCall{ID: b.id, Name: b.name, Args: args})
			}
		case "message_delta":
			out.Usage.Out = ev.Usage.OutputTokens
		}
		return nil
	})
	if err != nil {
		return Msg{}, scrub(err, p.key)
	}
	return out, nil
}

// --- OpenAI streaming --------------------------------------------------------

func (p *openaiProvider) ChatStream(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	return streamWithRetry(ctx, onText, func(cb func(string)) (Msg, error) {
		return p.chatStreamOnce(ctx, msgs, tools, forceTools, cb)
	})
}

func (p *openaiProvider) chatStreamOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool, onText func(string)) (Msg, error) {
	payload := p.payload(msgs, tools, forceTools)
	payload["stream"] = true
	payload["stream_options"] = map[string]any{"include_usage": true}
	out := Msg{Role: "assistant", Usage: &Usage{}}
	type acc struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*acc{}
	maxIdx := -1
	err := postStream(ctx, p.base()+"/v1/chat/completions", p.authHeaders(), payload, func(line []byte) error {
		data, ok := sseData(line)
		if !ok || string(data) == "[DONE]" {
			return nil
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil
		}
		if ev.Usage != nil {
			out.Usage.In = ev.Usage.PromptTokens
			out.Usage.Out = ev.Usage.CompletionTokens
		}
		if len(ev.Choices) == 0 {
			return nil
		}
		d := ev.Choices[0].Delta
		if d.Content != "" {
			out.Content += d.Content
			onText(d.Content)
		}
		for _, tc := range d.ToolCalls {
			a := calls[tc.Index]
			if a == nil {
				a = &acc{}
				calls[tc.Index] = a
				if tc.Index > maxIdx {
					maxIdx = tc.Index
				}
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
			if tc.Index > maxIdx {
				maxIdx = tc.Index
			}
		}
		return nil
	})
	if err != nil {
		return Msg{}, scrub(err, p.key)
	}
	for i := 0; i <= maxIdx; i++ {
		a := calls[i]
		if a == nil || a.name == "" {
			continue
		}
		args := map[string]any{}
		if s := a.args.String(); s != "" {
			_ = json.Unmarshal([]byte(s), &args)
		}
		out.Calls = append(out.Calls, ToolCall{ID: a.id, Name: a.name, Args: args})
	}
	return out, nil
}
