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

// Resposta idêntica à que o Gmail devolve quando a cota por minuto estoura.
const quotaBody = `{"error":{"code":403,"message":"Quota exceeded for quota metric 'Total Query Cost' and limit 'Units per minute per user' of service 'gmail.googleapis.com'","errors":[{"message":"Quota exceeded","domain":"usageLimits","reason":"rateLimitExceeded"}],"status":"PERMISSION_DENIED"}}`

func TestQuotaPauseAndResume(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) <= 3 { // as 3 primeiras chamadas estouram a cota
			w.WriteHeader(403)
			w.Write([]byte(quotaBody))
			return
		}
		w.Write([]byte(`{"id":"abc","threadId":"t1","payload":{"mimeType":"text/plain","headers":[{"name":"Subject","value":"Oi"}],"body":{"data":"T2zDoSBtdW5kbw=="}}}`))
	}))
	defer srv.Close()

	gsrv, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	prog := newProgress("teste", "emails", 1)
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, gmailBurst), prog: prog, pauseFor: 200 * time.Millisecond}

	start := time.Now()
	msg, err := getWithRetry(context.Background(), gsrv, gate, "abc")
	if err != nil {
		t.Fatalf("deveria ter se recuperado da cota, mas falhou: %v", err)
	}
	if took := time.Since(start); took < 3*200*time.Millisecond {
		t.Fatalf("não pausou entre as tentativas (levou %s)", took)
	}
	e := parseMessage(msg)
	if e.Subject != "Oi" || e.Body != "Olá mundo" {
		t.Fatalf("parse errado: %+v", e)
	}
	t.Logf("recuperou após %d chamadas em %s", calls.Load(), time.Since(start).Round(time.Millisecond))
}

func TestNonQuota403Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"code":403,"message":"Insufficient Permission","errors":[{"reason":"insufficientPermissions"}]}}`))
	}))
	defer srv.Close()
	gsrv, _ := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, 1), prog: newProgress("teste", "emails", 1), pauseFor: time.Hour}

	_, err := getWithRetry(context.Background(), gsrv, gate, "abc")
	if err == nil {
		t.Fatal("403 de permissão não deveria ser tratado como cota")
	}
	if got := shortErr(err); got != "HTTP 403: Insufficient Permission" {
		t.Fatalf("mensagem curta inesperada: %q", got)
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
	gate := &quotaGate{limiter: rate.NewLimiter(rate.Inf, gmailBurst), prog: newProgress("teste", "emails", 1), pauseFor: time.Millisecond}

	if _, err := getWithRetry(context.Background(), gsrv, gate, "abc"); err == nil {
		t.Fatal("deveria desistir depois de maxQuotaRetries")
	}
	if got := calls.Load(); got != maxQuotaRetries+1 {
		t.Fatalf("chamadas = %d, esperado %d", got, maxQuotaRetries+1)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	h := http.Header{}
	if _, ok := retryAfter(h); ok {
		t.Fatal("sem header não deveria haver Retry-After")
	}
	h.Set("Retry-After", "7")
	if d, ok := retryAfter(h); !ok || d != 7*time.Second {
		t.Fatalf("got %v %v", d, ok)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d, ok := retryAfter(h); !ok || d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("data HTTP: got %v %v", d, ok)
	}
}
