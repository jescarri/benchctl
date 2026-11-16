package sds1000xe

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/scpi/scpitest"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent/base"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/wavedesc_sds1104xe.bin")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeWaveDescFixture(t *testing.T) {
	d, err := DecodeWaveDesc(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if d.DescriptorName != "WAVEDESC" || d.TemplateName != "WAVEACE" {
		t.Errorf("names %q %q", d.DescriptorName, d.TemplateName)
	}
	if d.WaveDescLen != WaveDescSize || d.CommType != 0 || d.WaveArrayCount != 700000 {
		t.Errorf("len=%d comm=%d count=%d", d.WaveDescLen, d.CommType, d.WaveArrayCount)
	}
	// Captured live at C1 VDIV=2 V, OFST=-4.72 V, SARA=100 kSa/s.
	if d.VerticalGain != 2 || math.Abs(float64(d.VerticalOffset)+4.72) > 1e-6 || math.Abs(float64(d.HorizInterval)-1e-5) > 1e-12 {
		t.Errorf("gain=%v off=%v hint=%v", d.VerticalGain, d.VerticalOffset, d.HorizInterval)
	}
	if d.ProbeAtt != 1 || d.WaveSource != 0 {
		t.Errorf("probe=%v src=%v", d.ProbeAtt, d.WaveSource)
	}
	if _, err := DecodeWaveDesc(fixture(t)[:100]); err == nil {
		t.Error("short descriptor must fail")
	}
	bad := append([]byte{}, fixture(t)...)
	copy(bad, "NOTADESC")
	if _, err := DecodeWaveDesc(bad); err == nil {
		t.Error("bad name must fail")
	}
}

func TestCodeToVolts(t *testing.T) {
	// Live sample: code 0xC5 (-59) at 2 V/div, offset -4.72 V ≈ 0 V.
	if v := CodeToVolts(int8(-59), 2, -4.72); math.Abs(v) > 1e-9 {
		t.Errorf("got %v", v)
	}
	if v := CodeToVolts(25, 0.2, 0); math.Abs(v-0.2) > 1e-12 {
		t.Errorf("one division = %v", v)
	}
}

func TestParsePAVA(t *testing.T) {
	m, err := ParsePAVA("PKPK", "PKPK,2.40E-01")
	if err != nil || m.Value == nil || *m.Value != 0.24 || m.Unit != "V" || m.Name != "pkpk" {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = ParsePAVA("FREQ", "C2:PAVA FREQ,****")
	if err != nil || m.Value != nil || m.Unit != "Hz" {
		t.Fatalf("**** should be nil: %+v %v", m, err)
	}
	if _, err := ParsePAVA("FREQ", "garbage"); err == nil {
		t.Error("expected error")
	}
}

// sim is a stateful SDS1104X-E stand-in with the firmware quirks seen live.
type sim struct {
	esr                         int
	vdiv, ofst, attn            [5]float64
	cpl                         [5]string
	tra, bwl                    [5]bool
	tdiv, trdl, sara            float64
	trSrc, trMode, wfsu, trSlop string
	trlv                        float64
	desc                        []byte
	points                      int
	asetAfter                   int // TDIV? polls before autoset applies (-1: never changes)
	asetPending                 bool
}

func newSim(desc []byte) *sim {
	s := &sim{tdiv: 5e-4, sara: 1e5, trSrc: "C1", trMode: "AUTO", wfsu: "SP,1,NP,0,FP,0", trSlop: "POS", desc: desc, points: 700000}
	for i := range s.vdiv {
		s.vdiv[i], s.attn[i], s.cpl[i] = 1, 1, "D1M"
	}
	return s
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func blockMsg(prefix string, data []byte) string {
	return prefix + string(scpi.FormatBlock(data))
}

func (s *sim) handle(cmd string) (string, bool) {
	hdr, arg, _ := strings.Cut(cmd, " ")
	ch := 0
	if len(hdr) > 3 && hdr[0] == 'C' && hdr[2] == ':' {
		ch = int(hdr[1] - '0')
		hdr = hdr[3:]
	}
	num := func() float64 { v, _ := strconv.ParseFloat(arg, 64); return v }
	e := func(v float64) string { return strconv.FormatFloat(v, 'E', 2, 64) }
	switch hdr {
	case "CHDR":
		return "", false
	case "*ESR?":
		r := s.esr
		s.esr = 0
		return strconv.Itoa(r), true
	case "CMR?", "EXR?":
		return "0", true
	case "VDIV":
		if s.tra[ch] { // firmware ignores VDIV on a hidden trace
			s.vdiv[ch] = clamp(num(), 5e-4, 10) // silent clamp
		}
	case "VDIV?":
		return e(s.vdiv[ch]), true
	case "OFST":
		s.ofst[ch], s.tra[ch] = num(), true // OFST shows the trace
	case "OFST?":
		return e(s.ofst[ch]), true
	case "ATTN":
		s.attn[ch] = num()
	case "ATTN?":
		return strconv.FormatFloat(s.attn[ch], 'g', -1, 64), true
	case "CPL":
		s.cpl[ch], s.tra[ch] = arg, true // CPL shows the trace
	case "CPL?":
		return s.cpl[ch], true
	case "TRA":
		s.tra[ch] = arg == "ON"
	case "TRA?":
		return map[bool]string{true: "ON", false: "OFF"}[s.tra[ch]], true
	case "BWL?":
		return map[bool]string{true: "ON", false: "OFF"}[s.bwl[ch]], true
	case "BWL":
		c, v, _ := strings.Cut(arg, ",")
		s.bwl[int(c[1]-'0')] = v == "ON"
	case "TDIV":
		s.tdiv = clamp(num(), 1e-9, 100)
	case "TDIV?":
		if s.asetPending {
			if s.asetAfter == 0 {
				s.asetPending = false
				s.tdiv, s.vdiv[1], s.tra[1] = 5e-4, 0.2, true // what autoset picks
			} else if s.asetAfter > 0 {
				s.asetAfter--
			}
		}
		return e(s.tdiv), true
	case "TRDL":
		s.trdl = num()
	case "TRDL?":
		return e(s.trdl), true
	case "SARA?":
		return e(s.sara), true
	case "MSIZ?":
		return "14M", true
	case "TRSE":
		f := strings.Split(arg, ",")
		s.trSrc = f[2]
		if len(f) > 3 {
			s.esr |= 32 // quirk: hold-off arguments apply but flag CME
		}
	case "TRSE?":
		return "EDGE,SR," + s.trSrc + ",HT,OFF", true
	case "TRMD":
		s.trMode = arg
	case "TRMD?":
		return s.trMode, true
	case "TRLV":
		s.trlv = num()
	case "TRLV?":
		return e(s.trlv), true
	case "TRSL":
		s.trSlop = arg
	case "TRSL?":
		return s.trSlop, true
	case "WFSU":
		s.wfsu = arg
		s.esr |= 32 // quirk: always flags CME
	case "WFSU?":
		return s.wfsu, true
	case "PAVA?":
		if arg == "FREQ" {
			return "FREQ,****", true
		}
		return arg + ",1.00E+00", true
	case "WF?":
		if arg == "DESC" {
			return blockMsg("DESC,", s.desc), true
		}
		var sp, np, fp int
		_, _ = fmt.Sscanf(s.wfsu, "SP,%d,NP,%d,FP,%d", &sp, &np, &fp)
		n := s.points
		if sp > 1 {
			n = (n + sp - 1) / sp
		}
		if np > 0 && np < n {
			n = np
		}
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(int8(25 * math.Sin(2*math.Pi*float64(i)/100)))
		}
		return blockMsg("DAT2,", data), true
	case "SCDP":
		bmp := make([]byte, 64)
		copy(bmp, "BM")
		binary.LittleEndian.PutUint32(bmp[2:], 64)
		return string(bmp), true
	case "ASET":
		s.asetPending = true
	default:
		s.esr |= 32 // unknown command: CME, and no response
		return "", false
	}
	return "", false
}

func newScope(t *testing.T, s *sim) (*Scope, *scpitest.Transport) {
	t.Helper()
	tr := scpitest.New(s.handle)
	sc, err := New(context.Background(), base.Instrument{
		ID: domain.Identity{Model: "SDS1104X-E", Serial: "SN"}, S: scpi.NewSession(tr, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	sc.sleep = func(time.Duration) {}
	return sc, tr
}

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

func TestSetupChannel(t *testing.T) {
	s := newSim(fixture(t))
	sc, tr := newScope(t, s)
	if tr.Log[0] != "CHDR OFF" {
		t.Fatalf("first command %q", tr.Log[0])
	}
	if sc.Channels() != 4 || channelsFor("SDS1202X-E") != 2 {
		t.Fatal("channel count")
	}
	ctx := context.Background()
	err := sc.SetupChannel(ctx, 1, domain.ChannelSetup{VDiv: f(0.2), Offset: f(0.1), Probe: f(10), Coupling: "ac", On: b(true), BWLimit: b(true)})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sc.ChannelState(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ChannelState{Channel: 1, On: true, VDiv: 0.2, Offset: 0.1, Coupling: "ac", Probe: 10, BWLimit: true}
	if st != want {
		t.Fatalf("state %+v, want %+v", st, want)
	}
	// Silent clamp is detected by read-back.
	err = sc.SetupChannel(ctx, 1, domain.ChannelSetup{VDiv: f(999)})
	if err == nil || !strings.Contains(err.Error(), "applied 10") {
		t.Fatalf("clamp not detected: %v", err)
	}
	if err := sc.SetupChannel(ctx, 1, domain.ChannelSetup{Coupling: "bogus"}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("bad coupling: %v", err)
	}
	if err := sc.SetupChannel(ctx, 1, domain.ChannelSetup{Probe: f(3)}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("bad probe: %v", err)
	}
	if err := sc.SetupChannel(ctx, 5, domain.ChannelSetup{}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("bad channel: %v", err)
	}
}

// Verified on hardware (fw 8.1.6.1.35R2): VDIV is ignored on a hidden trace
// and OFST/CPL show it. Settings must still apply, and the trace must end in
// the requested state (or stay hidden when On is not given).
func TestSetupChannelHiddenTrace(t *testing.T) {
	s := newSim(fixture(t))
	sc, _ := newScope(t, s)
	ctx := context.Background()
	if err := sc.SetupChannel(ctx, 2, domain.ChannelSetup{VDiv: f(0.2), Offset: f(0.1), Coupling: "dc"}); err != nil {
		t.Fatal(err)
	}
	if s.vdiv[2] != 0.2 || s.ofst[2] != 0.1 || s.tra[2] {
		t.Fatalf("vdiv=%v ofst=%v tra=%v, want 0.2 0.1 hidden", s.vdiv[2], s.ofst[2], s.tra[2])
	}
	if err := sc.SetupChannel(ctx, 3, domain.ChannelSetup{VDiv: f(0.5), On: b(true)}); err != nil {
		t.Fatal(err)
	}
	if s.vdiv[3] != 0.5 || !s.tra[3] {
		t.Fatalf("vdiv=%v tra=%v, want 0.5 shown", s.vdiv[3], s.tra[3])
	}
	if err := sc.SetupChannel(ctx, 3, domain.ChannelSetup{VDiv: f(1), On: b(false)}); err != nil {
		t.Fatal(err)
	}
	if s.vdiv[3] != 1 || s.tra[3] {
		t.Fatalf("vdiv=%v tra=%v, want 1 hidden", s.vdiv[3], s.tra[3])
	}
	// A failed vertical setting must not leave a hidden trace shown.
	if err := sc.SetupChannel(ctx, 4, domain.ChannelSetup{VDiv: f(999)}); err == nil {
		t.Fatal("clamp not detected")
	}
	if s.tra[4] {
		t.Fatal("trace left shown after a failed setup")
	}
}

func TestTimebaseAndTrigger(t *testing.T) {
	s := newSim(fixture(t))
	sc, tr := newScope(t, s)
	ctx := context.Background()
	if err := sc.SetTimebase(ctx, f(200e-6), f(0)); err != nil {
		t.Fatal(err)
	}
	if err := sc.SetTimebase(ctx, f(1000), nil); err == nil {
		t.Fatal("TDIV clamp not detected")
	}
	tb, err := sc.Timebase(ctx)
	if err != nil || tb.MemDepth != "14M" {
		t.Fatalf("%+v %v", tb, err)
	}
	if err := sc.SetTrigger(ctx, domain.TriggerSetup{Source: "2", Mode: "normal", Slope: "fall", Level: f(0.5)}); err != nil {
		t.Fatal(err)
	}
	trig, err := sc.Trigger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if (trig != domain.TriggerState{Type: "edge", Source: "C2", Mode: "normal", Slope: "fall", Level: 0.5}) {
		t.Fatalf("%+v", trig)
	}
	found := false
	for _, c := range tr.Log {
		found = found || c == "TRSE EDGE,SR,C2"
		if strings.HasPrefix(c, "TRSE") && strings.Contains(c, "HT") {
			t.Fatalf("hold-off form flags CME on this firmware: %q", c)
		}
	}
	if !found {
		t.Fatal("short TRSE form not used")
	}
	for _, bad := range []domain.TriggerSetup{{Source: "C9"}, {Mode: "x"}, {Slope: "up"}} {
		if err := sc.SetTrigger(ctx, bad); domain.KindOf(err) != domain.KindUsage {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestMeasure(t *testing.T) {
	sc, _ := newScope(t, newSim(fixture(t)))
	ms, err := sc.Measure(context.Background(), 1, []string{"pkpk", "FREQ"})
	if err != nil {
		t.Fatal(err)
	}
	if *ms[0].Value != 1 || ms[1].Value != nil {
		t.Fatalf("%+v", ms)
	}
	if _, err := sc.Measure(context.Background(), 1, []string{"nope"}); domain.KindOf(err) != domain.KindUsage {
		t.Fatal("unknown param should be a usage error")
	}
}

func TestCaptureDecimatesAndToleratesWFSUQuirk(t *testing.T) {
	s := newSim(fixture(t))
	sc, tr := newScope(t, s)
	w, err := sc.Capture(context.Background(), 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if s.wfsu != "SP,700,NP,1000,FP,0" {
		t.Fatalf("WFSU = %q", s.wfsu)
	}
	if w.Points != 1000 || len(w.Volts) != 1000 {
		t.Fatalf("points %d", w.Points)
	}
	if math.Abs(w.Dt-700*1e-5) > 1e-9 {
		t.Fatalf("dt %v", w.Dt)
	}
	// fixture: vdiv 2 V, offset -4.72 V; code 25 → 2 + 4.72
	if math.Abs(w.T0-(-5e-4*7)) > 1e-12 {
		t.Fatalf("t0 %v", w.T0)
	}
	maxV := -1e9
	for _, v := range w.Volts {
		maxV = math.Max(maxV, v)
	}
	if math.Abs(maxV-(2+4.72)) > 1e-3 {
		t.Fatalf("max volts %v", maxV)
	}
	_ = tr
	// all points
	w, err = sc.Capture(context.Background(), 1, 0)
	if err != nil || w.Points != 700000 {
		t.Fatalf("all points: %d %v", w.Points, err)
	}
}

func TestScreenshotAndAutoset(t *testing.T) {
	sc, _ := newScope(t, newSim(fixture(t)))
	bmp, err := sc.Screenshot(context.Background())
	if err != nil || len(bmp) != 64 || string(bmp[:2]) != "BM" {
		t.Fatalf("%d %v", len(bmp), err)
	}
}

func TestAutosetWaitsForTheScope(t *testing.T) {
	s := newSim(fixture(t))
	s.tdiv, s.asetAfter = 0.5, 15 // slow timebase: applies after many polls
	sc, tr := newScope(t, s)
	changed, err := sc.Autoset(context.Background())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if s.tdiv != 5e-4 || s.vdiv[1] != 0.2 {
		t.Fatal("autoset result not applied")
	}
	// it must keep polling after the change until settings are stable
	polls := 0
	for _, c := range tr.Log {
		if c == "TDIV?" {
			polls++
		}
	}
	if polls < 15+AutosetStablePoll {
		t.Fatalf("returned after %d polls, before settings settled", polls)
	}

	s2 := newSim(fixture(t))
	s2.asetAfter = -1 // nothing to change
	sc2, _ := newScope(t, s2)
	changed, err = sc2.Autoset(context.Background())
	if err != nil || changed {
		t.Fatalf("unchanged: changed=%v err=%v", changed, err)
	}
}

func TestMatches(t *testing.T) {
	for m, want := range map[string]bool{"SDS1104X-E": true, "SDS1202X-E": true, "SDS2104X Plus": false, "SDG1032X": false} {
		if Matches(m) != want {
			t.Errorf("%s", m)
		}
	}
}

// Verified on hardware: with TRDL=+300 µs the descriptor holds -300 µs and the
// trigger edge must land at t=0, so the first sample is at -1700 µs at 200 µs/div.
func TestTimeAxisWithTriggerDelay(t *testing.T) {
	for _, c := range []struct{ trdl, wantT0 float64 }{
		{0, -1400e-6}, {300e-6, -1700e-6}, {-250e-6, -1150e-6},
	} {
		w := BuildWaveform(1, WaveDesc{HorizOffset: -c.trdl, HorizInterval: 2e-9, VerticalGain: 0.2}, nil, []byte{0}, 200e-6, 1)
		if math.Abs(w.T0-c.wantT0) > 1e-12 {
			t.Errorf("TRDL %g: t0 = %g, want %g", c.trdl, w.T0, c.wantT0)
		}
	}
}
