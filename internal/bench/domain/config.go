package domain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Bench is the inventory file (bench.json).
type Bench struct {
	Subnet      string                    `json:"subnet"`
	Instruments map[string]InstrumentRef  `json:"instruments"`
	Wiring      []Link                    `json:"wiring,omitempty"`
	Limits      map[string]map[string]any `json:"limits,omitempty"`
}

// InstrumentRef pins an instrument by serial number.
type InstrumentRef struct {
	Serial    string `json:"serial"`
	Model     string `json:"model"`
	LastIP    string `json:"last_ip"`
	Transport string `json:"transport,omitempty"` // "auto" (default), "vxi11", "raw"
	// SRQ caches the service-request probe: "working" or "unavailable: <why>".
	SRQ string `json:"srq,omitempty"`
}

// Link is one wiring connection, e.g. {"from": "fg:CH1", "to": "scope:C1"}.
// External sources use a non-instrument alias (e.g. "ext:5V") and may state
// what the destination should read.
type Link struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Expect *Expect `json:"expect,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// Expect is an expected reading on the destination of a link.
type Expect struct {
	Function  string  `json:"function"` // dmm function, e.g. "vdc"
	Value     float64 `json:"value"`
	Tolerance string  `json:"tolerance"` // "5%" or absolute "0.1"
}

// Endpoint is a parsed "alias:PORT" reference.
type Endpoint struct {
	Alias string
	Port  string
}

// ParseEndpoint parses "fg:CH1" or "dmm" (port optional).
func ParseEndpoint(s string) Endpoint {
	a, p, _ := strings.Cut(strings.TrimSpace(s), ":")
	return Endpoint{Alias: a, Port: strings.ToUpper(p)}
}

// ChannelNumber extracts the channel number from "CH1", "C2", "1".
func (e Endpoint) ChannelNumber() (int, error) {
	p := strings.TrimLeft(e.Port, "CHch")
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("endpoint %s:%s has no channel number", e.Alias, e.Port)
	}
	return n, nil
}

// Aliases returns the instrument aliases in sorted order.
func (b *Bench) Aliases() []string {
	out := make([]string, 0, len(b.Instruments))
	for k := range b.Instruments {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Limit returns a numeric limit, e.g. Limit("fg", "max_amp_vpp"). An invalid
// configured value reports ok=false; use LimitValue to tell it from "not set".
func (b *Bench) Limit(alias, key string) (float64, bool) {
	v, ok, err := b.LimitValue(alias, key)
	return v, ok && err == nil
}

// LimitValue returns a numeric limit. set is false when the limit is not
// configured; err is non-nil when it is configured but not a finite number
// (e.g. "5volts"), which must never be treated as "no limit".
func (b *Bench) LimitValue(alias, key string) (v float64, set bool, err error) {
	raw, ok := b.Limits[alias][key]
	if !ok {
		return 0, false, nil
	}
	switch x := raw.(type) {
	case float64:
		v = x
	case int:
		v = float64(x)
	case string:
		q, perr := ParseQuantity(x)
		if perr != nil {
			return 0, true, fmt.Errorf("limit %s.%s = %q: %v", alias, key, x, perr)
		}
		v = q.Value
	default:
		return 0, true, fmt.Errorf("limit %s.%s = %v: not a number", alias, key, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, true, fmt.Errorf("limit %s.%s = %v: must be a finite, non-negative number", alias, key, raw)
	}
	return v, true, nil
}

// ValidateLimits checks every configured limit is a usable number.
func (b *Bench) ValidateLimits() error {
	for alias, m := range b.Limits {
		for key := range m {
			if _, _, err := b.LimitValue(alias, key); err != nil {
				return Usagef("%v", err)
			}
		}
	}
	return nil
}
