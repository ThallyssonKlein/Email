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
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Gmail API limits (default quota: 6,000 units/minute per user; each
// messages.get costs 5 → at most 20 emails/s). The defaults stay below that.
const (
	gmailDefaultRate    = 15 // emails/s (-rate flag)
	gmailBurst          = 1
	gmailDefaultWorkers = 10               // simultaneous downloads (-workers flag)
	gmailQuotaPause     = 65 * time.Second // the quota window is 1 minute

	maxQuotaRetries       = 5                // attempts after quota exhaustion, per email
	maxServerErrorRetries = 5                // attempts after 5xx, per email
	retryBackoffBase      = 2 * time.Second  // initial backoff for 5xx
	perMessageTimeout     = 10 * time.Minute // time cap (with retries) per email
	oauthShutdownTimeout  = 5 * time.Second
)

// Email is what gets saved in the cache (one JSON line per email).
type Email struct {
	ID          string   `json:"id"`
	ThreadID    string   `json:"thread_id"`
	Date        string   `json:"date"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Cc          string   `json:"cc,omitempty"`
	Subject     string   `json:"subject"`
	Body        string   `json:"body"`
	HTMLOnly    bool     `json:"html_only,omitempty"` // body came only as HTML and was converted to text
	Attachments []string `json:"attachments,omitempty"`
}

// Text is exactly the text that would be sent to the AI to read.
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
		return nil, fmt.Errorf("reading %s (see the README to create it): %w", c.credentials, err)
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
			log.Printf("warning: could not save the token: %v", err)
		}
	}
	return gmail.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx, tok)))
}

// loginInBrowser runs the OAuth "desktop app" flow with a loopback redirect.
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
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			errCh <- fmt.Errorf("authorization denied: %s", e)
			fmt.Fprintln(w, "Authorization denied. You can close this tab.")
			return
		}
		codeCh <- r.URL.Query().Get("code")
		fmt.Fprintln(w, "Done! You can close this tab and go back to the terminal.")
	})}
	var serveGroup errgroup.Group
	serveGroup.Go(func() error {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			select {
			case errCh <- err:
			default:
			}
			return err
		}
		return nil
	})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), oauthShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if err := serveGroup.Wait(); err != nil {
			log.Printf("warning: login server exited with error: %v", err)
		}
	}()

	url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)
	fmt.Printf("Open this link in your browser to authorize (read-only) access to Gmail:\n\n%s\n\n", url)

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

	// 1) List all IDs (500 per page).
	fmt.Println("Listing messages...")
	var ids []string
	call := srv.Users.Messages.List("me").MaxResults(500).IncludeSpamTrash(c.includeSpam)
	if c.query != "" {
		call = call.Q(c.query)
	}
	err = call.Pages(ctx, func(r *gmail.ListMessagesResponse) error {
		for _, m := range r.Messages {
			ids = append(ids, m.Id)
		}
		fmt.Printf("\r  %d IDs found", len(ids))
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
	fmt.Printf("%d in total, %d already cached, %d to download.\n", len(ids), len(ids)-len(todo), len(todo))
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

	prog := newProgress("downloaded", "emails", len(todo))
	gate := newQuotaGate(c.rate, prog)
	var ok, failed atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.workers)
	for _, id := range todo {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			msg, err := getWithRetry(gctx, srv, gate, id)
			if err != nil {
				if gctx.Err() == nil {
					prog.Log(fmt.Sprintf("failed on %s: %s (will be retried on the next run)", id, shortErr(err)))
				}
				failed.Add(1)
				prog.Add(1, false)
				return nil // one email failing does not cancel the others
			}
			e := parseMessage(msg)
			mu.Lock()
			encErr := enc.Encode(e)
			mu.Unlock()
			if encErr != nil {
				return fmt.Errorf("writing cache: %w", encErr)
			}
			ok.Add(1)
			prog.Add(1, true)
			return nil
		})
	}
	waitErr := g.Wait()
	prog.Finish()
	fmt.Printf("Download finished: %d ok, %d failed.\n", ok.Load(), failed.Load())
	if ctx.Err() != nil {
		fmt.Println("Interrupted — run again to continue where it stopped.")
	}
	return waitErr
}

var errStop = errors.New("stop")

// quotaGate controls the pace of ALL downloads together. When Gmail says the
// quota is exhausted, it pauses everyone until the 1-minute window renews,
// instead of each download insisting (which keeps the quota exhausted).
// It must be created only once (newQuotaGate) and shared across workers.
type quotaGate struct {
	limiter  *rate.Limiter
	prog     *progress
	pauseFor time.Duration // how long to wait when the quota is exhausted and there is no Retry-After

	mu    sync.Mutex
	until time.Time
}

func newQuotaGate(perSecond float64, prog *progress) *quotaGate {
	return &quotaGate{
		limiter:  rate.NewLimiter(rate.Limit(perSecond), gmailBurst),
		prog:     prog,
		pauseFor: gmailQuotaPause,
	}
}

func (g *quotaGate) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		d := time.Until(g.until)
		g.mu.Unlock()
		if d <= 0 {
			break
		}
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
	g.prog.SetNote("")
	return g.limiter.Wait(ctx)
}

func (g *quotaGate) Pause(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := time.Now().Add(d); t.After(g.until) {
		g.until = t
		g.prog.SetNote(fmt.Sprintf("Gmail quota reached, paused until %s", t.Format("15:04:05")))
	}
}

// getWithRetry downloads an email honoring the gate. Every attempt (retries
// included) goes through the limiter; the total attempts and time are bounded.
func getWithRetry(ctx context.Context, srv *gmail.Service, gate *quotaGate, id string) (*gmail.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, perMessageTimeout)
	defer cancel()

	backoff := retryBackoffBase
	quotaRetries, serverRetries := 0, 0
	for {
		if err := gate.Wait(ctx); err != nil {
			return nil, err
		}
		msg, err := srv.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		if err == nil {
			return msg, nil
		}
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) {
			return nil, err
		}
		switch {
		case isRateLimit(gerr) && quotaRetries < maxQuotaRetries:
			// Quota limit: wait for Retry-After (or the window to renew), all together.
			quotaRetries++
			wait, ok := retryAfter(gerr.Header)
			if !ok {
				wait = gate.pauseFor
			}
			gate.Pause(wait)
		case gerr.Code >= 500 && serverRetries < maxServerErrorRetries:
			serverRetries++
			wait, ok := retryAfter(gerr.Header)
			if !ok {
				wait = withJitter(backoff)
				backoff *= 2
			}
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
		default:
			return nil, err
		}
	}
}

func isRateLimit(e *googleapi.Error) bool {
	if e.Code == 429 {
		return true
	}
	if e.Code == 403 {
		for _, item := range e.Errors {
			switch item.Reason {
			case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded":
				return true
			}
		}
		return strings.Contains(e.Message, "Quota exceeded")
	}
	return false
}

// shortErr summarizes the error in one line (without Google's JSON details).
func shortErr(err error) string {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return fmt.Sprintf("HTTP %d: %s", gerr.Code, gerr.Message)
	}
	return err.Error()
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

// htmlToText extracts only the visible text (no tags, scripts or styles),
// which is what you would send to the AI in practice.
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
