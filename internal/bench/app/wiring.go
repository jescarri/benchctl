package app

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// WiringStep is one human instruction.
type WiringStep struct {
	Step        int    `json:"step"`
	From        string `json:"from"`
	To          string `json:"to"`
	Instruction string `json:"instruction"`
}

// WiringShow renders links as step-by-step connection instructions for the
// human. links == nil means the inventory wiring.
func (a *App) WiringShow(links []domain.Link) ([]WiringStep, error) {
	b, err := a.bench()
	if err != nil {
		return nil, err
	}
	if links == nil {
		links = b.Wiring
	}
	if len(links) == 0 {
		return nil, domain.Usagef("no wiring given and none in %s", a.Store.Path())
	}
	var out []WiringStep
	for i, l := range links {
		out = append(out, WiringStep{Step: i + 1, From: l.From, To: l.To, Instruction: a.describeLink(b, l)})
	}
	return out, nil
}

func (a *App) describeLink(b *domain.Bench, l domain.Link) string {
	from, to := domain.ParseEndpoint(l.From), domain.ParseEndpoint(l.To)
	name := func(e domain.Endpoint) string {
		ref, ok := b.Instruments[e.Alias]
		if !ok {
			return strings.TrimSpace(e.Alias + " " + e.Port)
		}
		return strings.TrimSpace(fmt.Sprintf("%s (%s) %s", e.Alias, ref.Model, e.Port))
	}
	s := fmt.Sprintf("Connect %s → %s", name(from), name(to))
	fr, fok := b.Instruments[from.Alias]
	tr, tok := b.Instruments[to.Alias]
	switch {
	case fok && tok && a.roleOf(fr.Model) == domain.RoleGenerator && a.roleOf(tr.Model) == domain.RoleScope:
		s += " with a BNC-BNC coax cable (a probe works too: set its switch to 1X, or set the scope channel's probe factor to 10X to match)"
	case tok && a.roleOf(tr.Model) == domain.RoleDMM:
		s += " using the DMM's V/Ω (HI) and COM (LO) inputs"
	}
	if l.Expect != nil {
		s += fmt.Sprintf("; expect %s ≈ %g ± %s", l.Expect.Function, l.Expect.Value, l.Expect.Tolerance)
	}
	if l.Note != "" {
		s += " (" + l.Note + ")"
	}
	return s
}

// LinkCheck is the verification result of one link.
type LinkCheck struct {
	From     string `json:"from"`
	To       string `json:"to"`
	OK       bool   `json:"ok"`
	Method   string `json:"method"`
	Observed string `json:"observed"`
	Problem  string `json:"problem,omitempty"`
}

// WiringReport is the result of WiringVerify.
type WiringReport struct {
	OK    bool        `json:"ok"`
	Links []LinkCheck `json:"links"`
	// Unchecked lists links with no electrical check (e.g. to an external
	// device, or a DMM link without "expect"). They never count as verified.
	Unchecked []LinkCheck `json:"unchecked,omitempty"`
	Restored  []string    `json:"restored,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
}

// Test signal parameters for generator → scope checks: a distinctive
// frequency per generator channel, 1 Vpp into HiZ.
var (
	testFreqs     = []float64{1000, 3300, 7700, 13000}
	testWaves     = []string{"sine", "square", "sine", "square"}
	testAmp       = 1.0
	testVDiv      = 0.2
	testTDiv      = 1e-3
	freqTolerance = 0.02
	signalSettle  = 1500 * time.Millisecond
	dcTestLevel   = 1.0
	// minSignalVpp is the noise floor below which a frequency lock is ignored.
	minSignalVpp = 0.01
	// dominantRatio: among channels that lock onto the test frequency, only
	// those within this fraction of the strongest count as "seeing" it, so
	// crosstalk is not mistaken for a connection while a weak (attenuated)
	// but real path still is.
	dominantRatio = 0.3
	// ampLow/ampHigh bound the accepted amplitude ratio (measured/set). A 2×
	// or ½ ratio is the classic 50 Ω / HiZ load mismatch.
	ampLow, ampHigh = 0.8, 1.25
	// clipDivs: a pkpk reading at or above this many divisions is clipped
	// (the SDS1000X-E ADC spans about ten; a clipped 10 Vpp read 1.99 V at
	// 200 mV/div).
	clipDivs = 9.0
	// edgesOnlyRatio: a square wave whose top-to-base amplitude is below
	// this fraction of its pkpk is only its edges (capacitive coupling).
	edgesOnlyRatio = 0.3
)

// WiringVerify electrically checks every link in the inventory:
//   - generator → scope: drives a distinctive signal on each generator channel
//     in turn and checks which scope channels see it (detects swapped/missing cables);
//   - generator → DMM: drives a known DC level and checks the DMM reading;
//   - anything → DMM with an `expect`: checks the reading (e.g. an external PSU).
//
// Generator outputs are always left OFF afterwards; generator wave settings
// and scope channel/timebase/trigger settings are restored.
//
// links == nil means the inventory wiring.
func (a *App) WiringVerify(ctx context.Context, links []domain.Link) (rep WiringReport, err error) {
	b, err := a.benchForOutputs()
	if err != nil {
		return WiringReport{}, err
	}
	if links == nil {
		links = b.Wiring
	}
	if len(links) == 0 {
		return WiringReport{}, domain.Usagef("no wiring given and none in %s", a.Store.Path())
	}
	c := a.conns()
	defer c.closeAll()
	rep = WiringReport{OK: true}
	// Registered first so it runs last: any error, from link validation
	// onwards or from the output shutdown below, marks the report failed.
	defer func() {
		if err != nil {
			rep.OK = false
		}
	}()

	var fs []genScopeLink
	var dmmLinks []domain.Link
	for _, l := range links {
		from, to := domain.ParseEndpoint(l.From), domain.ParseEndpoint(l.To)
		fr, fok := b.Instruments[from.Alias]
		tr, tok := b.Instruments[to.Alias]
		switch {
		case !fok && !externalAlias(from.Alias):
			// An unknown source would be treated as undriven, so a stray
			// voltage matching expect could pass a link that does not exist.
			return rep, domain.Usagef("link %s → %s: unknown instrument %q (external sources use %s)",
				l.From, l.To, from.Alias, strings.Join(externalAliases, ", "))
		case fok && tok && a.roleOf(fr.Model) == domain.RoleGenerator && a.roleOf(tr.Model) == domain.RoleScope:
			fch, e1 := from.ChannelNumber()
			sch, e2 := to.ChannelNumber()
			if e1 != nil || e2 != nil {
				return rep, domain.Usagef("link %s → %s: channel numbers required", l.From, l.To)
			}
			fs = append(fs, genScopeLink{link: l, fg: from.Alias, scope: to.Alias, fgCh: fch, scopeCh: sch})
		case tok && a.roleOf(tr.Model) == domain.RoleDMM:
			dmmLinks = append(dmmLinks, l)
		case !tok && !externalAlias(to.Alias):
			// Every checkable destination is an inventory instrument, so an
			// unknown one is a typo, not a link to pass unchecked.
			return rep, domain.Usagef("link %s → %s: unknown instrument %q (external destinations use %s)",
				l.From, l.To, to.Alias, strings.Join(externalAliases, ", "))
		default:
			rep.Unchecked = append(rep.Unchecked, LinkCheck{From: l.From, To: l.To, Method: methodNone,
				Observed: "no electrical check available for this link type"})
		}
	}

	// Safety first: on any exit, every generator output involved goes off.
	gens := map[string]port.Generator{}
	defer func() {
		octx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, alias := range slices.Sorted(maps.Keys(gens)) {
			if offErr := gens[alias].(port.Outputs).AllOutputsOff(octx); offErr != nil {
				// An output that may still be on fails the run, whatever the links said.
				msg := fmt.Sprintf("could not switch off %s outputs: %v", alias, offErr)
				a.warnf("%s", msg)
				rep.Warnings = append(rep.Warnings, msg)
				rep.OK = false
				if err == nil {
					err = domain.Instrumentf("wiring verify: could not switch off %s outputs: %v", alias, offErr)
				}
			}
		}
	}()
	getGen := func(alias string) (port.Generator, error) {
		if g, ok := gens[alias]; ok {
			return g, nil
		}
		g, err := c.generator(ctx, alias)
		if err != nil {
			return nil, err
		}
		if _, ok := g.(port.Outputs); !ok {
			return nil, domain.Usagef("%s has no output control", alias)
		}
		gens[alias] = g
		return g, nil
	}

	if len(fs) > 0 {
		checks, restored, err := a.verifyGenScope(ctx, c, b, getGen, fs)
		rep.Restored = append(rep.Restored, restored...)
		if err != nil {
			return rep, err
		}
		rep.Links = append(rep.Links, checks...)
	}
	for _, l := range dmmLinks {
		chk, err := a.verifyDMMLink(ctx, c, b, getGen, l)
		if err != nil {
			return rep, err
		}
		if chk.Method == methodNone {
			rep.Unchecked = append(rep.Unchecked, chk)
			continue
		}
		rep.Links = append(rep.Links, chk)
	}
	for _, l := range rep.Unchecked {
		a.warnf("wiring: %s → %s not verified: %s", l.From, l.To, l.Observed)
	}
	if len(rep.Links) == 0 {
		rep.OK = false
		return rep, domain.Verifyf("wiring verification failed: none of the %d links can be checked electrically", len(rep.Unchecked))
	}
	for _, l := range rep.Links {
		rep.OK = rep.OK && l.OK
	}
	if !rep.OK {
		var bad []string
		for _, l := range rep.Links {
			if !l.OK {
				bad = append(bad, fmt.Sprintf("%s → %s: %s", l.From, l.To, l.Problem))
			}
		}
		return rep, domain.Verifyf("wiring verification failed: %s", strings.Join(bad, "; "))
	}
	return rep, nil
}

// methodNone marks a link that was not electrically checked.
const methodNone = "none"

// externalAliases are endpoint aliases for things outside the inventory
// (external supplies, the device under test). Links to them are shown but
// cannot be verified.
var externalAliases = []string{"ext", "dut"}

func externalAlias(alias string) bool {
	for _, e := range externalAliases {
		if alias == e {
			return true
		}
	}
	return false
}

type genScopeLink struct {
	link          domain.Link
	fg, scope     string
	fgCh, scopeCh int
}

type genGetter func(alias string) (port.Generator, error)

type chanKey struct {
	alias string
	ch    int
}

func (k chanKey) String() string { return fmt.Sprintf("%s:C%d", k.alias, k.ch) }

// restorableWave converts a saved generator state back into a WaveSpec.
func restorableWave(st domain.GenChannelState) domain.WaveSpec {
	w := domain.WaveSpec{Type: st.Wave}
	off := st.Offset
	switch st.Wave {
	case "sine", "square", "ramp", "pulse", "arb":
		f, amp := st.Freq, st.AmpVpp
		w.Freq, w.AmpVpp, w.Offset = &f, &amp, &off
	case "dc":
		w.Offset = &off
	}
	return w
}

func (a *App) verifyGenScope(ctx context.Context, c *conns, b *domain.Bench, getGen genGetter, links []genScopeLink) (checks []LinkCheck, restored []string, err error) {
	var gensUsed, scopesUsed []chanKey
	seenG, seenS := map[chanKey]bool{}, map[chanKey]bool{}
	for _, l := range links {
		g, s := chanKey{l.fg, l.fgCh}, chanKey{l.scope, l.scopeCh}
		if !seenG[g] {
			seenG[g] = true
			gensUsed = append(gensUsed, g)
		}
		if !seenS[s] {
			seenS[s] = true
			scopesUsed = append(scopesUsed, s)
		}
	}

	// Save state.
	savedGen := map[chanKey]domain.GenChannelState{}
	for _, g := range gensUsed {
		gen, err := getGen(g.alias)
		if err != nil {
			return nil, nil, err
		}
		st, err := gen.State(ctx, g.ch)
		if err != nil {
			return nil, nil, err
		}
		savedGen[g] = st
	}
	// The test signals are small (1 Vpp, no offset) but still must respect
	// the bench limits; check them all before any setting changes.
	for i, g := range gensUsed {
		f, amp, zero := testFreqs[i%len(testFreqs)], testAmp, 0.0
		w := domain.WaveSpec{Type: testWaves[i%len(testWaves)], Freq: &f, AmpVpp: &amp, Offset: &zero}
		if err := CheckGenLimits(b, g.alias, w, savedGen[g]); err != nil {
			return nil, nil, domain.Usagef("wiring test signal on %s:CH%d: %v", g.alias, g.ch, err)
		}
	}
	savedCh := map[chanKey]domain.ChannelState{}
	savedTB := map[string]domain.Timebase{}
	savedTrig := map[string]domain.TriggerState{}
	for _, s := range scopesUsed {
		sc, err := c.scope(ctx, s.alias)
		if err != nil {
			return nil, nil, err
		}
		cs, err := sc.ChannelState(ctx, s.ch)
		if err != nil {
			return nil, nil, err
		}
		savedCh[s] = cs
		if _, ok := savedTB[s.alias]; !ok {
			if savedTB[s.alias], err = sc.Timebase(ctx); err != nil {
				return nil, nil, err
			}
			if savedTrig[s.alias], err = sc.Trigger(ctx); err != nil {
				return nil, nil, err
			}
		}
	}

	// Restore on exit (outputs are switched off by the caller's defer).
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, g := range gensUsed {
			gen, _ := getGen(g.alias)
			st := savedGen[g]
			_ = gen.Output(rctx, g.ch, false)
			if e := gen.SetLoad(rctx, g.ch, st.Load); e != nil {
				a.warnf("restore %s:CH%d load: %v", g.alias, g.ch, e)
			}
			if e := gen.SetWave(rctx, g.ch, restorableWave(st)); e != nil {
				a.warnf("restore %s:CH%d wave: %v", g.alias, g.ch, e)
				continue
			}
			restored = append(restored, fmt.Sprintf("%s:CH%d wave/load (output left OFF)", g.alias, g.ch))
		}
		for _, s := range scopesUsed {
			sc, _ := c.scope(rctx, s.alias)
			cs := savedCh[s]
			on := cs.On
			if e := sc.SetupChannel(rctx, s.ch, domain.ChannelSetup{VDiv: &cs.VDiv, Offset: &cs.Offset, On: &on}); e != nil {
				a.warnf("restore %s: %v", s, e)
				continue
			}
			restored = append(restored, s.String()+" vdiv/offset/trace")
		}
		for alias, tb := range savedTB {
			sc, _ := c.scope(rctx, alias)
			td := tb.TDiv
			if e := sc.SetTimebase(rctx, &td, nil); e != nil {
				a.warnf("restore %s timebase: %v", alias, e)
			}
			tr := savedTrig[alias]
			lvl := tr.Level
			if e := sc.SetTrigger(rctx, domain.TriggerSetup{Source: tr.Source, Level: &lvl, Mode: tr.Mode}); e != nil {
				a.warnf("restore %s trigger: %v", alias, e)
			}
			restored = append(restored, alias+" timebase/trigger")
		}
	}()

	// Prepare: outputs off, HiZ load, scope channels on at a known scale.
	for _, g := range gensUsed {
		gen, _ := getGen(g.alias)
		if err := gen.Output(ctx, g.ch, false); err != nil {
			return nil, nil, err
		}
		if err := gen.SetLoad(ctx, g.ch, "hiz"); err != nil {
			return nil, nil, err
		}
	}
	tdiv, zero, on := testTDiv, 0.0, true
	vdiv := testVDiv
	prepared := map[string]bool{}
	for _, s := range scopesUsed {
		sc, err := c.scope(ctx, s.alias)
		if err != nil {
			return nil, nil, err
		}
		if err := sc.SetupChannel(ctx, s.ch, domain.ChannelSetup{On: &on, VDiv: &vdiv, Offset: &zero}); err != nil {
			return nil, nil, err
		}
		if !prepared[s.alias] {
			prepared[s.alias] = true
			if err := sc.SetTimebase(ctx, &tdiv, nil); err != nil {
				return nil, nil, err
			}
			if err := sc.SetTrigger(ctx, domain.TriggerSetup{Mode: "auto"}); err != nil {
				return nil, nil, err
			}
		}
	}

	// Drive each generator channel alone and see where its signal shows up.
	seenOn := map[chanKey][]chanKey{} // generator → scope channels that saw it
	observed := map[chanKey]map[chanKey]obs{}
	for i, g := range gensUsed {
		gen, _ := getGen(g.alias)
		f, amp := testFreqs[i%len(testFreqs)], testAmp
		if err := gen.SetWave(ctx, g.ch, domain.WaveSpec{Type: testWaves[i%len(testWaves)], Freq: &f, AmpVpp: &amp, Offset: &zero}); err != nil {
			return nil, nil, err
		}
		// Trigger on the channel this generator should reach, so the scope locks on.
		for _, l := range links {
			if l.fg == g.alias && l.fgCh == g.ch {
				sc, _ := c.scope(ctx, l.scope)
				if err := sc.SetTrigger(ctx, domain.TriggerSetup{Source: fmt.Sprintf("C%d", l.scopeCh), Level: &zero}); err != nil {
					return nil, nil, err
				}
				break
			}
		}
		if err := gen.Output(ctx, g.ch, true); err != nil {
			return nil, nil, err
		}
		a.sleep(signalSettle)
		observed[g] = map[chanKey]obs{}
		for _, s := range scopesUsed {
			sc, _ := c.scope(ctx, s.alias)
			ms, err := sc.Measure(ctx, s.ch, []string{"freq", "pkpk", "ampl"})
			if err != nil {
				_ = gen.Output(ctx, g.ch, false)
				return nil, nil, err
			}
			observed[g][s] = obs{freq: ms[0].Value, pkpk: ms[1].Value, ampl: ms[2].Value}
		}
		seenOn[g] = dominantMatches(observed[g], scopesUsed, f)
		if err := a.rescan(ctx, c, links, g, f, observed[g]); err != nil {
			_ = gen.Output(ctx, g.ch, false)
			return nil, nil, err
		}
		seenOn[g] = dominantMatches(observed[g], scopesUsed, f)
		if err := gen.Output(ctx, g.ch, false); err != nil {
			return nil, nil, err
		}
	}

	fmtObs := func(o obs) string {
		v := func(p *float64, unit string) string {
			if p == nil {
				return "****"
			}
			return fmt.Sprintf("%.4g%s", *p, unit)
		}
		return fmt.Sprintf("freq=%s pkpk=%s", v(o.freq, "Hz"), v(o.pkpk, "V"))
	}
	for i, l := range links {
		g, s := chanKey{l.fg, l.fgCh}, chanKey{l.scope, l.scopeCh}
		idx := 0
		for j, gg := range gensUsed {
			if gg == g {
				idx = j
			}
		}
		_ = i
		f := testFreqs[idx%len(testFreqs)]
		chk := LinkCheck{From: l.link.From, To: l.link.To,
			Method:   fmt.Sprintf("%s %gHz %gVpp on %s:CH%d, measured on %s", testWaves[idx%len(testWaves)], f, testAmp, g.alias, g.ch, s),
			Observed: fmtObs(observed[g][s])}
		hit := false
		var elsewhere []string
		for _, x := range seenOn[g] {
			if x == s {
				hit = true
			} else {
				elsewhere = append(elsewhere, x.String())
			}
		}
		switch {
		case hit:
			chk.OK = true
			if len(elsewhere) > 0 {
				chk.Observed += "; also seen on " + strings.Join(elsewhere, ", ")
			}
			if o := observed[g][s]; testWaves[idx%len(testWaves)] == "square" && edgesOnly(o) {
				chk.OK = false
				chk.Problem = fmt.Sprintf("only the square wave's edges arrive (top-to-base %.3g V, pkpk %.3g V): the path is capacitively coupled; probe tip, hook, ground or BNC not making contact?", *o.ampl, *o.pkpk)
			} else if o.pkpk != nil {
				if p := AmplitudeProblem(testAmp, *o.pkpk, savedCh[s].Probe); p != "" {
					chk.OK = false
					chk.Problem = fmt.Sprintf("connected (frequency matches) but %s", p)
				}
			}
		case len(elsewhere) > 0:
			chk.Problem = fmt.Sprintf("signal from %s:CH%d arrives on %s instead of %s (cables swapped?)", g.alias, g.ch, strings.Join(elsewhere, ", "), s)
		default:
			chk.Problem = fmt.Sprintf("signal from %s:CH%d not seen on any checked scope channel (cable missing or loose? a probe switched to 10X also shrinks it ×0.1)", g.alias, g.ch)
		}
		checks = append(checks, chk)
	}
	return checks, restored, nil
}

func (a *App) verifyDMMLink(ctx context.Context, c *conns, b *domain.Bench, getGen genGetter, l domain.Link) (LinkCheck, error) {
	from, to := domain.ParseEndpoint(l.From), domain.ParseEndpoint(l.To)
	chk := LinkCheck{From: l.From, To: l.To}
	d, err := c.dmm(ctx, to.Alias)
	if err != nil {
		return chk, err
	}
	exp := l.Expect
	fr, isInst := b.Instruments[from.Alias]
	drive := isInst && a.roleOf(fr.Model) == domain.RoleGenerator
	if drive && exp == nil {
		exp = &domain.Expect{Function: "vdc", Value: dcTestLevel, Tolerance: "5%"}
	}
	if exp == nil {
		chk.Method = methodNone
		chk.Observed = "no expectation configured; add \"expect\" to the link to verify it"
		return chk, nil
	}
	tol, err := domain.ParseTolerance(exp.Tolerance)
	if err != nil {
		return chk, domain.Usagef("link %s → %s: %v", l.From, l.To, err)
	}
	fn := domain.DMMFunction(strings.ToLower(exp.Function))
	if _, ok := domain.DMMFunctions[fn]; !ok {
		return chk, domain.Usagef("link %s → %s: unknown DMM function %q", l.From, l.To, exp.Function)
	}

	if drive {
		// The generator drives exp.Value as a DC level, which only means
		// something for a DC voltage reading.
		if fn != "vdc" {
			return chk, domain.Usagef("link %s → %s: a generator → DMM link can only be checked with function vdc, not %s", l.From, l.To, fn)
		}
		ch, err := from.ChannelNumber()
		if err != nil {
			return chk, domain.Usagef("link %s → %s: %v", l.From, l.To, err)
		}
		gen, err := getGen(from.Alias)
		if err != nil {
			return chk, err
		}
		saved, err := gen.State(ctx, ch)
		if err != nil {
			return chk, err
		}
		lvl := exp.Value
		if err := CheckGenLimits(b, from.Alias, domain.WaveSpec{Type: "dc", Offset: &lvl}, saved); err != nil {
			return chk, domain.Usagef("link %s → %s: DC test level: %v", l.From, l.To, err)
		}
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = gen.Output(rctx, ch, false)
			_ = gen.SetLoad(rctx, ch, saved.Load)
			if e := gen.SetWave(rctx, ch, restorableWave(saved)); e != nil {
				a.warnf("restore %s:CH%d: %v", from.Alias, ch, e)
			}
		}()
		if err := gen.SetLoad(ctx, ch, "hiz"); err != nil {
			return chk, err
		}
		if err := gen.SetWave(ctx, ch, domain.WaveSpec{Type: "dc", Offset: &lvl}); err != nil {
			return chk, err
		}
		if err := gen.Output(ctx, ch, true); err != nil {
			return chk, err
		}
		a.sleep(signalSettle)
		chk.Method = fmt.Sprintf("DC %gV from %s:CH%d, read as %s", lvl, from.Alias, ch, fn)
	} else {
		chk.Method = fmt.Sprintf("read %s on %s, expect %g ± %s", fn, to.Alias, exp.Value, tol)
	}

	if err := d.Configure(ctx, domain.DMMConfig{Function: fn, Range: "auto"}); err != nil {
		return chk, err
	}
	r, err := a.dmmRead(ctx, d, 3, 200*time.Millisecond)
	if err != nil {
		return chk, err
	}
	chk.Observed = fmt.Sprintf("mean %.6g%s over %d readings (min %.6g, max %.6g)", r.Mean, r.Unit, len(r.Values), r.Min, r.Max)
	if tol.Within(exp.Value, r.Mean) {
		chk.OK = true
	} else {
		chk.Problem = fmt.Sprintf("read %.6g%s, expected %g ± %s (source off or not connected?)", r.Mean, r.Unit, exp.Value, tol)
	}
	return chk, nil
}

// rescan looks again at the scope channels generator g should reach when the
// test scale could not show its signal properly:
//   - frequency not found: a probe switched to 10X (or an attenuator) leaves
//     the test signal at half a division, where the scope cannot measure its
//     frequency, so look at a 10× more sensitive scale;
//   - clipped (pkpk near full scale): a channel probe factor of 10X with a 1X
//     probe or cable shows the signal 10× too large, so look at a 10× coarser
//     scale.
//
// A reading found this way replaces the first one, so the link is reported
// with its real amplitude ratio instead of as missing or 50 Ω-mismatched.
func (a *App) rescan(ctx context.Context, c *conns, links []genScopeLink, g chanKey, f float64, seen map[chanKey]obs) error {
	normal := testVDiv
	for _, l := range links {
		s := chanKey{l.scope, l.scopeCh}
		if l.fg != g.alias || l.fgCh != g.ch {
			continue
		}
		o := seen[s]
		freqOK := o.freq != nil && math.Abs(*o.freq-f) <= f*freqTolerance
		var scale float64
		switch {
		case !freqOK:
			scale = testVDiv / 10
		case o.pkpk != nil && *o.pkpk >= clipDivs*testVDiv:
			scale = testVDiv * 10
		default:
			continue
		}
		sc, err := c.scope(ctx, s.alias)
		if err != nil {
			return err
		}
		if err := sc.SetupChannel(ctx, s.ch, domain.ChannelSetup{VDiv: &scale}); err != nil {
			return err
		}
		a.sleep(signalSettle)
		ms, err := sc.Measure(ctx, s.ch, []string{"freq", "pkpk", "ampl"})
		if err != nil {
			return err
		}
		if err := sc.SetupChannel(ctx, s.ch, domain.ChannelSetup{VDiv: &normal}); err != nil {
			return err
		}
		if fr := ms[0].Value; fr != nil && math.Abs(*fr-f) <= f*freqTolerance {
			seen[s] = obs{freq: fr, pkpk: ms[1].Value, ampl: ms[2].Value}
		}
	}
	return nil
}

// AmplitudeProblem explains a measured/set amplitude ratio outside the
// accepted band, or returns "". probe is the scope channel's probe factor
// (0 if unknown).
func AmplitudeProblem(set, measured, probe float64) string {
	r := measured / set
	if probe <= 0 {
		probe = 1
	}
	switch {
	case r >= ampLow && r <= ampHigh:
		return ""
	case r > 0.06 && r < 0.16: // an uncompensated 10X probe on the square test wave read ×0.08
		return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): a probe with its switch at 10X on a channel set to %gX? Set the probe switch to 1X, or the channel's probe factor to %gX", measured, set, r, probe, probe*10)
	case r > 7 && r < 14 && probe >= 10:
		return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): channel probe factor is %gX but the probe/cable is %gX? Set the channel's probe factor to %gX, or the probe switch to %gX", measured, set, r, probe, probe/10, probe/10, probe)
	case r > 1.7 && r < 2.3:
		return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): generator load is 50 Ω into a 1 MΩ input?", measured, set, r)
	case r > 0.4 && r < 0.6:
		return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): 50 Ω termination or 50 Ω scope input with generator load HiZ?", measured, set, r)
	case r < ampLow:
		return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): attenuator, 10× probe, damaged cable or weak generator output in this path?", measured, set, r)
	}
	return fmt.Sprintf("amplitude is %.3g Vpp for %.3g Vpp set (×%.2f): probe attenuation setting wrong?", measured, set, r)
}

// edgesOnly reports a square test wave whose flat top and base are gone
// while its edges get through: the SDS shows decaying spikes when a probe tip
// only touches capacitively (live: pkpk 0.84 V, ampl 0.016 V).
func edgesOnly(o obs) bool {
	return o.pkpk != nil && o.ampl != nil && *o.pkpk >= minSignalVpp && *o.ampl < edgesOnlyRatio**o.pkpk
}

// obs is one scope channel's view of a test signal.
type obs struct {
	freq, pkpk, ampl *float64
}

// dominantMatches returns the scope channels that see the test signal at
// frequency f: the frequency must match, the amplitude must be above the
// noise floor, and within dominantRatio of the strongest matching channel.
func dominantMatches(seen map[chanKey]obs, order []chanKey, f float64) []chanKey {
	best := 0.0
	match := func(o obs) bool {
		return o.freq != nil && o.pkpk != nil && math.Abs(*o.freq-f) <= f*freqTolerance && *o.pkpk >= minSignalVpp
	}
	for _, s := range order {
		if o := seen[s]; match(o) && *o.pkpk > best {
			best = *o.pkpk
		}
	}
	var out []chanKey
	for _, s := range order {
		if o := seen[s]; match(o) && *o.pkpk >= best*dominantRatio {
			out = append(out, s)
		}
	}
	return out
}
