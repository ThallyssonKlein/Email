package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Response identical to what Gmail returns when the per-minute quota is exhausted.
const quotaBody = `{"error":{"code":403,"message":"Quota exceeded for quota metric 'Total Query Cost' and limit 'Units per minute per user' of service 'gmail.googleapis.com'","errors":[{"message":"Quota exceeded","domain":"usageLimits","reason":"rateLimitExceeded"}],"status":"PERMISSION_DENIED"}}`

func TestQuotaPauseAndResume(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) <= 3 { // the first 3 calls exhaust the quota
			w.WriteHeader(403)
			w.Write([]byte(quotaBody))
			return
		}
		w.Write([]byte(`{"id":"abc","threadId":"t1","payload":{"mimeType":"text/plain","headers":[{"name":"Subject","value":"Hi"}],"body":{"data":"SGVsbG8gd29ybGQ="}}}`))
	}))
	defer srv.Close()

	gsrv, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	prog := newProgress("test", "emails", 1)
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, gmailBurst), prog: prog, pauseFor: 200 * time.Millisecond}

	start := time.Now()
	msg, err := getWithRetry(context.Background(), gsrv, gate, "abc")
	if err != nil {
		t.Fatalf("should have recovered from the quota error, but failed: %v", err)
	}
	if took := time.Since(start); took < 3*200*time.Millisecond {
		t.Fatalf("did not pause between attempts (took %s)", took)
	}
	e := parseMessage(msg)
	if e.Subject != "Hi" || e.Body != "Hello world" {
		t.Fatalf("wrong parse: %+v", e)
	}
	t.Logf("recovered after %d calls in %s", calls.Load(), time.Since(start).Round(time.Millisecond))
}

func TestNonQuota403Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"code":403,"message":"Insufficient Permission","errors":[{"reason":"insufficientPermissions"}]}}`))
	}))
	defer srv.Close()
	gsrv, _ := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, 1), prog: newProgress("test", "emails", 1), pauseFor: time.Hour}

	_, err := getWithRetry(context.Background(), gsrv, gate, "abc")
	if err == nil {
		t.Fatal("permission 403 should not be treated as quota")
	}
	if got := shortErr(err); got != "HTTP 403: Insufficient Permission" {
		t.Fatalf("unexpected short message: %q", got)
	}
}

func TestQuotaRetriesAreBounded(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		w.Write([]byte(quotaBody))
	}))
	defer srv.Close()
	gsrv, _ := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, gmailBurst), prog: newProgress("test", "emails", 1), pauseFor: time.Millisecond}

	if _, err := getWithRetry(context.Background(), gsrv, gate, "abc"); err == nil {
		t.Fatal("should give up after maxQuotaRetries")
	}
	if got := calls.Load(); got != maxQuotaRetries+1 {
		t.Fatalf("calls = %d, expected %d", got, maxQuotaRetries+1)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	h := http.Header{}
	if _, ok := retryAfter(h); ok {
		t.Fatal("without the header there should be no Retry-After")
	}
	h.Set("Retry-After", "7")
	if d, ok := retryAfter(h); !ok || d != 7*time.Second {
		t.Fatalf("got %v %v", d, ok)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d, ok := retryAfter(h); !ok || d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("HTTP date: got %v %v", d, ok)
	}
}
