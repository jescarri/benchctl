// Package connect opens instruments by alias: transport selection
// (VXI-11 / raw), serial-number pinning with rediscovery, locking and driver
// selection.
package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent"
	"github.com/jescarri/benchctl/internal/bench/adapter/transport/raw"
	"github.com/jescarri/benchctl/internal/bench/adapter/transport/vxi11"
	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// Transport names.
const (
	TransportAuto  = "auto"
	TransportVXI11 = "vxi11"
	TransportRaw   = "raw"
)

// RawHint is appended to VXI-11 connection failures. The raw socket is never
// used unless asked for: the SDS1000X-E never closes its side of a raw
// session, so each connection leaks one until its socket server stops
// answering (power cycle to recover).
const RawHint = "VXI-11 failed; if this instrument has no VXI-11 (or TCP 111 is blocked), retry with --transport raw or set \"transport\": \"raw\" for it in the inventory. Raw is a last resort: the SDS1000X-E leaks one raw session per connection until power-cycled"

func rawHintFor(transport string) string {
	if transport == TransportRaw {
		return ""
	}
	return "; " + RawHint
}

// Connector implements port.Connector.
type Connector struct {
	Store      port.ConfigStore
	Discoverer port.Discoverer
	// Transport overrides the per-instrument transport when non-empty (--transport).
	Transport string
	// IP overrides the address for every alias opened (--ip).
	IP string
	// Trace receives SCPI traffic when non-nil (--verbose).
	Trace io.Writer
	// Log receives diagnostics such as rediscovery notices.
	Log io.Writer
	// LockWait is how long to wait for the local file lock.
	LockWait time.Duration
	// LockDir overrides the lock directory (tests).
	LockDir string
	// VXI11Port overrides the portmapper port (tests).
	VXI11Port int
	// RawPort overrides the raw socket port (tests).
	RawPort int
	// DialTimeout bounds each connection attempt.
	DialTimeout time.Duration
	// IOTimeout is the default per-operation SCPI timeout (--timeout).
	IOTimeout time.Duration
}

var _ port.Connector = (*Connector)(nil)

func (c *Connector) logf(format string, args ...any) {
	if c.Log != nil {
		_, _ = fmt.Fprintf(c.Log, format+"\n", args...)
	}
}

// Open implements port.Connector.
func (c *Connector) Open(ctx context.Context, alias string) (port.Instrument, error) {
	bench, err := c.Store.Load()
	if err != nil {
		return nil, err
	}
	ref, ok := bench.Instruments[alias]
	if !ok {
		return nil, domain.Usagef("unknown instrument alias %q (known: %v); run `benchctl discover --save`", alias, bench.Aliases())
	}
	tr := ref.Transport
	if c.Transport != "" {
		tr = c.Transport
	}
	if c.IP != "" {
		return withSRQ(c.OpenAddr(ctx, c.IP, tr, ref.Serial))(ref)
	}
	inst, err := c.OpenAddr(ctx, ref.LastIP, tr, ref.Serial)
	if err == nil {
		return withSRQ(inst, nil)(ref)
	}
	if domain.KindOf(err) != domain.KindUnreachable || c.Discoverer == nil || bench.Subnet == "" {
		return nil, err
	}
	c.logf("benchctl: %s (%s) not at %s (%v); rescanning %s", alias, ref.Serial, ref.LastIP, err, bench.Subnet)
	found, serr := c.Discoverer.Scan(ctx, bench.Subnet)
	if serr != nil {
		return nil, domain.Unreachablef("%s: %v; rediscovery failed: %v", alias, err, serr)
	}
	for _, f := range found {
		if f.Identity.Serial != ref.Serial {
			continue
		}
		ref.LastIP = f.IP
		bench.Instruments[alias] = ref
		if serr := c.Store.Save(bench); serr != nil {
			c.logf("benchctl: could not save new IP for %s: %v", alias, serr)
		} else {
			c.logf("benchctl: %s moved to %s; inventory updated", alias, f.IP)
		}
		return withSRQ(c.OpenAddr(ctx, f.IP, tr, ref.Serial))(ref)
	}
	return nil, domain.Unreachablef("%s (serial %s) not found on %s%s", alias, ref.Serial, bench.Subnet, rawHintFor(tr))
}

// OpenAddr connects to ip with the given transport. If serial is non-empty
// the instrument's serial must match (otherwise an unreachable error is
// returned so the caller can rediscover).
func (c *Connector) OpenAddr(ctx context.Context, ip, transport, serial string) (port.Instrument, error) {
	if ip == "" {
		return nil, domain.Unreachablef("no IP address known")
	}
	var lock *scpi.FileLock
	if serial != "" {
		dir := c.LockDir
		if dir == "" {
			dir = scpi.LockDir()
		}
		var err error
		if lock, err = scpi.AcquireFileLock(ctx, dir, serial, c.LockWait); err != nil {
			return nil, err
		}
	}
	t, err := c.dialTransport(ctx, ip, transport)
	if err != nil {
		_ = lock.Release()
		return nil, domain.Unreachablef("%s: %v%s", ip, err, rawHintFor(transport))
	}
	if c.Trace != nil {
		t = scpi.NewTracer(t, ip, c.Trace)
	}
	t = scpi.WithIOTimeout(t, c.IOTimeout)
	closer := func() error {
		uctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = t.Unlock(uctx)
		err := t.Close()
		_ = lock.Release()
		return err
	}
	sess := scpi.NewSession(t, nil)
	idn, err := sess.Query(ctx, "*IDN?")
	if err != nil {
		_ = closer()
		return nil, domain.Unreachablef("%s: *IDN?: %v", ip, err)
	}
	id, err := domain.ParseIDN(idn)
	if err != nil {
		_ = closer()
		return nil, domain.Unreachablef("%s: %v", ip, err)
	}
	if serial != "" && id.Serial != serial {
		_ = closer()
		return nil, domain.Unreachablef("%s answers as %s %s, expected serial %s", ip, id.Model, id.Serial, serial)
	}
	// Instrument-side lock (VXI-11). Siglent firmware accepts it but does not
	// enforce it between links; the file lock is the effective guard.
	if err := t.Lock(ctx, c.LockWait); err != nil {
		if vxi11.IsCode(err, vxi11.ErrCodeLocked) {
			_ = closer()
			return nil, domain.Instrumentf("%s is locked by another VXI-11 client", ip)
		}
	}
	inst, err := siglent.Open(ctx, sess, id, closer)
	if err != nil {
		_ = closer()
		return nil, err
	}
	return inst, nil
}

func (c *Connector) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 3 * time.Second
}

func (c *Connector) rawAddr(ip string) string {
	p := c.RawPort
	if p == 0 {
		p = raw.DefaultPort
	}
	return net.JoinHostPort(ip, fmt.Sprint(p))
}

func (c *Connector) dialTransport(ctx context.Context, ip, transport string) (port.Transport, error) {
	dctx, cancel := context.WithTimeout(ctx, c.dialTimeout())
	defer cancel()
	switch transport {
	case TransportRaw:
		return raw.Dial(dctx, c.rawAddr(ip))
	case "", TransportAuto, TransportVXI11:
		// auto never falls back to the raw socket: raw is a last resort the
		// user must ask for (see RawHint).
		return vxi11.Dial(dctx, ip, vxi11.Options{PortmapperPort: c.VXI11Port})
	}
	return nil, domain.Usagef("unknown transport %q (auto, vxi11, raw)", transport)
}

// withSRQ seeds the session with the SRQ probe result remembered in the
// inventory, so `--wait auto` does not re-probe on every invocation.
func withSRQ(inst port.Instrument, err error) func(domain.InstrumentRef) (port.Instrument, error) {
	return func(ref domain.InstrumentRef) (port.Instrument, error) {
		if err != nil {
			return nil, err
		}
		if s, ok := inst.SCPI().(*scpi.Session); ok && ref.SRQ != "" {
			s.SetSRQStatus(ref.SRQ)
		}
		return inst, nil
	}
}
