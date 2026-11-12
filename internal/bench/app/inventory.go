package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// DiscoverResult is the output of Discover.
type DiscoverResult struct {
	Subnet      string                          `json:"subnet"`
	Found       []DiscoveredInstrument          `json:"found"`
	Saved       bool                            `json:"saved"`
	Config      string                          `json:"config,omitempty"`
	Instruments map[string]domain.InstrumentRef `json:"instruments,omitempty"`
}

// DiscoveredInstrument is one discovery hit with the alias it has (or would get).
type DiscoveredInstrument struct {
	port.Found
	Alias string `json:"alias"`
}

var defaultAlias = map[domain.Role]string{
	domain.RoleScope: "scope", domain.RoleGenerator: "fg", domain.RoleDMM: "dmm",
	domain.RolePSU: "psu", domain.RoleAnalyzer: "sa", domain.RoleUnknown: "inst",
}

// Discover scans subnet (or the inventory's subnet) and optionally saves the
// results, keeping existing aliases for known serial numbers.
func (a *App) Discover(ctx context.Context, subnet string, save bool) (DiscoverResult, error) {
	b, err := a.bench()
	if err != nil {
		return DiscoverResult{}, err
	}
	if subnet == "" {
		subnet = b.Subnet
	}
	if subnet == "" {
		return DiscoverResult{}, domain.Usagef("no subnet given and none in the inventory; pass --subnet CIDR")
	}
	found, err := a.Scanner.Scan(ctx, subnet)
	if err != nil {
		return DiscoverResult{}, err
	}
	res := DiscoverResult{Subnet: subnet}
	bySerial := map[string]string{}
	for alias, ref := range b.Instruments {
		bySerial[ref.Serial] = alias
	}
	for _, f := range found {
		alias, ok := bySerial[f.Identity.Serial]
		if !ok {
			alias = nextAlias(b, defaultAlias[f.Role])
			bySerial[f.Identity.Serial] = alias
		}
		ref := b.Instruments[alias]
		ref.Serial, ref.Model, ref.LastIP = f.Identity.Serial, f.Identity.Model, f.IP
		b.Instruments[alias] = ref
		res.Found = append(res.Found, DiscoveredInstrument{Found: f, Alias: alias})
	}
	if save {
		b.Subnet = subnet
		if err := a.Store.Save(b); err != nil {
			return res, err
		}
		res.Saved = true
		res.Config = a.Store.Path()
		res.Instruments = b.Instruments
	}
	return res, nil
}

func nextAlias(b *domain.Bench, base string) string {
	if base == "" {
		base = "inst"
	}
	if _, taken := b.Instruments[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		a := fmt.Sprintf("%s%d", base, i)
		if _, taken := b.Instruments[a]; !taken {
			return a
		}
	}
}

// InstrumentStatus is one entry of Status.
type InstrumentStatus struct {
	Alias     string           `json:"alias"`
	Reachable bool             `json:"reachable"`
	Identity  *domain.Identity `json:"identity,omitempty"`
	Role      domain.Role      `json:"role,omitempty"`
	Transport string           `json:"transport,omitempty"`
	State     any              `json:"state,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// ScopeSnapshot is the scope part of Status.
type ScopeSnapshot struct {
	Channels []domain.ChannelState `json:"channels"`
	Timebase domain.Timebase       `json:"timebase"`
	Trigger  domain.TriggerState   `json:"trigger"`
}

// Status reports identity and a settings snapshot for each alias (all if none given).
// It returns an unreachable error if any instrument could not be reached.
func (a *App) Status(ctx context.Context, aliases []string) ([]InstrumentStatus, error) {
	b, err := a.bench()
	if err != nil {
		return nil, err
	}
	if len(aliases) == 0 {
		aliases = b.Aliases()
	}
	if len(aliases) == 0 {
		return nil, domain.Usagef("inventory is empty; run `benchctl discover --save`")
	}
	c := a.conns()
	defer c.closeAll()
	var out []InstrumentStatus
	unreachable := 0
	var firstErr error
	for _, alias := range aliases {
		st := InstrumentStatus{Alias: alias}
		inst, err := c.get(ctx, alias)
		if err != nil {
			st.Error = err.Error()
			if domain.KindOf(err) == domain.KindUnreachable {
				unreachable++
			} else if firstErr == nil {
				firstErr = err
			}
			out = append(out, st)
			continue
		}
		id := inst.Identity()
		st.Reachable, st.Identity, st.Role, st.Transport = true, &id, inst.Role(), inst.SCPI().TransportName()
		snap, err := snapshot(ctx, inst)
		if err != nil {
			st.Error = err.Error()
		}
		st.State = snap
		out = append(out, st)
	}
	if unreachable > 0 {
		return out, domain.Unreachablef("%d of %d instruments unreachable", unreachable, len(aliases))
	}
	// Other open failures (unknown alias, lock timeout, serial mismatch)
	// must not exit 0 either.
	return out, firstErr
}

func snapshot(ctx context.Context, inst port.Instrument) (any, error) {
	switch d := inst.(type) {
	case port.Scope:
		var s ScopeSnapshot
		for ch := 1; ch <= d.Channels(); ch++ {
			cs, err := d.ChannelState(ctx, ch)
			if err != nil {
				return s, err
			}
			s.Channels = append(s.Channels, cs)
		}
		var err error
		if s.Timebase, err = d.Timebase(ctx); err != nil {
			return s, err
		}
		s.Trigger, err = d.Trigger(ctx)
		return s, err
	case port.Generator:
		var out []domain.GenChannelState
		for ch := 1; ch <= d.Channels(); ch++ {
			st, err := d.State(ctx, ch)
			if err != nil {
				return out, err
			}
			st.Raw = nil
			out = append(out, st)
		}
		return out, nil
	case port.DMM:
		return d.Config(ctx)
	}
	return nil, nil
}

// Caps returns the static capability sheet for alias's model.
func (a *App) Caps(alias string) (Capabilities, error) {
	b, err := a.bench()
	if err != nil {
		return Capabilities{}, err
	}
	ref, ok := b.Instruments[alias]
	if !ok {
		return Capabilities{}, domain.Usagef("unknown alias %q (known: %v)", alias, b.Aliases())
	}
	c, ok := capsFor(ref.Model)
	if !ok {
		known := make([]string, 0, len(capSheets))
		for k := range capSheets {
			known = append(known, k)
		}
		sort.Strings(known)
		return Capabilities{}, domain.Usagef("no capability sheet for %s (have: %v)", ref.Model, known)
	}
	return c, nil
}

// PingResult is the connection test for one instrument.
type PingResult struct {
	Alias     string           `json:"alias"`
	Reachable bool             `json:"reachable"`
	IP        string           `json:"ip,omitempty"`
	Transport string           `json:"transport,omitempty"`
	Identity  *domain.Identity `json:"identity,omitempty"`
	Serial    string           `json:"serial_pinned"`
	SerialOK  bool             `json:"serial_ok"`
	Connect   string           `json:"connect_time,omitempty"`
	RoundTrip string           `json:"idn_round_trip,omitempty"`
	SRQ       string           `json:"srq,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// Ping is the connection test: for each alias (all if none) it opens the
// instrument the normal way (last IP, serial check, rediscovery), times the
// connection and a few *IDN? round trips, and reports the transport in use.
// It returns an unreachable error if any instrument failed.
func (a *App) Ping(ctx context.Context, aliases []string, rounds int) ([]PingResult, error) {
	b, err := a.bench()
	if err != nil {
		return nil, err
	}
	if len(aliases) == 0 {
		aliases = b.Aliases()
	}
	if len(aliases) == 0 {
		return nil, domain.Usagef("inventory is empty; run `benchctl discover --save`")
	}
	if rounds < 1 {
		rounds = 3
	}
	var out []PingResult
	failed := 0
	for _, alias := range aliases {
		r := PingResult{Alias: alias, Serial: b.Instruments[alias].Serial}
		start := time.Now()
		inst, err := a.Connector.Open(ctx, alias)
		if err != nil {
			r.Error = err.Error()
			failed++
			out = append(out, r)
			continue
		}
		r.Connect = time.Since(start).Round(time.Microsecond).String()
		id := inst.Identity()
		r.Reachable, r.Identity, r.Transport = true, &id, inst.SCPI().TransportName()
		r.SerialOK = r.Serial == "" || id.Serial == r.Serial
		var total time.Duration
		for i := 0; i < rounds; i++ {
			t0 := time.Now()
			if _, err := inst.SCPI().Query(ctx, "*IDN?"); err != nil {
				r.Error = err.Error()
				break
			}
			total += time.Since(t0)
		}
		if r.Error == "" {
			r.RoundTrip = (total / time.Duration(rounds)).Round(time.Microsecond).String()
		} else {
			failed++
		}
		r.SRQ = inst.SCPI().SRQStatus()
		_ = inst.Close()
		if nb, err := a.bench(); err == nil { // the connector may have rediscovered the IP
			r.IP = nb.Instruments[alias].LastIP
		}
		out = append(out, r)
	}
	if failed > 0 {
		return out, domain.Unreachablef("%d of %d instruments failed the connection test", failed, len(aliases))
	}
	return out, nil
}
