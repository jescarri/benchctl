// Package base holds what every Siglent driver shares: identity, SCPI session,
// close handling and set-then-read-back verification.
package base

import (
	"context"
	"strconv"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// Instrument implements port.Instrument.
type Instrument struct {
	ID     domain.Identity
	R      domain.Role
	S      *scpi.Session
	Closer func() error
}

func (b *Instrument) Identity() domain.Identity { return b.ID }
func (b *Instrument) Role() domain.Role         { return b.R }
func (b *Instrument) SCPI() port.SCPI           { return b.S }

// Close releases the transport and lock.
func (b *Instrument) Close() error {
	if b.Closer == nil {
		return nil
	}
	err := b.Closer()
	b.Closer = nil
	return err
}

// F formats a float for SCPI arguments.
func F(v float64) string { return strconv.FormatFloat(v, 'E', 6, 64) }

// SetVerified sends cmd (error-checked), reads back with query, and fails if
// the applied value differs from want by more than max(rel*|want|, abs).
// Siglent instruments silently clamp out-of-range values, so this is the
// only way to notice that a set did not take effect as requested.
func (b *Instrument) SetVerified(ctx context.Context, what, cmd, query string, want, rel, abs float64) error {
	if err := b.S.Set(ctx, cmd); err != nil {
		return err
	}
	got, err := b.S.QueryFloat(ctx, query)
	if err != nil {
		return err
	}
	if !domain.Close(want, got, rel, abs) {
		return domain.Instrumentf("%s: requested %g but instrument applied %g (out of range or not a valid step)", what, want, got)
	}
	return nil
}

// DrainErrors clears stale instrument errors left by other clients.
func (b *Instrument) DrainErrors(ctx context.Context) {
	_ = b.S.CheckErrors(ctx)
}

// CheckChannel validates a 1-based channel number.
func CheckChannel(ch, n int) error {
	if ch < 1 || ch > n {
		return domain.Usagef("channel %d out of range 1..%d", ch, n)
	}
	return nil
}

// Errf is a short helper for driver parse errors.
func Errf(format string, args ...any) error { return domain.Instrumentf(format, args...) }

// MustNumber parses an instrument number or returns a descriptive error.
func MustNumber(what, s string) (float64, error) {
	v, ok, err := domain.ParseInstrumentNumber(s)
	if err != nil || !ok {
		return 0, domain.Instrumentf("%s: cannot parse %q", what, s)
	}
	return v, nil
}

// ScreenDump reads a Siglent SCDP screen dump (a BMP, without IEEE 488.2
// block framing) and trims anything after the size in its header. SCDP has
// no '?' but returns data, so it is sent as a query.
func (b *Instrument) ScreenDump(ctx context.Context) ([]byte, error) {
	data, err := b.S.QueryBytes(ctx, "SCDP")
	if err != nil {
		return nil, err
	}
	if len(data) < 6 || string(data[:2]) != "BM" {
		return nil, Errf("SCDP: response is not a BMP (%d bytes)", len(data))
	}
	size := int(uint32(data[2]) | uint32(data[3])<<8 | uint32(data[4])<<16 | uint32(data[5])<<24)
	if size > len(data) {
		return nil, Errf("SCDP: BMP truncated (%d of %d bytes)", len(data), size)
	}
	return data[:size], nil
}
