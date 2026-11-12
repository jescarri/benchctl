package domain

import (
	"fmt"
	"strings"
)

// Identity is the parsed *IDN? response.
type Identity struct {
	Vendor   string `json:"vendor"`
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	Firmware string `json:"firmware"`
}

// ParseIDN parses "Vendor,Model,Serial,Firmware".
func ParseIDN(s string) (Identity, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) < 4 {
		return Identity{}, fmt.Errorf("malformed *IDN? response %q", s)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return Identity{Vendor: parts[0], Model: parts[1], Serial: parts[2], Firmware: strings.Join(parts[3:], ",")}, nil
}

// Role is the kind of bench instrument.
type Role string

const (
	RoleScope     Role = "scope"
	RoleGenerator Role = "generator"
	RoleDMM       Role = "dmm"
	RolePSU       Role = "psu"
	RoleAnalyzer  Role = "analyzer"
	RoleUnknown   Role = "unknown"
)

// ---- Scope ----

// ChannelSetup is a partial scope channel configuration; nil fields are left unchanged.
type ChannelSetup struct {
	VDiv     *float64
	Offset   *float64
	Probe    *float64
	Coupling string // "dc", "ac", "gnd", "dc50", "ac50"
	On       *bool
	BWLimit  *bool
}

// ChannelState is the effective scope channel configuration.
type ChannelState struct {
	Channel  int     `json:"channel"`
	On       bool    `json:"on"`
	VDiv     float64 `json:"vdiv"`
	Offset   float64 `json:"offset"`
	Coupling string  `json:"coupling"`
	Probe    float64 `json:"probe"`
	BWLimit  bool    `json:"bw_limit"`
}

// Timebase is the effective horizontal configuration.
type Timebase struct {
	TDiv       float64 `json:"tdiv"`
	Delay      float64 `json:"delay"`
	SampleRate float64 `json:"sample_rate"`
	MemDepth   string  `json:"mem_depth"`
}

// TriggerSetup is a partial edge-trigger configuration; empty/nil fields are left unchanged.
type TriggerSetup struct {
	Source string // "C1".."C4", "EX", "EX5", "LINE"
	Mode   string // "auto", "normal", "single", "stop"
	Slope  string // "rise", "fall", "window"
	Level  *float64
}

// TriggerState is the effective trigger configuration.
type TriggerState struct {
	Type   string  `json:"type"`
	Source string  `json:"source"`
	Mode   string  `json:"mode"`
	Slope  string  `json:"slope"`
	Level  float64 `json:"level"`
}

// Measurement is a single scope parameter measurement. Value is nil when the
// instrument reports the measurement as invalid ("****").
type Measurement struct {
	Name  string   `json:"name"`
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
	Raw   string   `json:"raw"`
}

// Waveform is a captured trace converted to volts and seconds.
type Waveform struct {
	Channel    int       `json:"channel"`
	Points     int       `json:"points"`
	T0         float64   `json:"t0"`
	Dt         float64   `json:"dt"`
	VDiv       float64   `json:"vdiv"`
	Offset     float64   `json:"offset"`
	SampleRate float64   `json:"sample_rate"`
	Codes      []int8    `json:"-"`
	Volts      []float64 `json:"volts"`
	RawDesc    []byte    `json:"-"`
	RawData    []byte    `json:"-"`
}

// Time returns the timestamp of sample i.
func (w Waveform) Time(i int) float64 { return w.T0 + float64(i)*w.Dt }

// ---- Generator ----

// WaveSpec is a partial generator basic-wave configuration; nil/empty fields are left unchanged.
type WaveSpec struct {
	Type   string // sine, square, ramp, pulse, noise, dc, arb
	Freq   *float64
	AmpVpp *float64
	AmpRMS *float64
	Offset *float64
	Phase  *float64
	Duty   *float64
}

// GenChannelState is the effective generator channel configuration.
type GenChannelState struct {
	Channel  int               `json:"channel"`
	Wave     string            `json:"wave"`
	Freq     float64           `json:"freq"`
	AmpVpp   float64           `json:"amp_vpp"`
	AmpRMS   float64           `json:"amp_vrms"`
	Offset   float64           `json:"offset"`
	Phase    float64           `json:"phase"`
	HighV    float64           `json:"high_v"`
	LowV     float64           `json:"low_v"`
	Duty     *float64          `json:"duty,omitempty"`
	Output   bool              `json:"output"`
	Load     string            `json:"load"` // "hiz" or ohms as a string, e.g. "50"
	Polarity string            `json:"polarity"`
	Raw      map[string]string `json:"raw,omitempty"`
}

// ---- DMM ----

// DMMFunction is a measurement function name as used on the CLI.
type DMMFunction string

// DMMFunctions lists the supported DMM functions and their units.
var DMMFunctions = map[DMMFunction]string{
	"vdc": "V", "vac": "V", "idc": "A", "iac": "A", "res": "Ohm", "fres": "Ohm",
	"freq": "Hz", "per": "s", "cap": "F", "diode": "V", "cont": "Ohm", "temp": "C",
}

// DMMConfig is the DMM measurement configuration.
type DMMConfig struct {
	Function DMMFunction `json:"function"`
	Range    string      `json:"range"` // "auto" or a numeric value
	Raw      string      `json:"raw,omitempty"`
}

// DMMReading is a set of readings with simple statistics.
type DMMReading struct {
	Function DMMFunction `json:"function"`
	Unit     string      `json:"unit"`
	Values   []float64   `json:"values"`
	Mean     float64     `json:"mean"`
	Min      float64     `json:"min"`
	Max      float64     `json:"max"`
	StdDev   float64     `json:"stddev"`
	Range    string      `json:"range"`
}
