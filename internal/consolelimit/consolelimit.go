// Package consolelimit keeps what the services on a node print from
// flooding its console.
//
// janusd keeps every line a service prints (SystemService.Logs). The
// console is something else: a serial line init relays synchronously
// (rootfs/init/console.go), which a hypervisor emulates at some tens of
// KB/s - and pays for in CPU. HAProxy's access logs (`log stdout`) under
// load kept init's console pipe full: janusd's own log calls waited
// behind them (an API call that logs took up to 2 s instead of 15 ms)
// and the emulated serial port took most of a vCPU from HAProxy (1 800
// requests/s instead of 98 000 on a 2-vCPU guest). So the services share
// a small budget on the console, and a line that doesn't fit is left
// out - counted, and said so every few seconds - never waited for.
// janusd's own log isn't limited: it's little, and it's the boot's
// record.
package consolelimit

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

const (
	// Rate and Burst are the services' shared share of the console,
	// in bytes: a few lines a second, and room for what they all print
	// as they start.
	Rate  = 2 << 10
	Burst = 64 << 10

	// maxLine is as much of one line as the console gets.
	maxLine = 1 << 10
	// queued is how many lines may wait for the console at once.
	queued = 256
	// noteEvery is how often lines left out are said so.
	noteEvery = 10 * time.Second
)

// Budget is the services' share of one console writer.
type Budget struct {
	out   io.Writer
	rate  float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	tokens  float64
	last    time.Time
	dropped map[string]int
	lines   chan []byte
}

// New starts a budget of rate bytes a second, burst at most, in front
// of out (the console).
func New(out io.Writer, rate, burst int) *Budget {
	b := newBudget(out, rate, burst, time.Now)
	go b.run(time.NewTicker(noteEvery).C)
	return b
}

func newBudget(out io.Writer, rate, burst int, now func() time.Time) *Budget {
	return &Budget{
		out: out, rate: float64(rate), burst: float64(burst), now: now,
		tokens: float64(burst), last: now(),
		dropped: map[string]int{}, lines: make(chan []byte, queued),
	}
}

// For returns the writer a service's output goes through to the
// console. Its Write never waits for the console.
func (b *Budget) For(service string) io.Writer {
	return &writer{b: b, service: service}
}

// offer queues line for the console when the budget has room for it and
// the queue isn't full, and counts it as left out otherwise.
func (b *Budget) offer(service string, line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if n := float64(len(line)); n <= b.tokens {
		select {
		case b.lines <- line:
			b.tokens -= n
			return
		default:
		}
	}
	b.dropped[service]++
}

// notes returns one line per service whose lines were left out since
// the last call.
func (b *Budget) notes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for service, n := range b.dropped {
		lines := "lines"
		if n == 1 {
			lines = "line"
		}
		out = append(out, fmt.Sprintf("janusd: %d %s of %s's output not shown on the console - janusctl system logs %s\n", n, lines, service, service))
	}
	clear(b.dropped)
	sort.Strings(out)
	return out
}

func (b *Budget) run(tick <-chan time.Time) {
	for {
		select {
		case line := <-b.lines:
			_, _ = b.out.Write(line)
		case <-tick:
			for _, n := range b.notes() {
				_, _ = io.WriteString(b.out, n)
			}
		}
	}
}

// writer cuts a service's output into lines for its budget.
type writer struct {
	b       *Budget
	service string

	mu      sync.Mutex
	partial []byte
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.add(p)
			break
		}
		w.add(p[:i])
		line := append(w.partial, '\n')
		w.partial = nil
		w.b.offer(w.service, line)
		p = p[i+1:]
	}
	return n, nil
}

// add keeps up to maxLine bytes of the line being read.
func (w *writer) add(p []byte) {
	if room := maxLine - len(w.partial); room > 0 {
		w.partial = append(w.partial, p[:min(len(p), room)]...)
	}
}
