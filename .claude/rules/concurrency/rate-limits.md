# API Rate Limits Rule

Parallelism must never push a client past the limits of the APIs it calls. Every outbound call to an external API must respect its documented rate limit.

## Hard Rules

1. **Know and name the limit of every external API.**
   Keep the documented limit (requests per second/minute, burst, concurrent requests) as a named constant or configuration value, next to the client that uses it.

2. **Throttle with `golang.org/x/time/rate`.**
   Create one `rate.Limiter` per API (or per API key / tenant when the limit is scoped that way) and call `limiter.Wait(ctx)` before each request. Never use `time.Sleep` as a rate limiter.

   ```go
   // ✅ Correct — shared limiter, context-aware
   const (
       providerRequestsPerSecond = 10
       providerBurst             = 5
   )

   type ProviderClient struct {
       http    *http.Client
       limiter *rate.Limiter
   }

   func NewProviderClient(httpClient *http.Client) *ProviderClient {
       return &ProviderClient{
           http:    httpClient,
           limiter: rate.NewLimiter(rate.Limit(providerRequestsPerSecond), providerBurst),
       }
   }

   func (c *ProviderClient) Fetch(ctx context.Context, id string) (Item, error) {
       if err := c.limiter.Wait(ctx); err != nil {
           return Item{}, fmt.Errorf("waiting for rate limit: %w", err)
       }
       // ... perform request
   }

   // ❌ Wrong — fixed sleep, ignores bursts and parallel callers
   time.Sleep(100 * time.Millisecond)
   ```

3. **The limiter is shared by all goroutines hitting the same API.**
   Create it once in the client constructor — never inside the function that makes the call, and never one per goroutine.

4. **Combine rate limiting with bounded concurrency.**
   `errgroup.SetLimit` caps in-flight requests; the limiter caps request rate. Use both when the API limits both.

5. **Honor server signals: `429` and `Retry-After`.**
   On `429 Too Many Requests` or `503`, wait for the `Retry-After` header (or exponential backoff with jitter when absent) and retry up to a named maximum. Adjust to `X-RateLimit-*` headers when the API provides them.

   ```go
   // ✅ Correct
   if resp.StatusCode == http.StatusTooManyRequests {
       wait := retryAfter(resp.Header, defaultBackoff)
       select {
       case <-time.After(wait):
           continue
       case <-ctx.Done():
           return ctx.Err()
       }
   }
   ```

6. **Retries must also pass through the limiter.**
   A retry is a new request — it consumes the same budget.

7. **Never retry without a bound.**
   Cap attempts (`maxRetries`) and total time through `context.WithTimeout`.

## Why

Parallelism multiplies request volume. Without throttling, a fan-out of a few hundred goroutines gets the API key blocked or banned. A shared, context-aware limiter plus server-driven backoff keeps throughput as high as the API permits, without exceeding it.
