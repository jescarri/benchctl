package sdm3000x

import (
	"context"
	"strings"
	"testing"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/scpi/scpitest"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent/base"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func TestParseCONF(t *testing.T) {
	cases := map[string]domain.DMMConfig{
		`"VOLT +6.00000000E+00"`:    {Function: "vdc", Range: "6.000000E+00"},
		`"VOLT:AC +6.00000000E-01"`: {Function: "vac", Range: "6.000000E-01"},
		`"CURR +1.00000000E+01"`:    {Function: "idc", Range: "1.000000E+01"},
		`"DIOD"`:                    {Function: "diode"},
		`"FRES +6.00000000E+02"`:    {Function: "fres", Range: "6.000000E+02"},
	}
	for in, want := range cases {
		got, err := ParseCONF(in)
		got.Raw = ""
		if err != nil || got != want {
			t.Errorf("%s → %+v %v", in, got, err)
		}
	}
	if _, err := ParseCONF(`"WHAT"`); err == nil {
		t.Error("expected error")
	}
}

type sim struct {
	conf, rangeAuto, read string
	errs                  []string
}

func (s *sim) handle(cmd string) (string, bool) {
	switch {
	case cmd == "SYST:ERR?":
		if len(s.errs) == 0 {
			return `0,"No error"`, true
		}
		e := s.errs[0]
		s.errs = s.errs[1:]
		return e, true
	case cmd == "CONF?":
		return s.conf, true
	case strings.HasSuffix(cmd, ":RANG:AUTO?"):
		return s.rangeAuto, true
	case cmd == "READ?":
		return s.read, true
	case cmd == "CONF:VOLT:DC AUTO":
		s.conf, s.rangeAuto = `"VOLT +6.00000000E+00"`, "1"
	case cmd == "CONF:VOLT:DC 5.000000E+00":
		s.conf, s.rangeAuto = `"VOLT +6.00000000E+00"`, "0" // coerced up
	case cmd == "CONF:VOLT:DC 1.000000E+04":
		s.conf, s.rangeAuto = `"VOLT +1.00000000E+03"`, "0" // max range
	case cmd == "CONF:DIOD":
		s.conf = `"DIOD"`
	default:
		s.errs = append(s.errs, `-113,"Undefined header;`+cmd+`"`)
	}
	return "", false
}

func newDMM(t *testing.T, s *sim) *DMM {
	d, err := New(context.Background(), base.Instrument{ID: domain.Identity{Model: "SDM3045X"}, S: scpi.NewSession(scpitest.New(s.handle), nil)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConfigureAndRead(t *testing.T) {
	s := &sim{conf: `"VOLT:AC +6.00000000E-01"`, read: "+4.99784414E+00", errs: []string{`-113,"stale"`}}
	d := newDMM(t, s)
	ctx := context.Background()
	if err := d.Configure(ctx, domain.DMMConfig{Function: "vdc", Range: "auto"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := d.Config(ctx)
	if err != nil || cfg.Function != "vdc" || cfg.Range != "auto (6.000000E+00)" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := d.Configure(ctx, domain.DMMConfig{Function: "vdc", Range: "5"}); err != nil {
		t.Fatalf("coerced-up range should be accepted: %v", err)
	}
	if err := d.Configure(ctx, domain.DMMConfig{Function: "vdc", Range: "10k"}); err == nil {
		t.Fatal("range below request must fail")
	}
	if err := d.Configure(ctx, domain.DMMConfig{Function: "diode"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Configure(ctx, domain.DMMConfig{Function: "diode", Range: "6"}); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("diode has no range: %v", err)
	}
	if err := d.Configure(ctx, domain.DMMConfig{Function: "xyz"}); domain.KindOf(err) != domain.KindUsage {
		t.Fatal("unknown function")
	}
	v, err := d.Read(ctx)
	if err != nil || v != 4.99784414 {
		t.Fatalf("%v %v", v, err)
	}
	s.read = "+9.90000000E+37"
	if _, err := d.Read(ctx); err == nil || !strings.Contains(err.Error(), "overload") {
		t.Fatalf("overload: %v", err)
	}
}

func TestMatches(t *testing.T) {
	if !Matches("SDM3045X") || !Matches("SDM3065X") || Matches("SDS1104X-E") {
		t.Fatal()
	}
}
