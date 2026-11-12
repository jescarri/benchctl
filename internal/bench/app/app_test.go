package app

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jescarri/benchctl/internal/bench/adapter/fake"
	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// rig builds the real bench topology: fg CH1→scope C1, fg CH2→scope C2,
// external 5 V into the DMM.
func rig(t *testing.T, cables []fake.Cable) (*App, *fake.Bench, *fake.Store, *bytes.Buffer) {
	t.Helper()
	b := fake.NewBench()
	b.AddGenerator("fg")
	b.AddScope("scope")
	b.AddDMM("dmm", 4.997)
	b.Cables = cables
	inv := b.Inventory()
	inv.Wiring = []domain.Link{
		{From: "fg:CH1", To: "scope:C1"},
		{From: "fg:CH2", To: "scope:C2"},
		{From: "ext:5V", To: "dmm:V", Expect: &domain.Expect{Function: "vdc", Value: 5, Tolerance: "5%"}},
	}
	inv.Limits = map[string]map[string]any{"fg": {"max_amp_vpp": 10.0, "max_abs_v": 10.0}}
	store := &fake.Store{B: inv}
	var warn bytes.Buffer
	a := &App{Store: store, Connector: b, Scanner: &fake.Scanner{}, RoleOf: fake.RoleOf, Warn: &warn, Sleep: fake.NoSleep}
	return a, b, store, &warn
}

var straight = []fake.Cable{
	{Gen: "fg", GenCh: 1, To: "scope", ToCh: 1, Gain: 1},
	{Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 1},
}

func TestWiringVerifyOK(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	sc := b.Scopes["scope"]
	before, _ := sc.ChannelState(context.Background(), 1)
	rep, err := a.WiringVerify(context.Background(), nil)
	if err != nil {
		t.Fatalf("%v\n%+v", err, rep)
	}
	if !rep.OK || len(rep.Links) != 3 {
		t.Fatalf("%+v", rep)
	}
	for _, ch := range []int{1, 2} {
		st := b.Gens["fg"].Ch(ch)
		if st.Output {
			t.Errorf("CH%d left on", ch)
		}
		if st.Wave != "sine" || st.AmpVpp != 4 || st.Freq != 1000 {
			t.Errorf("CH%d not restored: %+v", ch, st)
		}
	}
	after, _ := sc.ChannelState(context.Background(), 1)
	if after != before {
		t.Errorf("scope C1 not restored: %+v vs %+v", after, before)
	}
	if tb, _ := sc.Timebase(context.Background()); tb.TDiv != 0.5 {
		t.Errorf("timebase not restored: %v", tb.TDiv)
	}
}

func TestWiringVerifyDetectsFaults(t *testing.T) {
	cases := map[string]struct {
		cables []fake.Cable
		want   string
	}{
		"swapped": {[]fake.Cable{
			{Gen: "fg", GenCh: 1, To: "scope", ToCh: 2, Gain: 1},
			{Gen: "fg", GenCh: 2, To: "scope", ToCh: 1, Gain: 1},
		}, "instead of"},
		"missing": {straight[:1], "not seen"},
		"attenuated": {[]fake.Cable{
			straight[0],
			{Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 0.3},
		}, "attenuator"},
		// The live CH2 case: probe switched to 10X, scope channel at 1X. At
		// the 200 mV/div test scale the scope cannot read its frequency; the
		// sensitive re-measure must find it and name the probe switch.
		"10x probe": {[]fake.Cable{
			straight[0],
			{Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 0.1},
		}, "switch at 10X on a channel set to 1X"},
		// Live CH2: probe tip not making contact, decaying spikes on C2.
		"edges only": {[]fake.Cable{
			straight[0],
			{Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 0.8, EdgesOnly: true},
		}, "capacitively coupled"},
		"load mismatch": {[]fake.Cable{
			straight[0],
			{Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 2},
		}, "50 Ω into a 1 MΩ"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a, b, _, _ := rig(t, c.cables)
			rep, err := a.WiringVerify(context.Background(), nil)
			if domain.KindOf(err) != domain.KindVerify {
				t.Fatalf("want verify error, got %v", err)
			}
			if rep.OK || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
			if b.Gens["fg"].Ch(1).Output || b.Gens["fg"].Ch(2).Output {
				t.Fatal("outputs left on after failure")
			}
		})
	}
}

func TestWiringVerifyDMM(t *testing.T) {
	a, b, store, _ := rig(t, straight)
	b.DMMs["dmm"].External = 0.01 // PSU off
	_, err := a.WiringVerify(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "source off or not connected") {
		t.Fatalf("got %v", err)
	}
	// Generator → DMM link driven with a DC level.
	b.DMMs["dmm"].External = 0
	b.Cables = append(b.Cables, fake.Cable{Gen: "fg", GenCh: 2, To: "dmm", Gain: 1})
	store.B.Wiring = []domain.Link{{From: "fg:CH2", To: "dmm:V"}}
	rep, err := a.WiringVerify(context.Background(), nil)
	if err != nil || !rep.OK || !strings.Contains(rep.Links[0].Method, "DC 1V") {
		t.Fatalf("%+v %v", rep, err)
	}
	if b.Gens["fg"].Ch(2).Output || b.Gens["fg"].Ch(2).Wave != "sine" {
		t.Fatal("generator not restored after DMM check")
	}
}

func TestWiringShow(t *testing.T) {
	a, _, _, _ := rig(t, straight)
	steps, err := a.WiringShow(nil)
	if err != nil || len(steps) != 3 {
		t.Fatal(err)
	}
	if !strings.Contains(steps[0].Instruction, "switch to 1X") {
		t.Errorf("instruction lacks probe switch advice: %s", steps[0].Instruction)
	}
	if !strings.Contains(steps[0].Instruction, "BNC-BNC") || !strings.Contains(steps[2].Instruction, "expect vdc") {
		t.Fatalf("%+v", steps)
	}
}

const plan = `
name: smoke
steps:
  - fg.set:        {ch: 1, wave: sine, freq: 1k, amp: 1Vpp, offset: 0, load: hiz}
  - fg.output:     {ch: 1, state: on}
  - scope.setup:   {ch: 1, vdiv: 200m, offset: 0, coupling: dc, probe: 1x, on: true}
  - scope.timebase: {tdiv: 500u}
  - scope.trigger: {source: C1, mode: auto, level: 0, slope: rise}
  - wait: 1ms
  - scope.measure: {ch: 1, params: [pkpk, freq], expect: {pkpk: "1 ± 3%", freq: "1k ± 0.1%"}}
  - scope.capture: {ch: 1, out: c1.csv, expect: {fft_peak_hz: "1k ± 2%"}}
  - dmm.config:    {function: vdc, range: auto}
  - dmm.read:      {samples: 3, expect: "5 ± 5%"}
  - raw:           {alias: scope, cmd: "TDIV?"}
`

func TestApplyPlan(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	b.Scopes["scope"].SCPI().(*fake.SCPI).Responses = map[string]string{"TDIV?": "5.00E-04"}
	p, err := LoadPlan(strings.NewReader(plan))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := a.Apply(context.Background(), p, dir)
	if err != nil {
		t.Fatalf("%v\n%+v", err, res)
	}
	if !res.OK || len(res.Steps) != 11 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "c1.csv")); err != nil {
		t.Fatal("capture file not written")
	}
	if !b.Gens["fg"].Ch(1).Output {
		t.Fatal("output should stay on without safe_on_exit")
	}
}

func TestApplyFailedAssertionRunsSafe(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	p, _ := LoadPlan(strings.NewReader(`
steps:
  - fg.set:    {ch: 1, wave: sine, freq: 1k, amp: 2Vpp}
  - fg.output: {ch: 1, state: on}
  - scope.measure: {ch: 1, params: [pkpk], expect: {pkpk: "1 ± 3%"}}
`))
	res, err := a.Apply(context.Background(), p, t.TempDir())
	if domain.KindOf(err) != domain.KindVerify || res.Failed != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if b.Gens["fg"].Ch(1).Output || len(res.Safe) == 0 {
		t.Fatal("safe did not run after a failed plan")
	}
}

func TestApplyErrors(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	must(t, b.Gens["fg"].Output(context.Background(), 2, true))
	p, _ := LoadPlan(strings.NewReader("steps:\n  - fg.explode: {}\n"))
	if _, err := a.Apply(context.Background(), p, "."); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("unknown action: %v", err)
	}
	if b.Gens["fg"].Ch(2).Output {
		t.Fatal("safe should run on step error")
	}
	p, _ = LoadPlan(strings.NewReader("steps:\n  - fg.set: {ch: 1, amp: 50Vpp}\n"))
	if _, err := a.Apply(context.Background(), p, "."); err == nil || !strings.Contains(err.Error(), "max_amp_vpp") {
		t.Fatalf("limit: %v", err)
	}
	for _, bad := range []string{"", "steps: []", "steps:\n  - {a: 1, b: 2}\n", "bogus: 1\nsteps:\n  - wait: 1s\n"} {
		if _, err := LoadPlan(strings.NewReader(bad)); err == nil {
			t.Errorf("plan %q should be rejected", bad)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must(t, b.Gens["fg"].Output(context.Background(), 1, true))
	p, _ = LoadPlan(strings.NewReader("steps:\n  - wait: 1s\n"))
	if _, err := a.Apply(ctx, p, "."); err == nil || b.Gens["fg"].Ch(1).Output {
		t.Fatal("cancelled plan must fail and run safe")
	}
}

func TestCheckGenLimits(t *testing.T) {
	b := &domain.Bench{Limits: map[string]map[string]any{"fg": {"max_amp_vpp": 5.0, "max_abs_v": 3.0, "max_freq_hz": 1e6}}}
	cur := domain.GenChannelState{Wave: "sine", AmpVpp: 1}
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		w  domain.WaveSpec
		ok bool
	}{
		{domain.WaveSpec{AmpVpp: f(4)}, true},
		{domain.WaveSpec{AmpVpp: f(6)}, false},
		{domain.WaveSpec{AmpVpp: f(4), Offset: f(1.5)}, false}, // peak 3.5
		{domain.WaveSpec{AmpRMS: f(2)}, false},                 // 5.66 Vpp
		{domain.WaveSpec{Type: "dc", Offset: f(2.9)}, true},
		{domain.WaveSpec{Freq: f(2e6)}, false},
	}
	for i, c := range cases {
		if err := CheckGenLimits(b, "fg", c.w, cur); (err == nil) != c.ok {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestFGSetWarnsOnLoadMismatch(t *testing.T) {
	a, _, _, warn := rig(t, straight)
	w, _ := WaveSpecFromStrings("sine", "1k", "1Vpp", "0", "", "")
	res, err := a.FGSet(context.Background(), FGSetRequest{Channel: 1, Wave: w, Load: "50"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(warn.String(), "2×") {
		t.Fatalf("%+v / %s", res.Warnings, warn)
	}
	if LoadMismatch("hiz", "dc50") == "" || LoadMismatch("hiz", "dc") != "" {
		t.Fatal("LoadMismatch")
	}
}

func TestMeasureWarnings(t *testing.T) {
	cs := domain.ChannelState{Channel: 1, On: true, VDiv: 0.2}
	f := func(v float64) *float64 { return &v }
	if w := MeasureWarnings(cs, f(1.0), nil); len(w) != 0 {
		t.Fatal(w)
	}
	if w := MeasureWarnings(cs, f(1.59), nil); len(w) != 1 || !strings.Contains(w[0], "clipped") {
		t.Fatal(w)
	}
	if w := MeasureWarnings(cs, f(0.05), nil); len(w) != 1 || !strings.Contains(w[0], "low resolution") {
		t.Fatal(w)
	}
	cs.On = false
	if w := MeasureWarnings(cs, f(1), []domain.Measurement{{Name: "freq"}}); len(w) != 2 {
		t.Fatal(w)
	}
}

func TestScopeMeasureReportsSettings(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	must(t, b.Gens["fg"].SetWave(context.Background(), 1, domain.WaveSpec{AmpVpp: ptr(1.0)}))
	must(t, b.Gens["fg"].Output(context.Background(), 1, true))
	must(t, b.Scopes["scope"].SetupChannel(context.Background(), 1, domain.ChannelSetup{VDiv: ptr(0.2)}))
	m, err := a.ScopeMeasure(context.Background(), "", 1, []string{"FREQ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Measurements) != 1 || *m.Measurements[0].Value != 1000 || m.Settings.VDiv != 0.2 || !m.Trustworthy {
		t.Fatalf("%+v", m)
	}
}

func ptr(v float64) *float64 { return &v }

func TestDMMReadStats(t *testing.T) {
	a, _, _, _ := rig(t, straight)
	r, err := a.DMMRead(context.Background(), "", 4, 0)
	if err != nil || len(r.Values) != 4 || r.Mean != 4.997 || r.StdDev != 0 || r.Unit != "V" {
		t.Fatalf("%+v %v", r, err)
	}
	rr := domain.DMMReading{Values: []float64{1, 2, 3}}
	Stats(&rr)
	if rr.Mean != 2 || rr.Min != 1 || rr.Max != 3 || rr.StdDev != 1 {
		t.Fatalf("%+v", rr)
	}
}

func TestPeakFrequency(t *testing.T) {
	dt := 1e-6
	v := make([]float64, 20000)
	for i := range v {
		v[i] = 0.3 + math.Sin(2*math.Pi*3300*float64(i)*dt)
	}
	if f := PeakFrequency(v, dt); math.Abs(f-3300)/3300 > 0.01 {
		t.Fatalf("peak %v", f)
	}
	if PeakFrequency(v[:4], dt) != 0 {
		t.Fatal("too short")
	}
}

func TestParseExpectation(t *testing.T) {
	e, err := ParseExpectation("1k ± 0.1%")
	if err != nil || e.Nominal != 1000 || !e.Tol.Relative {
		t.Fatalf("%+v %v", e, err)
	}
	if !e.Check("f", ptr(1000.9)).OK || e.Check("f", ptr(1002)).OK || e.Check("f", nil).OK {
		t.Fatal("check bounds")
	}
	if e, err := ParseExpectation("5 +- 0.25"); err != nil || e.Tol.Relative || e.Tol.Value != 0.25 {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := ParseExpectation("about 5"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseScript(t *testing.T) {
	lines, err := ParseScript(strings.NewReader("# comment\n\n// also comment\nC1:VDIV 1\n#\nC1:VDIV?\nWVDT M50,#14abcd\n"))
	if err != nil || len(lines) != 3 || lines[0].Line != 4 || lines[2].Command != "WVDT M50,#14abcd" {
		t.Fatalf("%+v %v", lines, err)
	}
}

func TestRawAndScript(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	s := b.Scopes["scope"].SCPI().(*fake.SCPI)
	s.Responses = map[string]string{"TDIV?": "5.00E-04", "C1:WF? DESC": "abc"}
	s.SetErrs = map[string]error{"BAD 1": domain.Instrumentf("rejected")}
	r, err := a.Raw(context.Background(), "scope", "TDIV?", RawOptions{})
	if err != nil || !r.Query || r.Response != "5.00E-04" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = a.Raw(context.Background(), "scope", "C1:WF? DESC", RawOptions{Block: true})
	if err != nil || !r.Block || string(r.Data) != "abc" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = a.Raw(context.Background(), "scope", "TDIV 1", RawOptions{OPC: true})
	if err != nil || r.Query || !r.OPC {
		t.Fatalf("%+v %v", r, err)
	}
	res, err := a.Script(context.Background(), "scope", []ScriptLine{{1, "TDIV 1"}, {2, "BAD 1"}, {3, "TDIV?"}}, 0)
	if err == nil || res.FailedAt != 2 || len(res.Results) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	for cmd, want := range map[string]bool{
		"*IDN?": true, "C1:PAVA? PKPK": true, "SANU? C1": true, "C1:VDIV 1": false,
		"*RST": false, "A 1;B?": true, `MMEM:DATA "x?",#13abc`: false,
	} {
		if isQuery(cmd) != want {
			t.Errorf("isQuery(%q) != %v", cmd, want)
		}
	}
}

func TestDiscoverKeepsAliases(t *testing.T) {
	a, _, store, _ := rig(t, straight)
	a.Scanner = &fake.Scanner{Found: []port.Found{
		{IP: "10.0.0.9", Identity: domain.Identity{Model: "SDS1104X-E", Serial: "SC-scope"}, Role: domain.RoleScope},
		{IP: "10.0.0.10", Identity: domain.Identity{Model: "SDS1202X-E", Serial: "NEW"}, Role: domain.RoleScope},
		{IP: "10.0.0.11", Identity: domain.Identity{Model: "SPD3303X-E", Serial: "PSU"}, Role: domain.RolePSU},
	}}
	res, err := a.Discover(context.Background(), "", true)
	if err != nil || !res.Saved {
		t.Fatal(err)
	}
	aliases := map[string]string{}
	for _, f := range res.Found {
		aliases[f.Identity.Serial] = f.Alias
	}
	if aliases["SC-scope"] != "scope" || aliases["NEW"] != "scope2" || aliases["PSU"] != "psu" {
		t.Fatalf("%+v", aliases)
	}
	if store.B.Instruments["scope"].LastIP != "10.0.0.9" || store.Saves != 1 {
		t.Fatal("inventory not updated")
	}
	if _, err := a.ResolveAlias("", domain.RoleScope); domain.KindOf(err) != domain.KindUsage {
		t.Fatal("two scopes must be ambiguous")
	}
}

func TestStatusAndSafe(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	st, err := a.Status(context.Background(), nil)
	if err != nil || len(st) != 3 {
		t.Fatalf("%+v %v", st, err)
	}
	b.OpenErrs["dmm"] = domain.Unreachablef("down")
	st, err = a.Status(context.Background(), nil)
	if domain.KindOf(err) != domain.KindUnreachable || len(st) != 3 {
		t.Fatalf("%v", err)
	}
	must(t, b.Gens["fg"].Output(context.Background(), 1, true))
	sr, err := a.Safe(context.Background())
	if err != nil || len(sr) != 1 || sr[0].Action != "all outputs off" || b.Gens["fg"].Ch(1).Output {
		t.Fatalf("%+v %v", sr, err)
	}
	if c, err := a.Caps("scope"); err != nil || c.Channels != 4 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := a.Caps("nope"); domain.KindOf(err) != domain.KindUsage {
		t.Fatal("unknown alias")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestClipped(t *testing.T) {
	wf := domain.Waveform{VDiv: 0.2, Offset: -0.5, Volts: []float64{1.0, 1.2}}
	// screen spans -offset ± 4 div = 0.5 ± 0.8 V → [-0.3, 1.3]
	if Clipped(wf) {
		t.Fatal("1.2 V is on screen")
	}
	wf.Volts = append(wf.Volts, 1.29)
	if !Clipped(wf) {
		t.Fatal("1.29 V is at the top edge")
	}
	if Clipped(domain.Waveform{Volts: []float64{100}}) {
		t.Fatal("no vdiv: cannot tell")
	}
}

func TestWriteWaveformFile(t *testing.T) {
	dir := t.TempDir()
	wf := domain.Waveform{Channel: 1, Points: 2, Dt: 1e-3, Volts: []float64{0.5, -0.25}}
	csv := filepath.Join(dir, "w.csv")
	must(t, WriteWaveformFile(csv, wf))
	b, err := os.ReadFile(csv)
	must(t, err)
	if !strings.Contains(string(b), "t,v\n0,0.5\n0.001,-0.25\n") {
		t.Fatalf("csv:\n%s", b)
	}
	must(t, WriteWaveformFile(filepath.Join(dir, "w.json"), wf))
	bad := filepath.Join(dir, "w.xlsx")
	if err := WriteWaveformFile(bad, wf); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("bad extension: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("unsupported format must not leave an empty file")
	}
	if err := WriteWaveformFile(filepath.Join(dir, "missing", "w.csv"), wf); err == nil {
		t.Fatal("expected create error")
	}
}

func TestWiringVerifyWeakPathAndCrosstalk(t *testing.T) {
	// The live CH2 fault: frequency arrives at ×0.048 amplitude. It must be
	// reported as attenuated, not as a missing cable.
	a, _, _, _ := rig(t, []fake.Cable{straight[0], {Gen: "fg", GenCh: 2, To: "scope", ToCh: 2, Gain: 0.048}})
	_, err := a.WiringVerify(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "attenuator") || strings.Contains(err.Error(), "not seen") {
		t.Fatalf("weak path: %v", err)
	}
	// 5% crosstalk from CH1 into C2 is not a connection.
	a, _, _, _ = rig(t, append([]fake.Cable{{Gen: "fg", GenCh: 1, To: "scope", ToCh: 2, Gain: 0.05}}, straight...))
	rep, err := a.WiringVerify(context.Background(), nil)
	if err != nil || !rep.OK {
		t.Fatalf("crosstalk misread: %v %+v", err, rep)
	}
}

const wiredPlan = `
name: wired
wiring:
  - {from: "fg:CH1", to: "scope:C1"}
  - {from: "ext:5V", to: "dmm:V", expect: {function: vdc, value: 5, tolerance: "5%"}}
steps:
  - fg.output: {ch: 1, state: on}
`

func TestApplyVerifiesPlanWiringFirst(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	p, err := LoadPlan(strings.NewReader(wiredPlan))
	must(t, err)
	res, err := a.Apply(context.Background(), p, t.TempDir())
	if err != nil || res.Wiring == nil || !res.Wiring.OK || len(res.Steps) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if !b.Gens["fg"].Ch(1).Output {
		t.Fatal("steps should run after a good wiring check")
	}

	// Miswired bench: no step may run.
	a, b, _, _ = rig(t, []fake.Cable{{Gen: "fg", GenCh: 1, To: "scope", ToCh: 2, Gain: 1}})
	res, err = a.Apply(context.Background(), p, t.TempDir())
	if domain.KindOf(err) != domain.KindVerify || len(res.Steps) != 0 || res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	if b.Gens["fg"].Ch(1).Output {
		t.Fatal("a step ran despite failed wiring")
	}

	// verify_wiring: false skips the check.
	off := strings.Replace(wiredPlan, "steps:", "verify_wiring: false\nsteps:", 1)
	p, err = LoadPlan(strings.NewReader(off))
	must(t, err)
	res, err = a.Apply(context.Background(), p, t.TempDir())
	if err != nil || res.Wiring != nil {
		t.Fatalf("%v %+v", err, res)
	}
	// A wiring-only plan is valid (for `wiring show --plan`).
	if _, err := LoadPlan(strings.NewReader("wiring:\n  - {from: \"fg:CH1\", to: \"scope:C1\"}\n")); err != nil {
		t.Fatal(err)
	}
	steps, err := a.WiringShow(p.Wiring)
	if err != nil || len(steps) != 2 {
		t.Fatalf("%+v %v", steps, err)
	}
}

func TestPing(t *testing.T) {
	a, b, _, _ := rig(t, straight)
	b.Scopes["scope"].SCPI().(*fake.SCPI).Responses = map[string]string{"*IDN?": "Fake,SDS1104X-E,SC-scope,1"}
	b.Gens["fg"].SCPI().(*fake.SCPI).Responses = map[string]string{"*IDN?": "Fake,SDG1032X,FG-fg,1"}
	b.DMMs["dmm"].SCPI().(*fake.SCPI).Responses = map[string]string{"*IDN?": "Fake,SDM3045X,DM-dmm,1"}
	r, err := a.Ping(context.Background(), nil, 2)
	if err != nil || len(r) != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, p := range r {
		if !p.Reachable || !p.SerialOK || p.RoundTrip == "" || p.Transport != "fake" || p.IP == "" {
			t.Fatalf("%+v", p)
		}
	}
	b.OpenErrs["dmm"] = domain.Unreachablef("down")
	r, err = a.Ping(context.Background(), []string{"dmm"}, 1)
	if domain.KindOf(err) != domain.KindUnreachable || r[0].Reachable || r[0].Error == "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestAmplitudeProblemProbeFactor(t *testing.T) {
	cases := []struct {
		measured, probe float64
		want            string
	}{
		{1.0, 1, ""},
		{0.1, 1, "probe factor to 10X"},
		{0.1, 0, "probe factor to 10X"},
		{10, 10, "probe factor to 1X"},
		{0.5, 1, "50 Ω termination"},
		{2, 1, "50 Ω into a 1 MΩ"},
		{0.3, 1, "attenuator"},
		{5, 1, "probe attenuation setting wrong"},
	}
	for _, c := range cases {
		got := AmplitudeProblem(1, c.measured, c.probe)
		if (c.want == "" && got != "") || !strings.Contains(got, c.want) {
			t.Errorf("×%g probe %g: %q, want %q", c.measured, c.probe, got, c.want)
		}
	}
}

func TestWiringVerifyProbeFactorMismatch(t *testing.T) {
	// Scope C2 set to 10X with a 1X cable: the signal clips at the test
	// scale; the coarse re-measure must report ×10 and name the probe factor,
	// not a 50 Ω mismatch.
	a, b, _, _ := rig(t, straight)
	ten := 10.0
	if err := b.Scopes["scope"].SetupChannel(context.Background(), 2, domain.ChannelSetup{Probe: &ten}); err != nil {
		t.Fatal(err)
	}
	_, err := a.WiringVerify(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "probe factor is 10X") || strings.Contains(err.Error(), "50 Ω") {
		t.Fatalf("got %v", err)
	}
}
