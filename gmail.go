package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/time/rate"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Email é o que fica salvo no cache (uma linha JSON por email).
type Email struct {
	ID          string   `json:"id"`
	ThreadID    string   `json:"thread_id"`
	Date        string   `json:"date"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Cc          string   `json:"cc,omitempty"`
	Subject     string   `json:"subject"`
	Body        string   `json:"body"`
	HTMLOnly    bool     `json:"html_only,omitempty"` // corpo veio só em HTML e foi convertido p/ texto
	Attachments []string `json:"attachments,omitempty"`
}

// Text é exatamente o texto que seria enviado para a IA ler.
func (e Email) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\nTo: %s\n", e.From, e.To)
	if e.Cc != "" {
		fmt.Fprintf(&b, "Cc: %s\n", e.Cc)
	}
	fmt.Fprintf(&b, "Date: %s\nSubject: %s\n", e.Date, e.Subject)
	if len(e.Attachments) > 0 {
		fmt.Fprintf(&b, "Attachments: %s\n", strings.Join(e.Attachments, ", "))
	}
	b.WriteString("\n")
	b.WriteString(e.Body)
	return b.String()
}

// ---------- OAuth ----------

func gmailService(ctx context.Context, c config) (*gmail.Service, error) {
	raw, err := os.ReadFile(c.credentials)
	if err != nil {
		return nil, fmt.Errorf("lendo %s (veja o README para criar): %w", c.credentials, err)
	}
	cfg, err := google.ConfigFromJSON(raw, gmail.GmailReadonlyScope)
	if err != nil {
		return nil, err
	}

	tok, err := loadToken(c.tokenFile)
	if err != nil {
		tok, err = loginInBrowser(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if err := saveToken(c.tokenFile, tok); err != nil {
			log.Printf("aviso: não consegui salvar o token: %v", err)
		}
	}
	return gmail.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx, tok)))
}

// loginInBrowser faz o fluxo OAuth de "app desktop" com redirect em loopback.
func loginInBrowser(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://%s", ln.Addr().String())

	stateBytes := make([]byte, 16)
	_, _ = rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state inválido", http.StatusBadRequest)
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			errCh <- fmt.Errorf("autorização negada: %s", e)
			fmt.Fprintln(w, "Autorização negada. Pode fechar esta aba.")
			return
		}
		codeCh <- r.URL.Query().Get("code")
		fmt.Fprintln(w, "Pronto! Pode fechar esta aba e voltar ao terminal.")
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)
	fmt.Printf("Abra este link no navegador para autorizar acesso (somente leitura) ao Gmail:\n\n%s\n\n", url)

	select {
	case code := <-codeCh:
		return cfg.Exchange(ctx, code)
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var t oauth2.Token
	return &t, json.NewDecoder(f).Decode(&t)
}

func saveToken(path string, t *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(t)
}

// ---------- Download ----------

func fetchAll(ctx context.Context, c config) error {
	srv, err := gmailService(ctx, c)
	if err != nil {
		return err
	}

	done := map[string]bool{}
	if cached, err := loadCache(c.cache); err == nil {
		for _, e := range cached {
			done[e.ID] = true
		}
	}

	// 1) Lista todos os IDs (500 por página).
	fmt.Println("Listando mensagens...")
	var ids []string
	call := srv.Users.Messages.List("me").MaxResults(500).IncludeSpamTrash(c.includeSpam)
	if c.query != "" {
		call = call.Q(c.query)
	}
	err = call.Pages(ctx, func(r *gmail.ListMessagesResponse) error {
		for _, m := range r.Messages {
			ids = append(ids, m.Id)
		}
		fmt.Printf("\r  %d IDs encontrados", len(ids))
		if c.max > 0 && len(ids) >= c.max {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return err
	}
	if c.max > 0 && len(ids) > c.max {
		ids = ids[:c.max]
	}
	fmt.Println()

	var todo []string
	for _, id := range ids {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	fmt.Printf("%d no total, %d já no cache, %d para baixar.\n", len(ids), len(ids)-len(todo), len(todo))
	if len(todo) == 0 {
		return nil
	}

	out, err := os.OpenFile(c.cache, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	var mu sync.Mutex
	enc := json.NewEncoder(out)

	// Cota do Gmail: 250 unidades/usuário/segundo; messages.get custa 5 → ~50/s.
	limiter := rate.NewLimiter(rate.Limit(40), 10)
	jobs := make(chan string)
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	prog := newProgress("baixados", "emails", len(todo))

	for w := 0; w < c.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				msg, err := getWithRetry(ctx, srv, limiter, id)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("\nfalha em %s: %v", id, err)
					}
					failed.Add(1)
					prog.Add(1, false)
					continue
				}
				e := parseMessage(msg)
				mu.Lock()
				_ = enc.Encode(e)
				mu.Unlock()
				ok.Add(1)
				prog.Add(1, true)
			}
		}()
	}

loop:
	for _, id := range todo {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	prog.Finish()
	fmt.Printf("Download concluído: %d ok, %d falhas.\n", ok.Load(), failed.Load())
	if ctx.Err() != nil {
		fmt.Println("Interrompido — rode de novo para continuar de onde parou.")
	}
	return nil
}

var errStop = errors.New("stop")

func getWithRetry(ctx context.Context, srv *gmail.Service, lim *rate.Limiter, id string) (*gmail.Message, error) {
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		if err := lim.Wait(ctx); err != nil {
			return nil, err
		}
		msg, err := srv.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		if err == nil {
			return msg, nil
		}
		var gerr *googleapi.Error
		retryable := errors.As(err, &gerr) && (gerr.Code == 429 || gerr.Code == 403 || gerr.Code >= 500)
		if !retryable || attempt >= 6 {
			return nil, err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff *= 2
	}
}

// ---------- Parsing ----------

func parseMessage(m *gmail.Message) Email {
	e := Email{ID: m.Id, ThreadID: m.ThreadId}
	if m.Payload == nil {
		return e
	}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			e.From = h.Value
		case "to":
			e.To = h.Value
		case "cc":
			e.Cc = h.Value
		case "subject":
			e.Subject = h.Value
		case "date":
			e.Date = h.Value
		}
	}

	var plain, htmls []string
	walkParts(m.Payload, &plain, &htmls, &e.Attachments)
	switch {
	case len(plain) > 0:
		e.Body = strings.Join(plain, "\n\n")
	case len(htmls) > 0:
		parts := make([]string, len(htmls))
		for i, h := range htmls {
			parts[i] = htmlToText(h)
		}
		e.Body = strings.Join(parts, "\n\n")
		e.HTMLOnly = true
	default:
		e.Body = m.Snippet
	}
	return e
}

func walkParts(p *gmail.MessagePart, plain, htmls, attachments *[]string) {
	if p.Filename != "" {
		*attachments = append(*attachments, p.Filename)
		return
	}
	if len(p.Parts) > 0 {
		for _, c := range p.Parts {
			walkParts(c, plain, htmls, attachments)
		}
		return
	}
	if p.Body == nil || p.Body.Data == "" {
		return
	}
	data, err := base64.URLEncoding.DecodeString(p.Body.Data)
	if err != nil {
		data, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(p.Body.Data, "="))
		if err != nil {
			return
		}
	}
	switch strings.ToLower(p.MimeType) {
	case "text/plain":
		*plain = append(*plain, string(data))
	case "text/html":
		*htmls = append(*htmls, string(data))
	}
}

// htmlToText extrai só o texto visível (sem tags, scripts e estilos),
// que é o que você mandaria para a IA na prática.
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapseBlankLines(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				skip++
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "table":
				b.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				if skip > 0 {
					skip--
				}
			}
		case html.TextToken:
			if skip == 0 {
				t := strings.Join(strings.Fields(string(z.Text())), " ")
				if t != "" {
					b.WriteString(t)
					b.WriteString(" ")
				}
			}
		}
	}
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ---------- Cache ----------

func loadCache(path string) ([]Email, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Email
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 256*1024*1024)
	for sc.Scan() {
		var e Email
		if json.Unmarshal(sc.Bytes(), &e) == nil && !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
