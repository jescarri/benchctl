//go:build bench

// Package integration runs against real hardware. It needs the inventory
// (BENCHCTL_CONFIG or ~/.config/benchctl/bench.json) and the wiring
// fg:CH1 → scope:C1, fg:CH2 → scope:C2, external 5 V → dmm.
//
//	make integration
package integration

import (
	"context"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/bmp"
	"github.com/jescarri/benchctl/internal/bench/adapter/config"
	"github.com/jescarri/benchctl/internal/bench/adapter/connect"
	"github.com/jescarri/benchctl/internal/bench/adapter/discovery"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent"
	"github.com/jescarri/benchctl/internal/bench/app"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func newApp(t *testing.T, transport string) *app.App {
	t.Helper()
	store := config.New(os.Getenv("BENCHCTL_CONFIG"))
	b, err := store.Load()
	if err != nil {
		t.Fatalf("inventory %s: %v", store.Path(), err) // a broken inventory must not look like a pass
	}
	if len(b.Instruments) == 0 {
		t.Skip("no inventory; run `benchctl discover --save`")
	}
	var trace io.Writer
	if os.Getenv("BENCHCTL_TRACE") != "" {
		trace = os.Stderr
	}
	scanner := &discovery.Scanner{}
	return &app.App{
		Store: store, Scanner: scanner, RoleOf: siglent.RoleFor, Warn: os.Stderr,
		Connector: &connect.Connector{Store: store, Discoverer: scanner, Transport: transport,
			Log: os.Stderr, Trace: trace, LockWait: 5 * time.Second, IOTimeout: 5 * time.Second},
	}
}

// mustSafe switches every output off and fails the test unless each one is
// confirmed off (an unreachable generator does not count).
func mustSafe(t *testing.T, a *app.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := a.Safe(ctx)
	if err != nil {
		t.Errorf("safe: outputs not confirmed off: %v (%+v)", err, r)
	}
}

func f(v float64) *float64 { return &v }
func bp(v bool) *bool      { return &v }

func within(t *testing.T, name string, got *float64, want, rel float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: invalid (****)", name)
		return
	}
	if math.Abs(*got-want) > want*rel {
		t.Errorf("%s = %g, want %g ± %g%%", name, *got, want, rel*100)
	}
}

// TestFGToScope is the ADR's first integration test: FG CH1 1 kHz 1 Vpp sine
// → scope C1. Note: PKPK includes noise peaks and reads ~6% high on the
// SDS1104X-E at 200 mV/div, so amplitude is asserted with AMPL and RMS.
func TestFGToScope(t *testing.T) {
	for _, tr := range []string{"vxi11", "raw"} {
		t.Run(tr, func(t *testing.T) {
			if tr == "raw" && os.Getenv("BENCH_SCOPE_RAW") == "" {
				// The SDS1000X-E never closes its side of a raw-socket session;
				// each one leaks until the scope's socket server stops answering
				// (power cycle to recover). Raw transport is covered on the
				// SDG/SDM by TestRawTransport.
				t.Skip("set BENCH_SCOPE_RAW=1 to exercise the scope over raw sockets (leaks scope sessions)")
			}
			a := newApp(t, tr)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			defer mustSafe(t, a)

			w, _ := app.WaveSpecFromStrings("sine", "1k", "1Vpp", "0", "0", "")
			if _, err := a.FGSet(ctx, app.FGSetRequest{Channel: 1, Wave: w, Load: "hiz"}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ScopeSetup(ctx, "", 1, domain.ChannelSetup{VDiv: f(0.2), Offset: f(0), Coupling: "dc", Probe: f(1), On: bp(true)}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ScopeTimebase(ctx, "", f(2e-3), f(0)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ScopeTrigger(ctx, "", domain.TriggerSetup{Source: "C1", Mode: "auto", Level: f(0), Slope: "rise"}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.FGOutput(ctx, "", 1, true); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)

			m, err := a.ScopeMeasure(ctx, "", 1, []string{"ampl", "rms", "pkpk", "freq"})
			if err != nil {
				t.Fatal(err)
			}
			v := func(n string) *float64 { x, _ := m.Value(n); return x }
			within(t, "AMPL", v("ampl"), 1.0, 0.03)
			within(t, "RMS", v("rms"), 1/(2*math.Sqrt2), 0.03)
			within(t, "PKPK", v("pkpk"), 1.0, 0.08)
			within(t, "FREQ", v("freq"), 1000, 0.001)

			wf, err := a.ScopeCapture(ctx, "", 1, 20000)
			if err != nil {
				t.Fatal(err)
			}
			s := app.Summarize(wf)
			within(t, "FFT peak", &s.PeakFreqHz, 1000, 0.005)
			// Volts conversion (code*vdiv/25 - offset) agrees with the scope's own PKPK.
			span := s.Max - s.Min
			within(t, "capture span vs PAVA PKPK", &span, *v("pkpk"), 0.03)
			if math.Abs(s.Duration-14*2e-3) > 1e-4 {
				t.Errorf("capture duration %g, want 14 div × 2 ms", s.Duration)
			}
		})
	}
}

func TestDMMReadsExternal5V(t *testing.T) {
	a := newApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.DMMConfigure(ctx, "", domain.DMMConfig{Function: "vdc", Range: "auto"}); err != nil {
		t.Fatal(err)
	}
	r, err := a.DMMRead(ctx, "", 5, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "VDC mean", &r.Mean, 5.0, 0.05)
}

func TestClampIsDetected(t *testing.T) {
	a := newApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, err := a.ScopeSetup(ctx, "", 3, domain.ChannelSetup{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = a.ScopeSetup(context.Background(), "", 3, domain.ChannelSetup{VDiv: &before.VDiv}) }()
	_, err = a.ScopeSetup(ctx, "", 3, domain.ChannelSetup{VDiv: f(999)})
	if err == nil {
		t.Fatal("VDIV 999 should be reported as clamped")
	}
}

func TestWiringVerify(t *testing.T) {
	a := newApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	rep, err := a.WiringVerify(ctx, nil)
	for _, l := range rep.Links {
		t.Logf("%s → %s ok=%v observed=%s %s", l.From, l.To, l.OK, l.Observed, l.Problem)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestTriggerDelayTimeAxis: with any trigger delay, the captured time axis
// must put the trigger point (rising zero crossing of a 1 kHz sine, trigger
// level 0) at t = 0.
func TestTriggerDelayTimeAxis(t *testing.T) {
	a := newApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	defer mustSafe(t, a)
	defer func() { _, _ = a.ScopeTimebase(context.Background(), "", nil, f(0)) }()

	w, _ := app.WaveSpecFromStrings("sine", "1k", "1Vpp", "0", "0", "")
	if _, err := a.FGSet(ctx, app.FGSetRequest{Channel: 1, Wave: w, Load: "hiz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ScopeSetup(ctx, "", 1, domain.ChannelSetup{VDiv: f(0.2), Offset: f(0), On: bp(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ScopeTrigger(ctx, "", domain.TriggerSetup{Source: "C1", Mode: "auto", Level: f(0), Slope: "rise"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FGOutput(ctx, "", 1, true); err != nil {
		t.Fatal(err)
	}
	for _, d := range []float64{0, 300e-6, -250e-6} {
		if _, err := a.ScopeTimebase(ctx, "", f(200e-6), f(d)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		wf, err := a.ScopeCapture(ctx, "", 1, 200000)
		if err != nil {
			t.Fatal(err)
		}
		edge, ok := nearestRisingZero(wf.Volts, wf.T0, wf.Dt)
		if !ok {
			t.Fatalf("delay %g: no rising edge found", d)
		}
		t.Logf("delay %+.0f µs: t0=%.1f µs, trigger edge at %.2f µs", d*1e6, wf.T0*1e6, edge*1e6)
		if math.Abs(edge) > 5e-6 {
			t.Errorf("delay %g: trigger edge at %g s, want 0", d, edge)
		}
	}
}

// nearestRisingZero finds the rising zero crossing closest to t=0 using
// ±100 mV hysteresis so noise at falling crossings is ignored.
func nearestRisingZero(v []float64, t0, dt float64) (float64, bool) {
	best, found := math.Inf(1), false
	low := -1
	for i, x := range v {
		switch {
		case x < -0.1:
			low = i
		case x > 0.1 && low >= 0:
			j := low
			for j < i && v[j] < 0 {
				j++
			}
			tc := t0 + float64(j)*dt
			if math.Abs(tc) < math.Abs(best) {
				best, found = tc, true
			}
			low = -1
		}
	}
	return best, found
}

func TestPingAll(t *testing.T) {
	a := newApp(t, "")
	r, err := a.Ping(context.Background(), nil, 3)
	for _, p := range r {
		t.Logf("%s: %s via %s, connect %s, *IDN? %s, srq %q", p.Alias, p.IP, p.Transport, p.Connect, p.RoundTrip, p.SRQ)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestScreenshots(t *testing.T) {
	a := newApp(t, "")
	want := map[string][2]int{"scope": {800, 480}, "fg": {480, 272}, "dmm": {480, 272}}
	for alias, wh := range want {
		img, err := a.Screenshot(context.Background(), alias)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		info, err := bmp.ParseInfo(img)
		if err != nil || info.Width != wh[0] || info.Height != wh[1] {
			t.Fatalf("%s: %+v %v", alias, info, err)
		}
		if _, err := bmp.ToPNG(img); err != nil {
			t.Fatalf("%s: png: %v", alias, err)
		}
	}
}

// TestAutoset starts from a bad setup (500 ms/div, 2 V/div) with a 1 kHz
// 1 Vpp sine on C1 and expects autoset to settle near 500 µs/div, 200 mV/div.
func TestAutoset(t *testing.T) {
	a := newApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer mustSafe(t, a)
	w, _ := app.WaveSpecFromStrings("sine", "1k", "1Vpp", "0", "0", "")
	if _, err := a.FGSet(ctx, app.FGSetRequest{Channel: 1, Wave: w, Load: "hiz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FGOutput(ctx, "", 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ScopeSetup(ctx, "", 1, domain.ChannelSetup{VDiv: f(2), On: bp(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ScopeTimebase(ctx, "", f(0.5), f(0)); err != nil {
		t.Fatal(err)
	}
	r, err := a.ScopeAutoset(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	after := r.After.Channels[0]
	t.Logf("changed=%v elapsed=%s tdiv=%g C1 vdiv=%g", r.Changed, r.Elapsed, r.After.Timebase.TDiv, after.VDiv)
	if !r.Changed || r.After.Timebase.TDiv > 2e-3 || after.VDiv > 0.5 {
		t.Fatalf("autoset did not settle on the 1 kHz signal: %+v", r.After.Timebase)
	}
	m, err := a.ScopeMeasure(ctx, "", 1, []string{"freq", "ampl"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := m.Value("freq")
	within(t, "FREQ after autoset", v, 1000, 0.001)
}

// TestCompletionMethods checks the wait strategies against what each
// instrument supports (SRQ is not delivered by current Siglent firmware).
func TestCompletionMethods(t *testing.T) {
	a := newApp(t, "vxi11")
	for alias, want := range map[string]string{"fg": "poll", "dmm": "poll", "scope": "opc"} {
		r, err := a.Raw(context.Background(), alias, "*CLS", app.RawOptions{Wait: "auto"})
		if err != nil || r.Wait != want {
			t.Errorf("%s: wait=%q err=%v, want %q", alias, r.Wait, err, want)
		}
	}
}

// TestRawTransport exercises the raw socket on the instruments whose socket
// server closes sessions properly (SDG, SDM).
func TestRawTransport(t *testing.T) {
	a := newApp(t, "raw")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := a.Ping(ctx, []string{"fg", "dmm"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r {
		if p.Transport != "raw" {
			t.Fatalf("%s used %s", p.Alias, p.Transport)
		}
	}
	for _, alias := range []string{"fg", "dmm"} {
		img, err := a.Screenshot(ctx, alias)
		if err != nil {
			t.Fatalf("%s screenshot over raw: %v", alias, err)
		}
		if info, err := bmp.ParseInfo(img); err != nil || info.Width != 480 {
			t.Fatalf("%s: %+v %v", alias, info, err)
		}
	}
	read, err := a.DMMRead(ctx, "", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "VDC over raw", &read.Mean, 5.0, 0.05)
}
