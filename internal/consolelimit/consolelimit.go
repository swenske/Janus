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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	out io.Writer
	now func() time.Time

	mu      sync.Mutex
	rate    float64
	burst   float64
	tokens  float64
	last    time.Time
	dropped map[string]int    // since the last note
	leftOut map[string]uint64 // since the start
	lines   chan []byte
}

// Config is a node's own budget, kept on STATE (config/console.json):
// what a node whose console costs it - an emulated serial port on an old
// core, HAProxy logging every request - sets lower, down to 0: nothing
// of the services' output on the console at all (janusd's own log
// stays). The lines left out are still kept whole (SystemService.Logs,
// the LOGS partition, the log targets).
type Config struct {
	RateBytesPerSecond int `json:"rate_bytes_per_second"`
	BurstBytes         int `json:"burst_bytes"`
}

// DefaultConfig is Rate and Burst.
func DefaultConfig() Config { return Config{RateBytesPerSecond: Rate, BurstBytes: Burst} }

// MaxRate and MaxBurst bound a configuration: more than this is the
// flood the budget exists against.
const (
	MaxRate  = 1 << 20
	MaxBurst = 16 << 20
)

// Validate checks c: 0 is allowed (the services' output never reaches
// the console); a burst below the rate is raised to it.
func (c *Config) Validate() error {
	if c.RateBytesPerSecond < 0 || c.BurstBytes < 0 {
		return errors.New("the console budget can't be negative")
	}
	if c.RateBytesPerSecond > MaxRate {
		return fmt.Errorf("a console budget above %d bytes a second is the flood it exists against", MaxRate)
	}
	if c.BurstBytes > MaxBurst {
		return fmt.Errorf("a console burst above %d bytes is the flood it exists against", MaxBurst)
	}
	if c.BurstBytes < c.RateBytesPerSecond {
		c.BurstBytes = c.RateBytesPerSecond
	}
	return nil
}

// Dir holds the configuration file; configFile its name.
var Dir = "/etc/.state/config"

const configFile = "console.json"

// Load reads the persisted configuration; isDefault reports that none
// was saved (or that the one saved is unreadable - said in err).
func Load() (cfg Config, isDefault bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), true, nil
	}
	if err != nil {
		return DefaultConfig(), true, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig(), true, fmt.Errorf("%s: %w", configFile, err)
	}
	if err := cfg.Validate(); err != nil {
		return DefaultConfig(), true, fmt.Errorf("%s: %w", configFile, err)
	}
	return cfg, false, nil
}

// Save persists cfg, atomically; Reset removes it (the default again).
func Save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(Dir, "."+configFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Dir, configFile))
}

// Reset removes the saved configuration.
func Reset() error {
	err := os.Remove(filepath.Join(Dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Set changes the budget from now on: the tokens in hand are capped at
// the new burst, so a lower budget takes hold at once.
func (b *Budget) Set(rate, burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rate, b.burst = float64(rate), float64(burst)
	b.tokens = min(b.tokens, b.burst)
}

// Settings returns the budget in force.
func (b *Budget) Settings() (rate, burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.rate), int(b.burst)
}

// LeftOut returns, by service, the lines the console didn't get since
// the start.
func (b *Budget) LeftOut() map[string]uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]uint64, len(b.leftOut))
	for k, v := range b.leftOut {
		out[k] = v
	}
	return out
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
		dropped: map[string]int{}, leftOut: map[string]uint64{}, lines: make(chan []byte, queued),
	}
}

// For returns the writer a service's output goes through to the
// console. Its Write never waits for the console.
func (b *Budget) For(service string) io.Writer {
	return &writer{b: b, service: service}
}

// offer queues line for the console when the budget has room for it and
// the queue isn't full - it then keeps line - and counts it as left out
// otherwise, returning false: the caller may reuse line.
func (b *Budget) offer(service string, line []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if n := float64(len(line)); n <= b.tokens {
		select {
		case b.lines <- line:
			b.tokens -= n
			return true
		default:
		}
	}
	b.dropped[service]++
	b.leftOut[service]++
	return false
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
		if w.b.offer(w.service, line) {
			w.partial = nil // the queue holds it
		} else {
			w.partial = w.partial[:0] // left out: nothing allocated for the next
		}
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
