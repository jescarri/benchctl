package sdg1000x

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/scpi/scpitest"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent/base"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func TestParseBSWV(t *testing.T) {
	st, err := ParseBSWV("C1:BSWV WVTP,SINE,FRQ,1000HZ,PERI,0.001S,AMP,4V,AMPVRMS,1.414Vrms,OFST,0V,HLEV,2V,LLEV,-2V,PHSE,0")
	if err != nil {
		t.Fatal(err)
	}
	if st.Wave != "sine" || st.Freq != 1000 || st.AmpVpp != 4 || st.AmpRMS != 1.414 || st.HighV != 2 || st.LowV != -2 || st.Duty != nil {
		t.Fatalf("%+v", st)
	}
	st, _ = ParseBSWV("C2:BSWV WVTP,SQUARE,FRQ,3300HZ,AMP,1V,OFST,0V,DUTY,50")
	if st.Duty == nil || *st.Duty != 50 {
		t.Fatal("duty")
	}
	if _, err := ParseBSWV("C1:BSWV WVTP"); err == nil {
		t.Fatal("odd field count must fail")
	}
	// An invalid or missing amplitude must not read as 0 V.
	for _, bad := range []string{
		"C1:BSWV WVTP,SINE,FRQ,1000HZ,AMP,****,OFST,0V",
		"C1:BSWV WVTP,SINE,FRQ,1000HZ,AMP,4volts,OFST,0V",
		"C1:BSWV WVTP,SINE,FRQ,1000HZ,OFST,0V",
	} {
		if _, err := ParseBSWV(bad); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	if st, err := ParseBSWV("C1:BSWV WVTP,DC,OFST,1.5V"); err != nil || st.Offset != 1.5 {
		t.Fatalf("dc: %+v %v", st, err)
	}
}

func TestParseOUTP(t *testing.T) {
	var st domain.GenChannelState
	if err := ParseOUTP("C1:OUTP OFF,LOAD,HZ,PLRT,NOR", &st); err != nil || st.Output || st.Load != "hiz" || st.Polarity != "nor" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := ParseOUTP("C1:OUTP ON,LOAD,50,PLRT,INVT", &st); err != nil || !st.Output || st.Load != "50" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := ParseOUTP("C1:OUTP MAYBE", &st); err == nil {
		t.Fatal("expected error")
	}
}

// sim is an SDG1032X stand-in: silent clamping, error queue, header echo.
type sim struct {
	wave       [3]string
	frq, amp   [3]float64
	ofst, duty [3]float64
	phse       [3]float64
	out        [3]bool
	load       [3]string
	errs       []string
}

func newSim() *sim {
	s := &sim{}
	for i := range s.wave {
		s.wave[i], s.frq[i], s.amp[i], s.load[i], s.duty[i] = "SINE", 1000, 4, "HZ", 50
	}
	return s
}

func (s *sim) handle(cmd string) (string, bool) {
	if cmd == "SYST:ERR?" {
		if len(s.errs) == 0 {
			return "+0, No error", true
		}
		e := s.errs[0]
		s.errs = s.errs[1:]
		return e, true
	}
	if len(cmd) < 4 || cmd[0] != 'C' || cmd[2] != ':' {
		s.errs = append(s.errs, "-113,Undefined header,"+cmd)
		return "", false
	}
	ch := int(cmd[1] - '0')
	hdr, arg, _ := strings.Cut(cmd[3:], " ")
	switch hdr {
	case "BSWV?":
		r := fmt.Sprintf("C%d:BSWV WVTP,%s,FRQ,%gHZ,AMP,%gV,AMPVRMS,%gVrms,OFST,%gV,PHSE,%g", ch, s.wave[ch], s.frq[ch], s.amp[ch], s.amp[ch]/2/math.Sqrt2, s.ofst[ch], s.phse[ch])
		if s.wave[ch] == "SQUARE" {
			r += fmt.Sprintf(",DUTY,%g", s.duty[ch])
		}
		return r, true
	case "OUTP?":
		return fmt.Sprintf("C%d:OUTP %s,LOAD,%s,PLRT,NOR", ch, map[bool]string{true: "ON", false: "OFF"}[s.out[ch]], s.load[ch]), true
	case "BSWV":
		f := strings.Split(arg, ",")
		for i := 0; i+1 < len(f); i += 2 {
			v, _ := strconv.ParseFloat(f[i+1], 64)
			switch f[i] {
			case "WVTP":
				s.wave[ch] = f[i+1]
			case "FRQ":
				s.frq[ch] = math.Min(v, 30e6) // silent clamp
			case "AMP":
				s.amp[ch] = math.Min(v, 20)
			case "OFST":
				s.ofst[ch] = v
			case "DUTY":
				s.duty[ch] = v
			case "PHSE":
				s.phse[ch] = math.Mod(v, 360) // the SDG wraps: 400 → 40
			case "AMPVRMS":
				s.amp[ch] = v * 2 * math.Sqrt2
			default:
				s.errs = append(s.errs, "-108,Parameter not allowed")
			}
		}
	case "OUTP":
		for _, p := range strings.Split(arg, ",") {
			switch p {
			case "ON":
				s.out[ch] = true
			case "OFF":
				s.out[ch] = false
			case "LOAD":
			default:
				s.load[ch] = p
			}
		}
	default:
		s.errs = append(s.errs, "-113,Undefined header,"+cmd)
	}
	return "", false
}

func newGen(t *testing.T, s *sim) *Generator {
	g, err := New(context.Background(), base.Instrument{ID: domain.Identity{Model: "SDG1032X"}, S: scpi.NewSession(scpitest.New(s.handle), nil)})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func f(v float64) *float64 { return &v }

func TestSetWaveAndReadBack(t *testing.T) {
	s := newSim()
	s.errs = []string{"-113,Undefined header,stale"} // drained at connect
	g := newGen(t, s)
	ctx := context.Background()
	if err := g.SetWave(ctx, 2, domain.WaveSpec{Type: "square", Freq: f(3300), AmpVpp: f(1), Offset: f(0), Duty: f(25)}); err != nil {
		t.Fatal(err)
	}
	st, err := g.State(ctx, 2)
	if err != nil || st.Wave != "square" || st.Freq != 3300 || st.AmpVpp != 1 || *st.Duty != 25 {
		t.Fatalf("%+v %v", st, err)
	}
	err = g.SetWave(ctx, 1, domain.WaveSpec{Freq: f(1e12)})
	if err == nil || !strings.Contains(err.Error(), "applied 3e+07") {
		t.Fatalf("clamp not detected: %v", err)
	}
	if err := g.SetWave(ctx, 1, domain.WaveSpec{AmpRMS: f(1)}); err != nil {
		t.Fatalf("Vrms: %v", err)
	}
	if err := g.SetWave(ctx, 1, domain.WaveSpec{Phase: f(90)}); err != nil {
		t.Fatalf("phase 90: %v", err)
	}
	// 400° would be applied as 40°: refuse it rather than report success.
	for _, ph := range []float64{400, -10} {
		if err := g.SetWave(ctx, 1, domain.WaveSpec{Phase: f(ph)}); domain.KindOf(err) != domain.KindUsage {
			t.Fatalf("phase %g: %v", ph, err)
		}
	}
	if err := g.SetWave(ctx, 1, domain.WaveSpec{Type: "triangle"}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("unknown wave: %v", err)
	}
	if err := g.SetWave(ctx, 3, domain.WaveSpec{}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("bad channel: %v", err)
	}
}

func TestLoadAndOutput(t *testing.T) {
	s := newSim()
	g := newGen(t, s)
	ctx := context.Background()
	if err := g.SetLoad(ctx, 1, "50"); err != nil {
		t.Fatal(err)
	}
	if s.load[1] != "50" {
		t.Fatalf("load %q", s.load[1])
	}
	if err := g.SetLoad(ctx, 1, "hiz"); err != nil || s.load[1] != "HZ" {
		t.Fatalf("hiz: %v %q", err, s.load[1])
	}
	if err := g.SetLoad(ctx, 1, "1"); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("1 ohm: %v", err)
	}
	if err := g.Output(ctx, 1, true); err != nil || !s.out[1] {
		t.Fatalf("on: %v", err)
	}
	s.out[2] = true
	if err := g.AllOutputsOff(ctx); err != nil || s.out[1] || s.out[2] {
		t.Fatalf("all off: %v %v", err, s.out)
	}
}

func TestErrorQueueSurfaces(t *testing.T) {
	s := newSim()
	g := newGen(t, s)
	err := g.S.Set(context.Background(), "C1:BOGUS 1")
	if err == nil || !strings.Contains(err.Error(), "-113") {
		t.Fatalf("got %v", err)
	}
}
