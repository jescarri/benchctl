package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jescarri/benchctl/internal/bench/domain"
)

// Plan is a declarative setup + measurement + assertion sequence.
//
//	name: fg to scope smoke test
//	safe_on_exit: true
//	steps:
//	  - fg.set:        {ch: 1, wave: sine, freq: 1k, amp: 1Vpp, offset: 0, load: hiz}
//	  - fg.output:     {ch: 1, state: on}
//	  - scope.setup:   {ch: 1, vdiv: 200m, offset: 0, coupling: dc, probe: 1, on: true}
//	  - scope.timebase: {tdiv: 500u}
//	  - scope.trigger: {source: C1, mode: auto, level: 0, slope: rise}
//	  - wait: 1s
//	  - scope.measure: {ch: 1, params: [pkpk, freq], expect: {pkpk: "1 ± 3%", freq: "1k ± 0.1%"}}
//	  - scope.capture: {ch: 1, points: 10000, out: c1.csv, expect: {fft_peak_hz: "1k ± 1%"}}
//	  - dmm.config:    {function: vdc, range: auto}
//	  - dmm.read:      {samples: 5, expect: "5 ± 5%"}
//	  - raw:           {alias: scope, cmd: "SARA?"}
//	  - safe: {}
type Plan struct {
	Name       string `yaml:"name"`
	SafeOnExit bool   `yaml:"safe_on_exit"`
	// Wiring is the cabling this plan needs (ADR §7). `wiring show --plan`
	// turns it into instructions for the human; Apply verifies it
	// electrically before the first step unless VerifyWiring is false.
	Wiring       []domain.Link          `yaml:"wiring"`
	VerifyWiring *bool                  `yaml:"verify_wiring"`
	Steps        []map[string]yaml.Node `yaml:"steps"`
}

// verifyWiring reports whether Apply should verify the plan's wiring first.
func (p *Plan) verifyWiring() bool {
	return len(p.Wiring) > 0 && (p.VerifyWiring == nil || *p.VerifyWiring)
}

// LoadPlan parses a YAML plan.
func LoadPlan(r io.Reader) (*Plan, error) {
	var p Plan
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, domain.Usagef("parse plan: %v", err)
	}
	if len(p.Steps) == 0 && len(p.Wiring) == 0 {
		return nil, domain.Usagef("plan has no steps and no wiring")
	}
	for i, s := range p.Steps {
		if len(s) != 1 {
			return nil, domain.Usagef("step %d: exactly one action per step (got %d)", i+1, len(s))
		}
	}
	return &p, nil
}

// StepResult is the outcome of one step.
type StepResult struct {
	Step       int         `json:"step"`
	Action     string      `json:"action"`
	OK         bool        `json:"ok"`
	Result     any         `json:"result,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Assertion is one checked expectation.
type Assertion struct {
	Name     string   `json:"name"`
	Expected string   `json:"expected"`
	Got      *float64 `json:"got"`
	OK       bool     `json:"ok"`
}

// PlanResult is the outcome of Apply.
type PlanResult struct {
	Name   string        `json:"name,omitempty"`
	OK     bool          `json:"ok"`
	Steps  []StepResult  `json:"steps"`
	Safe   []SafeResult  `json:"safe,omitempty"`
	Failed int           `json:"failed_assertions"`
	Wiring *WiringReport `json:"wiring,omitempty"`
}

// Expectation is "nominal ± tolerance", e.g. "1k ± 0.1%" or "5 +- 0.25".
type Expectation struct {
	Nominal float64
	Tol     domain.Tolerance
	Raw     string
}

// ParseExpectation parses "1k ± 0.1%", "5 +- 0.25" or ">= 4.5" / "<= 5.5".
func ParseExpectation(s string) (Expectation, error) {
	s = strings.TrimSpace(s)
	for _, sep := range []string{"±", "+-", "+/-"} {
		if n, t, ok := strings.Cut(s, sep); ok {
			q, err := domain.ParseQuantity(strings.TrimSpace(n))
			if err != nil {
				return Expectation{}, err
			}
			tol, err := domain.ParseTolerance(strings.TrimSpace(t))
			if err != nil {
				return Expectation{}, err
			}
			return Expectation{Nominal: q.Value, Tol: tol, Raw: s}, nil
		}
	}
	return Expectation{}, fmt.Errorf("expectation %q must look like \"1k ± 1%%\" or \"5 +- 0.1\"", s)
}

// Check evaluates got against the expectation.
func (e Expectation) Check(name string, got *float64) Assertion {
	a := Assertion{Name: name, Expected: e.Raw, Got: got}
	a.OK = got != nil && e.Tol.Within(e.Nominal, *got)
	return a
}

// Step argument types.
type (
	stepFGSet struct {
		Alias  string `yaml:"alias"`
		Ch     int    `yaml:"ch"`
		Wave   string `yaml:"wave"`
		Freq   string `yaml:"freq"`
		Amp    string `yaml:"amp"`
		Offset string `yaml:"offset"`
		Phase  string `yaml:"phase"`
		Duty   string `yaml:"duty"`
		Load   string `yaml:"load"`
	}
	stepFGOutput struct {
		Alias string `yaml:"alias"`
		Ch    int    `yaml:"ch"`
		State string `yaml:"state"`
	}
	stepScopeSetup struct {
		Alias    string `yaml:"alias"`
		Ch       int    `yaml:"ch"`
		VDiv     string `yaml:"vdiv"`
		Offset   string `yaml:"offset"`
		Coupling string `yaml:"coupling"`
		Probe    string `yaml:"probe"`
		On       *bool  `yaml:"on"`
		BWLimit  *bool  `yaml:"bw_limit"`
	}
	stepTimebase struct {
		Alias string `yaml:"alias"`
		TDiv  string `yaml:"tdiv"`
		Delay string `yaml:"delay"`
	}
	stepTrigger struct {
		Alias  string `yaml:"alias"`
		Source string `yaml:"source"`
		Mode   string `yaml:"mode"`
		Level  string `yaml:"level"`
		Slope  string `yaml:"slope"`
	}
	stepMeasure struct {
		Alias  string            `yaml:"alias"`
		Ch     int               `yaml:"ch"`
		Params []string          `yaml:"params"`
		Expect map[string]string `yaml:"expect"`
	}
	stepCapture struct {
		Alias  string            `yaml:"alias"`
		Ch     int               `yaml:"ch"`
		Points int               `yaml:"points"`
		Out    string            `yaml:"out"`
		Expect map[string]string `yaml:"expect"`
	}
	stepDMMConfig struct {
		Alias    string `yaml:"alias"`
		Function string `yaml:"function"`
		Range    string `yaml:"range"`
	}
	stepDMMRead struct {
		Alias    string `yaml:"alias"`
		Samples  int    `yaml:"samples"`
		Interval string `yaml:"interval"`
		Expect   string `yaml:"expect"`
	}
	stepRaw struct {
		Alias   string `yaml:"alias"`
		Cmd     string `yaml:"cmd"`
		OPC     bool   `yaml:"opc"`
		Timeout string `yaml:"timeout"`
	}
)

func optQ(s string, units ...string) (*float64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	v, _, err := domain.ParseIn(s, units...)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func decodeStep(n *yaml.Node, v any) error {
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Value == "") {
		return nil
	}
	// yaml.Node.Decode ignores unknown keys; a misspelled key such as
	// "expects:" would silently drop the assertions, so decode strictly.
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// Apply runs a plan. Outputs are switched off (safe) if any step fails, the
// context is cancelled (SIGINT), or the plan sets safe_on_exit. Paths in the
// plan (capture outputs) are relative to baseDir.
func (a *App) Apply(ctx context.Context, p *Plan, baseDir string) (res PlanResult, err error) {
	// Limits are validated by the steps that can energize an output
	// (CheckGenLimits), so a plan of only safe steps still runs.
	b, err := a.bench()
	if err != nil {
		return PlanResult{}, err
	}
	res = PlanResult{Name: p.Name, OK: true}
	if p.verifyWiring() {
		rep, werr := a.WiringVerify(ctx, p.Wiring)
		res.Wiring = &rep
		if werr != nil {
			res.OK = false
			return res, fmt.Errorf("wiring check before step 1: %w", werr)
		}
	}
	c := a.conns()
	defer c.closeAll()
	defer func() {
		if err != nil || ctx.Err() != nil || p.SafeOnExit {
			sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			sr, serr := a.safeWith(sctx, c, b)
			res.Safe = sr
			if serr != nil {
				a.warnf("safe: %v", serr)
			}
		}
	}()
	for i, step := range p.Steps {
		var action string
		var node yaml.Node
		for k, v := range step {
			action, node = k, v
		}
		sr := StepResult{Step: i + 1, Action: action}
		if ctx.Err() != nil {
			sr.Error = ctx.Err().Error()
			res.Steps = append(res.Steps, sr)
			res.OK = false
			return res, domain.Instrumentf("plan interrupted at step %d: %v", i+1, ctx.Err())
		}
		out, asserts, serr := a.runStep(ctx, c, b, action, &node, baseDir)
		sr.Result, sr.Assertions = out, asserts
		sr.OK = serr == nil
		for _, as := range asserts {
			if !as.OK {
				sr.OK = false
				res.Failed++
			}
		}
		if serr != nil {
			sr.Error = serr.Error()
			res.Steps = append(res.Steps, sr)
			res.OK = false
			return res, fmt.Errorf("step %d (%s): %w", i+1, action, serr)
		}
		res.Steps = append(res.Steps, sr)
	}
	if res.Failed > 0 {
		res.OK = false
		return res, domain.Verifyf("%d assertion(s) failed", res.Failed)
	}
	return res, nil
}

func (a *App) alias(given string, role domain.Role) (string, error) {
	return a.ResolveAlias(given, role)
}

func (a *App) runStep(ctx context.Context, c *conns, b *domain.Bench, action string, n *yaml.Node, baseDir string) (any, []Assertion, error) {
	switch action {
	case "wait":
		var s string
		if err := n.Decode(&s); err != nil {
			return nil, nil, domain.Usagef("wait: %v", err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, nil, domain.Usagef("wait: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(d):
		}
		return map[string]string{"waited": d.String()}, nil, nil

	case "safe":
		r, err := a.safeWith(ctx, c, b)
		return r, nil, err

	case "fg.set":
		var s stepFGSet
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("fg.set: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleGenerator)
		if err != nil {
			return nil, nil, err
		}
		w, err := WaveSpecFromStrings(s.Wave, s.Freq, s.Amp, s.Offset, s.Phase, s.Duty)
		if err != nil {
			return nil, nil, err
		}
		g, err := c.generator(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		cur, err := g.State(ctx, s.Ch)
		if err != nil {
			return nil, nil, err
		}
		if err := CheckGenLimits(b, alias, w, cur); err != nil {
			return nil, nil, err
		}
		if s.Load != "" {
			if err := g.SetLoad(ctx, s.Ch, s.Load); err != nil {
				return nil, nil, err
			}
		}
		if err := g.SetWave(ctx, s.Ch, w); err != nil {
			return nil, nil, err
		}
		st, err := g.State(ctx, s.Ch)
		st.Raw = nil
		return st, nil, err

	case "fg.output":
		var s stepFGOutput
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("fg.output: %v", err)
		}
		on, err := ParseOnOff(s.State)
		if err != nil {
			return nil, nil, err
		}
		alias, err := a.alias(s.Alias, domain.RoleGenerator)
		if err != nil {
			return nil, nil, err
		}
		g, err := c.generator(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		if err := switchOutput(ctx, b, g, alias, s.Ch, on); err != nil {
			return nil, nil, err
		}
		return map[string]any{"alias": alias, "ch": s.Ch, "output": on}, nil, nil

	case "scope.setup":
		var s stepScopeSetup
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("scope.setup: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		cs, err := ChannelSetupFromStrings(s.VDiv, s.Offset, s.Coupling, s.Probe, s.On, s.BWLimit)
		if err != nil {
			return nil, nil, err
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		if err := sc.SetupChannel(ctx, s.Ch, cs); err != nil {
			return nil, nil, err
		}
		st, err := sc.ChannelState(ctx, s.Ch)
		return st, nil, err

	case "scope.timebase":
		var s stepTimebase
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("scope.timebase: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		td, err := optQ(s.TDiv, "s")
		if err != nil {
			return nil, nil, domain.Usagef("tdiv: %v", err)
		}
		dl, err := optQ(s.Delay, "s")
		if err != nil {
			return nil, nil, domain.Usagef("delay: %v", err)
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		if err := sc.SetTimebase(ctx, td, dl); err != nil {
			return nil, nil, err
		}
		tb, err := sc.Timebase(ctx)
		return tb, nil, err

	case "scope.trigger":
		var s stepTrigger
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("scope.trigger: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		lvl, err := optQ(s.Level, "V")
		if err != nil {
			return nil, nil, domain.Usagef("level: %v", err)
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		if err := sc.SetTrigger(ctx, domain.TriggerSetup{Source: s.Source, Mode: s.Mode, Slope: s.Slope, Level: lvl}); err != nil {
			return nil, nil, err
		}
		t, err := sc.Trigger(ctx)
		return t, nil, err

	case "scope.autoset":
		var st struct {
			Alias string `yaml:"alias"`
		}
		if err := decodeStep(n, &st); err != nil {
			return nil, nil, domain.Usagef("scope.autoset: %v", err)
		}
		alias, err := a.alias(st.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		changed, err := sc.Autoset(ctx)
		return map[string]bool{"changed": changed}, nil, err

	case "scope.measure":
		var s stepMeasure
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("scope.measure: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		params := s.Params
		for k := range s.Expect {
			found := false
			for _, p := range params {
				found = found || strings.EqualFold(p, k)
			}
			if !found {
				params = append(params, k)
			}
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		m, err := measure(ctx, sc, alias, s.Ch, params)
		if err != nil {
			return nil, nil, err
		}
		var asserts []Assertion
		for k, e := range s.Expect {
			exp, err := ParseExpectation(e)
			if err != nil {
				return m, nil, domain.Usagef("expect %s: %v", k, err)
			}
			v, _ := m.Value(strings.ToLower(k))
			asserts = append(asserts, exp.Check(k, v))
		}
		return m, asserts, nil

	case "scope.capture":
		var s stepCapture
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("scope.capture: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleScope)
		if err != nil {
			return nil, nil, err
		}
		sc, err := c.scope(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		points := s.Points
		if points == 0 {
			points = 10000
		}
		wf, err := sc.Capture(ctx, s.Ch, points)
		if err != nil {
			return nil, nil, err
		}
		sum := Summarize(wf)
		if s.Out != "" {
			path := s.Out
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			if err := WriteWaveformFile(path, wf); err != nil {
				return sum, nil, err
			}
			sum.Files = []string{path}
		}
		var asserts []Assertion
		vals := map[string]float64{"fft_peak_hz": sum.PeakFreqHz, "vmin": sum.Min, "vmax": sum.Max, "vmean": sum.Mean}
		for k, e := range s.Expect {
			exp, err := ParseExpectation(e)
			if err != nil {
				return sum, nil, domain.Usagef("expect %s: %v", k, err)
			}
			v, ok := vals[k]
			if !ok {
				return sum, nil, domain.Usagef("capture expect: unknown key %q (fft_peak_hz, vmin, vmax, vmean)", k)
			}
			asserts = append(asserts, exp.Check(k, &v))
		}
		return sum, asserts, nil

	case "dmm.config":
		var s stepDMMConfig
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("dmm.config: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleDMM)
		if err != nil {
			return nil, nil, err
		}
		d, err := c.dmm(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		if err := d.Configure(ctx, domain.DMMConfig{Function: domain.DMMFunction(strings.ToLower(s.Function)), Range: s.Range}); err != nil {
			return nil, nil, err
		}
		cfg, err := d.Config(ctx)
		return cfg, nil, err

	case "dmm.read":
		var s stepDMMRead
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("dmm.read: %v", err)
		}
		alias, err := a.alias(s.Alias, domain.RoleDMM)
		if err != nil {
			return nil, nil, err
		}
		var iv time.Duration
		if s.Interval != "" {
			if iv, err = time.ParseDuration(s.Interval); err != nil {
				return nil, nil, domain.Usagef("interval: %v", err)
			}
		}
		d, err := c.dmm(ctx, alias)
		if err != nil {
			return nil, nil, err
		}
		r, err := a.dmmRead(ctx, d, s.Samples, iv)
		if err != nil {
			return nil, nil, err
		}
		var asserts []Assertion
		if s.Expect != "" {
			exp, err := ParseExpectation(s.Expect)
			if err != nil {
				return r, nil, domain.Usagef("expect: %v", err)
			}
			asserts = append(asserts, exp.Check("mean", &r.Mean))
		}
		return r, asserts, nil

	case "raw":
		var s stepRaw
		if err := decodeStep(n, &s); err != nil {
			return nil, nil, domain.Usagef("raw: %v", err)
		}
		if s.Alias == "" || s.Cmd == "" {
			return nil, nil, domain.Usagef("raw: alias and cmd are required")
		}
		var to time.Duration
		if s.Timeout != "" {
			var err error
			if to, err = time.ParseDuration(s.Timeout); err != nil {
				return nil, nil, domain.Usagef("raw timeout: %v", err)
			}
		}
		inst, err := c.get(ctx, s.Alias)
		if err != nil {
			return nil, nil, err
		}
		r, err := a.rawOn(ctx, inst.SCPI(), s.Cmd, RawOptions{OPC: s.OPC, Timeout: to})
		return r, nil, err
	}
	return nil, nil, domain.Usagef("unknown action %q (fg.set, fg.output, scope.setup, scope.timebase, scope.trigger, scope.autoset, scope.measure, scope.capture, dmm.config, dmm.read, raw, wait, safe)", action)
}

// WriteWaveformFile writes a capture as CSV or JSON depending on the extension.
func WriteWaveformFile(path string, wf domain.Waveform) error {
	var write func(io.Writer, domain.Waveform) error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		write = WriteWaveformJSON
	case ".csv", "":
		write = WriteWaveformCSV
	default:
		return domain.Usagef("unsupported capture format %q (use .csv or .json)", filepath.Ext(path))
	}
	f, err := os.Create(path)
	if err != nil {
		return domain.Usagef("create %s: %v", path, err)
	}
	if err := write(f, wf); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ParseOnOff parses on/off/true/false/1/0.
func ParseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, domain.Usagef("expected on or off, got %q", s)
}

// WaveSpecFromStrings builds a WaveSpec from CLI/plan strings ("" = unchanged).
// amp accepts Vpp (default) or Vrms ("1.414Vrms").
func WaveSpecFromStrings(wave, freq, amp, offset, phase, duty string) (domain.WaveSpec, error) {
	w := domain.WaveSpec{Type: wave}
	var err error
	if w.Freq, err = optQ(freq, "Hz"); err != nil {
		return w, domain.Usagef("freq: %v", err)
	}
	if amp != "" {
		v, unit, err := domain.ParseIn(amp, "V", "Vpp", "Vrms")
		if err != nil {
			return w, domain.Usagef("amp: %v", err)
		}
		if unit == "Vrms" {
			w.AmpRMS = &v
		} else {
			w.AmpVpp = &v
		}
	}
	if w.Offset, err = optQ(offset, "V"); err != nil {
		return w, domain.Usagef("offset: %v", err)
	}
	if w.Phase, err = optQ(phase, "deg"); err != nil {
		return w, domain.Usagef("phase: %v", err)
	}
	if w.Duty, err = optQ(duty, "%"); err != nil {
		return w, domain.Usagef("duty: %v", err)
	}
	return w, nil
}

// ChannelSetupFromStrings builds a ChannelSetup from CLI/plan strings ("" = unchanged).
// probe accepts "10", "10x" or "10X".
func ChannelSetupFromStrings(vdiv, offset, coupling, probe string, on, bwl *bool) (domain.ChannelSetup, error) {
	cs := domain.ChannelSetup{Coupling: coupling, On: on, BWLimit: bwl}
	var err error
	if cs.VDiv, err = optQ(vdiv, "V"); err != nil {
		return cs, domain.Usagef("vdiv: %v", err)
	}
	if cs.Offset, err = optQ(offset, "V"); err != nil {
		return cs, domain.Usagef("offset: %v", err)
	}
	if p := strings.TrimRight(strings.TrimSpace(probe), "xX"); p != "" {
		if cs.Probe, err = optQ(p); err != nil {
			return cs, domain.Usagef("probe: %v", err)
		}
	}
	return cs, nil
}
