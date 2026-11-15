# Parallelism Rule

Whenever the work allows it, run independent operations in parallel. Sequential code is the exception and needs a reason (data dependency, ordering requirement, or a rate limit).

## Hard Rules

1. **Parallelize independent I/O and CPU work.**
   Calls with no data dependency between them (HTTP requests, DB queries, file reads, per-item processing) must not run one after another.

   ```go
   // ✅ Correct — independent calls run concurrently
   g, ctx := errgroup.WithContext(ctx)
   var customer Customer
   var orders []Order
   g.Go(func() (err error) { customer, err = customerClient.Fetch(ctx, id); return })
   g.Go(func() (err error) { orders, err = orderClient.List(ctx, id); return })
   if err := g.Wait(); err != nil {
       return Summary{}, err
   }

   // ❌ Wrong — second call waits for the first for no reason
   customer, err := customerClient.Fetch(ctx, id)
   ...
   orders, err := orderClient.List(ctx, id)
   ```

2. **Use `errgroup` for fan-out/fan-in.**
   Prefer `golang.org/x/sync/errgroup` over hand-rolled `sync.WaitGroup` + error channels. It propagates the first error and cancels the shared context.

3. **Always bound the concurrency.**
   Never spawn one goroutine per item of an unbounded collection. Use `errgroup.Group.SetLimit(n)` or a worker pool, and name the limit as a constant.

   ```go
   // ✅ Correct — bounded fan-out
   const maxConcurrentFetches = 10

   g, ctx := errgroup.WithContext(ctx)
   g.SetLimit(maxConcurrentFetches)
   for _, id := range ids {
       g.Go(func() error { return process(ctx, id) })
   }
   return g.Wait()

   // ❌ Wrong — 100k items = 100k goroutines and 100k open connections
   for _, id := range ids {
       go process(ctx, id)
   }
   ```

4. **Every goroutine must have an owner, a way to stop, and a way to report errors.**
   No fire-and-forget `go func()`. Pass a `context.Context`, wait for completion (`errgroup` / `WaitGroup`), and never leak goroutines.

5. **Write results without data races.**
   Each goroutine writes to its own slot (`results[i]`) or sends over a channel. Protect shared state with `sync.Mutex` / `atomic`, and run tests with `go test -race`.

   ```go
   // ✅ Correct — one slot per goroutine, order preserved
   results := make([]Result, len(items))
   for i, item := range items {
       g.Go(func() (err error) { results[i], err = handle(ctx, item); return })
   }
   ```

6. **Propagate `context.Context` everywhere.**
   It is the first parameter of every function that does I/O or spawns goroutines, and it carries cancellation and deadlines.

7. **Do not parallelize when order or dependency matters.**
   Sequential steps, transactions on the same rows, or operations whose result feeds the next one stay sequential.

## Why

Go makes concurrency cheap, and most services spend their time waiting on I/O. Running independent work in parallel cuts latency, while bounded concurrency, owned goroutines, and context cancellation keep it safe under load.
