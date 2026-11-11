package domain

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-12*math.Max(1, math.Abs(b)) }

func TestParseQuantity(t *testing.T) {
	cases := []struct {
		in   string
		v    float64
		unit string
	}{
		{"1k", 1e3, ""},
		{"200m", 0.2, ""},
		{"200mV", 0.2, "V"},
		{"4Vpp", 4, "Vpp"},
		{"1.414Vrms", 1.414, "Vrms"},
		{"-2.5V", -2.5, "V"},
		{"10MHz", 10e6, "Hz"},
		{"1M", 1e6, ""},
		{"1e3", 1e3, ""},
		{"2.5e-3", 2.5e-3, ""},
		{"200u", 200e-6, ""},
		{"200us", 200e-6, "s"},
		{"50ohm", 50, "Ohm"},
		{"3.3kHz", 3300, "Hz"},
		{" 0 ", 0, ""},
		{"+5", 5, ""},
		{"-10dBm", -10, "dBm"},
		{"500n", 500e-9, ""},
		{"1G", 1e9, ""},
	}
	for _, c := range cases {
		q, err := ParseQuantity(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !approx(q.Value, c.v) || q.Unit != c.unit {
			t.Errorf("%q = %g %q, want %g %q", c.in, q.Value, q.Unit, c.v, c.unit)
		}
	}
	for _, bad := range []string{"", "abc", "1x", "k1", "1kk", "--1"} {
		if _, err := ParseQuantity(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestParseIn(t *testing.T) {
	if _, _, err := ParseIn("1Vrms", "V", "Vpp"); err == nil {
		t.Error("Vrms should be rejected when not allowed")
	}
	v, u, err := ParseIn("1.5", "Hz")
	if err != nil || v != 1.5 || u != "" {
		t.Errorf("unitless accepted: %v %q %v", v, u, err)
	}
}

func TestParseInstrumentNumber(t *testing.T) {
	cases := []struct {
		in string
		v  float64
		ok bool
	}{
		{"2.00E+00", 2, true},
		{"-4.72E+00V", -4.72, true},
		{"1000HZ", 1000, true},
		{"0.001S", 0.001, true},
		{"+4.99784414E+00", 4.99784414, true},
		{"700K", 700e3, true},
		{"14M", 14e6, true},
		{"****", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		v, ok, err := ParseInstrumentNumber(c.in)
		if err != nil || ok != c.ok || (ok && !approx(v, c.v)) {
			t.Errorf("%q = %v %v %v, want %v %v", c.in, v, ok, err, c.v, c.ok)
		}
	}
	if _, _, err := ParseInstrumentNumber("ON"); err == nil {
		t.Error("expected error for non-number")
	}
}

func TestTolerance(t *testing.T) {
	rel, err := ParseTolerance("3%")
	if err != nil || !rel.Relative || !approx(rel.Value, 0.03) {
		t.Fatalf("3%%: %+v %v", rel, err)
	}
	if !rel.Within(1.0, 1.029) || rel.Within(1.0, 1.031) {
		t.Error("relative tolerance bounds wrong")
	}
	abs, err := ParseTolerance("50m")
	if err != nil || abs.Relative || !approx(abs.Value, 0.05) {
		t.Fatalf("50m: %+v %v", abs, err)
	}
	if !abs.Within(5, 5.05) || abs.Within(5, 5.06) {
		t.Error("absolute tolerance bounds wrong")
	}
	if rel.String() != "3%" || abs.String() != "0.05" {
		t.Errorf("String: %s %s", rel, abs)
	}
}

func TestClose(t *testing.T) {
	if !Close(10, 10.05, 0.01, 0) || Close(10, 10.2, 0.01, 0) {
		t.Error("relative")
	}
	if !Close(0, 0.001, 0.01, 0.002) {
		t.Error("absolute floor near zero")
	}
}

func TestParseIDN(t *testing.T) {
	id, err := ParseIDN("Siglent Technologies,SDS1104X-E,SDSMMFCX5R4853,8.1.6.1.35R2\n")
	if err != nil {
		t.Fatal(err)
	}
	if id.Model != "SDS1104X-E" || id.Serial != "SDSMMFCX5R4853" || id.Firmware != "8.1.6.1.35R2" {
		t.Errorf("%+v", id)
	}
	if _, err := ParseIDN("garbage"); err == nil {
		t.Error("expected error")
	}
}

func TestEndpoint(t *testing.T) {
	e := ParseEndpoint("fg:ch1")
	n, err := e.ChannelNumber()
	if e.Alias != "fg" || e.Port != "CH1" || err != nil || n != 1 {
		t.Errorf("%+v %d %v", e, n, err)
	}
	if n, _ := ParseEndpoint("scope:C3").ChannelNumber(); n != 3 {
		t.Errorf("C3 → %d", n)
	}
	if _, err := ParseEndpoint("ext:5V").ChannelNumber(); err == nil {
		t.Error("5V is not a channel")
	}
}

func TestBenchLimit(t *testing.T) {
	b := Bench{Limits: map[string]map[string]any{"fg": {"max_amp_vpp": 10.0, "max_abs_v": "5"}}}
	if v, ok := b.Limit("fg", "max_amp_vpp"); !ok || v != 10 {
		t.Error("float limit")
	}
	if v, ok := b.Limit("fg", "max_abs_v"); !ok || v != 5 {
		t.Error("string limit")
	}
	if _, ok := b.Limit("scope", "x"); ok {
		t.Error("missing limit")
	}
}

func TestErrorKinds(t *testing.T) {
	cases := map[error]int{
		nil:                               0,
		Usagef("x"):                       2,
		Instrumentf("x"):                  1,
		Unreachablef("x"):                 3,
		Verifyf("x"):                      4,
		errors.New("plain"):               1,
		fmt.Errorf("w: %w", Verifyf("x")): 4,
	}
	for err, want := range cases {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
