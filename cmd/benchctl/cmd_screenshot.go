package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/jescarri/benchctl/internal/bench/adapter/bmp"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

type shotResult struct {
	Alias  string `json:"alias"`
	File   string `json:"file"`
	Format string `json:"format"`
	Bytes  int    `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func screenshotFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "output", Aliases: []string{"o"},
			Usage: "file (.png or .bmp) or existing directory; default <alias>-<timestamp>.png in the current directory"},
		&cli.StringFlag{Name: "format", Usage: "png or bmp (default: from the file extension, else png)"},
	}
}

func screenshotCmd(e *env) *cli.Command {
	return &cli.Command{
		Name:      "screenshot",
		Usage:     "save the screen of one or more instruments to disk (SCDP; PNG or BMP)",
		ArgsUsage: "<alias>...",
		Flags:     screenshotFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() == 0 {
				return domain.Usagef("usage: benchctl screenshot <alias>... [-o file|dir]")
			}
			return e.screenshots(ctx, c, c.Args().Slice())
		},
	}
}

func (e *env) screenshots(ctx context.Context, c *cli.Command, aliases []string) error {
	stamp := time.Now().Format("20060102-150405")
	var out []shotResult
	for _, alias := range aliases {
		path, format, err := shotPath(c.String("output"), c.String("format"), alias, stamp, len(aliases) > 1)
		if err != nil {
			return err
		}
		raw, err := e.app.Screenshot(ctx, alias)
		if err != nil {
			return err
		}
		info, err := bmp.ParseInfo(raw)
		if err != nil {
			return domain.Instrumentf("%s: %v", alias, err)
		}
		data := raw
		if format == "png" {
			if data, err = bmp.ToPNG(raw); err != nil {
				return domain.Instrumentf("%s: convert to PNG: %v", alias, err)
			}
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return domain.Usagef("%v", err)
		}
		out = append(out, shotResult{Alias: alias, File: path, Format: format, Bytes: len(data), Width: info.Width, Height: info.Height})
	}
	return e.emit(out)
}

// shotPath resolves the output path and format for one alias.
func shotPath(output, format, alias, stamp string, many bool) (string, string, error) {
	format = strings.ToLower(format)
	if format != "" && format != "png" && format != "bmp" {
		return "", "", domain.Usagef("unknown format %q (png, bmp)", format)
	}
	dir, file := ".", ""
	if output != "" {
		if st, err := os.Stat(output); err == nil && st.IsDir() {
			dir = output
		} else {
			file = output
		}
	}
	if file == "" {
		if format == "" {
			format = "png"
		}
		return filepath.Join(dir, fmt.Sprintf("%s-%s.%s", alias, stamp, format)), format, nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(file), "."))
	switch {
	case format == "" && (ext == "png" || ext == "bmp"):
		format = ext
	case format == "":
		format = "png"
	}
	if many {
		file = strings.TrimSuffix(file, filepath.Ext(file)) + "-" + alias + filepath.Ext(file)
	}
	return file, format, nil
}
