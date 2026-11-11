// Package sdm3000x drives Siglent SDM3000X digital multimeters (SCPI-1999 tree).
package sdm3000x

import (
	"context"
	"math"
	"strings"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/base"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// DMM implements port.DMM.
type DMM struct {
	base.Instrument
}

var (
	_ port.DMM           = (*DMM)(nil)
	_ port.Screenshotter = (*DMM)(nil)
)

// Matches reports whether model is handled by this driver.
func Matches(model string) bool { return strings.HasPrefix(model, "SDM3") }

// New initialises the DMM session and clears stale errors.
func New(ctx context.Context, b base.Instrument) (*DMM, error) {
	b.R = domain.RoleDMM
	b.S.SetChecker(scpi.SCPI1999Checker)
	b.S.OPCEvent = true // *OPC sets ESR bit 0 (verified on fw 5.01.01.06R1)
	b.DrainErrors(ctx)
	return &DMM{Instrument: b}, nil
}

// funcSCPI maps CLI function names to CONFigure subsystems.
var funcSCPI = map[domain.DMMFunction]string{
	"vdc": "VOLT:DC", "vac": "VOLT:AC", "idc": "CURR:DC", "iac": "CURR:AC",
	"res": "RES", "fres": "FRES", "freq": "FREQ", "per": "PER", "cap": "CAP",
	"diode": "DIOD", "cont": "CONT", "temp": "TEMP",
}

// confNames maps the function token reported by CONF? back to CLI names.
var confNames = map[string]domain.DMMFunction{
	"VOLT": "vdc", "VOLT:DC": "vdc", "VOLT:AC": "vac", "CURR": "idc", "CURR:DC": "idc",
	"CURR:AC": "iac", "RES": "res", "FRES": "fres", "FREQ": "freq", "PER": "per",
	"CAP": "cap", "DIOD": "diode", "CONT": "cont", "TEMP": "temp",
}

// ranged reports whether the function takes a range argument.
func ranged(f domain.DMMFunction) bool {
	switch f {
	case "diode", "cont", "temp", "freq", "per":
		return false
	}
	return true
}

// Configure implements port.DMM.
func (d *DMM) Configure(ctx context.Context, cfg domain.DMMConfig) error {
	sub, ok := funcSCPI[cfg.Function]
	if !ok {
		return domain.Usagef("unknown DMM function %q", cfg.Function)
	}
	cmd := "CONF:" + sub
	var wantRange float64
	if ranged(cfg.Function) {
		r := strings.ToLower(strings.TrimSpace(cfg.Range))
		switch r {
		case "", "auto":
			cmd += " AUTO"
		default:
			q, err := domain.ParseQuantity(cfg.Range)
			if err != nil {
				return domain.Usagef("invalid range %q: %v", cfg.Range, err)
			}
			wantRange = q.Value
			cmd += " " + base.F(q.Value)
		}
	} else if cfg.Range != "" && !strings.EqualFold(cfg.Range, "auto") {
		return domain.Usagef("function %s has no range", cfg.Function)
	}
	if err := d.S.Set(ctx, cmd); err != nil {
		return err
	}
	got, err := d.Config(ctx)
	if err != nil {
		return err
	}
	if got.Function != cfg.Function {
		return domain.Instrumentf("function: requested %s but instrument reports %s", cfg.Function, got.Function)
	}
	if wantRange > 0 {
		// The DMM coerces to the next range up, so applied >= requested is fine.
		if v, ok, _ := domain.ParseInstrumentNumber(got.Range); ok && v < wantRange*(1-1e-6) {
			return domain.Instrumentf("range: requested %g but instrument applied %g", wantRange, v)
		}
	}
	return nil
}

// ParseCONF parses `"VOLT +6.00000000E+00"` or `"DIOD"`.
func ParseCONF(r string) (domain.DMMConfig, error) {
	r = strings.Trim(strings.TrimSpace(r), `"`)
	tok, rng, _ := strings.Cut(r, " ")
	f, ok := confNames[tok]
	if !ok {
		return domain.DMMConfig{Raw: r}, base.Errf("CONF?: unknown function %q", r)
	}
	cfg := domain.DMMConfig{Function: f, Raw: r}
	if rng = strings.TrimSpace(rng); rng != "" {
		if v, ok, _ := domain.ParseInstrumentNumber(strings.Split(rng, ",")[0]); ok {
			cfg.Range = base.F(v)
		} else {
			cfg.Range = rng
		}
	}
	return cfg, nil
}

// Config implements port.DMM. Range is the effective range; when auto-ranging
// is on it is reported as "auto (<range>)".
func (d *DMM) Config(ctx context.Context) (domain.DMMConfig, error) {
	r, err := d.S.Query(ctx, "CONF?")
	if err != nil {
		return domain.DMMConfig{}, err
	}
	cfg, err := ParseCONF(r)
	if err != nil {
		return cfg, err
	}
	if ranged(cfg.Function) {
		if a, err := d.S.Query(ctx, funcSCPI[cfg.Function]+":RANG:AUTO?"); err == nil {
			if strings.TrimSpace(a) == "1" || strings.EqualFold(strings.TrimSpace(a), "ON") {
				cfg.Range = "auto (" + cfg.Range + ")"
			}
		} else {
			d.DrainErrors(ctx)
		}
	}
	return cfg, nil
}

// Overload is the value SCPI instruments return for an overrange reading.
const Overload = 9.9e37

// Read implements port.DMM: one triggered reading (READ?).
func (d *DMM) Read(ctx context.Context) (float64, error) {
	r, err := d.S.Query(ctx, "READ?")
	if err != nil {
		return 0, err
	}
	v, err := base.MustNumber("READ?", r)
	if err != nil {
		return 0, err
	}
	if math.Abs(v) >= Overload*0.99 {
		return v, domain.Instrumentf("DMM reading overloaded (%s); increase the range", r)
	}
	return v, nil
}

// Screenshot implements port.Screenshotter (480x272 24-bit BMP via SCDP).
func (d *DMM) Screenshot(ctx context.Context) ([]byte, error) { return d.ScreenDump(ctx) }
