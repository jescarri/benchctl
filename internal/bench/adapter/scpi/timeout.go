package scpi

import (
	"context"
	"time"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// IOTimeout wraps a Transport so every operation without a caller deadline
// gets a default one (the --timeout flag). A caller deadline, e.g. from
// `raw --timeout 120s` or *OPC? waiting, always takes precedence.
type IOTimeout struct {
	port.Transport
	D time.Duration
}

// WithIOTimeout returns t with a default per-operation timeout d.
func WithIOTimeout(t port.Transport, d time.Duration) port.Transport {
	if d <= 0 {
		return t
	}
	return &IOTimeout{Transport: t, D: d}
}

// Unwrap returns the wrapped transport.
func (t *IOTimeout) Unwrap() port.Transport { return t.Transport }

func (t *IOTimeout) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, t.D)
}

func (t *IOTimeout) Write(ctx context.Context, msg []byte) error {
	ctx, cancel := t.ctx(ctx)
	defer cancel()
	return t.Transport.Write(ctx, msg)
}

func (t *IOTimeout) Read(ctx context.Context) ([]byte, error) {
	ctx, cancel := t.ctx(ctx)
	defer cancel()
	return t.Transport.Read(ctx)
}

func (t *IOTimeout) ReadSTB(ctx context.Context) (byte, error) {
	ctx, cancel := t.ctx(ctx)
	defer cancel()
	return t.Transport.ReadSTB(ctx)
}

func (t *IOTimeout) Clear(ctx context.Context) error {
	ctx, cancel := t.ctx(ctx)
	defer cancel()
	return t.Transport.Clear(ctx)
}

func (t *IOTimeout) Unlock(ctx context.Context) error {
	ctx, cancel := t.ctx(ctx)
	defer cancel()
	return t.Transport.Unlock(ctx)
}
