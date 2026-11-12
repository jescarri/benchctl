// Package port defines the interfaces the application layer depends on.
// Adapters (transports, Siglent drivers, config storage, discovery) implement them.
package port

import (
	"context"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
)

// Transport moves SCPI messages to and from one instrument (VXI-11 or raw socket).
type Transport interface {
	// Write sends one complete program message.
	Write(ctx context.Context, msg []byte) error
	// Read returns one complete response message (line or IEEE 488.2 block, terminator included).
	Read(ctx context.Context) ([]byte, error)
	// Clear performs a device clear and recovers a desynchronised stream.
	Clear(ctx context.Context) error
	// Lock acquires the instrument-side lock, waiting up to wait. Transports without
	// instrument-side locking return nil.
	Lock(ctx context.Context, wait time.Duration) error
	Unlock(ctx context.Context) error
	// ReadSTB returns the IEEE 488.2 status byte.
	ReadSTB(ctx context.Context) (byte, error)
	// Name returns "vxi11" or "raw".
	Name() string
	Close() error
}

// SCPI is generic, error-checked SCPI access to an instrument. It is the
// escape hatch that keeps every instrument command reachable.
type SCPI interface {
	// Write sends a command without checking errors.
	Write(ctx context.Context, cmd string) error
	// Set sends a command and then checks the instrument's error state.
	Set(ctx context.Context, cmd string) error
	// Query sends a query and returns the response with the trailing terminator removed.
	Query(ctx context.Context, cmd string) (string, error)
	// QueryBytes sends a query and returns the raw response message.
	QueryBytes(ctx context.Context, cmd string) ([]byte, error)
	// QueryBlock sends a query and returns the IEEE 488.2 block payload.
	QueryBlock(ctx context.Context, cmd string) ([]byte, error)
	// CheckErrors reads and clears the instrument's error state.
	CheckErrors(ctx context.Context) error
	// WaitComplete waits until all pending operations finish (*OPC?).
	WaitComplete(ctx context.Context) error
	// RunAndWait sends cmd and waits for operation complete via SRQ, *STB?
	// polling or *OPC? ("auto", "srq", "poll", "opc"); returns the method used.
	RunAndWait(ctx context.Context, cmd, method string) (string, error)
	// SRQStatus reports whether service requests work on this session.
	SRQStatus() string
	// Clear sends a device clear.
	Clear(ctx context.Context) error
	TransportName() string
}

// Instrument is an open, identified instrument.
type Instrument interface {
	Identity() domain.Identity
	Role() domain.Role
	SCPI() SCPI
	Close() error
}

// Scope is an oscilloscope.
type Scope interface {
	Instrument
	Channels() int
	SetupChannel(ctx context.Context, ch int, s domain.ChannelSetup) error
	ChannelState(ctx context.Context, ch int) (domain.ChannelState, error)
	SetTimebase(ctx context.Context, tdiv, delay *float64) error
	Timebase(ctx context.Context) (domain.Timebase, error)
	SetTrigger(ctx context.Context, t domain.TriggerSetup) error
	Trigger(ctx context.Context) (domain.TriggerState, error)
	// Autoset runs autoset, waits until the scope has applied it, and reports
	// whether any setting changed.
	Autoset(ctx context.Context) (changed bool, err error)
	Measure(ctx context.Context, ch int, params []string) ([]domain.Measurement, error)
	Capture(ctx context.Context, ch int, points int) (domain.Waveform, error)
	Screenshot(ctx context.Context) ([]byte, error)
}

// Generator is a function/arbitrary waveform generator.
type Generator interface {
	Instrument
	Channels() int
	SetWave(ctx context.Context, ch int, w domain.WaveSpec) error
	SetLoad(ctx context.Context, ch int, load string) error
	Output(ctx context.Context, ch int, on bool) error
	State(ctx context.Context, ch int) (domain.GenChannelState, error)
}

// DMM is a digital multimeter.
type DMM interface {
	Instrument
	Configure(ctx context.Context, cfg domain.DMMConfig) error
	Config(ctx context.Context) (domain.DMMConfig, error)
	Read(ctx context.Context) (float64, error)
}

// Screenshotter is implemented by instruments that can dump their screen.
// The image is returned in the instrument's native format (Siglent: BMP).
type Screenshotter interface {
	Screenshot(ctx context.Context) ([]byte, error)
}

// Outputs is implemented by instruments with outputs that `safe` must switch off.
type Outputs interface {
	AllOutputsOff(ctx context.Context) error
}

// Connector opens instruments by alias from the inventory.
type Connector interface {
	Open(ctx context.Context, alias string) (Instrument, error)
}

// Found is one instrument found by discovery.
type Found struct {
	IP       string          `json:"ip"`
	Identity domain.Identity `json:"identity"`
	Role     domain.Role     `json:"role"`
	Via      string          `json:"via,omitempty"` // transport used to identify it
}

// Discoverer scans a subnet for SCPI instruments.
type Discoverer interface {
	Scan(ctx context.Context, cidr string) ([]Found, error)
}

// ConfigStore loads and saves the bench inventory.
type ConfigStore interface {
	Load() (*domain.Bench, error)
	Save(*domain.Bench) error
	Path() string
}
