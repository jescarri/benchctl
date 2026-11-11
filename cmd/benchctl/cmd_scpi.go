package main

import (
	"context"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/app"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
)

func rawCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "raw",
		Usage:     "send any SCPI command (locked, timed out and error-checked like typed commands)",
		ArgsUsage: "<alias> \"<SCPI>\"",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "block", Usage: "response is an IEEE 488.2 block; save its payload with -o"},
			&cli.BoolFlag{Name: "read", Usage: "read a response even though the command has no '?' (e.g. SCDP)"},
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write the response (or block payload) to a file"},
			&cli.BoolFlag{Name: "opc", Usage: "wait for operation complete after the command (method: --wait)"},
			&cli.StringFlag{Name: "wait", Usage: "completion method: auto (srq if proven, else *STB? poll, else *OPC?), srq, poll, opc; implies --opc"},
			&cli.DurationFlag{Name: "timeout", Usage: "timeout for this command (overrides the global per-operation timeout)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() != 2 {
				return domain.Usagef("usage: benchctl raw <alias> \"<SCPI>\"")
			}
			r, err := e.app.Raw(ctx, c.Args().Get(0), c.Args().Get(1), app.RawOptions{
				Block: c.Bool("block"), Read: c.Bool("read"), OPC: c.Bool("opc"), Wait: c.String("wait"), Timeout: c.Duration("timeout"),
			})
			if err != nil {
				return err
			}
			if o := c.String("output"); o != "" {
				if err := os.WriteFile(o, r.Data, 0o644); err != nil {
					return domain.Usagef("%v", err)
				}
				r.Response = ""
				r.Bytes = len(r.Data)
				return e.emit(map[string]any{"result": r, "file": o})
			}
			if r.Block {
				// never dump binary to stdout
				return e.emit(map[string]any{"result": r, "hint": "use -o to save the block payload"})
			}
			return e.emit(r)
		},
	}
}

func scriptCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "script",
		Usage:     "run a .scpi file in one locked session; stops at the first SCPI error",
		ArgsUsage: "<alias> <file.scpi>",
		Flags: []cli.Flag{
			&cli.DurationFlag{Name: "cmd-timeout", Usage: "timeout per command (default: global --timeout)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() != 2 {
				return domain.Usagef("usage: benchctl script <alias> <file.scpi>")
			}
			f, err := os.Open(c.Args().Get(1))
			if err != nil {
				return domain.Usagef("%v", err)
			}
			defer func() { _ = f.Close() }()
			lines, err := app.ParseScript(f)
			if err != nil {
				return domain.Usagef("%v", err)
			}
			r, err := e.app.Script(ctx, c.Args().Get(0), lines, c.Duration("cmd-timeout"))
			return e.emitPartial(r, err)
		},
	}
}

func transferCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "transfer",
		Usage:     "copy a file to/from instrument mass memory (MMEM:DATA; instrument-dependent)",
		ArgsUsage: "<alias> get|put <instrument-path> <local-path>",
		Flags: []cli.Flag{
			&cli.DurationFlag{Name: "timeout", Value: 60 * time.Second, Usage: "transfer timeout"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() != 4 {
				return domain.Usagef("usage: benchctl transfer <alias> get|put <instrument-path> <local-path>")
			}
			alias, dir, ipath, lpath := c.Args().Get(0), c.Args().Get(1), c.Args().Get(2), c.Args().Get(3)
			ctx, cancel := context.WithTimeout(ctx, c.Duration("timeout"))
			defer cancel()
			switch dir {
			case "get":
				data, err := e.app.TransferGet(ctx, alias, ipath)
				if err != nil {
					return err
				}
				if err := os.WriteFile(lpath, data, 0o644); err != nil {
					return domain.Usagef("%v", err)
				}
				return e.emit(map[string]any{"direction": "get", "from": ipath, "to": lpath, "bytes": len(data)})
			case "put":
				data, err := os.ReadFile(lpath)
				if err != nil {
					return domain.Usagef("%v", err)
				}
				if err := e.app.TransferPut(ctx, alias, ipath, data); err != nil {
					return err
				}
				return e.emit(map[string]any{"direction": "put", "from": lpath, "to": ipath, "bytes": len(data)})
			}
			return domain.Usagef("direction must be get or put")
		},
	}
}
