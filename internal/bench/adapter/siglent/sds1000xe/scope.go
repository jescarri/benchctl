// Package sds1000xe drives Siglent SDS1000X-E oscilloscopes using the legacy
// command set (C1:VDIV, TDIV, PAVA, WF?).
package sds1000xe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent/base"
	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// Settle delays. Variables so tests can zero them.
var (
	TimebaseSettle = 300 * time.Millisecond

	// Autoset completion. *OPC? returns immediately after ASET, and the scope
	// only applies the new settings after acquiring (≈6 s from 500 ms/div),
	// so completion is detected by polling the settings until they change
	// and then stay unchanged.
	AutosetPoll       = 300 * time.Millisecond
	AutosetStablePoll = 4                // unchanged polls after a change (≈1.2 s)
	AutosetMargin     = 8 * time.Second  // added to one screen (14 × TDIV)
	AutosetMax        = 60 * time.Second // cap for very slow timebases
)

// Scope implements port.Scope.
type Scope struct {
	base.Instrument
	channels int
	sleep    func(time.Duration)
}

var _ port.Scope = (*Scope)(nil)

// Matches reports whether model is handled by this driver.
func Matches(model string) bool {
	return strings.HasPrefix(model, "SDS1") && strings.HasSuffix(model, "X-E")
}

// New initialises the scope session: headers off and stale errors cleared.
func New(ctx context.Context, b base.Instrument) (*Scope, error) {
	b.R = domain.RoleScope
	b.S.SetChecker(scpi.SiglentLegacyChecker)
	s := &Scope{Instrument: b, channels: channelsFor(b.ID.Model), sleep: time.Sleep}
	if err := b.S.Write(ctx, "CHDR OFF"); err != nil {
		return nil, err
	}
	b.DrainErrors(ctx)
	for _, q := range []string{"CMR?", "EXR?"} {
		_, _ = b.S.Query(ctx, q) // reading clears the registers
	}
	return s, nil
}

// channelsFor derives the analog channel count from e.g. "SDS1104X-E" (4) or "SDS1202X-E" (2).
func channelsFor(model string) int {
	if len(model) >= 7 && model[6] == '4' {
		return 4
	}
	return 2
}

// Channels implements port.Scope.
func (s *Scope) Channels() int { return s.channels }

var couplingToSCPI = map[string]string{
	"dc": "D1M", "ac": "A1M", "gnd": "GND", "dc50": "D50", "ac50": "A50",
	"d1m": "D1M", "a1m": "A1M", "d50": "D50", "a50": "A50",
}

var couplingFromSCPI = map[string]string{"D1M": "dc", "A1M": "ac", "GND": "gnd", "D50": "dc50", "A50": "ac50"}

var validProbe = []float64{0.1, 0.2, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

// SetupChannel implements port.Scope. Order matters: probe first, since it
// rescales VDIV and OFST.
func (s *Scope) SetupChannel(ctx context.Context, ch int, c domain.ChannelSetup) error {
	if err := base.CheckChannel(ch, s.channels); err != nil {
		return err
	}
	p := fmt.Sprintf("C%d:", ch)
	if c.Probe != nil {
		ok := false
		for _, v := range validProbe {
			ok = ok || v == *c.Probe
		}
		if !ok {
			return domain.Usagef("probe attenuation %gx not supported (valid: %v)", *c.Probe, validProbe)
		}
		if err := s.SetVerified(ctx, "probe", p+"ATTN "+fmt.Sprint(*c.Probe), p+"ATTN?", *c.Probe, 1e-6, 0); err != nil {
			return err
		}
	}
	if c.Coupling != "" {
		cpl, ok := couplingToSCPI[strings.ToLower(c.Coupling)]
		if !ok {
			return domain.Usagef("unknown coupling %q (dc, ac, gnd, dc50, ac50)", c.Coupling)
		}
		if err := s.S.Set(ctx, p+"CPL "+cpl); err != nil {
			return err
		}
		got, err := s.S.Query(ctx, p+"CPL?")
		if err != nil {
			return err
		}
		if got != cpl {
			return domain.Instrumentf("coupling: requested %s but instrument reports %s", cpl, got)
		}
	}
	if c.VDiv != nil {
		if err := s.SetVerified(ctx, "vdiv", p+"VDIV "+base.F(*c.VDiv), p+"VDIV?", *c.VDiv, 0.01, 0); err != nil {
			return err
		}
	}
	if c.Offset != nil {
		vdiv, err := s.S.QueryFloat(ctx, p+"VDIV?")
		if err != nil {
			return err
		}
		if err := s.SetVerified(ctx, "offset", p+"OFST "+base.F(*c.Offset), p+"OFST?", *c.Offset, 0.01, vdiv*0.05); err != nil {
			return err
		}
	}
	if c.BWLimit != nil {
		if err := s.S.Set(ctx, fmt.Sprintf("BWL C%d,%s", ch, onOff(*c.BWLimit))); err != nil {
			return err
		}
	}
	if c.On != nil {
		if err := s.S.Set(ctx, p+"TRA "+onOff(*c.On)); err != nil {
			return err
		}
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// ChannelState implements port.Scope.
func (s *Scope) ChannelState(ctx context.Context, ch int) (domain.ChannelState, error) {
	if err := base.CheckChannel(ch, s.channels); err != nil {
		return domain.ChannelState{}, err
	}
	p := fmt.Sprintf("C%d:", ch)
	st := domain.ChannelState{Channel: ch}
	var err error
	if st.VDiv, err = s.S.QueryFloat(ctx, p+"VDIV?"); err != nil {
		return st, err
	}
	if st.Offset, err = s.S.QueryFloat(ctx, p+"OFST?"); err != nil {
		return st, err
	}
	if st.Probe, err = s.S.QueryFloat(ctx, p+"ATTN?"); err != nil {
		return st, err
	}
	cpl, err := s.S.Query(ctx, p+"CPL?")
	if err != nil {
		return st, err
	}
	st.Coupling = couplingFromSCPI[cpl]
	if st.Coupling == "" {
		st.Coupling = strings.ToLower(cpl)
	}
	tra, err := s.S.Query(ctx, p+"TRA?")
	if err != nil {
		return st, err
	}
	st.On = tra == "ON"
	bwl, err := s.S.Query(ctx, p+"BWL?")
	if err != nil {
		return st, err
	}
	st.BWLimit = bwl == "ON"
	return st, nil
}

// SetTimebase implements port.Scope.
func (s *Scope) SetTimebase(ctx context.Context, tdiv, delay *float64) error {
	if tdiv != nil {
		if err := s.SetVerified(ctx, "tdiv", "TDIV "+base.F(*tdiv), "TDIV?", *tdiv, 0.01, 0); err != nil {
			return err
		}
	}
	if delay != nil {
		td, err := s.S.QueryFloat(ctx, "TDIV?")
		if err != nil {
			return err
		}
		if err := s.SetVerified(ctx, "trigger delay", "TRDL "+base.F(*delay), "TRDL?", *delay, 0.01, td*0.02); err != nil {
			return err
		}
	}
	if tdiv != nil || delay != nil {
		s.sleep(TimebaseSettle)
	}
	return nil
}

// Timebase implements port.Scope.
func (s *Scope) Timebase(ctx context.Context) (domain.Timebase, error) {
	var tb domain.Timebase
	var err error
	if tb.TDiv, err = s.S.QueryFloat(ctx, "TDIV?"); err != nil {
		return tb, err
	}
	if tb.Delay, err = s.S.QueryFloat(ctx, "TRDL?"); err != nil {
		return tb, err
	}
	if tb.SampleRate, err = s.S.QueryFloat(ctx, "SARA?"); err != nil {
		return tb, err
	}
	if tb.MemDepth, err = s.S.Query(ctx, "MSIZ?"); err != nil {
		return tb, err
	}
	return tb, nil
}

var trigModes = map[string]string{"auto": "AUTO", "normal": "NORM", "norm": "NORM", "single": "SINGLE", "stop": "STOP"}
var trigModesBack = map[string]string{"AUTO": "auto", "NORM": "normal", "SINGLE": "single", "STOP": "stop"}
var slopes = map[string]string{"rise": "POS", "pos": "POS", "fall": "NEG", "neg": "NEG", "window": "WINDOW"}
var slopesBack = map[string]string{"POS": "rise", "NEG": "fall", "WINDOW": "window"}

func normSource(src string) (string, error) {
	u := strings.ToUpper(strings.TrimSpace(src))
	switch u {
	case "C1", "C2", "C3", "C4", "EX", "EX5", "LINE":
		return u, nil
	case "1", "2", "3", "4":
		return "C" + u, nil
	case "CH1", "CH2", "CH3", "CH4":
		return "C" + u[2:], nil
	}
	return "", domain.Usagef("unknown trigger source %q (C1..C4, EX, EX5, LINE)", src)
}

// SetTrigger implements port.Scope (edge trigger).
func (s *Scope) SetTrigger(ctx context.Context, t domain.TriggerSetup) error {
	src := ""
	if t.Source != "" {
		var err error
		if src, err = normSource(t.Source); err != nil {
			return err
		}
		// Only the short form is accepted cleanly: any HT (hold-off) argument
		// is applied but flags ESR CME on fw 8.1.6.1.35R2. The short form
		// resets hold-off to OFF.
		if err := s.S.Set(ctx, "TRSE EDGE,SR,"+src); err != nil {
			return err
		}
	}
	if src == "" && (t.Level != nil || t.Slope != "") {
		cur, err := s.Trigger(ctx)
		if err != nil {
			return err
		}
		src = cur.Source
	}
	if t.Slope != "" {
		sl, ok := slopes[strings.ToLower(t.Slope)]
		if !ok {
			return domain.Usagef("unknown slope %q (rise, fall, window)", t.Slope)
		}
		if err := s.S.Set(ctx, src+":TRSL "+sl); err != nil {
			return err
		}
	}
	if t.Level != nil {
		if err := s.SetVerified(ctx, "trigger level", src+":TRLV "+base.F(*t.Level), src+":TRLV?", *t.Level, 0.02, 0.01); err != nil {
			return err
		}
	}
	if t.Mode != "" {
		m, ok := trigModes[strings.ToLower(t.Mode)]
		if !ok {
			return domain.Usagef("unknown trigger mode %q (auto, normal, single, stop)", t.Mode)
		}
		if err := s.S.Set(ctx, "TRMD "+m); err != nil {
			return err
		}
	}
	return nil
}

// Trigger implements port.Scope.
func (s *Scope) Trigger(ctx context.Context) (domain.TriggerState, error) {
	var st domain.TriggerState
	trse, err := s.S.Query(ctx, "TRSE?")
	if err != nil {
		return st, err
	}
	// "EDGE,SR,C1,HT,TI,HV,8.00E-08s"
	f := strings.Split(trse, ",")
	st.Type = strings.ToLower(f[0])
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "SR" {
			st.Source = f[i+1]
		}
	}
	if st.Source == "" {
		return st, base.Errf("TRSE?: no source in %q", trse)
	}
	mode, err := s.S.Query(ctx, "TRMD?")
	if err != nil {
		return st, err
	}
	st.Mode = trigModesBack[mode]
	if st.Mode == "" {
		st.Mode = strings.ToLower(mode)
	}
	sl, err := s.S.Query(ctx, st.Source+":TRSL?")
	if err != nil {
		return st, err
	}
	st.Slope = slopesBack[sl]
	if st.Slope == "" {
		st.Slope = strings.ToLower(sl)
	}
	if st.Level, err = s.S.QueryFloat(ctx, st.Source+":TRLV?"); err != nil {
		return st, err
	}
	return st, nil
}

// setQuirk is for commands that this firmware applies correctly but still
// flags with ESR CME (observed: every WFSU form, TRSE with a hold-off
// argument). ESR is read and discarded, and success is decided by an exact
// read-back instead, which is the stronger check anyway.
func (s *Scope) setQuirk(ctx context.Context, cmd, query, want string) error {
	if err := s.S.Write(ctx, cmd); err != nil {
		return err
	}
	if _, err := s.S.ESR(ctx); err != nil {
		return err
	}
	got, err := s.S.Query(ctx, query)
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(got), want) {
		return domain.Instrumentf("%q: instrument reports %q, want %q", cmd, got, want)
	}
	return nil
}

// Autoset implements port.Scope.
//
// It returns once the scope has applied new settings and they have been
// stable for AutosetStablePoll polls, or, if nothing changes within one
// screen time plus AutosetMargin, assumes autoset kept the current settings.
func (s *Scope) Autoset(ctx context.Context) (changed bool, err error) {
	before, err := s.settingsKey(ctx)
	if err != nil {
		return false, err
	}
	tdiv, err := s.S.QueryFloat(ctx, "TDIV?")
	if err != nil {
		return false, err
	}
	if err := s.S.Write(ctx, "ASET"); err != nil {
		return false, err
	}
	limit := time.Duration(tdiv*HorizontalDivs*float64(time.Second)) + AutosetMargin
	if limit > AutosetMax {
		limit = AutosetMax
	}
	maxPolls := int(limit / AutosetPoll)
	last, stable := before, 0
	for i := 0; i < maxPolls; i++ {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		s.sleep(AutosetPoll)
		cur, err := s.settingsKey(ctx)
		if err != nil {
			return changed, err
		}
		switch {
		case cur != last:
			changed, last, stable = changed || cur != before, cur, 0
		case changed:
			stable++
			if stable >= AutosetStablePoll {
				return true, s.S.CheckErrors(ctx)
			}
		}
	}
	return changed, s.S.CheckErrors(ctx)
}

// settingsKey fingerprints the settings autoset may change.
func (s *Scope) settingsKey(ctx context.Context) (string, error) {
	var b strings.Builder
	for _, q := range []string{"TDIV?", "TRDL?"} {
		r, err := s.S.Query(ctx, q)
		if err != nil {
			return "", err
		}
		b.WriteString(r + "|")
	}
	for ch := 1; ch <= s.channels; ch++ {
		for _, q := range []string{"TRA?", "VDIV?", "OFST?"} {
			r, err := s.S.Query(ctx, fmt.Sprintf("C%d:%s", ch, q))
			if err != nil {
				return "", err
			}
			b.WriteString(r + "|")
		}
	}
	return b.String(), nil
}

// paramUnits maps PAVA parameters to units.
var paramUnits = map[string]string{
	"PKPK": "V", "MAX": "V", "MIN": "V", "AMPL": "V", "TOP": "V", "BASE": "V", "CMEAN": "V",
	"MEAN": "V", "STDEV": "V", "VSTD": "V", "RMS": "V", "CRMS": "V", "LEVELX": "V",
	"OVSN": "%", "FPRE": "%", "OVSP": "%", "RPRE": "%", "DUTY": "%", "NDUTY": "%",
	"PER": "s", "PWID": "s", "NWID": "s", "RISE": "s", "FALL": "s", "WID": "s",
	"DELAY": "s", "TIMEL": "s", "FREQ": "Hz",
}

// ParamNames lists the supported measurement names (lower case on the CLI).
func ParamNames() []string {
	out := make([]string, 0, len(paramUnits))
	for k := range paramUnits {
		out = append(out, strings.ToLower(k))
	}
	return out
}

// ParsePAVA parses "PKPK,2.40E-01" or "C1:PAVA PKPK,2.40E-01V". "****" yields a nil value.
func ParsePAVA(param, resp string) (domain.Measurement, error) {
	m := domain.Measurement{Name: strings.ToLower(param), Unit: paramUnits[param], Raw: resp}
	i := strings.LastIndexByte(resp, ',')
	if i < 0 {
		return m, base.Errf("PAVA %s: unexpected response %q", param, resp)
	}
	v, ok, err := domain.ParseInstrumentNumber(resp[i+1:])
	if err != nil {
		return m, base.Errf("PAVA %s: %v", param, err)
	}
	if ok {
		m.Value = &v
	}
	return m, nil
}

// Measure implements port.Scope.
func (s *Scope) Measure(ctx context.Context, ch int, params []string) ([]domain.Measurement, error) {
	if err := base.CheckChannel(ch, s.channels); err != nil {
		return nil, err
	}
	out := make([]domain.Measurement, 0, len(params))
	for _, p := range params {
		up := strings.ToUpper(p)
		if _, ok := paramUnits[up]; !ok {
			return nil, domain.Usagef("unknown measurement %q", p)
		}
		r, err := s.S.Query(ctx, fmt.Sprintf("C%d:PAVA? %s", ch, up))
		if err != nil {
			return nil, err
		}
		m, err := ParsePAVA(up, r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Capture implements port.Scope. points <= 0 transfers every sample in memory.
func (s *Scope) Capture(ctx context.Context, ch int, points int) (domain.Waveform, error) {
	if err := base.CheckChannel(ch, s.channels); err != nil {
		return domain.Waveform{}, err
	}
	// Point count comes from the descriptor with transfer set to "all points":
	// SANU? only answers for C1 on fw 8.1.6.1.35R2 (other channels time out).
	if err := s.setQuirk(ctx, "WFSU SP,1,NP,0,FP,0", "WFSU?", "SP,1,NP,0,FP,0"); err != nil {
		return domain.Waveform{}, err
	}
	descRaw, err := s.S.QueryBlock(ctx, fmt.Sprintf("C%d:WF? DESC", ch))
	if err != nil {
		return domain.Waveform{}, err
	}
	desc, err := DecodeWaveDesc(descRaw)
	if err != nil {
		return domain.Waveform{}, base.Errf("%v", err)
	}
	total := int(desc.WaveArrayCount)
	sp, np := 1, 0
	if points > 0 && total > points {
		sp = (total + points - 1) / points
		np = points
	}
	if sp != 1 || np != 0 {
		wfsu := fmt.Sprintf("SP,%d,NP,%d,FP,0", sp, np)
		if err := s.setQuirk(ctx, "WFSU "+wfsu, "WFSU?", wfsu); err != nil {
			return domain.Waveform{}, err
		}
	}
	data, err := s.S.QueryBlock(ctx, fmt.Sprintf("C%d:WF? DAT2", ch))
	if err != nil {
		return domain.Waveform{}, err
	}
	tdiv, err := s.S.QueryFloat(ctx, "TDIV?")
	if err != nil {
		return domain.Waveform{}, err
	}
	return BuildWaveform(ch, desc, descRaw, data, tdiv, sp), nil
}

// BuildWaveform converts raw codes to volts and computes the time axis.
//
//	V  = code * vdiv/25 - offset         (vdiv = WAVEDESC vertical_gain on SDS1000X-E)
//	t0 = horiz_offset - tdiv * 14/2      (horiz_offset = -TRDL; t = 0 is the trigger point)
//	dt = horiz_interval * sparsing
func BuildWaveform(ch int, desc WaveDesc, descRaw, data []byte, tdiv float64, sparsing int) domain.Waveform {
	vdiv := float64(desc.VerticalGain)
	off := float64(desc.VerticalOffset)
	w := domain.Waveform{
		Channel: ch,
		Points:  len(data),
		VDiv:    vdiv,
		Offset:  off,
		Dt:      float64(desc.HorizInterval) * float64(sparsing),
		T0:      desc.HorizOffset - tdiv*HorizontalDivs/2,
		Codes:   make([]int8, len(data)),
		Volts:   make([]float64, len(data)),
		RawDesc: descRaw,
		RawData: data,
	}
	if desc.HorizInterval > 0 {
		w.SampleRate = 1 / float64(desc.HorizInterval)
	}
	for i, b := range data {
		c := int8(b)
		w.Codes[i] = c
		w.Volts[i] = CodeToVolts(c, vdiv, off)
	}
	return w
}

// Screenshot implements port.Screenshotter (800x480 16-bit BMP via SCDP).
func (s *Scope) Screenshot(ctx context.Context) ([]byte, error) { return s.ScreenDump(ctx) }
