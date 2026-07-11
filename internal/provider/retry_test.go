package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Transient 5xx must be retried and then succeed.
func TestRetryOn500(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	p := &anthropicProvider{model: "m", key: "k", url: srv.URL}
	msg, err := p.Chat(context.Background(), nil, nil, false)
	if err != nil {
		t.Fatalf("should have recovered after retries: %v", err)
	}
	if msg.Content != "ok" || n.Load() != 3 {
		t.Fatalf("content=%q attempts=%d", msg.Content, n.Load())
	}
}

// Auth errors must NOT be retried — fail fast with the message.
func TestNoRetryOn401(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	p := &anthropicProvider{model: "m", key: "k", url: srv.URL}
	if _, err := p.Chat(context.Background(), nil, nil, false); err == nil {
		t.Fatal("401 must surface as an error")
	}
	if n.Load() != 1 {
		t.Fatalf("401 was retried %d times", n.Load())
	}
}

// A cancelled context must abort promptly, not sit in backoff.
func TestRetryRespectsCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &anthropicProvider{model: "m", key: "k", url: srv.URL}
	if _, err := p.Chat(ctx, nil, nil, false); err == nil {
		t.Fatal("cancelled context must error")
	}
}
