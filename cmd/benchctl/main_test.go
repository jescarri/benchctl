package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/app"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(context.Background(), append([]string{"benchctl"}, args...), strings.NewReader(""), &out, &errw)
	return code, out.String(), errw.String()
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExitCodes(t *testing.T) {
	empty := writeConfig(t, `{"instruments":{}}`)
	inv := writeConfig(t, `{"instruments":{"fg":{"serial":"X","model":"SDG1032X","last_ip":"127.0.0.1","transport":"raw"}},
		"wiring":[{"from":"fg:CH1","to":"scope:C1"}]}`)
	cases := []struct {
		args []string
		code int
		in   string
	}{
		{[]string{"version"}, 0, ""},
		{[]string{"--no-such-flag", "status"}, 2, "usage"},
		{[]string{"--config", empty, "status"}, 2, "inventory is empty"},
		{[]string{"--config", empty, "caps"}, 2, "usage"},
		{[]string{"--config", empty, "fg", "output", "1", "maybe"}, 2, "on or off"},
		{[]string{"--config", empty, "fg", "set", "x"}, 2, "invalid channel"},
		{[]string{"--config", empty, "fg", "set", "1", "--amp", "1Vrmsx"}, 2, "amp"},
		{[]string{"--config", empty, "scope", "setup", "1", "--on", "--off"}, 2, "exclusive"},
		{[]string{"--config", empty, "dmm", "config", "ohms"}, 2, "unknown function"},
		{[]string{"--config", empty, "fg", "state"}, 2, "no generator"},
		{[]string{"--config", inv, "--transport", "raw", "--lock-wait", "0s", "fg", "state"}, 3, "unreachable"},
		{[]string{"--config", empty, "apply", "/nonexistent.yaml"}, 2, "no such file"},
		{[]string{"--config", empty, "wiring", "show"}, 2, "no wiring"},
	}
	for _, c := range cases {
		code, out, errw := runCLI(t, c.args...)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d (stderr %s)", c.args, code, c.code, errw)
			continue
		}
		if c.code == 0 {
			if !json.Valid([]byte(out)) {
				t.Errorf("%v: stdout is not JSON: %q", c.args, out)
			}
			continue
		}
		var e map[string]any
		line := strings.TrimSpace(errw[strings.LastIndex(strings.TrimSpace(errw), "\n")+1:])
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("%v: stderr last line not JSON: %q", c.args, errw)
			continue
		}
		if !strings.Contains(strings.ToLower(e["error"].(string)+e["kind"].(string)), c.in) {
			t.Errorf("%v: error %q does not mention %q", c.args, e["error"], c.in)
		}
		if out != "" {
			t.Errorf("%v: unexpected stdout on error: %q", c.args, out)
		}
	}
}

func TestWiringShowHuman(t *testing.T) {
	cfg := writeConfig(t, `{"instruments":{"fg":{"serial":"A","model":"SDG1032X"},"scope":{"serial":"B","model":"SDS1104X-E"}},
		"wiring":[{"from":"fg:CH1","to":"scope:C1"}]}`)
	code, out, errw := runCLI(t, "--config", cfg, "--human", "wiring", "show")
	if code != 0 || !strings.Contains(out, "BNC-BNC") || !strings.Contains(out, "INSTRUCTION") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
}

func TestRenderHuman(t *testing.T) {
	var b bytes.Buffer
	v := map[string]any{
		"alias": "scope", "value": 1.5, "ok": true, "none": nil, "list": []any{1, 2},
		"rows":   []map[string]any{{"a": 1, "b": "x"}, {"a": 2, "c": 3}},
		"blocks": []map[string]any{{"a": 1, "n": map[string]any{"deep": "yes"}}},
		"nested": map[string]any{"k": "v"},
	}
	if err := renderHuman(&b, v); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	for _, want := range []string{"alias:", "scope", "1.5", "true", "none:", "-", "1, 2", "A", "B", "C", "[0]", "deep:", "yes", "k:", "v"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestShotPath(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		output, format, alias string
		many                  bool
		wantPath, wantFormat  string
	}{
		{"", "", "scope", false, "scope-T.png", "png"},
		{"", "bmp", "fg", false, "fg-T.bmp", "bmp"},
		{dir, "", "dmm", false, filepath.Join(dir, "dmm-T.png"), "png"},
		{"x.bmp", "", "scope", false, "x.bmp", "bmp"},
		{"x.png", "", "scope", true, "x-scope.png", "png"},
		{"x", "", "scope", false, "x", "png"},
	}
	for _, c := range cases {
		p, f, err := shotPath(c.output, c.format, c.alias, "T", c.many)
		if err != nil || p != c.wantPath || f != c.wantFormat {
			t.Errorf("%+v → %q %q %v", c, p, f, err)
		}
	}
	if _, _, err := shotPath("", "gif", "a", "T", false); err == nil {
		t.Error("gif should be rejected")
	}
}

func TestConfirmWiring(t *testing.T) {
	steps := []app.WiringStep{{Step: 1, Instruction: "Connect fg CH1 → scope C1"}}
	for in, ok := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "": false, "maybe\n": false} {
		var w bytes.Buffer
		err := confirmWiring(&w, strings.NewReader(in), steps)
		if (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
		if !strings.Contains(w.String(), "1. Connect fg CH1") {
			t.Errorf("instructions not shown: %q", w.String())
		}
	}
}

func TestPlanLinks(t *testing.T) {
	if l, err := planLinks(""); l != nil || err != nil {
		t.Fatal("no plan → inventory")
	}
	p := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(p, []byte("wiring:\n  - {from: \"fg:CH1\", to: \"scope:C1\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := planLinks(p); err != nil || len(l) != 1 || l[0].To != "scope:C1" {
		t.Fatalf("%+v %v", l, err)
	}
	if err := os.WriteFile(p, []byte("steps:\n  - wait: 1s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := planLinks(p); err == nil {
		t.Fatal("plan without wiring")
	}
}
