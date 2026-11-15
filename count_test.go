package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"golang.org/x/time/rate"
)

func testAnthropicClient(url string) *anthropicClient {
	return &anthropicClient{
		api:      anthropic.NewClient(option.WithBaseURL(url), option.WithAPIKey("x"), option.WithMaxRetries(0)),
		limiter:  rate.NewLimiter(rate.Inf, 1),
		inflight: make(chan struct{}, maxConcurrentCountRequests),
	}
}

func TestAnthropicHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer srv.Close()

	start := time.Now()
	n, err := testAnthropicClient(srv.URL).count(context.Background(), "m", []string{"oi"})
	if err != nil || n != 42 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("não esperou o Retry-After")
	}
}

func TestAnthropicClientErrorDoesNotRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer srv.Close()

	if _, err := testAnthropicClient(srv.URL).count(context.Background(), "m", []string{"oi"}); err == nil {
		t.Fatal("esperava erro")
	}
	if calls.Load() != 1 {
		t.Fatalf("401 não deve ter retry, chamadas = %d", calls.Load())
	}
}
