package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"math/rand/v2"
)

// retryAfter lê o header Retry-After (segundos ou data HTTP). ok=false se
// ausente ou inválido.
func retryAfter(h http.Header) (d time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// withJitter devolve um valor aleatório em [d/2, d], para que vários
// workers não tentem de novo todos no mesmo instante.
func withJitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(half+1)
}
