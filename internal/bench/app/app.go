// Package app holds the benchctl use cases. It depends only on domain and
// port; adapters are injected.
package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// App wires the use cases to their ports.
type App struct {
	Store     port.ConfigStore
	Connector port.Connector
	Scanner   port.Discoverer
	// RoleOf maps a model to its role (adapter knowledge, injected).
	RoleOf func(model string) domain.Role
	// Warn receives human-readable warnings (stderr).
	Warn io.Writer
	// Sleep is time.Sleep; tests replace it.
	Sleep func(time.Duration)
}

func (a *App) sleep(d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (a *App) warnf(format string, args ...any) {
	if a.Warn != nil {
		_, _ = fmt.Fprintf(a.Warn, "warning: "+format+"\n", args...)
	}
}

// conns caches open instruments for the duration of one use case.
type conns struct {
	a *App
	m map[string]port.Instrument
}

func (a *App) conns() *conns { return &conns{a: a, m: map[string]port.Instrument{}} }

func (c *conns) get(ctx context.Context, alias string) (port.Instrument, error) {
	if inst, ok := c.m[alias]; ok {
		return inst, nil
	}
	inst, err := c.a.Connector.Open(ctx, alias)
	if err != nil {
		return nil, err
	}
	c.m[alias] = inst
	return inst, nil
}

func (c *conns) closeAll() {
	for k, inst := range c.m {
		_ = inst.Close()
		delete(c.m, k)
	}
}

func (c *conns) scope(ctx context.Context, alias string) (port.Scope, error) {
	inst, err := c.get(ctx, alias)
	if err != nil {
		return nil, err
	}
	s, ok := inst.(port.Scope)
	if !ok {
		return nil, domain.Usagef("%s (%s) is not a supported oscilloscope", alias, inst.Identity().Model)
	}
	return s, nil
}

func (c *conns) generator(ctx context.Context, alias string) (port.Generator, error) {
	inst, err := c.get(ctx, alias)
	if err != nil {
		return nil, err
	}
	g, ok := inst.(port.Generator)
	if !ok {
		return nil, domain.Usagef("%s (%s) is not a supported function generator", alias, inst.Identity().Model)
	}
	return g, nil
}

func (c *conns) dmm(ctx context.Context, alias string) (port.DMM, error) {
	inst, err := c.get(ctx, alias)
	if err != nil {
		return nil, err
	}
	d, ok := inst.(port.DMM)
	if !ok {
		return nil, domain.Usagef("%s (%s) is not a supported multimeter", alias, inst.Identity().Model)
	}
	return d, nil
}

// bench loads the inventory.
func (a *App) bench() (*domain.Bench, error) { return a.Store.Load() }

// benchForOutputs loads the inventory for an operation that can energize a
// generator output: every configured limit must be a usable number, so a
// typo is a config error instead of "no limit". Shutdown paths (safe) use
// bench, so an invalid limit never keeps an output on.
func (a *App) benchForOutputs() (*domain.Bench, error) {
	b, err := a.bench()
	if err != nil {
		return nil, err
	}
	if err := b.ValidateLimits(); err != nil {
		return nil, domain.Usagef("%s: %v", a.Store.Path(), err)
	}
	return b, nil
}

func (a *App) roleOf(model string) domain.Role {
	if a.RoleOf == nil {
		return domain.RoleUnknown
	}
	return a.RoleOf(model)
}

// ResolveAlias returns alias if set, else the single inventory alias with the given role.
func (a *App) ResolveAlias(alias string, role domain.Role) (string, error) {
	if alias != "" {
		return alias, nil
	}
	b, err := a.bench()
	if err != nil {
		return "", err
	}
	var matches []string
	for name, ref := range b.Instruments {
		if a.roleOf(ref.Model) == role {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", domain.Usagef("no %s in the inventory (%s); run `benchctl discover --save`", role, a.Store.Path())
	case 1:
		return matches[0], nil
	}
	return "", domain.Usagef("several instruments with role %s (%v); pass --alias", role, matches)
}
