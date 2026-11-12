package app

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// ---------- Generator ----------

// FGSetRequest is a generator configuration request.
type FGSetRequest struct {
	Alias   string
	Channel int
	Wave    domain.WaveSpec
	Load    string // "" = unchanged
}

// FGResult is a generator channel state plus warnings.
type FGResult struct {
	Alias    string                 `json:"alias"`
	State    domain.GenChannelState `json:"state"`
	Warnings []string               `json:"warnings,omitempty"`
}

// CheckGenLimits enforces bench.json limits for a generator alias:
// max_amp_vpp and max_abs_v (|offset| + amp/2).
func CheckGenLimits(b *domain.Bench, alias string, w domain.WaveSpec, cur domain.GenChannelState) error {
	amp := cur.AmpVpp
	if w.AmpVpp != nil {
		amp = *w.AmpVpp
	}
	if w.AmpRMS != nil {
		amp = *w.AmpRMS * 2 * math.Sqrt2 // sine-equivalent Vpp, conservative for other shapes
	}
	off := cur.Offset
	if w.Offset != nil {
		off = *w.Offset
	}
	wave := cur.Wave
	if w.Type != "" {
		wave = strings.ToLower(w.Type)
	}
	if wave == "dc" {
		amp = 0
	}
	if lim, ok := b.Limit(alias, "max_amp_vpp"); ok && amp > lim {
		return domain.Usagef("amplitude %g Vpp exceeds %s limit max_amp_vpp=%g", amp, alias, lim)
	}
	if lim, ok := b.Limit(alias, "max_abs_v"); ok && math.Abs(off)+amp/2 > lim {
		return domain.Usagef("peak |offset|+amp/2 = %g V exceeds %s limit max_abs_v=%g", math.Abs(off)+amp/2, alias, lim)
	}
	if lim, ok := b.Limit(alias, "max_freq_hz"); ok && w.Freq != nil && *w.Freq > lim {
		return domain.Usagef("frequency %g Hz exceeds %s limit max_freq_hz=%g", *w.Freq, alias, lim)
	}
	return nil
}

// FGSet configures a generator channel. It never touches the output state.
func (a *App) FGSet(ctx context.Context, r FGSetRequest) (FGResult, error) {
	alias, err := a.ResolveAlias(r.Alias, domain.RoleGenerator)
	if err != nil {
		return FGResult{}, err
	}
	b, err := a.bench()
	if err != nil {
		return FGResult{}, err
	}
	c := a.conns()
	defer c.closeAll()
	g, err := c.generator(ctx, alias)
	if err != nil {
		return FGResult{}, err
	}
	cur, err := g.State(ctx, r.Channel)
	if err != nil {
		return FGResult{}, err
	}
	if err := CheckGenLimits(b, alias, r.Wave, cur); err != nil {
		return FGResult{}, err
	}
	if r.Load != "" {
		if err := g.SetLoad(ctx, r.Channel, r.Load); err != nil {
			return FGResult{}, err
		}
	}
	if err := g.SetWave(ctx, r.Channel, r.Wave); err != nil {
		return FGResult{}, err
	}
	st, err := g.State(ctx, r.Channel)
	if err != nil {
		return FGResult{}, err
	}
	res := FGResult{Alias: alias, State: st}
	res.Warnings = a.loadWarnings(ctx, c, b, alias, r.Channel, st.Load)
	return res, nil
}

// loadWarnings checks the wiring for scope inputs fed by alias:CHn and warns
// when the generator load setting does not match the scope input impedance.
func (a *App) loadWarnings(ctx context.Context, c *conns, b *domain.Bench, alias string, ch int, load string) []string {
	var out []string
	for _, l := range b.Wiring {
		from, to := domain.ParseEndpoint(l.From), domain.ParseEndpoint(l.To)
		if from.Alias != alias {
			continue
		}
		if n, err := from.ChannelNumber(); err != nil || n != ch {
			continue
		}
		ref, ok := b.Instruments[to.Alias]
		if !ok || a.roleOf(ref.Model) != domain.RoleScope {
			continue
		}
		sch, err := to.ChannelNumber()
		if err != nil {
			continue
		}
		s, err := c.scope(ctx, to.Alias)
		if err != nil {
			continue
		}
		cs, err := s.ChannelState(ctx, sch)
		if err != nil {
			continue
		}
		if w := LoadMismatch(load, cs.Coupling); w != "" {
			msg := fmt.Sprintf("%s:CH%d → %s:C%d: %s", alias, ch, to.Alias, sch, w)
			a.warnf("%s", msg)
			out = append(out, msg)
		}
	}
	return out
}

// LoadMismatch explains a generator-load / scope-impedance mismatch, or "".
func LoadMismatch(load, coupling string) string {
	fifty := strings.HasSuffix(coupling, "50")
	switch {
	case load == "50" && !fifty:
		return "generator load is 50 Ω but the scope input is 1 MΩ: the scope will read 2× the set amplitude"
	case load == "hiz" && fifty:
		return "generator load is HiZ but the scope input is 50 Ω: the scope will read ½ the set amplitude"
	}
	return ""
}

// FGOutput switches a generator output.
func (a *App) FGOutput(ctx context.Context, alias string, ch int, on bool) (FGResult, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleGenerator)
	if err != nil {
		return FGResult{}, err
	}
	c := a.conns()
	defer c.closeAll()
	g, err := c.generator(ctx, alias)
	if err != nil {
		return FGResult{}, err
	}
	if err := g.Output(ctx, ch, on); err != nil {
		return FGResult{}, err
	}
	st, err := g.State(ctx, ch)
	return FGResult{Alias: alias, State: st}, err
}

// FGState returns the state of one or all generator channels.
func (a *App) FGState(ctx context.Context, alias string, ch int) ([]domain.GenChannelState, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleGenerator)
	if err != nil {
		return nil, err
	}
	c := a.conns()
	defer c.closeAll()
	g, err := c.generator(ctx, alias)
	if err != nil {
		return nil, err
	}
	chs := []int{ch}
	if ch == 0 {
		chs = nil
		for i := 1; i <= g.Channels(); i++ {
			chs = append(chs, i)
		}
	}
	var out []domain.GenChannelState
	for _, n := range chs {
		st, err := g.State(ctx, n)
		if err != nil {
			return out, err
		}
		out = append(out, st)
	}
	return out, nil
}

// ---------- Scope ----------

// ScopeSetup configures a scope channel and returns its effective state.
func (a *App) ScopeSetup(ctx context.Context, alias string, ch int, s domain.ChannelSetup) (domain.ChannelState, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return domain.ChannelState{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return domain.ChannelState{}, err
	}
	if err := sc.SetupChannel(ctx, ch, s); err != nil {
		return domain.ChannelState{}, err
	}
	return sc.ChannelState(ctx, ch)
}

// ScopeTimebase sets (if given) and returns the timebase.
func (a *App) ScopeTimebase(ctx context.Context, alias string, tdiv, delay *float64) (domain.Timebase, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return domain.Timebase{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return domain.Timebase{}, err
	}
	if err := sc.SetTimebase(ctx, tdiv, delay); err != nil {
		return domain.Timebase{}, err
	}
	return sc.Timebase(ctx)
}

// ScopeTrigger sets (if given) and returns the trigger.
func (a *App) ScopeTrigger(ctx context.Context, alias string, t domain.TriggerSetup) (domain.TriggerState, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return domain.TriggerState{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return domain.TriggerState{}, err
	}
	if err := sc.SetTrigger(ctx, t); err != nil {
		return domain.TriggerState{}, err
	}
	return sc.Trigger(ctx)
}

// AutosetResult reports what autoset did.
type AutosetResult struct {
	Alias   string        `json:"alias"`
	Changed bool          `json:"changed"`
	Elapsed string        `json:"elapsed"`
	Before  ScopeSnapshot `json:"before"`
	After   ScopeSnapshot `json:"after"`
	Note    string        `json:"note,omitempty"`
}

// ScopeAutoset runs autoset, waits for the scope to apply it, and returns
// the settings before and after.
func (a *App) ScopeAutoset(ctx context.Context, alias string) (AutosetResult, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return AutosetResult{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return AutosetResult{}, err
	}
	res := AutosetResult{Alias: alias}
	before, err := snapshot(ctx, sc)
	if err != nil {
		return res, err
	}
	res.Before = before.(ScopeSnapshot)
	start := time.Now()
	if res.Changed, err = sc.Autoset(ctx); err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	after, err := snapshot(ctx, sc)
	if err != nil {
		return res, err
	}
	res.After = after.(ScopeSnapshot)
	if !res.Changed {
		res.Note = "no setting changed: the current settings already suit the signal, or there is no signal to lock onto"
	}
	return res, nil
}

// MeasureResult is a scope measurement with the settings in effect.
type MeasureResult struct {
	Alias        string               `json:"alias"`
	Channel      int                  `json:"channel"`
	Measurements []domain.Measurement `json:"measurements"`
	Settings     domain.ChannelState  `json:"settings"`
	Timebase     domain.Timebase      `json:"timebase"`
	Trustworthy  bool                 `json:"trustworthy"`
	Warnings     []string             `json:"warnings,omitempty"`
}

// Value returns a measurement by name.
func (m MeasureResult) Value(name string) (*float64, bool) {
	for _, x := range m.Measurements {
		if x.Name == name {
			return x.Value, true
		}
	}
	return nil, false
}

// ScopeMeasure measures params on ch and flags readings that cannot be trusted.
func (a *App) ScopeMeasure(ctx context.Context, alias string, ch int, params []string) (MeasureResult, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return MeasureResult{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return MeasureResult{}, err
	}
	return measure(ctx, sc, alias, ch, params)
}

func measure(ctx context.Context, sc port.Scope, alias string, ch int, params []string) (MeasureResult, error) {
	if len(params) == 0 {
		return MeasureResult{}, domain.Usagef("no measurements requested (e.g. pkpk freq rms mean)")
	}
	norm := make([]string, len(params))
	for i, p := range params {
		norm[i] = strings.ToLower(p)
	}
	ask := norm
	hasPkpk := false
	for _, p := range norm {
		hasPkpk = hasPkpk || p == "pkpk"
	}
	if !hasPkpk {
		ask = append(append([]string{}, norm...), "pkpk")
	}
	ms, err := sc.Measure(ctx, ch, ask)
	if err != nil {
		return MeasureResult{}, err
	}
	res := MeasureResult{Alias: alias, Channel: ch}
	var pkpk *float64
	for _, m := range ms {
		if m.Name == "pkpk" {
			pkpk = m.Value
		}
	}
	res.Measurements = ms[:len(norm)]
	if res.Settings, err = sc.ChannelState(ctx, ch); err != nil {
		return res, err
	}
	if res.Timebase, err = sc.Timebase(ctx); err != nil {
		return res, err
	}
	res.Warnings = MeasureWarnings(res.Settings, pkpk, res.Measurements)
	res.Trustworthy = len(res.Warnings) == 0
	return res, nil
}

// MeasureWarnings flags conditions that make a reading untrustworthy.
func MeasureWarnings(cs domain.ChannelState, pkpk *float64, ms []domain.Measurement) []string {
	var w []string
	if !cs.On {
		w = append(w, fmt.Sprintf("channel C%d is off", cs.Channel))
	}
	if pkpk != nil && cs.VDiv > 0 {
		divs := *pkpk / cs.VDiv
		switch {
		case divs >= 7.8:
			w = append(w, fmt.Sprintf("signal spans %.1f of 8 divisions: likely clipped; increase vdiv", divs))
		case divs < 0.5:
			w = append(w, fmt.Sprintf("signal spans only %.2f divisions: low resolution; decrease vdiv", divs))
		}
	}
	for _, m := range ms {
		if m.Value == nil {
			w = append(w, fmt.Sprintf("%s is invalid (****): no signal, wrong timebase or no trigger", m.Name))
		}
	}
	return w
}

// ScopeCapture captures a waveform.
func (a *App) ScopeCapture(ctx context.Context, alias string, ch, points int) (domain.Waveform, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleScope)
	if err != nil {
		return domain.Waveform{}, err
	}
	c := a.conns()
	defer c.closeAll()
	sc, err := c.scope(ctx, alias)
	if err != nil {
		return domain.Waveform{}, err
	}
	return sc.Capture(ctx, ch, points)
}

// Screenshot returns the screen of any instrument that supports it (BMP).
func (a *App) Screenshot(ctx context.Context, alias string) ([]byte, error) {
	c := a.conns()
	defer c.closeAll()
	inst, err := c.get(ctx, alias)
	if err != nil {
		return nil, err
	}
	s, ok := inst.(port.Screenshotter)
	if !ok {
		return nil, domain.Usagef("%s (%s) does not support screenshots", alias, inst.Identity().Model)
	}
	return s.Screenshot(ctx)
}

// ---------- DMM ----------

// DMMConfigure configures the DMM and returns the effective configuration.
func (a *App) DMMConfigure(ctx context.Context, alias string, cfg domain.DMMConfig) (domain.DMMConfig, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleDMM)
	if err != nil {
		return domain.DMMConfig{}, err
	}
	c := a.conns()
	defer c.closeAll()
	d, err := c.dmm(ctx, alias)
	if err != nil {
		return domain.DMMConfig{}, err
	}
	if err := d.Configure(ctx, cfg); err != nil {
		return domain.DMMConfig{}, err
	}
	return d.Config(ctx)
}

// DMMRead takes n readings, interval apart.
func (a *App) DMMRead(ctx context.Context, alias string, n int, interval time.Duration) (domain.DMMReading, error) {
	alias, err := a.ResolveAlias(alias, domain.RoleDMM)
	if err != nil {
		return domain.DMMReading{}, err
	}
	c := a.conns()
	defer c.closeAll()
	d, err := c.dmm(ctx, alias)
	if err != nil {
		return domain.DMMReading{}, err
	}
	return a.dmmRead(ctx, d, n, interval)
}

func (a *App) dmmRead(ctx context.Context, d port.DMM, n int, interval time.Duration) (domain.DMMReading, error) {
	if n < 1 {
		n = 1
	}
	cfg, err := d.Config(ctx)
	if err != nil {
		return domain.DMMReading{}, err
	}
	r := domain.DMMReading{Function: cfg.Function, Unit: domain.DMMFunctions[cfg.Function], Range: cfg.Range}
	for i := 0; i < n; i++ {
		if i > 0 && interval > 0 {
			a.sleep(interval)
		}
		v, err := d.Read(ctx)
		if err != nil {
			return r, err
		}
		r.Values = append(r.Values, v)
	}
	Stats(&r)
	return r, nil
}

// Stats fills mean/min/max/stddev (sample standard deviation).
func Stats(r *domain.DMMReading) {
	if len(r.Values) == 0 {
		return
	}
	r.Min, r.Max = math.Inf(1), math.Inf(-1)
	sum := 0.0
	for _, v := range r.Values {
		sum += v
		r.Min = math.Min(r.Min, v)
		r.Max = math.Max(r.Max, v)
	}
	r.Mean = sum / float64(len(r.Values))
	if len(r.Values) > 1 {
		ss := 0.0
		for _, v := range r.Values {
			ss += (v - r.Mean) * (v - r.Mean)
		}
		r.StdDev = math.Sqrt(ss / float64(len(r.Values)-1))
	}
}
