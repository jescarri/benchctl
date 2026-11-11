package domain

import (
	"fmt"
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

// Limit returns a numeric limit, e.g. Limit("fg", "max_amp_vpp").
func (b *Bench) Limit(alias, key string) (float64, bool) {
	m, ok := b.Limits[alias]
	if !ok {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		q, err := ParseQuantity(v)
		if err == nil {
			return q.Value, true
		}
	}
	return 0, false
}
