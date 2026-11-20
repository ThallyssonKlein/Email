// email-tokens: downloads all emails from a Gmail account and estimates how
// many tokens an AI (Anthropic Claude / OpenAI GPT) would consume to read them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/joho/godotenv"
	"golang.org/x/sync/errgroup"
)

type config struct {
	envFile      string
	credentials  string
	tokenFile    string
	cache        string
	query        string
	max          int
	includeSpam  bool
	skipFetch    bool
	workers      int
	rate         float64
	models       string
	noAnthropic  bool
	openaiPrice  float64
	openaiEncode string
}

func main() {
	var c config
	flag.StringVar(&c.envFile, "env", ".env", ".env file with ANTHROPIC_API_KEY (optional)")
	flag.StringVar(&c.credentials, "credentials", "credentials.json", "OAuth client file (Desktop app) downloaded from the Google Cloud Console")
	flag.StringVar(&c.tokenFile, "token", "token.json", "where to save/read the user's OAuth token")
	flag.StringVar(&c.cache, "cache", "emails.jsonl", "local cache of downloaded emails (allows resuming)")
	flag.StringVar(&c.query, "q", "", `Gmail search filter, e.g. "after:2024/01/01 -category:promotions"`)
	flag.IntVar(&c.max, "max", 0, "limit of emails to download (0 = all)")
	flag.BoolVar(&c.includeSpam, "spam", false, "include Spam and Trash")
	flag.BoolVar(&c.skipFetch, "skip-fetch", false, "download nothing, only count what is already in the cache")
	flag.IntVar(&c.workers, "workers", gmailDefaultWorkers, "simultaneous Gmail downloads")
	flag.Float64Var(&c.rate, "rate", gmailDefaultRate, "maximum emails/s downloaded (the default Gmail quota allows up to 20)")
	flag.StringVar(&c.models, "models", "claude-opus-5-5,claude-sonnet-5-5,claude-haiku-5-5", "Claude models used to count tokens (comma-separated)")
	flag.BoolVar(&c.noAnthropic, "no-anthropic", false, "skip counting through the Anthropic API")
	flag.Float64Var(&c.openaiPrice, "openai-price", 0, "OpenAI input price in USD per 1M tokens, only used by the report to compute the estimate (no charge is made)")
	flag.StringVar(&c.openaiEncode, "openai-encoding", "o200k_base", "tiktoken encoding (o200k_base = GPT-4o/4.1/5; cl100k_base = GPT-4/3.5)")
	flag.Parse()

	// Load the .env if it exists. Variables already exported in the terminal take priority.
	if err := godotenv.Load(c.envFile); err == nil {
		fmt.Printf("Variables loaded from %s\n", c.envFile)
	} else if !os.IsNotExist(err) {
		log.Fatalf("error reading %s: %v", c.envFile, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if !c.skipFetch {
		if err := fetchAll(ctx, c); err != nil {
			log.Fatalf("error downloading emails: %v", err)
		}
	}

	emails, err := loadCache(c.cache)
	if err != nil {
		log.Fatalf("error reading cache: %v", err)
	}
	if len(emails) == 0 {
		log.Fatal("no emails in the cache")
	}
	fmt.Printf("\n%d emails in the cache. Counting tokens...\n", len(emails))

	report := &report{emails: len(emails)}
	for _, e := range emails {
		report.chars += int64(len([]rune(e.Text())))
		if e.HTMLOnly {
			report.htmlOnly++
		}
		report.attachments += len(e.Attachments)
	}

	if !c.noAnthropic && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		fmt.Println("  (ANTHROPIC_API_KEY not set; trying the `ant auth login` profile)")
	}

	var models []string
	if !c.noAnthropic {
		for _, m := range strings.Split(c.models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
	}
	batches := makeBatches(emails)

	// The OpenAI count (local) and the count for each Claude model are independent:
	// they run together, with a single progress bar. The Anthropic client
	// (and therefore the rate limiter) is shared across models.
	prog := newProgress("counting tokens:", "steps", len(emails)+len(models)*len(batches))
	client := newAnthropicClient()
	claude := make([]modelCount, len(models)) // one slot per goroutine
	claudeOK := make([]bool, len(models))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentCounts)
	g.Go(func() error {
		if err := countOpenAI(emails, c.openaiEncode, report, prog); err != nil {
			prog.Log(fmt.Sprintf("warning: OpenAI count failed: %v", err))
		}
		return nil
	})
	for i, m := range models {
		g.Go(func() error {
			n, err := client.countTokens(gctx, batches, m, prog)
			if err != nil {
				prog.Log(fmt.Sprintf("warning: Anthropic count (%s) failed: %v", m, err))
				return nil // one model failing does not cancel the others
			}
			claude[i], claudeOK[i] = modelCount{model: m, tokens: n}, true
			return nil
		})
	}
	_ = g.Wait()
	prog.Finish()
	for i := range claude {
		if claudeOK[i] {
			report.claude = append(report.claude, claude[i])
		}
	}

	report.print(c)
}
