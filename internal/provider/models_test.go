package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListAnthropicModels(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		w.Write([]byte(`{"data":[
			{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"},
			{"id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5"}
		]}`))
	}))
	defer srv.Close()
	models, err := listAnthropicModelsAt(srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models[0].ID != "claude-sonnet-5" || models[0].DisplayName != "Claude Sonnet 5" {
		t.Fatalf("wrong model: %+v", models[0])
	}
	if gotHeaders.Get("x-api-key") != "sk-ant-test" || gotHeaders.Get("anthropic-version") == "" {
		t.Fatalf("auth headers missing: %v", gotHeaders)
	}
	if got := models[0].Label(); got != "Claude Sonnet 5 (claude-sonnet-5)" {
		t.Fatalf("Label() = %q", got)
	}
}

func TestListOpenAIModelsFiltersAndSortsByRecency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth header = %q", got)
		}
		w.Write([]byte(`{"data":[
			{"id":"gpt-4o-mini","created":100},
			{"id":"gpt-4.1","created":300},
			{"id":"whisper-1","created":200},
			{"id":"text-embedding-3-large","created":200},
			{"id":"o3","created":250},
			{"id":"gpt-4o-audio-preview","created":260},
			{"id":"tts-1","created":200},
			{"id":"dall-e-3","created":200},
			{"id":"gpt-4o-mini-transcribe","created":260}
		]}`))
	}))
	defer srv.Close()
	models, err := listOpenAIModelsAt(srv.URL, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := []string{"gpt-4.1", "o3", "gpt-4o-mini"} // sorted newest-created first
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v (non-chat models must be filtered)", ids, want)
	}
}

func TestListModelsUnknownVendor(t *testing.T) {
	if _, err := ListModels("mistral", "x"); err == nil {
		t.Fatal("unknown vendor must error")
	}
}

func TestListModelsScrubsKeyOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte("bad key sk-ant-secret123"))
	}))
	defer srv.Close()
	_, err := listAnthropicModelsAt(srv.URL, "sk-ant-secret123")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-ant-secret123") {
		t.Fatalf("key leaked into error: %v", err)
	}
}
