package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// progress mostra uma linha de progresso que se atualiza no lugar, com
// porcentagem, velocidade e tempo restante. Pode ser usado por várias
// goroutines ao mesmo tempo.
type progress struct {
	label  string
	unit   string
	total  int64
	start  time.Time
	done   atomic.Int64
	failed atomic.Int64

	mu        sync.Mutex
	lastPrint time.Time
}

func newProgress(label, unit string, total int) *progress {
	p := &progress{label: label, unit: unit, total: int64(total), start: time.Now()}
	p.print()
	return p
}

// Add registra n itens concluídos (ok=false conta como falha).
func (p *progress) Add(n int, ok bool) {
	d := p.done.Add(int64(n))
	if !ok {
		p.failed.Add(int64(n))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Redesenha no máximo a cada 500ms (e sempre no último item).
	if d < p.total && time.Since(p.lastPrint) < 500*time.Millisecond {
		return
	}
	p.printLocked()
}

// Finish desenha o estado final e pula a linha.
func (p *progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.printLocked()
	fmt.Printf(" · levou %s\n", fmtDuration(time.Since(p.start)))
}

func (p *progress) print() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.printLocked()
}

func (p *progress) printLocked() {
	p.lastPrint = time.Now()
	done := p.done.Load()
	elapsed := time.Since(p.start).Seconds()

	var b strings.Builder
	fmt.Fprintf(&b, "\r\033[K  %s %s/%s", p.label, human(done), human(p.total))
	if p.total > 0 {
		pct := float64(done) / float64(p.total) * 100
		fmt.Fprintf(&b, " (%s%%)", strings.Replace(fmt.Sprintf("%.1f", pct), ".", ",", 1))
	}
	if done > 0 && elapsed > 0 {
		rate := float64(done) / elapsed
		fmt.Fprintf(&b, " · %s %s/s", strings.Replace(fmt.Sprintf("%.1f", rate), ".", ",", 1), p.unit)
		if remaining := p.total - done; remaining > 0 {
			fmt.Fprintf(&b, " · faltam ~%s", fmtDuration(time.Duration(float64(remaining)/rate*float64(time.Second))))
		}
	}
	if f := p.failed.Load(); f > 0 {
		fmt.Fprintf(&b, " · %d falhas", f)
	}
	fmt.Print(b.String())
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
