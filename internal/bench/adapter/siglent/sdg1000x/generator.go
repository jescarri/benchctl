// Package sdg1000x drives Siglent SDG1000X function/arbitrary waveform generators.
package sdg1000x

import (
	"context"
	"fmt"
	"strings"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/base"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// Generator implements port.Generator and port.Outputs.
type Generator struct {
	base.Instrument
	channels int
}

var (
	_ port.Generator     = (*Generator)(nil)
	_ port.Outputs       = (*Generator)(nil)
	_ port.Screenshotter = (*Generator)(nil)
)

// Matches reports whether model is handled by this driver.
func Matches(model string) bool {
	return strings.HasPrefix(model, "SDG1") && strings.HasSuffix(model, "X")
}

// New initialises the generator session and clears stale errors.
func New(ctx context.Context, b base.Instrument) (*Generator, error) {
	b.R = domain.RoleGenerator
	b.S.SetChecker(scpi.SCPI1999Checker)
	b.S.OPCEvent = true // *OPC sets ESR bit 0 (verified on fw 1.01.01.33R1B6)
	b.DrainErrors(ctx)
	return &Generator{Instrument: b, channels: 2}, nil
}

// Channels implements port.Generator.
func (g *Generator) Channels() int { return g.channels }

var waveTypes = map[string]string{
	"sine": "SINE", "square": "SQUARE", "ramp": "RAMP", "pulse": "PULSE",
	"noise": "NOISE", "dc": "DC", "arb": "ARB",
}

// SetWave implements port.Generator. Every numeric parameter is read back,
// because the SDG silently clamps out-of-range values (e.g. FRQ 1E12 → 30 MHz).
func (g *Generator) SetWave(ctx context.Context, ch int, w domain.WaveSpec) error {
	if err := base.CheckChannel(ch, g.channels); err != nil {
		return err
	}
	p := fmt.Sprintf("C%d:BSWV ", ch)
	if w.Type != "" {
		t, ok := waveTypes[strings.ToLower(w.Type)]
		if !ok {
			return domain.Usagef("unknown wave type %q (sine, square, ramp, pulse, noise, dc, arb)", w.Type)
		}
		if err := g.S.Set(ctx, p+"WVTP,"+t); err != nil {
			return err
		}
	}
	var args []string
	add := func(k string, v *float64) {
		if v != nil {
			args = append(args, k, base.F(*v))
		}
	}
	add("FRQ", w.Freq)
	add("AMP", w.AmpVpp)
	add("AMPVRMS", w.AmpRMS)
	add("OFST", w.Offset)
	add("PHSE", w.Phase)
	add("DUTY", w.Duty)
	if len(args) > 0 {
		if err := g.S.Set(ctx, p+strings.Join(args, ",")); err != nil {
			return err
		}
	}
	st, err := g.State(ctx, ch)
	if err != nil {
		return err
	}
	if w.Type != "" && st.Wave != strings.ToLower(w.Type) {
		return domain.Instrumentf("wave type: requested %s but instrument reports %s", w.Type, st.Wave)
	}
	check := func(what string, want *float64, got, rel, abs float64) error {
		if want != nil && !domain.Close(*want, got, rel, abs) {
			return domain.Instrumentf("%s: requested %g but instrument applied %g (out of range for this wave/load?)", what, *want, got)
		}
		return nil
	}
	if st.Wave != "dc" && st.Wave != "noise" {
		if err := check("frequency", w.Freq, st.Freq, 1e-6, 1e-6); err != nil {
			return err
		}
		if err := check("amplitude", w.AmpVpp, st.AmpVpp, 0.01, 1e-3); err != nil {
			return err
		}
		if err := check("amplitude rms", w.AmpRMS, st.AmpRMS, 0.01, 1e-3); err != nil {
			return err
		}
	}
	if err := check("offset", w.Offset, st.Offset, 0.01, 2e-3); err != nil {
		return err
	}
	if st.Duty != nil {
		if err := check("duty", w.Duty, *st.Duty, 0.01, 0.1); err != nil {
			return err
		}
	}
	return nil
}

// SetLoad implements port.Generator: "hiz" or a resistance in ohms (e.g. "50").
func (g *Generator) SetLoad(ctx context.Context, ch int, load string) error {
	if err := base.CheckChannel(ch, g.channels); err != nil {
		return err
	}
	l := strings.ToUpper(strings.TrimSpace(load))
	switch l {
	case "HIZ", "HZ":
		l = "HZ"
	default:
		v, _, err := domain.ParseIn(load, "Ohm")
		if err != nil || v < 50 || v > 100000 {
			return domain.Usagef("invalid load %q (hiz or 50..100k ohm)", load)
		}
		l = fmt.Sprint(v)
	}
	if err := g.S.Set(ctx, fmt.Sprintf("C%d:OUTP LOAD,%s", ch, l)); err != nil {
		return err
	}
	st, err := g.State(ctx, ch)
	if err != nil {
		return err
	}
	if want := normLoad(l); st.Load != want {
		return domain.Instrumentf("load: requested %s but instrument reports %s", want, st.Load)
	}
	return nil
}

// Output implements port.Generator.
func (g *Generator) Output(ctx context.Context, ch int, on bool) error {
	if err := base.CheckChannel(ch, g.channels); err != nil {
		return err
	}
	state := "OFF"
	if on {
		state = "ON"
	}
	if err := g.S.Set(ctx, fmt.Sprintf("C%d:OUTP %s", ch, state)); err != nil {
		return err
	}
	st, err := g.State(ctx, ch)
	if err != nil {
		return err
	}
	if st.Output != on {
		return domain.Instrumentf("output CH%d: requested %s but instrument reports otherwise", ch, state)
	}
	return nil
}

// AllOutputsOff implements port.Outputs.
func (g *Generator) AllOutputsOff(ctx context.Context) error {
	var first error
	for ch := 1; ch <= g.channels; ch++ {
		if err := g.Output(ctx, ch, false); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// State implements port.Generator.
func (g *Generator) State(ctx context.Context, ch int) (domain.GenChannelState, error) {
	if err := base.CheckChannel(ch, g.channels); err != nil {
		return domain.GenChannelState{}, err
	}
	bswv, err := g.S.Query(ctx, fmt.Sprintf("C%d:BSWV?", ch))
	if err != nil {
		return domain.GenChannelState{}, err
	}
	st, err := ParseBSWV(bswv)
	if err != nil {
		return st, err
	}
	st.Channel = ch
	outp, err := g.S.Query(ctx, fmt.Sprintf("C%d:OUTP?", ch))
	if err != nil {
		return st, err
	}
	if err := ParseOUTP(outp, &st); err != nil {
		return st, err
	}
	return st, nil
}

// stripHeader removes an echoed header such as "C1:BSWV ".
func stripHeader(r string) string {
	r = strings.TrimSpace(r)
	if i := strings.IndexByte(r, ' '); i >= 0 && strings.Contains(r[:i], ":") {
		return r[i+1:]
	}
	return r
}

// ParseBSWV parses "C1:BSWV WVTP,SINE,FRQ,1000HZ,PERI,0.001S,AMP,4V,...".
func ParseBSWV(r string) (domain.GenChannelState, error) {
	f := strings.Split(stripHeader(r), ",")
	if len(f) < 2 || len(f)%2 != 0 {
		return domain.GenChannelState{}, base.Errf("BSWV?: unexpected response %q", r)
	}
	st := domain.GenChannelState{Raw: map[string]string{}}
	for i := 0; i+1 < len(f); i += 2 {
		st.Raw[f[i]] = f[i+1]
	}
	st.Wave = strings.ToLower(st.Raw["WVTP"])
	num := func(k string) float64 {
		v, _, _ := domain.ParseInstrumentNumber(st.Raw[k])
		return v
	}
	st.Freq = num("FRQ")
	st.AmpVpp = num("AMP")
	st.AmpRMS = num("AMPVRMS")
	st.Offset = num("OFST")
	st.Phase = num("PHSE")
	st.HighV = num("HLEV")
	st.LowV = num("LLEV")
	if _, ok := st.Raw["DUTY"]; ok {
		d := num("DUTY")
		st.Duty = &d
	}
	return st, nil
}

// ParseOUTP parses "C1:OUTP OFF,LOAD,HZ,PLRT,NOR" into st.
func ParseOUTP(r string, st *domain.GenChannelState) error {
	f := strings.Split(stripHeader(r), ",")
	if len(f) < 1 || (f[0] != "ON" && f[0] != "OFF") {
		return base.Errf("OUTP?: unexpected response %q", r)
	}
	st.Output = f[0] == "ON"
	for i := 1; i+1 < len(f); i += 2 {
		switch f[i] {
		case "LOAD":
			st.Load = normLoad(f[i+1])
		case "PLRT":
			st.Polarity = strings.ToLower(f[i+1])
		}
	}
	return nil
}

func normLoad(l string) string {
	if strings.EqualFold(l, "HZ") || strings.EqualFold(l, "HIZ") {
		return "hiz"
	}
	if v, ok, err := domain.ParseInstrumentNumber(l); err == nil && ok {
		return fmt.Sprint(v)
	}
	return strings.ToLower(l)
}

// Screenshot implements port.Screenshotter (480x272 24-bit BMP via SCDP).
func (g *Generator) Screenshot(ctx context.Context) ([]byte, error) { return g.ScreenDump(ctx) }
