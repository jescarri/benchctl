package scpi

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// Wait methods for RunAndWait.
const (
	WaitAuto = "auto" // srq if it is proven to work, else poll if supported, else opc
	WaitSRQ  = "srq"  // *ESE 1, *SRE 32, cmd, *OPC, wait for the VXI-11 service request
	WaitPoll = "poll" // *ESE 1, cmd, *OPC, poll *STB? until ESB is set
	WaitOPC  = "opc"  // cmd, *OPC? (blocks until the instrument answers)
)

// Status byte bits.
const (
	STBESB = 1 << 5 // event status bit (ESR & ESE != 0)
	STBMSS = 1 << 6 // master summary / RQS
)

// SRQTransport is implemented by transports with an interrupt channel (VXI-11).
type SRQTransport interface {
	EnableSRQ(ctx context.Context) error
	WaitSRQ(ctx context.Context) error
	DrainSRQ()
}

// Unwrapper is implemented by transport decorators (tracer, timeouts).
type Unwrapper interface {
	Unwrap() port.Transport
}

func findSRQ(t port.Transport) (SRQTransport, bool) {
	for t != nil {
		if s, ok := t.(SRQTransport); ok {
			return s, true
		}
		u, ok := t.(Unwrapper)
		if !ok {
			return nil, false
		}
		t = u.Unwrap()
	}
	return nil, false
}

// srqProbeTimeout bounds the one-time check that an instrument actually
// delivers service requests after accepting device_enable_srq.
var srqProbeTimeout = 400 * time.Millisecond

// pollInterval is the *STB? polling period.
var pollInterval = 20 * time.Millisecond

// STBQuery reads the status byte with *STB?. VXI-11 device_readstb is not
// trusted: Siglent firmware always answers 0 there.
func (s *Session) STBQuery(ctx context.Context) (byte, error) {
	r, err := s.Query(ctx, "*STB?")
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(lastField(r))
	if err != nil || v < 0 || v > 255 {
		return 0, domain.Instrumentf("*STB?: unexpected response %q", r)
	}
	return byte(v), nil
}

// SRQStatus reports what is known about SRQ on this session:
// "unknown", "working", or "unavailable: <reason>".
func (s *Session) SRQStatus() string {
	switch {
	case s.srqWorks > 0:
		return "working"
	case s.srqWorks < 0:
		return "unavailable: " + s.srqWhy
	}
	return "unknown"
}

// SetSRQStatus seeds the probe result from a previous session ("working" or
// "unavailable: ..."). "working" still enables the channel on first use.
func (s *Session) SetSRQStatus(status string) {
	switch {
	case status == "working":
		s.srqKnownGood = true
	case strings.HasPrefix(status, "unavailable"):
		s.srqWorks, s.srqWhy = -1, strings.TrimPrefix(strings.TrimPrefix(status, "unavailable"), ": ")+" (cached in inventory)"
	}
}

// ProbeSRQ enables the interrupt channel and checks, once per session, that
// an *OPC event actually arrives as a service request. Siglent firmware
// accepts create_intr_chan/device_enable_srq but never connects back, so
// acceptance alone proves nothing.
func (s *Session) ProbeSRQ(ctx context.Context) bool {
	if s.srqWorks != 0 {
		return s.srqWorks > 0
	}
	fail := func(why string) bool {
		s.srqWorks, s.srqWhy = -1, why
		return false
	}
	srq, ok := findSRQ(s.t)
	if !ok {
		return fail("transport has no interrupt channel (use vxi11)")
	}
	if !s.OPCEvent {
		return fail("instrument does not set ESR OPC on *OPC")
	}
	if err := srq.EnableSRQ(ctx); err != nil {
		return fail(err.Error())
	}
	if s.srqKnownGood {
		s.srqWorks = 1
		return true
	}
	srq.DrainSRQ()
	for _, c := range []string{"*CLS", "*ESE 1", "*SRE 32", "*OPC"} {
		if err := s.Write(ctx, c); err != nil {
			return fail(err.Error())
		}
	}
	pctx, cancel := context.WithTimeout(ctx, srqProbeTimeout)
	err := srq.WaitSRQ(pctx)
	cancel()
	_ = s.Write(ctx, "*SRE 0")
	_, _ = s.ESR(ctx)
	if err != nil {
		return fail("instrument accepted the interrupt channel but sent no service request")
	}
	s.srqWorks = 1
	return true
}

// RunAndWait sends cmd and waits until the instrument reports the operation
// complete, using the requested method (see the Wait* constants). It returns
// the method actually used. Instrument errors raised by cmd are reported.
func (s *Session) RunAndWait(ctx context.Context, cmd, method string) (string, error) {
	if method == "" {
		method = WaitAuto
	}
	switch method {
	case WaitAuto:
		switch {
		case s.ProbeSRQ(ctx):
			method = WaitSRQ
		case s.OPCEvent:
			method = WaitPoll
		default:
			method = WaitOPC
		}
	case WaitSRQ:
		if !s.ProbeSRQ(ctx) {
			return method, domain.Instrumentf("SRQ %s", s.SRQStatus())
		}
	case WaitPoll:
		if !s.OPCEvent {
			return method, domain.Usagef("this instrument does not set ESR OPC on *OPC; use --wait opc")
		}
	case WaitOPC:
	default:
		return method, domain.Usagef("unknown wait method %q (auto, srq, poll, opc)", method)
	}

	if method == WaitOPC {
		if err := s.Write(ctx, cmd); err != nil {
			return method, err
		}
		if err := s.WaitComplete(ctx); err != nil {
			return method, err
		}
		return method, s.CheckErrors(ctx)
	}

	srq, _ := findSRQ(s.t)
	setup := []string{"*CLS", "*ESE 1"}
	if method == WaitSRQ {
		setup = append(setup, "*SRE 32")
		srq.DrainSRQ()
	}
	for _, c := range append(setup, cmd, "*OPC") {
		if err := s.Write(ctx, c); err != nil {
			return method, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, s.OPCTimeout)
	defer cancel()
	var waitErr error
	if method == WaitSRQ {
		waitErr = srq.WaitSRQ(ctx)
		_ = s.Write(ctx, "*SRE 0")
	} else {
		waitErr = s.pollESB(ctx)
	}
	if waitErr != nil {
		return method, domain.Instrumentf("waiting for %q to complete (%s): %w", cmd, method, waitErr)
	}
	esr, err := s.ESR(ctx)
	if err != nil {
		return method, err
	}
	if esr&esrErrorMask != 0 {
		detail := s.CheckErrors(ctx)
		return method, domain.Instrumentf("%q failed: ESR %s (%v)", cmd, DescribeESR(esr), detail)
	}
	return method, s.CheckErrors(ctx)
}

func (s *Session) pollESB(ctx context.Context) error {
	for {
		stb, err := s.STBQuery(ctx)
		if err != nil {
			return err
		}
		if stb&STBESB != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
