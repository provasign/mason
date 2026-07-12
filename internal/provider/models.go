package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RemoteModel is one entry from a vendor's own live model-list endpoint —
// the authoritative, always-current source. Hand-maintained catalogs go
// stale the moment a vendor ships a new model; querying the vendor's API
// directly never does.
type RemoteModel struct {
	ID          string
	DisplayName string
}

// Label renders a human-friendly line for the picker.
func (m RemoteModel) Label() string {
	if m.DisplayName != "" && m.DisplayName != m.ID {
		return fmt.Sprintf("%s (%s)", m.DisplayName, m.ID)
	}
	return m.ID
}

const listModelsTimeout = 10 * time.Second

// ListModels queries vendor's live model-list endpoint and returns every
// model mason can plausibly drive (chat/coding-capable; vendor-side
// embeddings, TTS, image, and moderation models are filtered out).
func ListModels(vendor, key string) ([]RemoteModel, error) {
	switch vendor {
	case "anthropic":
		return listAnthropicModelsAt(defaultAnthropicBase, key)
	case "openai":
		return listOpenAIModelsAt(defaultOpenAIBase, key)
	default:
		return nil, fmt.Errorf("no live model catalog for %q", vendor)
	}
}

const (
	defaultAnthropicBase = "https://api.anthropic.com"
	defaultOpenAIBase    = "https://api.openai.com"
)

func getJSON(url string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listModelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
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

func listAnthropicModelsAt(base, key string) ([]RemoteModel, error) {
	raw, err := getJSON(base+"/v1/models?limit=1000", map[string]string{
		"x-api-key": key, "anthropic-version": "2023-06-01",
	})
	if err != nil {
		return nil, scrub(err, key)
	}
	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	out := make([]RemoteModel, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, RemoteModel{ID: d.ID, DisplayName: d.DisplayName})
	}
	return out, nil
}

// openaiChatModelRe matches OpenAI model IDs mason can drive as a chat
// model (gpt-*, o-series reasoning models, chatgpt-*).
var openaiChatModelRe = regexp.MustCompile(`^(gpt-|o[0-9]|chatgpt-)`)

// openaiExcludeRe screens out vendor-side non-chat variants that otherwise
// match the prefix above (audio/transcription/TTS/embedding/etc).
var openaiExcludeRe = regexp.MustCompile(`(?i)audio|realtime|transcribe|tts|whisper|embedding|moderation|instruct|image|search-preview`)

func listOpenAIModelsAt(base, key string) ([]RemoteModel, error) {
	raw, err := getJSON(base+"/v1/models", map[string]string{"Authorization": "Bearer " + key})
	if err != nil {
		return nil, scrub(err, key)
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	type stamped struct {
		RemoteModel
		created int64
	}
	var kept []stamped
	for _, d := range resp.Data {
		id := strings.ToLower(d.ID)
		if !openaiChatModelRe.MatchString(id) || openaiExcludeRe.MatchString(id) {
			continue
		}
		kept = append(kept, stamped{RemoteModel{ID: d.ID}, d.Created})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].created > kept[j].created })
	out := make([]RemoteModel, len(kept))
	for i, k := range kept {
		out[i] = k.RemoteModel
	}
	return out, nil
}
