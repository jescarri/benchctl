// Package scpitest provides an in-memory port.Transport driven by a handler
// function, for driver and session tests.
package scpitest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrNoResponse is returned by Read when the handler produced no response
// (what a real instrument does for an unknown query: nothing, until timeout).
var ErrNoResponse = errors.New("scpitest: no response pending (timeout)")

// Handler returns the response to cmd and whether there is one.
type Handler func(cmd string) (resp string, ok bool)

// Transport is a fake port.Transport.
type Transport struct {
	mu      sync.Mutex
	Handler Handler
	pending []string
	Log     []string
	STB     byte
	Clears  int
	Locked  bool
	Closed  bool
	Kind    string
}

// New returns a Transport using h.
func New(h Handler) *Transport { return &Transport{Handler: h, Kind: "fake"} }

func (t *Transport) Write(_ context.Context, msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cmd := strings.TrimRight(string(msg), "\r\n")
	t.Log = append(t.Log, cmd)
	if t.Handler != nil {
		if r, ok := t.Handler(cmd); ok {
			t.pending = append(t.pending, r)
		}
	}
	return nil
}

func (t *Transport) Read(_ context.Context) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return nil, ErrNoResponse
	}
	r := t.pending[0]
	t.pending = t.pending[1:]
	if !strings.HasSuffix(r, "\n") {
		r += "\n"
	}
	return []byte(r), nil
}

func (t *Transport) Clear(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = nil
	t.Clears++
	return nil
}

func (t *Transport) Lock(context.Context, time.Duration) error { t.Locked = true; return nil }
func (t *Transport) Unlock(context.Context) error              { t.Locked = false; return nil }
func (t *Transport) ReadSTB(context.Context) (byte, error)     { return t.STB, nil }
func (t *Transport) Name() string                              { return t.Kind }
func (t *Transport) Close() error                              { t.Closed = true; return nil }

// Commands returns the logged commands, excluding those matching any of skip prefixes.
func (t *Transport) Commands(skip ...string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
outer:
	for _, c := range t.Log {
		for _, s := range skip {
			if strings.HasPrefix(c, s) {
				continue outer
			}
		}
		out = append(out, c)
	}
	return out
}

// Map is a Handler backed by a static command→response map.
func Map(m map[string]string) Handler {
	return func(cmd string) (string, bool) {
		r, ok := m[cmd]
		return r, ok
	}
}
