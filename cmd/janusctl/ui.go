package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"
)

// Colours and symbols - only on a terminal, never with NO_COLOR
// (https://no-color.org) or TERM=dumb: scripts read janusctl's output,
// and a context running on several nodes pipes each one's.

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// colorFor reports whether w gets colours.
func colorFor(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || !isTerminal(f) {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	return os.Getenv("TERM") != "dumb"
}

type style struct{ on bool }

func styleFor(w io.Writer) style { return style{colorFor(w)} }

func (s style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) red(t string) string    { return s.wrap("31", t) }
func (s style) green(t string) string  { return s.wrap("32", t) }
func (s style) yellow(t string) string { return s.wrap("33", t) }
func (s style) cyan(t string) string   { return s.wrap("36", t) }

// setupLog makes log.Fatal's messages an error line on a terminal - no
// timestamp, a red ✖ -, and leaves them as they were elsewhere.
func setupLog() {
	if !colorFor(os.Stderr) {
		return
	}
	log.SetFlags(0)
	log.SetOutput(errorWriter{os.Stderr})
}

type errorWriter struct{ w io.Writer }

func (e errorWriter) Write(p []byte) (int, error) {
	st := style{true}
	msg := strings.TrimRight(string(p), "\n")
	if _, err := io.WriteString(e.w, st.red("✖ ")+msg+"\n"); err != nil {
		return 0, err
	}
	return len(p), nil
}

// stateWords are the states janusctl's tables show, by colour.
var (
	goodState = regexp.MustCompile(`\b(running|healthy|ready|up|ok|confirmed|established|enabled|MASTER|active|yes)\b`)
	badState  = regexp.MustCompile(`\b(failed|unhealthy|down|error|stopped|exited|crashed|FAULT|disabled|no)\b`)
	waitState = regexp.MustCompile(`\b(waiting|pending|starting|trial|BACKUP|connecting|idle|backoff)\b`)
)

// tableWriter is a tabwriter whose output gets its header in bold and
// its states coloured on a terminal - after the columns are aligned,
// so the escapes never shift them.
type tableWriter struct {
	*tabwriter.Writer
	buf bytes.Buffer
	out io.Writer
}

func newTable(out io.Writer) *tableWriter {
	t := &tableWriter{out: out}
	t.Writer = tabwriter.NewWriter(&t.buf, 0, 0, 2, ' ', 0)
	return t
}

func (t *tableWriter) Flush() error {
	if err := t.Writer.Flush(); err != nil {
		return err
	}
	text := t.buf.String()
	t.buf.Reset()
	st := styleFor(t.out)
	if st.on {
		head, rest, _ := strings.Cut(text, "\n")
		rest = goodState.ReplaceAllStringFunc(rest, st.green)
		rest = badState.ReplaceAllStringFunc(rest, st.red)
		rest = waitState.ReplaceAllStringFunc(rest, st.yellow)
		text = st.bold(head) + "\n" + rest
	}
	_, err := io.WriteString(t.out, text)
	return err
}
