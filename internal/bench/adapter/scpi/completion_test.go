package scpi_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/scpi/scpitest"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

// statusSim models the IEEE 488.2 status registers of a SCPI-1999 instrument.
type statusSim struct {
	esr, ese, sre int
	opcEvent      bool // *OPC sets ESR bit 0
	failCmd       string
	srq           chan struct{}
	deliverSRQ    bool
}

func (s *statusSim) stb() int {
	v := 0
	if s.esr&s.ese != 0 {
		v |= 32
	}
	if v&s.sre != 0 {
		v |= 64
	}
	return v
}

func (s *statusSim) handle(cmd string) (string, bool) {
	hdr, arg, _ := strings.Cut(cmd, " ")
	n, _ := strconv.Atoi(arg)
	switch hdr {
	case "*CLS":
		s.esr = 0
	case "*ESE":
		s.ese = n
	case "*SRE":
		s.sre = n
	case "*OPC":
		if s.opcEvent {
			s.esr |= 1
			if s.deliverSRQ && s.stb()&64 != 0 {
				select {
				case s.srq <- struct{}{}:
				default:
				}
			}
		}
	case "*OPC?":
		return "1", true
	case "*STB?":
		return strconv.Itoa(s.stb()), true
	case "*ESR?":
		r := s.esr
		s.esr = 0
		return strconv.Itoa(r), true
	case "SYST:ERR?":
		return `0,"No error"`, true
	default:
		if cmd == s.failCmd {
			s.esr |= scpi.ESREXE
		}
	}
	return "", false
}

// srqTransport adds an interrupt channel to the scripted transport.
type srqTransport struct {
	*scpitest.Transport
	sim     *statusSim
	enabled bool
}

func (t *srqTransport) EnableSRQ(context.Context) error { t.enabled = true; return nil }
func (t *srqTransport) DrainSRQ() {
	select {
	case <-t.sim.srq:
	default:
	}
}
func (t *srqTransport) WaitSRQ(ctx context.Context) error {
	select {
	case <-t.sim.srq:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newStatusSession(opcEvent, srqCapable, deliver bool) (*scpi.Session, *statusSim, *scpitest.Transport) {
	sim := &statusSim{opcEvent: opcEvent, srq: make(chan struct{}, 1), deliverSRQ: deliver}
	tr := scpitest.New(sim.handle)
	var s *scpi.Session
	if srqCapable {
		// wrapped in the timeout decorator to prove Unwrap is honoured
		s = scpi.NewSession(scpi.WithIOTimeout(&srqTransport{Transport: tr, sim: sim}, time.Second), scpi.SCPI1999Checker)
	} else {
		s = scpi.NewSession(tr, scpi.SCPI1999Checker)
	}
	s.OPCEvent = opcEvent
	return s, sim, tr
}

func TestRunAndWaitAutoPicksBestMethod(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                   string
		opcEvent, srq, deliver bool
		want                   string
		wantStatus             string
	}{
		{"srq works", true, true, true, scpi.WaitSRQ, "working"},
		{"srq silent (Siglent)", true, true, false, scpi.WaitPoll, "unavailable: instrument accepted"},
		{"no interrupt channel", true, false, false, scpi.WaitPoll, "unavailable: transport"},
		{"no OPC event (SDS)", false, true, true, scpi.WaitOPC, "unavailable: instrument does not set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, tr := newStatusSession(c.opcEvent, c.srq, c.deliver)
			m, err := s.RunAndWait(ctx, "INIT", scpi.WaitAuto)
			if err != nil || m != c.want {
				t.Fatalf("method %q err %v", m, err)
			}
			if !strings.HasPrefix(s.SRQStatus(), c.wantStatus) {
				t.Fatalf("status %q", s.SRQStatus())
			}
			found := false
			for _, l := range tr.Log {
				found = found || l == "INIT"
			}
			if !found {
				t.Fatal("command not sent")
			}
		})
	}
}

func TestRunAndWaitForcedMethods(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newStatusSession(false, false, false)
	if _, err := s.RunAndWait(ctx, "INIT", scpi.WaitPoll); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("poll without OPC event: %v", err)
	}
	if _, err := s.RunAndWait(ctx, "INIT", scpi.WaitSRQ); err == nil {
		t.Fatal("srq without channel must fail")
	}
	if _, err := s.RunAndWait(ctx, "INIT", "carrier-pigeon"); domain.KindOf(err) != domain.KindUsage {
		t.Fatal("unknown method")
	}
	if m, err := s.RunAndWait(ctx, "INIT", scpi.WaitOPC); err != nil || m != scpi.WaitOPC {
		t.Fatal(err)
	}
}

func TestRunAndWaitReportsExecutionError(t *testing.T) {
	for _, m := range []string{scpi.WaitPoll, scpi.WaitSRQ} {
		s, sim, _ := newStatusSession(true, true, true)
		sim.failCmd = "BAD"
		_, err := s.RunAndWait(context.Background(), "BAD", m)
		if err == nil || !strings.Contains(err.Error(), "execution error") {
			t.Fatalf("%s: %v", m, err)
		}
	}
}

func TestSetSRQStatusFromCache(t *testing.T) {
	s, _, _ := newStatusSession(true, true, false)
	s.SetSRQStatus("unavailable: no callback")
	if m, err := s.RunAndWait(context.Background(), "INIT", scpi.WaitAuto); err != nil || m != scpi.WaitPoll {
		t.Fatalf("%s %v", m, err)
	}
	if !strings.Contains(s.SRQStatus(), "cached") {
		t.Fatal(s.SRQStatus())
	}
	s2, _, _ := newStatusSession(true, true, true)
	s2.SetSRQStatus("working")
	if m, _ := s2.RunAndWait(context.Background(), "INIT", scpi.WaitAuto); m != scpi.WaitSRQ {
		t.Fatalf("cached working: %s", m)
	}
}

func TestSTBQuery(t *testing.T) {
	s, sim, _ := newStatusSession(true, false, false)
	sim.esr, sim.ese = 1, 1
	if b, err := s.STB(context.Background()); err != nil || b != 32 {
		t.Fatalf("%d %v", b, err)
	}
}
