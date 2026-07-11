// Package provider abstracts one chat turn against any model backend
// (local Ollama, Anthropic, OpenAI) behind a provider-neutral message shape.
// API keys are injected by the caller (see internal/creds) and are never
// read from disk or written anywhere by this package; error paths are
// scrubbed so a key can never appear in output or session files.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Msg is one chat message in the provider-neutral shape.
type Msg struct {
	Role    string     // "system" | "user" | "assistant" | "tool"
	Content string     // text content (or tool result payload for Role=="tool")
	Calls   []ToolCall // tool calls the assistant issued (Role=="assistant")
	CallID  string     // for Role=="tool": which call this result answers
	Name    string     // for Role=="tool": the tool name
	Usage   *Usage     // token usage of the API call that produced this reply
}

// Usage is one API call's token accounting as the provider reported it.
type Usage struct {
	In  int // input/prompt tokens
	Out int // output/completion tokens
}

// ToolCall is a provider-neutral tool invocation.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolDef is a provider-neutral tool definition (JSON-schema parameters).
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Provider abstracts one chat turn against any model backend.
type Provider interface {
	// Chat sends the conversation and returns the assistant's reply.
	// forceTools requests that the model MUST call a tool this turn (the
	// invocation wall for small local models); providers that cannot express
	// it may ignore it. Cancelling ctx aborts the request.
	Chat(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error)
	Name() string
}

// httpError carries the status code so the retry layer can classify.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

// retryable reports whether an error is worth retrying: transport failures
// and 429/5xx. Context cancellation and 4xx (auth, bad request) are not.
func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.code == 429 || he.code >= 500
	}
	return true // transport-level (conn refused, reset, EOF)
}

// withRetry runs fn up to 3 times with 1s/4s backoff on retryable errors.
func withRetry(ctx context.Context, fn func() (Msg, error)) (Msg, error) {
	var msg Msg
	var err error
	for attempt, delay := 0, time.Second; attempt < 3; attempt, delay = attempt+1, delay*4 {
		msg, err = fn()
		if err == nil || !retryable(err) {
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

var httpc = &http.Client{Timeout: 10 * time.Minute}

func postJSON(ctx context.Context, url string, headers map[string]string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &httpError{code: resp.StatusCode,
			msg: fmt.Sprintf("%s: HTTP %d: %s", url, resp.StatusCode, truncate(string(out), 300))}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// NewProvider resolves a model spec to a provider:
//
//	ollama:<model>     local Ollama (default http://localhost:11434)
//	claude:<model>     Anthropic API (ANTHROPIC_API_KEY)
//	openai:<model>     OpenAI API (OPENAI_API_KEY)
//
// A bare spec with no prefix is treated as an Ollama model name.
// getKey resolves the API key for a vendor ("anthropic" | "openai"); it is
// only consulted for paid providers, so local-only usage never touches
// credentials at all.
func NewProvider(spec string, getKey func(vendor string) (string, error)) (Provider, error) {
	kind, model, found := strings.Cut(spec, ":")
	if !found {
		kind, model = "ollama", spec
	}
	switch kind {
	case "ollama":
		return &ollamaProvider{model: model, url: envOr("OLLAMA_HOST_URL", "http://localhost:11434")}, nil
	case "claude", "anthropic":
		key, err := getKey("anthropic")
		if err != nil {
			return nil, fmt.Errorf("claude:%s: %w", model, err)
		}
		return &anthropicProvider{model: model, key: key}, nil
	case "openai", "gpt":
		key, err := getKey("openai")
		if err != nil {
			return nil, fmt.Errorf("openai:%s: %w", model, err)
		}
		return &openaiProvider{model: model, key: key}, nil
	default:
		// "qwen3-coder:30b" — the whole spec is an Ollama model tag.
		return &ollamaProvider{model: spec, url: envOr("OLLAMA_HOST_URL", "http://localhost:11434")}, nil
	}
}

// scrub removes the API key from an error before it can reach output,
// transcripts, or session files. Defense in depth: no current error path
// embeds the key, but provider errors quote server responses and URLs, and
// this guarantee must survive refactors.
func scrub(err error, key string) error {
	if err == nil || key == "" {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), key, "[redacted]")
	// Preserve the error's classification: losing the *httpError type here
	// made the retry layer treat auth failures as transient (measured: a 401
	// was retried 3 times before this was caught by TestNoRetryOn401).
	var he *httpError
	if errors.As(err, &he) {
		return &httpError{code: he.code, msg: msg}
	}
	return fmt.Errorf("%s", msg)
}

// numCtx is Ollama's context window; MASON_NUM_CTX overrides the default.
func numCtx() int {
	if v := os.Getenv("MASON_NUM_CTX"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 16384
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// --- Ollama ---------------------------------------------------------------

type ollamaProvider struct {
	model string
	url   string
}

func (p *ollamaProvider) Name() string { return "ollama:" + p.model }

func (p *ollamaProvider) payload(msgs []Msg, tools []ToolDef, forceTools bool) map[string]any {
	messages := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		mm := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Calls) > 0 {
			var calls []map[string]any
			for _, c := range m.Calls {
				calls = append(calls, map[string]any{
					"function": map[string]any{"name": c.Name, "arguments": c.Args},
				})
			}
			mm["tool_calls"] = calls
		}
		messages = append(messages, mm)
	}
	var tdefs []map[string]any
	for _, t := range tools {
		tdefs = append(tdefs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters,
			},
		})
	}
	payload := map[string]any{
		"model": p.model, "messages": messages, "tools": tdefs, "stream": false,
		"options": map[string]any{"temperature": 0, "num_ctx": numCtx()},
	}
	if forceTools {
		payload["tool_choice"] = "required"
	}
	return payload
}

func (p *ollamaProvider) Chat(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	return withRetry(ctx, func() (Msg, error) { return p.chatOnce(ctx, msgs, tools, forceTools) })
}

func (p *ollamaProvider) chatOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	raw, err := postJSON(ctx, p.url+"/api/chat", nil, p.payload(msgs, tools, forceTools))
	if err != nil {
		return Msg{}, err
	}
	var resp struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Msg{}, err
	}
	out := Msg{Role: "assistant", Content: resp.Message.Content,
		Usage: &Usage{In: resp.PromptEvalCount, Out: resp.EvalCount}}
	for i, c := range resp.Message.ToolCalls {
		args := map[string]any{}
		_ = json.Unmarshal(c.Function.Arguments, &args)
		out.Calls = append(out.Calls, ToolCall{
			ID: fmt.Sprintf("call_%d", i), Name: c.Function.Name, Args: args,
		})
	}
	// Some Ollama model templates never populate tool_calls and emit the
	// call(s) as raw JSON in content instead (qwen2.5-coder:14b) — sometimes
	// SEVERAL fenced blocks in one reply. The model's decisions are correct;
	// only the serialization is nonstandard — parse them all, in order.
	if len(out.Calls) == 0 {
		if calls := parseContentToolCalls(out.Content, tools); len(calls) > 0 {
			out.Calls = calls
			out.Content = ""
		}
	}
	return out, nil
}

// parseContentToolCalls recovers every tool call serialized into content:
// the whole content as one JSON object, or any number of fenced blocks.
func parseContentToolCalls(content string, tools []ToolDef) []ToolCall {
	if c := parseContentToolCall(content, tools); c != nil {
		return []ToolCall{*c}
	}
	var out []ToolCall
	for i, chunk := range strings.Split(content, "```") {
		chunk = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(chunk), "json"))
		if !strings.HasPrefix(chunk, "{") {
			continue
		}
		if c := parseContentToolCall(chunk, tools); c != nil {
			c.ID = fmt.Sprintf("call_content_%d", i)
			out = append(out, *c)
		}
	}
	return out
}

// parseContentToolCall recognizes `{"name": <known tool>, "arguments": {...}}`
// (optionally fenced) emitted as plain content and converts it to a ToolCall.
func parseContentToolCall(content string, tools []ToolDef) *ToolCall {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var obj struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj.Name == "" {
		return nil
	}
	for _, t := range tools {
		if t.Name == obj.Name {
			args := obj.Arguments
			if args == nil {
				args = map[string]any{}
			}
			return &ToolCall{ID: "call_content", Name: obj.Name, Args: args}
		}
	}
	return nil
}

// --- Anthropic --------------------------------------------------------------

type anthropicProvider struct {
	model string
	key   string
	url   string // base URL, overridable for tests
}

func (p *anthropicProvider) base() string {
	if p.url != "" {
		return p.url
	}
	return "https://api.anthropic.com"
}

func (p *anthropicProvider) Name() string { return "claude:" + p.model }

func (p *anthropicProvider) payload(msgs []Msg, tools []ToolDef, forceTools bool) map[string]any {
	var system string
	var messages []map[string]any
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = m.Content
		case "assistant":
			blocks := []map[string]any{}
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.Calls {
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Args,
				})
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{
				"type": "tool_result", "tool_use_id": m.CallID, "content": m.Content,
			}
			// All tool_results answering one assistant turn must arrive in ONE
			// user message — parallel tool calls break otherwise.
			if n := len(messages); n > 0 && messages[n-1]["role"] == "user" {
				if blocks, ok := messages[n-1]["content"].([]map[string]any); ok && len(blocks) > 0 && blocks[0]["type"] == "tool_result" {
					messages[n-1]["content"] = append(blocks, block)
					continue
				}
			}
			messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{block}})
		default:
			messages = append(messages, map[string]any{"role": "user", "content": m.Content})
		}
	}
	var tdefs []map[string]any
	for _, t := range tools {
		tdefs = append(tdefs, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": t.Parameters,
		})
	}
	payload := map[string]any{
		"model": p.model, "max_tokens": 8192, "system": system,
		"messages": messages, "tools": tdefs,
	}
	if forceTools {
		payload["tool_choice"] = map[string]any{"type": "any"}
	}
	return payload
}

func (p *anthropicProvider) Chat(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	return withRetry(ctx, func() (Msg, error) { return p.chatOnce(ctx, msgs, tools, forceTools) })
}

func (p *anthropicProvider) chatOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	raw, err := postJSON(ctx, p.base()+"/v1/messages", map[string]string{
		"x-api-key": p.key, "anthropic-version": "2023-06-01",
	}, p.payload(msgs, tools, forceTools))
	if err != nil {
		return Msg{}, scrub(err, p.key)
	}
	var resp struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Msg{}, err
	}
	out := Msg{Role: "assistant",
		Usage: &Usage{In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens}}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			out.Content += b.Text
		case "tool_use":
			out.Calls = append(out.Calls, ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	return out, nil
}

// --- OpenAI -----------------------------------------------------------------

type openaiProvider struct {
	model string
	key   string
	url   string // base URL, overridable for tests
}

func (p *openaiProvider) base() string {
	if p.url != "" {
		return p.url
	}
	return "https://api.openai.com"
}

func (p *openaiProvider) Name() string { return "openai:" + p.model }

func (p *openaiProvider) payload(msgs []Msg, tools []ToolDef, forceTools bool) map[string]any {
	var messages []map[string]any
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			mm := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.Calls) > 0 {
				var calls []map[string]any
				for _, c := range m.Calls {
					args, _ := json.Marshal(c.Args)
					calls = append(calls, map[string]any{
						"id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": string(args)},
					})
				}
				mm["tool_calls"] = calls
			}
			messages = append(messages, mm)
		case "tool":
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": m.CallID, "content": m.Content,
			})
		default:
			messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	var tdefs []map[string]any
	for _, t := range tools {
		tdefs = append(tdefs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters,
			},
		})
	}
	payload := map[string]any{"model": p.model, "messages": messages, "tools": tdefs}
	if forceTools {
		payload["tool_choice"] = "required"
	}
	return payload
}

func (p *openaiProvider) Chat(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	return withRetry(ctx, func() (Msg, error) { return p.chatOnce(ctx, msgs, tools, forceTools) })
}

func (p *openaiProvider) chatOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	raw, err := postJSON(ctx, p.base()+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + p.key,
	}, p.payload(msgs, tools, forceTools))
	if err != nil {
		return Msg{}, scrub(err, p.key)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Msg{}, err
	}
	if len(resp.Choices) == 0 {
		return Msg{}, fmt.Errorf("openai: empty choices")
	}
	m := resp.Choices[0].Message
	out := Msg{Role: "assistant", Content: m.Content,
		Usage: &Usage{In: resp.Usage.PromptTokens, Out: resp.Usage.CompletionTokens}}
	for _, c := range m.ToolCalls {
		args := map[string]any{}
		_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
		out.Calls = append(out.Calls, ToolCall{ID: c.ID, Name: c.Function.Name, Args: args})
	}
	return out, nil
}


// ErrNoModel signals that no provider is available — the CLI catches this
// to offer the guided local-model setup instead of a bare error.
var errNoModel = fmt.Errorf("no model available")

// IsNoModel reports whether err is the no-provider-available condition.
func IsNoModel(err error) bool {
	return errors.Is(err, errNoModel)
}

