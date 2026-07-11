package provider

import (
	"context"
	"encoding/json"
	"fmt"
)

// --- Google Gemini ----------------------------------------------------------

type geminiProvider struct {
	model string
	key   string
}

func (p *geminiProvider) Name() string { return "gemini:" + p.model }

func (p *geminiProvider) Chat(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	return withRetry(ctx, func() (Msg, error) { return p.chatOnce(ctx, msgs, tools, forceTools) })
}

func (p *geminiProvider) chatOnce(ctx context.Context, msgs []Msg, tools []ToolDef, forceTools bool) (Msg, error) {
	var system string
	var contents []map[string]any
	// callID → tool name: Gemini's functionResponse is keyed by NAME, and our
	// tool msgs carry both, so no state beyond this conversation is needed.
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = m.Content
		case "assistant":
			var parts []map[string]any
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, c := range m.Calls {
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{"name": c.Name, "args": c.Args},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}
		case "tool":
			part := map[string]any{
				"functionResponse": map[string]any{
					"name":     m.Name,
					"response": map[string]any{"result": m.Content},
				},
			}
			// Gemini expects all functionResponses for one model turn in a
			// single user content (mirrors the Anthropic coalescing).
			if n := len(contents); n > 0 && contents[n-1]["role"] == "user" {
				if parts, ok := contents[n-1]["parts"].([]map[string]any); ok && len(parts) > 0 {
					if _, isFR := parts[0]["functionResponse"]; isFR {
						contents[n-1]["parts"] = append(parts, part)
						continue
					}
				}
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{part}})
		default:
			contents = append(contents, map[string]any{
				"role": "user", "parts": []map[string]any{{"text": m.Content}},
			})
		}
	}

	var decls []map[string]any
	for _, t := range tools {
		d := map[string]any{"name": t.Name, "description": t.Description}
		// Gemini rejects an object schema with zero properties — omit instead.
		if props, ok := t.Parameters["properties"].(map[string]any); ok && len(props) > 0 {
			d["parameters"] = t.Parameters
		}
		decls = append(decls, d)
	}
	payload := map[string]any{
		"contents":         contents,
		"tools":            []map[string]any{{"functionDeclarations": decls}},
		"generationConfig": map[string]any{"temperature": 0},
	}
	if system != "" {
		payload["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
	}
	if forceTools {
		payload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", p.model)
	raw, err := postJSON(ctx, url, map[string]string{"x-goog-api-key": p.key}, payload)
	if err != nil {
		return Msg{}, scrub(err, p.key)
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Msg{}, err
	}
	out := Msg{Role: "assistant", Usage: &Usage{
		In: resp.UsageMetadata.PromptTokenCount, Out: resp.UsageMetadata.CandidatesTokenCount}}
	if len(resp.Candidates) == 0 {
		return Msg{}, fmt.Errorf("gemini: no candidates")
	}
	for i, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			out.Content += part.Text
		}
		if fc := part.FunctionCall; fc != nil {
			args := fc.Args
			if args == nil {
				args = map[string]any{}
			}
			out.Calls = append(out.Calls, ToolCall{
				ID: fmt.Sprintf("call_%d", i), Name: fc.Name, Args: args,
			})
		}
	}
	return out, nil
}
