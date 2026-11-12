package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/jescarri/benchctl/internal/bench/adapter/connect"
	"github.com/jescarri/benchctl/internal/bench/app"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func discoverCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "discover",
		Usage: "scan a subnet for SCPI instruments (TCP 5025 + *IDN?)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "subnet", Usage: "CIDR to scan (default: inventory subnet)"},
			&cli.BoolFlag{Name: "save", Usage: "write results to the inventory, pinned by serial number"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := e.app.Discover(ctx, c.String("subnet"), c.Bool("save"))
			if err == nil && len(res.Found) == 0 && c.String("transport") != connect.TransportRaw {
				_, _ = fmt.Fprintln(e.errw, "benchctl: no VXI-11 instruments found; if yours has no VXI-11, rerun with --transport raw (last resort)")
			}
			return e.emitResult(res, err)
		},
	}
}

func statusCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "identity and current settings snapshot",
		ArgsUsage: "[alias...]",
		Action: func(ctx context.Context, c *cli.Command) error {
			st, err := e.app.Status(ctx, c.Args().Slice())
			if st == nil {
				return err
			}
			return e.emitPartial(st, err)
		},
	}
}

func capsCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "caps",
		Usage:     "static capability sheet for the instrument's model",
		ArgsUsage: "<alias>",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() != 1 {
				return domain.Usagef("usage: benchctl caps <alias>")
			}
			return e.emitResult(e.app.Caps(c.Args().First()))
		},
	}
}

func safeCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "safe",
		Usage: "switch off every output on every reachable instrument",
		Action: func(ctx context.Context, c *cli.Command) error {
			r, err := e.app.Safe(ctx)
			return e.emitPartial(r, err)
		},
	}
}

func wiringCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "wiring",
		Usage: "human-in-the-loop wiring protocol",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "step-by-step connection instructions (inventory wiring, or a plan's with --plan)",
				Flags: []cli.Flag{planFlag},
				Action: func(ctx context.Context, c *cli.Command) error {
					links, err := planLinks(c.String("plan"))
					if err != nil {
						return err
					}
					return e.emitResult(e.app.WiringShow(links))
				},
			},
			{
				Name:  "verify",
				Usage: "electrically verify every link (drives test signals; outputs left OFF)",
				Flags: []cli.Flag{
					planFlag,
					&cli.BoolFlag{Name: "confirm", Usage: "print the instructions and wait for the human to confirm (y) on stdin first"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					links, err := planLinks(c.String("plan"))
					if err != nil {
						return err
					}
					if c.Bool("confirm") {
						steps, err := e.app.WiringShow(links)
						if err != nil {
							return err
						}
						if err := confirmWiring(e.errw, e.in, steps); err != nil {
							return err
						}
					}
					r, err := e.app.WiringVerify(ctx, links)
					if r.Links == nil {
						return err
					}
					return e.emitPartial(r, err)
				},
			},
		},
	}
}

func applyCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "apply",
		Usage:     "run a declarative plan: setup, measurements and assertions",
		ArgsUsage: "<plan.yaml>",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() != 1 {
				return domain.Usagef("usage: benchctl apply <plan.yaml>")
			}
			path := c.Args().First()
			f, err := os.Open(path)
			if err != nil {
				return domain.Usagef("%v", err)
			}
			defer func() { _ = f.Close() }()
			p, err := app.LoadPlan(f)
			if err != nil {
				return err
			}
			r, err := e.app.Apply(ctx, p, filepath.Dir(path))
			return e.emitPartial(r, err)
		},
	}
}

var planFlag = &cli.StringFlag{Name: "plan", Usage: "use the `wiring:` section of this apply plan instead of the inventory wiring"}

// planLinks returns the wiring of a plan file, or nil (inventory wiring).
func planLinks(path string) ([]domain.Link, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, domain.Usagef("%v", err)
	}
	defer func() { _ = f.Close() }()
	p, err := app.LoadPlan(f)
	if err != nil {
		return nil, err
	}
	if len(p.Wiring) == 0 {
		return nil, domain.Usagef("plan %s has no wiring section", path)
	}
	return p.Wiring, nil
}

// confirmWiring is ADR §7 step 2: show the instructions and wait for the
// human. Anything but y/yes aborts with a usage error (exit 2).
func confirmWiring(w io.Writer, in io.Reader, steps []app.WiringStep) error {
	_, _ = fmt.Fprintln(w, "Connect the bench as follows:")
	for _, s := range steps {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", s.Step, s.Instruction)
	}
	_, _ = fmt.Fprint(w, "Done? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return domain.Usagef("wiring not confirmed (no answer on stdin)")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return domain.Usagef("wiring not confirmed; aborted before driving any signal")
}

func pingCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "ping",
		Usage:     "connection test: reach each instrument, check its serial, time *IDN? round trips",
		ArgsUsage: "[alias...]",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "rounds", Usage: "*IDN? round trips to average", Value: 3},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			r, err := e.app.Ping(ctx, c.Args().Slice(), c.Int("rounds"))
			if r == nil {
				return err
			}
			return e.emitPartial(r, err)
		},
	}
}
