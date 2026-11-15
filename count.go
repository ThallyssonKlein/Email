package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

// ---------- OpenAI (local, grátis, offline) ----------

func countOpenAI(emails []Email, encoding string, r *report, prog *progress) error {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return err
	}
	var total int64
	for _, e := range emails {
		// EncodeOrdinary trata "<|endoftext|>" etc. como texto comum.
		total += int64(len(enc.EncodeOrdinary(e.Text())))
		prog.Add(1, true)
	}
	r.openai = total
	r.openaiEnc = encoding
	return nil
}

// ---------- Anthropic (API count_tokens, grátis, com rate limit) ----------

// Limites da API count_tokens (limite por organização, varia por tier; o
// menor é 100 requisições/min). Ficamos abaixo disso, e o client é único e
// compartilhado por todos os modelos.
const (
	anthropicRequestsPerSecond = 1.5 // ~90/min
	anthropicBurst             = 4
	maxConcurrentCountRequests = 4 // requisições em voo, somando todos os modelos
	maxConcurrentCounts        = 4 // tarefas de contagem simultâneas (OpenAI + modelos)

	anthropicMaxRetries     = 5 // após 429/5xx, por requisição
	anthropicBackoffBase    = 2 * time.Second
	anthropicRequestTimeout = 10 * time.Minute // teto de tempo (com retries) por requisição
)

// Agrupa vários emails por requisição para fazer poucas chamadas.
// ~300k caracteres fica bem abaixo do contexto de 1M tokens mesmo em textos
// "caros" (código, CJK etc.).
const maxCharsPerRequest = 300_000

// anthropicClient junta o SDK, o rate limiter e o teto de requisições em voo.
// Crie um só (newAnthropicClient) e compartilhe entre goroutines.
type anthropicClient struct {
	api      anthropic.Client
	limiter  *rate.Limiter
	inflight chan struct{}
}

func newAnthropicClient() *anthropicClient {
	return &anthropicClient{
		// Retries ficam por nossa conta: assim cada tentativa passa pelo limiter.
		api:      anthropic.NewClient(option.WithMaxRetries(0)),
		limiter:  rate.NewLimiter(rate.Limit(anthropicRequestsPerSecond), anthropicBurst),
		inflight: make(chan struct{}, maxConcurrentCountRequests),
	}
}

// count faz uma requisição count_tokens, respeitando limiter, 429/5xx e Retry-After.
func (c *anthropicClient) count(ctx context.Context, model string, blocks []string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, anthropicRequestTimeout)
	defer cancel()

	content := make([]anthropic.ContentBlockParamUnion, len(blocks))
	for i, b := range blocks {
		content[i] = anthropic.NewTextBlock(b)
	}
	params := anthropic.MessageCountTokensParams{
		Model:    model,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(content...)},
	}

	backoff := anthropicBackoffBase
	for attempt := 0; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, fmt.Errorf("waiting for rate limit: %w", err)
		}
		res, err := c.do(ctx, params)
		if err == nil {
			return res.InputTokens, nil
		}
		var aerr *anthropic.Error
		retryable := errors.As(err, &aerr) &&
			(aerr.StatusCode == http.StatusTooManyRequests || aerr.StatusCode >= 500)
		if !retryable || attempt >= anthropicMaxRetries {
			return 0, err
		}
		var wait time.Duration
		if aerr.Response != nil {
			wait, _ = retryAfter(aerr.Response.Header)
		}
		if wait == 0 {
			wait = withJitter(backoff)
			backoff *= 2
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return 0, ctx.Err()
		}
	}
}

func (c *anthropicClient) do(ctx context.Context, params anthropic.MessageCountTokensParams) (*anthropic.MessageTokensCount, error) {
	select {
	case c.inflight <- struct{}{}:
		defer func() { <-c.inflight }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.api.Messages.CountTokens(ctx, params)
}

// countTokens soma os tokens de todos os lotes para um modelo.
func (c *anthropicClient) countTokens(ctx context.Context, batches [][]string, model string, prog *progress) (int64, error) {
	// Overhead fixo de uma requisição (tokens de formatação da mensagem),
	// descontado de cada lote para sobrar só o conteúdo dos emails.
	// Credencial faltando ou modelo inválido (4xx) falham rápido, sem retry.
	overhead, err := c.count(ctx, model, []string{"."})
	if err != nil {
		return 0, err
	}
	overhead-- // o "." em si

	var total int64
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentCountRequests)
	for _, b := range batches {
		g.Go(func() error {
			n, err := c.count(gctx, model, b)
			if err != nil {
				return err
			}
			mu.Lock()
			total += n - overhead
			mu.Unlock()
			prog.Add(1, true)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	return total, nil
}

func makeBatches(emails []Email) [][]string {
	var batches [][]string
	var cur []string
	size := 0
	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
	}
	for _, e := range emails {
		t := e.Text()
		// Email gigante: divide em pedaços (não trunca, para não subestimar).
		for len(t) > maxCharsPerRequest {
			flush()
			cut := maxCharsPerRequest
			for cut > 0 && !utf8Start(t[cut]) {
				cut--
			}
			batches = append(batches, []string{t[:cut]})
			t = t[cut:]
		}
		if size+len(t) > maxCharsPerRequest {
			flush()
		}
		cur = append(cur, t)
		size += len(t)
	}
	flush()
	return batches
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ---------- Relatório ----------

type modelCount struct {
	model  string
	tokens int64
}

type report struct {
	emails, htmlOnly, attachments int
	chars                         int64
	openai                        int64
	openaiEnc                     string
	claude                        []modelCount
}

// Preço de input em USD por 1M tokens (API Anthropic, 1ª parte).
var claudeInputPrice = map[string]float64{
	"claude-fable-5-1":  10.00,
	"claude-opus-5-5":   4.00,
	"claude-opus-5":     5.00,
	"claude-sonnet-5-5": 2.00,
	"claude-sonnet-5":   2.00,
	"claude-haiku-5-5":  0.10, // prompts até 100K tokens
	"claude-haiku-4-5":  1.00,
}

func (r *report) print(c config) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 64))
	fmt.Printf("Emails:              %d\n", r.emails)
	fmt.Printf("Caracteres:          %s\n", human(r.chars))
	fmt.Printf("Só HTML (convertido):%d\n", r.htmlOnly)
	fmt.Printf("Anexos (só o nome):  %d\n", r.attachments)
	fmt.Println(strings.Repeat("=", 64))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "Modelo\tTokens de input\tMédia/email\tCusto (USD)\tCusto Batch API (USD)\t")
	if r.openai > 0 {
		cost, batch := "-", "-"
		if c.openaiPrice > 0 {
			v := float64(r.openai) / 1e6 * c.openaiPrice
			cost, batch = fmt.Sprintf("$%.2f", v), fmt.Sprintf("$%.2f", v/2)
		}
		fmt.Fprintf(w, "OpenAI (%s)\t%s\t%d\t%s\t%s\t\n", r.openaiEnc, human(r.openai), r.openai/int64(r.emails), cost, batch)
	}
	for _, m := range r.claude {
		cost, batch := "-", "-"
		if p, ok := claudeInputPrice[m.model]; ok {
			v := float64(m.tokens) / 1e6 * p
			cost, batch = fmt.Sprintf("$%.2f", v), fmt.Sprintf("$%.2f", v/2)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t\n", m.model, human(m.tokens), m.tokens/int64(r.emails), cost, batch)
	}
	w.Flush()
	fmt.Println()
	fmt.Println("Obs: custo considera só a LEITURA (input). Se a IA gerar resposta por email,")
	fmt.Println("some os tokens de output, e o prompt de instrução multiplicado pelo nº de chamadas.")
}

func human(n int64) string {
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(ch)
	}
	return b.String()
}
