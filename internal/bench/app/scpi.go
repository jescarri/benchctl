package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// RawOptions tunes a raw command.
type RawOptions struct {
	Block   bool          // response is an IEEE 488.2 block; return its payload
	Read    bool          // read a response even without '?' (e.g. Siglent SCDP)
	OPC     bool          // wait for operation complete after the command
	Wait    string        // completion method: auto, srq, poll, opc (implies OPC)
	Timeout time.Duration // per-command timeout (0 = caller's context)
}

// RawResult is the result of one raw command.
type RawResult struct {
	Command  string `json:"command"`
	Query    bool   `json:"query"`
	Block    bool   `json:"block,omitempty"`
	Response string `json:"response,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	Data     []byte `json:"-"`
	OPC      bool   `json:"opc_waited,omitempty"`
	Wait     string `json:"wait_method,omitempty"`
	Elapsed  string `json:"elapsed"`
}

// isQuery reports whether any message unit's header ends in '?'.
func isQuery(cmd string) bool {
	for _, unit := range strings.Split(cmd, ";") {
		hdr, _, _ := strings.Cut(strings.TrimSpace(unit), " ")
		if strings.Contains(hdr, "?") {
			return true
		}
	}
	return false
}

// Raw sends any SCPI command with the same locking, timeouts and error
// checking as typed commands. Queries return their response; other commands
// are followed by an error check.
func (a *App) Raw(ctx context.Context, alias, cmd string, o RawOptions) (RawResult, error) {
	c := a.conns()
	defer c.closeAll()
	inst, err := c.get(ctx, alias)
	if err != nil {
		return RawResult{}, err
	}
	defer a.rememberSRQ(alias, inst)
	return a.rawOn(ctx, inst.SCPI(), cmd, o)
}

func (a *App) rawOn(ctx context.Context, s port.SCPI, cmd string, o RawOptions) (RawResult, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	start := time.Now()
	res := RawResult{Command: cmd, Query: isQuery(cmd) || o.Read}
	if res.Query {
		if o.Block {
			data, err := s.QueryBlock(ctx, cmd)
			if err != nil {
				return res, err
			}
			res.Data, res.Bytes, res.Block = data, len(data), true
		} else {
			b, err := s.QueryBytes(ctx, cmd)
			if err != nil {
				return res, err
			}
			res.Response = strings.TrimRight(string(b), "\r\n")
			res.Data = b
		}
	} else if o.OPC || o.Wait != "" {
		m, err := s.RunAndWait(ctx, cmd, o.Wait)
		res.Wait = m
		if err != nil {
			return res, err
		}
		res.OPC = true
	} else if err := s.Set(ctx, cmd); err != nil {
		return res, err
	}
	if res.Query && (o.OPC || o.Wait != "") {
		if err := s.WaitComplete(ctx); err != nil {
			return res, err
		}
		res.OPC, res.Wait = true, "opc"
	}
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

// ScriptResult is the outcome of Script.
type ScriptResult struct {
	Alias    string      `json:"alias"`
	Results  []RawResult `json:"results"`
	FailedAt int         `json:"failed_at,omitempty"` // 1-based line number, 0 if none
	Error    string      `json:"error,omitempty"`
}

// ParseScript returns the commands in a .scpi file: one per line; blank lines
// and lines starting with "//" or "# " (hash + space) are comments, so
// IEEE 488.2 block arguments such as "#9..." are not mistaken for comments.
func ParseScript(r io.Reader) ([]ScriptLine, error) {
	var out []ScriptLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") || line == "#" || strings.HasPrefix(line, "# ") {
			continue
		}
		out = append(out, ScriptLine{Line: n, Command: line})
	}
	return out, sc.Err()
}

// ScriptLine is one command with its source line number.
type ScriptLine struct {
	Line    int
	Command string
}

// Script runs commands in one session, stopping at the first error.
func (a *App) Script(ctx context.Context, alias string, lines []ScriptLine, perCmd time.Duration) (ScriptResult, error) {
	res := ScriptResult{Alias: alias}
	c := a.conns()
	defer c.closeAll()
	inst, err := c.get(ctx, alias)
	if err != nil {
		return res, err
	}
	defer a.rememberSRQ(alias, inst)
	for _, l := range lines {
		r, err := a.rawOn(ctx, inst.SCPI(), l.Command, RawOptions{Timeout: perCmd})
		if err != nil {
			res.FailedAt, res.Error = l.Line, err.Error()
			return res, err
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

// TransferGet reads a file from instrument mass memory (MMEM:DATA?).
// Support is instrument-dependent.
func (a *App) TransferGet(ctx context.Context, alias, path string) ([]byte, error) {
	r, err := a.Raw(ctx, alias, fmt.Sprintf("MMEM:DATA? %q", path), RawOptions{Block: true})
	if err != nil {
		return nil, err
	}
	return r.Data, nil
}

// TransferPut writes a file to instrument mass memory (MMEM:DATA "path",#...).
func (a *App) TransferPut(ctx context.Context, alias, path string, data []byte) error {
	c := a.conns()
	defer c.closeAll()
	inst, err := c.get(ctx, alias)
	if err != nil {
		return err
	}
	l := strconv.Itoa(len(data))
	cmd := fmt.Sprintf("MMEM:DATA %q,#%d%s", path, len(l), l) + string(data)
	return inst.SCPI().Set(ctx, cmd)
}

// SafeResult reports what `safe` did per instrument.
type SafeResult struct {
	Alias  string `json:"alias"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

// Safe switches off every output on every reachable instrument.
func (a *App) Safe(ctx context.Context) ([]SafeResult, error) {
	b, err := a.bench()
	if err != nil {
		return nil, err
	}
	c := a.conns()
	defer c.closeAll()
	return a.safeWith(ctx, c, b)
}

func (a *App) safeWith(ctx context.Context, c *conns, b *domain.Bench) ([]SafeResult, error) {
	var out []SafeResult
	var firstErr error
	var unconfirmed []string // outputs not confirmed off
	unreachable := false
	for _, alias := range b.Aliases() {
		ref := b.Instruments[alias]
		role := a.roleOf(ref.Model)
		if role != domain.RoleGenerator && role != domain.RolePSU {
			continue
		}
		r := SafeResult{Alias: alias}
		inst, err := c.get(ctx, alias)
		if err != nil {
			r.Action, r.Error = "unreachable", err.Error()
			unconfirmed = append(unconfirmed, alias)
			unreachable = unreachable || domain.KindOf(err) == domain.KindUnreachable
			out = append(out, r)
			continue
		}
		o, ok := inst.(port.Outputs)
		if !ok {
			r.Action = "no output control in driver"
			unconfirmed = append(unconfirmed, alias)
			out = append(out, r)
			continue
		}
		if err := c.allOff(ctx, alias, o); err != nil {
			r.Action, r.Error = "failed", err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			r.Action = "all outputs off"
		}
		out = append(out, r)
	}
	if firstErr == nil && len(unconfirmed) > 0 {
		// An output that could not be switched off is not safe: never exit 0.
		msg := fmt.Sprintf("outputs not confirmed off on %s", strings.Join(unconfirmed, ", "))
		if unreachable {
			return out, domain.Unreachablef("%s", msg)
		}
		return out, domain.Instrumentf("%s", msg)
	}
	return out, firstErr
}

// rememberSRQ stores a fresh SRQ probe result in the inventory so later
// invocations skip the probe. Results that only reflect the transport in use
// (e.g. --transport raw) or that came from the cache are not stored.
func (a *App) rememberSRQ(alias string, inst port.Instrument) {
	st := inst.SCPI().SRQStatus()
	if st == "unknown" || strings.Contains(st, "cached in inventory") || strings.Contains(st, "transport has no interrupt channel") {
		return
	}
	b, err := a.bench()
	if err != nil {
		return
	}
	ref, ok := b.Instruments[alias]
	if !ok || ref.SRQ == st {
		return
	}
	ref.SRQ = st
	b.Instruments[alias] = ref
	if err := a.Store.Save(b); err != nil {
		a.warnf("could not cache SRQ status for %s: %v", alias, err)
	}
}
