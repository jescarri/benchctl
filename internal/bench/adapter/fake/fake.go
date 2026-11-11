// Package fake provides in-memory instruments wired together like a real
// bench, for application-layer tests: generator channels drive scope
// channels through "cables" with a gain, and DMMs read either a generator
// DC level or an external source.
package fake

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// Cable connects a generator channel to a scope channel (or DMM, ScopeCh=0).
type Cable struct {
	Gen   string
	GenCh int
	To    string
	ToCh  int
	Gain  float64 // 1 = ideal; 0.1 = 10x attenuation
	// EdgesOnly models a path coupled only through a stray capacitance (a
	// probe tip or connector not making contact): only a square wave's edges
	// arrive, so top-to-base amplitude collapses while pkpk does not.
	EdgesOnly bool
}

// Bench is a set of fake instruments plus the physical cabling.
type Bench struct {
	mu       sync.Mutex
	Gens     map[string]*Generator
	Scopes   map[string]*Scope
	DMMs     map[string]*DMM
	Cables   []Cable
	OpenErrs map[string]error
	Opened   []string
}

// NewBench returns an empty bench.
func NewBench() *Bench {
	return &Bench{Gens: map[string]*Generator{}, Scopes: map[string]*Scope{}, DMMs: map[string]*DMM{}, OpenErrs: map[string]error{}}
}

// AddGenerator adds a 2-channel generator.
func (b *Bench) AddGenerator(alias string) *Generator {
	g := &Generator{bench: b, id: domain.Identity{Vendor: "Fake", Model: "SDG1032X", Serial: "FG-" + alias}}
	for i := range g.ch {
		g.ch[i] = domain.GenChannelState{Channel: i, Wave: "sine", Freq: 1000, AmpVpp: 4, Load: "hiz"}
	}
	b.Gens[alias] = g
	return g
}

// AddScope adds a 4-channel scope.
func (b *Bench) AddScope(alias string) *Scope {
	s := &Scope{bench: b, alias: alias, id: domain.Identity{Vendor: "Fake", Model: "SDS1104X-E", Serial: "SC-" + alias},
		tb: domain.Timebase{TDiv: 0.5, SampleRate: 1e5, MemDepth: "700K"},
		tr: domain.TriggerState{Type: "edge", Source: "C1", Mode: "auto", Slope: "rise", Level: 0.96}}
	for i := range s.ch {
		s.ch[i] = domain.ChannelState{Channel: i, VDiv: 1, Probe: 1, Coupling: "dc", On: i <= 2}
	}
	b.Scopes[alias] = s
	return s
}

// AddDMM adds a DMM reading External when nothing else drives it.
func (b *Bench) AddDMM(alias string, external float64) *DMM {
	d := &DMM{bench: b, alias: alias, External: external, id: domain.Identity{Vendor: "Fake", Model: "SDM3045X", Serial: "DM-" + alias},
		cfg: domain.DMMConfig{Function: "vdc", Range: "auto"}}
	b.DMMs[alias] = d
	return d
}

// Bench returns a matching inventory.
func (b *Bench) Inventory() *domain.Bench {
	inv := &domain.Bench{Subnet: "10.0.0.0/24", Instruments: map[string]domain.InstrumentRef{}}
	add := func(alias string, id domain.Identity) {
		inv.Instruments[alias] = domain.InstrumentRef{Serial: id.Serial, Model: id.Model, LastIP: "10.0.0.1"}
	}
	for a, g := range b.Gens {
		add(a, g.id)
	}
	for a, s := range b.Scopes {
		add(a, s.id)
	}
	for a, d := range b.DMMs {
		add(a, d.id)
	}
	return inv
}

// Open implements port.Connector.
func (b *Bench) Open(_ context.Context, alias string) (port.Instrument, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.OpenErrs[alias]; err != nil {
		return nil, err
	}
	b.Opened = append(b.Opened, alias)
	if g, ok := b.Gens[alias]; ok {
		return g, nil
	}
	if s, ok := b.Scopes[alias]; ok {
		return s, nil
	}
	if d, ok := b.DMMs[alias]; ok {
		return d, nil
	}
	return nil, domain.Usagef("unknown alias %q", alias)
}

// RoleOf maps fake models to roles.
func RoleOf(model string) domain.Role {
	switch {
	case strings.HasPrefix(model, "SDS"):
		return domain.RoleScope
	case strings.HasPrefix(model, "SDG"):
		return domain.RoleGenerator
	case strings.HasPrefix(model, "SDM"):
		return domain.RoleDMM
	}
	return domain.RoleUnknown
}

// ---- SCPI stub ----

// SCPI records raw commands and answers queries from Responses.
type SCPI struct {
	mu        sync.Mutex
	Log       []string
	Responses map[string]string
	SetErrs   map[string]error
}

func (s *SCPI) Write(_ context.Context, cmd string) error { s.log(cmd); return nil }
func (s *SCPI) Set(_ context.Context, cmd string) error {
	s.log(cmd)
	return s.SetErrs[cmd]
}
func (s *SCPI) Query(_ context.Context, cmd string) (string, error) {
	s.log(cmd)
	if r, ok := s.Responses[cmd]; ok {
		return r, nil
	}
	return "", domain.Instrumentf("no response to %q", cmd)
}
func (s *SCPI) QueryBytes(ctx context.Context, cmd string) ([]byte, error) {
	r, err := s.Query(ctx, cmd)
	return []byte(r + "\n"), err
}
func (s *SCPI) QueryBlock(ctx context.Context, cmd string) ([]byte, error) {
	r, err := s.Query(ctx, cmd)
	return []byte(r), err
}
func (s *SCPI) CheckErrors(context.Context) error  { return nil }
func (s *SCPI) WaitComplete(context.Context) error { s.log("*OPC?"); return nil }
func (s *SCPI) Clear(context.Context) error        { return nil }
func (s *SCPI) SRQStatus() string                  { return "unavailable: fake" }
func (s *SCPI) RunAndWait(ctx context.Context, cmd, method string) (string, error) {
	if err := s.Set(ctx, cmd); err != nil {
		return method, err
	}
	s.log("*OPC?")
	if method == "" || method == "auto" {
		method = "opc"
	}
	return method, nil
}
func (s *SCPI) TransportName() string { return "fake" }
func (s *SCPI) log(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Log = append(s.Log, c)
}

// ---- Generator ----

// Generator is a fake 2-channel generator.
type Generator struct {
	bench *Bench
	id    domain.Identity
	ch    [3]domain.GenChannelState
	scpi  SCPI
}

func (g *Generator) Identity() domain.Identity { return g.id }
func (g *Generator) Role() domain.Role         { return domain.RoleGenerator }
func (g *Generator) SCPI() port.SCPI           { return &g.scpi }
func (g *Generator) Close() error              { return nil }
func (g *Generator) Channels() int             { return 2 }

func (g *Generator) check(ch int) error {
	if ch < 1 || ch > 2 {
		return domain.Usagef("channel %d out of range", ch)
	}
	return nil
}

func (g *Generator) SetWave(_ context.Context, ch int, w domain.WaveSpec) error {
	if err := g.check(ch); err != nil {
		return err
	}
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	st := &g.ch[ch]
	if w.Type != "" {
		st.Wave = strings.ToLower(w.Type)
	}
	if w.Freq != nil {
		st.Freq = math.Min(*w.Freq, 30e6)
	}
	if w.AmpVpp != nil {
		st.AmpVpp = *w.AmpVpp
	}
	if w.AmpRMS != nil {
		st.AmpVpp = *w.AmpRMS * 2 * math.Sqrt2
	}
	if w.Offset != nil {
		st.Offset = *w.Offset
	}
	if w.Freq != nil && *w.Freq > 30e6 {
		return domain.Instrumentf("frequency: requested %g but instrument applied %g", *w.Freq, st.Freq)
	}
	return nil
}

func (g *Generator) SetLoad(_ context.Context, ch int, load string) error {
	if err := g.check(ch); err != nil {
		return err
	}
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	g.ch[ch].Load = strings.ToLower(load)
	return nil
}

func (g *Generator) Output(_ context.Context, ch int, on bool) error {
	if err := g.check(ch); err != nil {
		return err
	}
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	g.ch[ch].Output = on
	return nil
}

func (g *Generator) State(_ context.Context, ch int) (domain.GenChannelState, error) {
	if err := g.check(ch); err != nil {
		return domain.GenChannelState{}, err
	}
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	return g.ch[ch], nil
}

func (g *Generator) AllOutputsOff(ctx context.Context) error {
	for ch := 1; ch <= 2; ch++ {
		_ = g.Output(ctx, ch, false)
	}
	return nil
}

// Ch returns channel state without locking concerns (tests).
func (g *Generator) Ch(ch int) domain.GenChannelState {
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	return g.ch[ch]
}

// ---- Scope ----

// Scope is a fake 4-channel scope whose measurements follow the cabling.
type Scope struct {
	bench *Bench
	alias string
	id    domain.Identity
	ch    [5]domain.ChannelState
	tb    domain.Timebase
	tr    domain.TriggerState
	scpi  SCPI
}

func (s *Scope) Identity() domain.Identity { return s.id }
func (s *Scope) Role() domain.Role         { return domain.RoleScope }
func (s *Scope) SCPI() port.SCPI           { return &s.scpi }
func (s *Scope) Close() error              { return nil }
func (s *Scope) Channels() int             { return 4 }

func (s *Scope) SetupChannel(_ context.Context, ch int, c domain.ChannelSetup) error {
	if ch < 1 || ch > 4 {
		return domain.Usagef("channel %d out of range", ch)
	}
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	st := &s.ch[ch]
	if c.VDiv != nil {
		if *c.VDiv > 10 {
			return domain.Instrumentf("vdiv: requested %g but instrument applied 10", *c.VDiv)
		}
		st.VDiv = *c.VDiv
	}
	if c.Offset != nil {
		st.Offset = *c.Offset
	}
	if c.Probe != nil {
		st.Probe = *c.Probe
	}
	if c.Coupling != "" {
		st.Coupling = c.Coupling
	}
	if c.On != nil {
		st.On = *c.On
	}
	if c.BWLimit != nil {
		st.BWLimit = *c.BWLimit
	}
	return nil
}

func (s *Scope) ChannelState(_ context.Context, ch int) (domain.ChannelState, error) {
	if ch < 1 || ch > 4 {
		return domain.ChannelState{}, domain.Usagef("channel %d out of range", ch)
	}
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	return s.ch[ch], nil
}

func (s *Scope) SetTimebase(_ context.Context, tdiv, delay *float64) error {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	if tdiv != nil {
		s.tb.TDiv = *tdiv
	}
	if delay != nil {
		s.tb.Delay = *delay
	}
	return nil
}

func (s *Scope) Timebase(context.Context) (domain.Timebase, error) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	return s.tb, nil
}

func (s *Scope) SetTrigger(_ context.Context, t domain.TriggerSetup) error {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	if t.Source != "" {
		s.tr.Source = strings.ToUpper(t.Source)
	}
	if t.Mode != "" {
		s.tr.Mode = t.Mode
	}
	if t.Slope != "" {
		s.tr.Slope = t.Slope
	}
	if t.Level != nil {
		s.tr.Level = *t.Level
	}
	return nil
}

func (s *Scope) Trigger(context.Context) (domain.TriggerState, error) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	return s.tr, nil
}

// Autoset mimics autoset: channels with a signal get a vdiv that fits it.
func (s *Scope) Autoset(context.Context) (bool, error) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	changed := false
	for ch := 1; ch <= 4; ch++ {
		if st, gain, ok := s.signal(ch); ok {
			v := st.AmpVpp * gain / 5
			changed = changed || s.ch[ch].VDiv != v
			s.ch[ch].VDiv, s.ch[ch].On = v, true
		}
	}
	return changed, nil
}

// signal returns the generator signal arriving at scope channel ch.
func (s *Scope) signal(ch int) (domain.GenChannelState, float64, bool) {
	st, c, ok := s.cable(ch)
	return st, c.Gain, ok
}

// cable returns the generator signal and the cable arriving at scope channel ch.
func (s *Scope) cable(ch int) (domain.GenChannelState, Cable, bool) {
	for _, c := range s.bench.Cables {
		if c.To != s.alias || c.ToCh != ch {
			continue
		}
		g, ok := s.bench.Gens[c.Gen]
		if !ok {
			continue
		}
		st := g.ch[c.GenCh]
		if st.Output {
			return st, c, true
		}
	}
	return domain.GenChannelState{}, Cable{}, false
}

func (s *Scope) Measure(_ context.Context, ch int, params []string) ([]domain.Measurement, error) {
	if ch < 1 || ch > 4 {
		return nil, domain.Usagef("channel %d out of range", ch)
	}
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	st, cab, ok := s.cable(ch)
	gain := cab.Gain
	// Readings are in probe-scaled volts, like the instrument's.
	if p := s.ch[ch].Probe; p > 0 {
		gain *= p
	}
	// The ADC spans about ten divisions; beyond that pkpk clips.
	fullScale := 10 * s.ch[ch].VDiv
	var out []domain.Measurement
	for _, p := range params {
		m := domain.Measurement{Name: strings.ToLower(p)}
		var v float64
		valid := true
		switch m.Name {
		case "freq":
			m.Unit = "Hz"
			// Like the SDS1000X-E, no frequency reading for a signal under
			// one division (e.g. a 10X probe at the 1X test scale).
			v, valid = st.Freq, ok && st.Wave != "dc" && st.AmpVpp*gain >= s.ch[ch].VDiv
		case "pkpk":
			m.Unit = "V"
			v = 0.004
			if ok {
				v = math.Min(st.AmpVpp*gain, fullScale)
			}
		case "ampl":
			m.Unit = "V"
			v = 0.004
			if ok {
				v = math.Min(st.AmpVpp*gain, fullScale)
				if cab.EdgesOnly && st.Wave == "square" {
					v *= 0.02
				}
			}
		case "rms":
			m.Unit = "V"
			if ok {
				v = st.AmpVpp * gain / 2 / math.Sqrt2
			}
		case "mean":
			m.Unit = "V"
			if ok {
				v = st.Offset * gain
			}
		default:
			return nil, domain.Usagef("unknown measurement %q", p)
		}
		if valid {
			vv := v
			m.Value = &vv
		}
		out = append(out, m)
	}
	return out, nil
}

// Capture synthesises 1024 samples of the arriving signal at 100 kSa/s.
func (s *Scope) Capture(_ context.Context, ch int, points int) (domain.Waveform, error) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	st, gain, ok := s.signal(ch)
	n := 1024
	w := domain.Waveform{Channel: ch, Points: n, Dt: 1e-5, T0: -float64(n) / 2 * 1e-5, VDiv: s.ch[ch].VDiv, Codes: make([]int8, n), Volts: make([]float64, n)}
	for i := range w.Volts {
		if ok {
			w.Volts[i] = st.AmpVpp / 2 * gain * math.Sin(2*math.Pi*st.Freq*float64(i)*w.Dt)
		}
	}
	return w, nil
}

func (s *Scope) Screenshot(context.Context) ([]byte, error) { return []byte("BM\x06\x00\x00\x00"), nil }

// ---- DMM ----

// DMM is a fake multimeter.
type DMM struct {
	bench    *Bench
	alias    string
	id       domain.Identity
	cfg      domain.DMMConfig
	External float64
	scpi     SCPI
	Reads    int
}

func (d *DMM) Identity() domain.Identity { return d.id }
func (d *DMM) Role() domain.Role         { return domain.RoleDMM }
func (d *DMM) SCPI() port.SCPI           { return &d.scpi }
func (d *DMM) Close() error              { return nil }

func (d *DMM) Configure(_ context.Context, cfg domain.DMMConfig) error {
	if _, ok := domain.DMMFunctions[cfg.Function]; !ok {
		return domain.Usagef("unknown function %q", cfg.Function)
	}
	d.cfg = cfg
	return nil
}

func (d *DMM) Config(context.Context) (domain.DMMConfig, error) { return d.cfg, nil }

func (d *DMM) Read(context.Context) (float64, error) {
	d.bench.mu.Lock()
	defer d.bench.mu.Unlock()
	d.Reads++
	for _, c := range d.bench.Cables {
		if c.To != d.alias {
			continue
		}
		if g, ok := d.bench.Gens[c.Gen]; ok && g.ch[c.GenCh].Output && g.ch[c.GenCh].Wave == "dc" {
			return g.ch[c.GenCh].Offset * c.Gain, nil
		}
	}
	return d.External, nil
}

// ---- Store & scanner ----

// Store is an in-memory port.ConfigStore.
type Store struct {
	B     *domain.Bench
	Saves int
}

func (s *Store) Load() (*domain.Bench, error) {
	// return a deep-enough copy so callers can mutate safely
	c := *s.B
	c.Instruments = map[string]domain.InstrumentRef{}
	for k, v := range s.B.Instruments {
		c.Instruments[k] = v
	}
	return &c, nil
}
func (s *Store) Save(b *domain.Bench) error { s.B = b; s.Saves++; return nil }
func (s *Store) Path() string               { return "mem://bench.json" }

// Scanner is a fake port.Discoverer.
type Scanner struct {
	Found []port.Found
	Err   error
}

func (s *Scanner) Scan(context.Context, string) ([]port.Found, error) {
	out := append([]port.Found{}, s.Found...)
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, s.Err
}

// NoSleep is a Sleep replacement for tests.
func NoSleep(time.Duration) {}
