package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// progress shows a progress line that updates in place, with
// percentage, speed and remaining time. It can be used by several
// goroutines at the same time.
type progress struct {
	label  string
	unit   string
	total  int64
	start  time.Time
	done   atomic.Int64
	failed atomic.Int64

	mu        sync.Mutex
	lastPrint time.Time
	note      string
}

// SetNote shows a notice at the end of the line (e.g. quota pause). "" removes it.
func (p *progress) SetNote(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.note == s {
		return
	}
	p.note = s
	p.printLocked()
}

// Log prints a message above the progress line, without messing it up.
func (p *progress) Log(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Printf("\r\033[K  ! %s\n", msg)
	p.printLocked()
}

func newProgress(label, unit string, total int) *progress {
	p := &progress{label: label, unit: unit, total: int64(total), start: time.Now()}
	p.print()
	return p
}

// Add records n completed items (ok=false counts them as failures).
func (p *progress) Add(n int, ok bool) {
	d := p.done.Add(int64(n))
	if !ok {
		p.failed.Add(int64(n))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Redraws at most every 500ms (and always on the last item).
	if d < p.total && time.Since(p.lastPrint) < 500*time.Millisecond {
		return
	}
	p.printLocked()
}

// Finish draws the final state and moves to the next line.
func (p *progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.printLocked()
	fmt.Printf(" · took %s\n", fmtDuration(time.Since(p.start)))
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
		fmt.Fprintf(&b, " (%.1f%%)", pct)
	}
	if done > 0 && elapsed > 0 {
		rate := float64(done) / elapsed
		fmt.Fprintf(&b, " · %.1f %s/s", rate, p.unit)
		if remaining := p.total - done; remaining > 0 {
			fmt.Fprintf(&b, " · ~%s left", fmtDuration(time.Duration(float64(remaining)/rate*float64(time.Second))))
		}
	}
	if f := p.failed.Load(); f > 0 {
		fmt.Fprintf(&b, " · %d failed", f)
	}
	if p.note != "" {
		fmt.Fprintf(&b, " · %s", p.note)
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
