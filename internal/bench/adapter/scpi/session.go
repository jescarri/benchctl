// Package scpi is the shared SCPI layer on top of a port.Transport: message
// helpers, IEEE 488.2 blocks and status model, per-dialect error checking,
// completion waiting, traffic tracing and the local per-instrument lock.
package scpi

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// ErrorChecker reads and clears an instrument's error state, returning a
// domain instrument error if anything was reported.
type ErrorChecker func(ctx context.Context, s *Session) error

// Session is error-checked SCPI access over a Transport. It implements port.SCPI.
type Session struct {
	t       port.Transport
	checker ErrorChecker
	// OPCTimeout bounds WaitComplete when the context has no earlier deadline.
	OPCTimeout time.Duration
	// OPCEvent is true when the instrument sets ESR bit 0 on *OPC (full
	// IEEE 488.2 status model), enabling SRQ/poll completion. Set by drivers.
	OPCEvent bool

	srqWorks     int // 0 unknown, 1 verified, -1 unavailable
	srqWhy       string
	srqKnownGood bool
}

var _ port.SCPI = (*Session)(nil)

// NewSession wraps t. checker may be nil (no error checking).
func NewSession(t port.Transport, checker ErrorChecker) *Session {
	return &Session{t: t, checker: checker, OPCTimeout: 30 * time.Second}
}

// Transport returns the underlying transport.
func (s *Session) Transport() port.Transport { return s.t }

// SetChecker replaces the error checker (used once the dialect is known).
func (s *Session) SetChecker(c ErrorChecker) { s.checker = c }

// TransportName implements port.SCPI.
func (s *Session) TransportName() string { return s.t.Name() }

// Write implements port.SCPI.
func (s *Session) Write(ctx context.Context, cmd string) error {
	if err := s.t.Write(ctx, []byte(strings.TrimRight(cmd, "\r\n")+"\n")); err != nil {
		return domain.Instrumentf("write %q: %w", cmd, err)
	}
	return nil
}

// Set implements port.SCPI: write then check errors.
func (s *Session) Set(ctx context.Context, cmd string) error {
	if err := s.Write(ctx, cmd); err != nil {
		return err
	}
	if err := s.CheckErrors(ctx); err != nil {
		return domain.Instrumentf("%q rejected: %w", cmd, err)
	}
	return nil
}

// QueryBytes implements port.SCPI.
func (s *Session) QueryBytes(ctx context.Context, cmd string) ([]byte, error) {
	if err := s.Write(ctx, cmd); err != nil {
		return nil, err
	}
	resp, err := s.t.Read(ctx)
	if err != nil {
		return nil, domain.Instrumentf("read response to %q: %w", cmd, err)
	}
	return resp, nil
}

// Query implements port.SCPI.
func (s *Session) Query(ctx context.Context, cmd string) (string, error) {
	b, err := s.QueryBytes(ctx, cmd)
	if err != nil {
		return "", err
	}
	return TrimResponse(b), nil
}

// QueryFloat queries and parses a number (ignoring unit suffixes). An
// invalid-measurement marker ("****") is returned as an error.
func (s *Session) QueryFloat(ctx context.Context, cmd string) (float64, error) {
	r, err := s.Query(ctx, cmd)
	if err != nil {
		return 0, err
	}
	v, ok, err := domain.ParseInstrumentNumber(lastField(r))
	if err != nil || !ok {
		return 0, domain.Instrumentf("%q: unexpected response %q", cmd, r)
	}
	return v, nil
}

// QueryBlock queries and returns the IEEE 488.2 block payload.
func (s *Session) QueryBlock(ctx context.Context, cmd string) ([]byte, error) {
	b, err := s.QueryBytes(ctx, cmd)
	if err != nil {
		return nil, err
	}
	_, data, err := ParseBlock(b)
	if err != nil {
		return nil, domain.Instrumentf("%q: %w", cmd, err)
	}
	return data, nil
}

// CheckErrors implements port.SCPI.
func (s *Session) CheckErrors(ctx context.Context) error {
	if s.checker == nil {
		return nil
	}
	return s.checker(ctx, s)
}

// WaitComplete implements port.SCPI using *OPC?, bounded by OPCTimeout.
func (s *Session) WaitComplete(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.OPCTimeout)
	defer cancel()
	r, err := s.Query(ctx, "*OPC?")
	if err != nil {
		return err
	}
	if lastField(r) != "1" {
		return domain.Instrumentf("*OPC? returned %q", r)
	}
	return nil
}

// Clear implements port.SCPI.
func (s *Session) Clear(ctx context.Context) error {
	if err := s.t.Clear(ctx); err != nil {
		return domain.Instrumentf("device clear: %w", err)
	}
	return nil
}

// ESR reads and clears the standard event status register.
func (s *Session) ESR(ctx context.Context) (byte, error) {
	v, err := s.QueryFloat(ctx, "*ESR?")
	if err != nil {
		return 0, err
	}
	return byte(int(v)), nil
}

// STB reads the status byte with *STB? (see STBQuery for why not device_readstb).
func (s *Session) STB(ctx context.Context) (byte, error) { return s.STBQuery(ctx) }

// TrimResponse removes trailing terminators and whitespace.
func TrimResponse(b []byte) string {
	return strings.TrimRight(string(b), "\r\n\t ")
}

// lastField returns the text after the last space, for responses that may carry a header.
func lastField(r string) string {
	r = strings.TrimSpace(r)
	if i := strings.LastIndexByte(r, ' '); i >= 0 {
		return r[i+1:]
	}
	return r
}

// ParseBlock extracts an IEEE 488.2 definite-length block from a response,
// which may be preceded by a header (e.g. "C1:WF DAT2,#9000000346...").
// prefix is the text before '#'.
func ParseBlock(msg []byte) (prefix string, data []byte, err error) {
	i := bytes.IndexByte(msg, '#')
	for i >= 0 && i > 0 && !strings.ContainsRune(" ,\t", rune(msg[i-1])) {
		j := bytes.IndexByte(msg[i+1:], '#')
		if j < 0 {
			i = -1
			break
		}
		i += 1 + j
	}
	if i < 0 || i+2 > len(msg) {
		return "", nil, fmt.Errorf("no IEEE 488.2 block in response (%d bytes)", len(msg))
	}
	nd := int(msg[i+1] - '0')
	if nd == 0 {
		// Indefinite-length block: data runs to the final terminator.
		return string(msg[:i]), bytes.TrimRight(msg[i+2:], "\n"), nil
	}
	if nd < 1 || nd > 9 || i+2+nd > len(msg) {
		return "", nil, fmt.Errorf("malformed block header")
	}
	var n int
	for _, c := range msg[i+2 : i+2+nd] {
		if c < '0' || c > '9' {
			return "", nil, fmt.Errorf("malformed block length %q", msg[i+2:i+2+nd])
		}
		n = n*10 + int(c-'0')
	}
	start := i + 2 + nd
	if start+n > len(msg) {
		return "", nil, fmt.Errorf("block truncated: have %d of %d bytes", len(msg)-start, n)
	}
	return string(msg[:i]), msg[start : start+n], nil
}

// FormatBlock encodes data as an IEEE 488.2 definite-length block.
func FormatBlock(data []byte) []byte {
	l := fmt.Sprint(len(data))
	out := make([]byte, 0, 2+len(l)+len(data))
	out = append(out, '#', byte('0'+len(l)))
	out = append(out, l...)
	return append(out, data...)
}
