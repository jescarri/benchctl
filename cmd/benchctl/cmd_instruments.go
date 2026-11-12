package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/jescarri/benchctl/internal/bench/app"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

var aliasFlag = &cli.StringFlag{Name: "alias", Usage: "instrument alias (default: the only one with this role)"}

func argChannel(c *cli.Command, i int) (int, error) {
	s := strings.TrimLeft(strings.ToUpper(c.Args().Get(i)), "CH")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, domain.Usagef("invalid channel %q", c.Args().Get(i))
	}
	return n, nil
}

// ---------------- fg ----------------

func fgCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "fg",
		Usage: "function generator",
		Commands: []*cli.Command{
			{
				Name:      "set",
				Usage:     "configure a channel's basic wave (never changes the output state)",
				ArgsUsage: "<ch>",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.StringFlag{Name: "wave", Usage: "sine, square, ramp, pulse, noise, dc, arb"},
					&cli.StringFlag{Name: "freq", Usage: "frequency, e.g. 1k, 2.5MHz"},
					&cli.StringFlag{Name: "amp", Usage: "amplitude, e.g. 1Vpp, 200m, 1.414Vrms"},
					&cli.StringFlag{Name: "offset", Usage: "DC offset, e.g. 0, -500m"},
					&cli.StringFlag{Name: "phase", Usage: "phase in degrees"},
					&cli.StringFlag{Name: "duty", Usage: "duty cycle in % (square/pulse)"},
					&cli.StringFlag{Name: "load", Usage: "hiz or ohms (e.g. 50)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					ch, err := argChannel(c, 0)
					if err != nil {
						return err
					}
					w, err := app.WaveSpecFromStrings(c.String("wave"), c.String("freq"), c.String("amp"), c.String("offset"), c.String("phase"), c.String("duty"))
					if err != nil {
						return err
					}
					return e.emitResult(e.app.FGSet(ctx, app.FGSetRequest{Alias: c.String("alias"), Channel: ch, Wave: w, Load: c.String("load")}))
				},
			},
			{
				Name:      "output",
				Usage:     "switch a channel output on or off (explicit only)",
				ArgsUsage: "<ch> on|off",
				Flags:     []cli.Flag{aliasFlag},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() != 2 {
						return domain.Usagef("usage: benchctl fg output <ch> on|off")
					}
					ch, err := argChannel(c, 0)
					if err != nil {
						return err
					}
					on, err := app.ParseOnOff(c.Args().Get(1))
					if err != nil {
						return err
					}
					return e.emitResult(e.app.FGOutput(ctx, c.String("alias"), ch, on))
				},
			},
			{
				Name:      "state",
				Usage:     "show channel state (all channels if none given)",
				ArgsUsage: "[ch]",
				Flags:     []cli.Flag{aliasFlag},
				Action: func(ctx context.Context, c *cli.Command) error {
					ch := 0
					if c.NArg() > 0 {
						var err error
						if ch, err = argChannel(c, 0); err != nil {
							return err
						}
					}
					return e.emitResult(e.app.FGState(ctx, c.String("alias"), ch))
				},
			},
		},
	}
}

// ---------------- scope ----------------

func scopeCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "scope",
		Usage: "oscilloscope",
		Commands: []*cli.Command{
			{
				Name:      "setup",
				Usage:     "configure a channel",
				ArgsUsage: "<ch>",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.StringFlag{Name: "vdiv", Usage: "volts/div, e.g. 200m"},
					&cli.StringFlag{Name: "offset", Usage: "offset in volts"},
					&cli.StringFlag{Name: "coupling", Usage: "dc, ac, gnd"},
					&cli.StringFlag{Name: "probe", Usage: "probe attenuation, e.g. 1x, 10x"},
					&cli.BoolFlag{Name: "on", Usage: "turn the trace on"},
					&cli.BoolFlag{Name: "off", Usage: "turn the trace off"},
					&cli.StringFlag{Name: "bwl", Usage: "20 MHz bandwidth limit: on|off"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					ch, err := argChannel(c, 0)
					if err != nil {
						return err
					}
					var on *bool
					if c.Bool("on") && c.Bool("off") {
						return domain.Usagef("--on and --off are exclusive")
					}
					if c.Bool("on") || c.Bool("off") {
						v := c.Bool("on")
						on = &v
					}
					var bwl *bool
					if c.String("bwl") != "" {
						v, err := app.ParseOnOff(c.String("bwl"))
						if err != nil {
							return err
						}
						bwl = &v
					}
					cs, err := app.ChannelSetupFromStrings(c.String("vdiv"), c.String("offset"), c.String("coupling"), c.String("probe"), on, bwl)
					if err != nil {
						return err
					}
					return e.emitResult(e.app.ScopeSetup(ctx, c.String("alias"), ch, cs))
				},
			},
			{
				Name:  "timebase",
				Usage: "set/show the timebase",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.StringFlag{Name: "tdiv", Usage: "seconds/div, e.g. 200u"},
					&cli.StringFlag{Name: "delay", Usage: "trigger delay in seconds"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					td, dl, err := parseTimebase(c.String("tdiv"), c.String("delay"))
					if err != nil {
						return err
					}
					return e.emitResult(e.app.ScopeTimebase(ctx, c.String("alias"), td, dl))
				},
			},
			{
				Name:  "trigger",
				Usage: "set/show the edge trigger",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.StringFlag{Name: "source", Usage: "C1..C4, EX, EX5, LINE"},
					&cli.StringFlag{Name: "mode", Usage: "auto, normal, single, stop"},
					&cli.StringFlag{Name: "level", Usage: "trigger level in volts"},
					&cli.StringFlag{Name: "slope", Usage: "rise, fall, window"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					t := domain.TriggerSetup{Source: c.String("source"), Mode: c.String("mode"), Slope: c.String("slope")}
					if s := c.String("level"); s != "" {
						v, _, err := domain.ParseIn(s, "V")
						if err != nil {
							return domain.Usagef("level: %v", err)
						}
						t.Level = &v
					}
					return e.emitResult(e.app.ScopeTrigger(ctx, c.String("alias"), t))
				},
			},
			{
				Name:  "autoset",
				Usage: "run autoset and show the resulting settings",
				Flags: []cli.Flag{aliasFlag},
				Action: func(ctx context.Context, c *cli.Command) error {
					return e.emitResult(e.app.ScopeAutoset(ctx, c.String("alias")))
				},
			},
			{
				Name:      "measure",
				Usage:     "measure parameters (PAVA); invalid (****) readings are null",
				ArgsUsage: "<ch> <param>... (pkpk freq rms mean max min ampl per rise fall duty ...)",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.DurationFlag{Name: "settle", Usage: "wait before measuring (let acquisitions update)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() < 2 {
						return domain.Usagef("usage: benchctl scope measure <ch> <param>...")
					}
					ch, err := argChannel(c, 0)
					if err != nil {
						return err
					}
					if d := c.Duration("settle"); d > 0 {
						t := time.NewTimer(d)
						select {
						case <-ctx.Done():
							t.Stop()
							return ctx.Err()
						case <-t.C:
						}
					}
					return e.emitResult(e.app.ScopeMeasure(ctx, c.String("alias"), ch, c.Args().Slice()[1:]))
				},
			},
			{
				Name:      "capture",
				Usage:     "capture waveform(s) (WF? DAT2) as volts/seconds",
				ArgsUsage: "<ch>...",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.IntFlag{Name: "points", Usage: "max points per channel (0 = all in memory)", Value: 10000},
					&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "out.csv or out.json (per-channel suffix added for several channels)"},
					&cli.BoolFlag{Name: "raw", Usage: "also save the raw WAVEDESC and DAT2 blocks for audit"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() < 1 {
						return domain.Usagef("usage: benchctl scope capture <ch>... -o out.csv")
					}
					if c.Bool("raw") && c.String("output") == "" {
						return domain.Usagef("--raw saves the blocks next to the --output file; give -o out.csv")
					}
					var out []app.WaveSummary
					for i := 0; i < c.NArg(); i++ {
						ch, err := argChannel(c, i)
						if err != nil {
							return err
						}
						wf, err := e.app.ScopeCapture(ctx, c.String("alias"), ch, c.Int("points"))
						if err != nil {
							return err
						}
						sum := app.Summarize(wf)
						if o := c.String("output"); o != "" {
							path := o
							if c.NArg() > 1 {
								ext := filepath.Ext(o)
								path = fmt.Sprintf("%s_C%d%s", strings.TrimSuffix(o, ext), ch, ext)
							}
							if err := app.WriteWaveformFile(path, wf); err != nil {
								return err
							}
							sum.Files = append(sum.Files, path)
							if c.Bool("raw") {
								base := strings.TrimSuffix(path, filepath.Ext(path))
								for suffix, data := range map[string][]byte{".desc.bin": wf.RawDesc, ".dat2.bin": wf.RawData} {
									if err := os.WriteFile(base+suffix, data, 0o644); err != nil {
										return err
									}
									sum.Files = append(sum.Files, base+suffix)
								}
							}
						}
						out = append(out, sum)
					}
					return e.emit(out)
				},
			},
			{
				Name:  "screenshot",
				Usage: "save the scope screen (same as `benchctl screenshot <scope-alias>`)",
				Flags: append([]cli.Flag{aliasFlag}, screenshotFlags()...),
				Action: func(ctx context.Context, c *cli.Command) error {
					alias, err := e.app.ResolveAlias(c.String("alias"), domain.RoleScope)
					if err != nil {
						return err
					}
					return e.screenshots(ctx, c, []string{alias})
				},
			},
		},
	}
}

func parseTimebase(tdiv, delay string) (*float64, *float64, error) {
	var td, dl *float64
	if tdiv != "" {
		v, _, err := domain.ParseIn(tdiv, "s")
		if err != nil {
			return nil, nil, domain.Usagef("tdiv: %v", err)
		}
		td = &v
	}
	if delay != "" {
		v, _, err := domain.ParseIn(delay, "s")
		if err != nil {
			return nil, nil, domain.Usagef("delay: %v", err)
		}
		dl = &v
	}
	return td, dl, nil
}

// ---------------- dmm ----------------

func dmmCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "dmm",
		Usage: "multimeter",
		Commands: []*cli.Command{
			{
				Name:      "config",
				Usage:     "set the measurement function and range",
				ArgsUsage: "vdc|vac|idc|iac|res|fres|freq|per|cap|diode|cont|temp",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.StringFlag{Name: "range", Usage: "auto or a value, e.g. 6, 600m", Value: "auto"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() != 1 {
						return domain.Usagef("usage: benchctl dmm config <function> [--range auto|<v>]")
					}
					fn := domain.DMMFunction(strings.ToLower(c.Args().First()))
					if _, ok := domain.DMMFunctions[fn]; !ok {
						return domain.Usagef("unknown function %q", fn)
					}
					rng := c.String("range")
					switch fn {
					case "diode", "cont", "temp", "freq", "per":
						if !c.IsSet("range") {
							rng = ""
						}
					}
					return e.emitResult(e.app.DMMConfigure(ctx, c.String("alias"), domain.DMMConfig{Function: fn, Range: rng}))
				},
			},
			{
				Name:  "read",
				Usage: "take readings with statistics",
				Flags: []cli.Flag{
					aliasFlag,
					&cli.IntFlag{Name: "samples", Usage: "number of readings", Value: 1},
					&cli.DurationFlag{Name: "interval", Usage: "time between readings", Value: 100 * time.Millisecond},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					return e.emitResult(e.app.DMMRead(ctx, c.String("alias"), c.Int("samples"), c.Duration("interval")))
				},
			},
		},
	}
}
