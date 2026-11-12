// Command benchctl drives Siglent bench instruments over SCPI for agentic workflows.
//
// All commands print JSON to stdout (--human for tables), diagnostics to
// stderr, and exit with 0 ok, 1 instrument/SCPI error, 2 usage error,
// 3 instrument unreachable, 4 verification failed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/jescarri/benchctl/internal/bench/adapter/config"
	"github.com/jescarri/benchctl/internal/bench/adapter/connect"
	"github.com/jescarri/benchctl/internal/bench/adapter/discovery"
	"github.com/jescarri/benchctl/internal/bench/adapter/siglent"
	"github.com/jescarri/benchctl/internal/bench/app"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

// Set by the linker (see Makefile).
var (
	version = "dev"
	commit  = "none"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// env bundles what every command needs.
type env struct {
	app   *app.App
	in    io.Reader
	out   io.Writer
	errw  io.Writer
	human bool
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{in: stdin, out: stdout, errw: stderr}
	root := newRoot(e)
	setUsageErrors(root)
	err := root.Run(ctx, args)
	if err != nil {
		if ctx.Err() != nil && domain.KindOf(err) == domain.KindInstrument {
			err = fmt.Errorf("interrupted: %w", err)
		}
		writeError(stderr, err)
	}
	return domain.ExitCode(err)
}

func writeError(w io.Writer, err error) {
	kind := domain.KindOf(err)
	b, _ := json.Marshal(map[string]any{"error": err.Error(), "kind": kind.String(), "exit_code": int(kind)})
	_, _ = fmt.Fprintln(w, string(b))
}

// setUsageErrors makes flag-parsing failures exit with the usage code.
func setUsageErrors(c *cli.Command) {
	c.OnUsageError = func(_ context.Context, _ *cli.Command, err error, _ bool) error {
		return domain.Usagef("%v", err)
	}
	for _, sub := range c.Commands {
		setUsageErrors(sub)
	}
}

func newRoot(e *env) *cli.Command {
	return &cli.Command{
		Name:                  "benchctl",
		Usage:                 "drive Siglent bench instruments (scope, function generator, DMM) over SCPI",
		Version:               version + " (" + commit + ")",
		Writer:                e.out,
		ErrWriter:             e.errw,
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Usage: "inventory file", Value: config.DefaultPath(), Sources: cli.EnvVars("BENCHCTL_CONFIG")},
			&cli.DurationFlag{Name: "timeout", Usage: "default per-operation SCPI I/O timeout", Value: 5 * time.Second, Sources: cli.EnvVars("BENCHCTL_TIMEOUT")},
			&cli.BoolFlag{Name: "human", Usage: "print tables instead of JSON"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log SCPI traffic to stderr"},
			&cli.StringFlag{Name: "transport", Usage: "override transport: auto (= vxi11) or raw (last resort; never used unless asked for)", Sources: cli.EnvVars("BENCHCTL_TRANSPORT")},
			&cli.StringFlag{Name: "ip", Usage: "override the instrument IP (skips inventory address and rediscovery)"},
			&cli.DurationFlag{Name: "lock-wait", Usage: "how long to wait for another benchctl process to release an instrument", Value: 2 * time.Second},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			store := config.New(c.String("config"))
			// The raw socket is a last resort: only probed when asked for.
			scanner := &discovery.Scanner{Raw: c.String("transport") == connect.TransportRaw}
			conn := &connect.Connector{
				Store: store, Discoverer: scanner, Transport: c.String("transport"), IP: c.String("ip"),
				Log: e.errw, LockWait: c.Duration("lock-wait"), IOTimeout: c.Duration("timeout"),
			}
			if c.Bool("verbose") {
				conn.Trace = e.errw
			}
			e.human = c.Bool("human")
			e.app = &app.App{Store: store, Connector: conn, Scanner: scanner, RoleOf: siglent.RoleFor, Warn: e.errw}
			return ctx, nil
		},
		Commands: []*cli.Command{
			discoverCmd(e), pingCmd(e), statusCmd(e), capsCmd(e),
			fgCmd(e), scopeCmd(e), dmmCmd(e),
			screenshotCmd(e), rawCmd(e), scriptCmd(e), transferCmd(e),
			applyCmd(e), wiringCmd(e), safeCmd(e), versionCmd(e),
		},
	}
}

// emit prints v as JSON or a human table.
func (e *env) emit(v any) error {
	if e.human {
		return renderHuman(e.out, v)
	}
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emitResult prints v on success and passes err through.
func (e *env) emitResult(v any, err error) error {
	if err != nil {
		return err
	}
	return e.emit(v)
}

// emitPartial prints v even when err is set, for commands whose partial
// results matter (status, wiring verify, apply, script).
func (e *env) emitPartial(v any, err error) error {
	if perr := e.emit(v); perr != nil && err == nil {
		err = perr
	}
	return err
}

func versionCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print version",
		Action: func(ctx context.Context, c *cli.Command) error {
			return e.emit(map[string]string{"version": version, "commit": commit})
		},
	}
}
