package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"math/rand/v2"
)

// retryAfter reads the Retry-After header (seconds or HTTP date). ok=false if
// absent or invalid.
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

// withJitter returns a random value in [d/2, d], so that several
// workers do not retry all at the same instant.
func withJitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(half+1)
}
