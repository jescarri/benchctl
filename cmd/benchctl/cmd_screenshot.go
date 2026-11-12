package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// Generated names (no -o, or -o dir) never replace an existing file, and
	// no file written in this run is replaced (e.g. the same alias twice).
	generated := c.String("output") == ""
	if st, err := os.Stat(c.String("output")); err == nil && st.IsDir() {
		generated = true
	}
	written := map[string]bool{}
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
		if generated || written[path] {
			path, err = writeNew(path, data)
		} else {
			err = os.WriteFile(path, data, 0o644)
		}
		if err != nil {
			return domain.Usagef("%v", err)
		}
		written[path] = true
		out = append(out, shotResult{Alias: alias, File: path, Format: format, Bytes: len(data), Width: info.Width, Height: info.Height})
	}
	return e.emit(out)
}

// shotPath resolves the output path and format for one alias.
// writeNew writes data to path, or to path-2, path-3, ... if it exists, and
// returns the name used. O_EXCL makes it safe against concurrent runs.
func writeNew(path string, data []byte) (string, error) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		p := path
		if i > 1 {
			p = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return "", err
		}
		return p, f.Close()
	}
	return "", fmt.Errorf("no free file name for %s", path)
}

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
