package scpi

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/jescarri/benchctl/internal/bench/port"
)

// Tracer wraps a Transport and logs every exchange to w (the --verbose flag).
type Tracer struct {
	port.Transport
	Label string
	mu    sync.Mutex
	w     io.Writer
}

// Unwrap returns the wrapped transport.
func (t *Tracer) Unwrap() port.Transport { return t.Transport }

// NewTracer returns a tracing transport.
func NewTracer(t port.Transport, label string, w io.Writer) *Tracer {
	return &Tracer{Transport: t, Label: label, w: w}
}

func (t *Tracer) logf(dir string, format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintf(t.w, "%s %s[%s] %s %s\n", time.Now().Format("15:04:05.000"), t.Label, t.Name(), dir, fmt.Sprintf(format, args...))
}

// Summarize renders a message for logs: printable text verbatim, binary as a size summary.
func Summarize(b []byte) string {
	const max = 200
	printable := true
	for _, c := range b {
		if (c < 0x20 && c != '\n' && c != '\r' && c != '\t') || c > 0x7e {
			printable = false
			break
		}
	}
	if printable {
		s := strconv.Quote(string(b))
		if len(s) > max {
			s = s[:max] + "…"
		}
		return s
	}
	head := b
	if len(head) > 16 {
		head = head[:16]
	}
	return fmt.Sprintf("<%d bytes binary, starts %q>", len(b), head)
}

func (t *Tracer) Write(ctx context.Context, msg []byte) error {
	t.logf(">", "%s", Summarize(msg))
	err := t.Transport.Write(ctx, msg)
	if err != nil {
		t.logf("!", "write error: %v", err)
	}
	return err
}

func (t *Tracer) Read(ctx context.Context) ([]byte, error) {
	start := time.Now()
	b, err := t.Transport.Read(ctx)
	if err != nil {
		t.logf("!", "read error after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return b, err
	}
	t.logf("<", "%s (%s)", Summarize(b), time.Since(start).Round(time.Millisecond))
	return b, nil
}

func (t *Tracer) ReadSTB(ctx context.Context) (byte, error) {
	b, err := t.Transport.ReadSTB(ctx)
	t.logf("<", "STB=%d err=%v", b, err)
	return b, err
}

func (t *Tracer) Clear(ctx context.Context) error {
	err := t.Transport.Clear(ctx)
	t.logf("*", "device clear err=%v", err)
	return err
}
