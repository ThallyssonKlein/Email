# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`email-tokens`: a single-package Go CLI (module `email-tokens`) that downloads all emails from a Gmail account (read-only OAuth) into a local JSONL cache, then estimates how many tokens an AI would use to read them (OpenAI via local `tiktoken`, Claude via the Anthropic `count_tokens` API) plus an input-cost estimate. README.md documents setup (Google OAuth `credentials.json`, `.env` with `ANTHROPIC_API_KEY`) and CLI flags. Everything is in English: docs, code comments, test messages and user-facing strings; follow that.

## Commands

```bash
go build -o email-tokens .
go test -race ./...                      # project rules require running tests with -race
go test -race -run TestName ./...        # single test
go vet ./...

./email-tokens -max 500                  # quick run
./email-tokens -skip-fetch               # recount cached emails only (no Gmail calls)
./email-tokens -no-anthropic             # skip Anthropic API (offline OpenAI count only)
```

`credentials.json`, `token.json`, `.env`, `emails.jsonl` and the built binary hold secrets/personal data and are gitignored. Never commit them or print their contents.

## Architecture

Flow in `main.go`: parse flags → load `.env` → `fetchAll` (unless `-skip-fetch`) → `loadCache` → count tokens → `report.print`.

- `gmail.go`: OAuth login (browser flow, token saved to `token.json`), listing message IDs, and concurrent download. Already-cached IDs (`emails.jsonl`) are skipped, so interrupted runs resume. `parseMessage`/`walkParts` prefer `text/plain`, fall back to HTML converted to text (`htmlToText`, sets `HTMLOnly`); attachments contribute only their names. `Email.Text()` is the exact text that gets counted.
- `quotaGate` (gmail.go): the single shared rate limiter for all download workers. On a Gmail quota error it pauses *all* workers until the quota window renews (`Pause`), rather than each retrying independently. `getWithRetry` sends every attempt through the gate.
- `count.go`: `countOpenAI` (local, offline) and `anthropicClient` (shared `rate.Limiter` + in-flight cap; SDK retries disabled with `WithMaxRetries(0)` so every retry passes through our limiter). `makeBatches` packs many emails per request (`maxCharsPerRequest`) to keep API calls few. `claudeInputPrice` holds per-model prices; OpenAI price comes only from the `-openai-price` flag.
- `main.go` runs the OpenAI count and one count per Claude model concurrently in an `errgroup`; each goroutine writes to its own result slot, and a failure of one model is logged and does not cancel the others.
- `retry.go`: `Retry-After` parsing and jitter helpers. `progress.go`: shared progress bar (`SetNote`/`Log`/`Add`).

## Project rules (`.claude/rules/concurrency/`)

These are mandatory for any new or changed concurrent/network code:

- Parallelize independent I/O; use `errgroup` (not hand-rolled WaitGroup + error channels), always bounded via `SetLimit` with a named constant. No fire-and-forget goroutines; each needs an owner, a stop path via `context.Context`, and error reporting. Results go to per-goroutine slots or channels (no data races). `ctx` is the first parameter of anything doing I/O.
- Every external API call goes through a shared `golang.org/x/time/rate` limiter created once in the client constructor (never `time.Sleep` as throttling), with the documented limit as a named constant next to the client. Combine with an in-flight cap when the API limits both.
- Honor `429`/`503` and `Retry-After` (else exponential backoff with jitter), with a named max retry count and a `context.WithTimeout` bound. Retries must also pass through the limiter.
