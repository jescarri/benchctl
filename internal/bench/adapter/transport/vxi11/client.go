// Package vxi11 implements the VXI-11 (TCP/IP Instrument Protocol) client side
// in pure Go: portmapper lookup, core channel and abort channel.
package vxi11

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// Core channel procedures (VXI-11 spec B.6.2).
const (
	procCreateLink    = 10
	procDeviceWrite   = 11
	procDeviceRead    = 12
	procDeviceReadSTB = 13
	procDeviceClear   = 15
	procDeviceLock    = 18
	procDeviceUnlock  = 19
	procDestroyLink   = 23
)

// Device_Flags and read reasons.
const (
	flagWaitLock = 1
	flagEnd      = 8
	reasonReqCnt = 1
	reasonChr    = 2
	reasonEnd    = 4
)

// DefaultIOTimeout is used when the context carries no deadline.
const DefaultIOTimeout = 10 * time.Second

// netMargin is added to the device io_timeout for the socket deadline, so the
// instrument reports its own timeout before the TCP read gives up.
const netMargin = 2 * time.Second

// deviceLead is how much earlier than the context deadline the device is
// asked to time out.
const deviceLead = 300 * time.Millisecond

// DeviceError is a non-zero VXI-11 Device_ErrorCode.
type DeviceError struct {
	Op   string
	Code int32
}

var deviceErrText = map[int32]string{
	1: "syntax error", 3: "device not accessible", 4: "invalid link identifier",
	5: "parameter error", 6: "channel not established", 8: "operation not supported",
	9: "out of resources", 11: "device locked by another link", 12: "no lock held by this link",
	15: "I/O timeout", 17: "I/O error", 21: "invalid address", 23: "abort",
	29: "channel already established",
}

func (e *DeviceError) Error() string {
	t := deviceErrText[e.Code]
	if t == "" {
		t = "unknown error"
	}
	return fmt.Sprintf("vxi11 %s: %s (code %d)", e.Op, t, e.Code)
}

// Codes worth matching on.
const (
	ErrCodeLocked  = 11
	ErrCodeTimeout = 15
)

// IsCode reports whether err is a DeviceError with the given code.
func IsCode(err error, code int32) bool {
	var de *DeviceError
	return errors.As(err, &de) && de.Code == code
}

// Options configures Dial.
type Options struct {
	PortmapperPort int    // default 111
	Device         string // default "inst0"
	ClientID       int32
}

// Client is a VXI-11 link to one device.
type Client struct {
	mu        sync.Mutex
	host      string
	core      *rpcConn
	lid       int32
	abortPort int
	maxRecv   uint32
	opts      Options
	srq       srqState
}

// Dial resolves the core channel via the portmapper and creates a link.
func Dial(ctx context.Context, host string, opts Options) (*Client, error) {
	if opts.PortmapperPort == 0 {
		opts.PortmapperPort = 111
	}
	if opts.Device == "" {
		opts.Device = "inst0"
	}
	dl := deadline(ctx, DefaultIOTimeout)
	port, err := getPort(ctx, host, opts.PortmapperPort, progCore, versVXI11, dl)
	if err != nil {
		return nil, err
	}
	core, err := dialRPC(ctx, net.JoinHostPort(host, fmt.Sprint(port)), progCore, versVXI11)
	if err != nil {
		return nil, fmt.Errorf("vxi11 core channel: %w", err)
	}
	e := &xdrEnc{}
	e.i32(opts.ClientID)
	e.boolean(false) // lockDevice
	e.u32(0)         // lock_timeout
	e.str(opts.Device)
	res, err := core.call(ctx, procCreateLink, e.b, dl)
	if err != nil {
		_ = core.close()
		return nil, fmt.Errorf("vxi11 create_link: %w", err)
	}
	d := &xdrDec{b: res}
	code := d.i32()
	lid := d.i32()
	abortPort := d.u32()
	maxRecv := d.u32()
	if d.err != nil {
		_ = core.close()
		return nil, fmt.Errorf("vxi11 create_link: %w", d.err)
	}
	if code != 0 {
		_ = core.close()
		return nil, &DeviceError{Op: "create_link", Code: code}
	}
	if maxRecv == 0 {
		maxRecv = 1024
	}
	if abortPort == 0 {
		// Some firmware (Siglent) reports abortPort=0 but still registers the
		// abort program with the portmapper.
		if p, err := getPort(ctx, host, opts.PortmapperPort, progAbort, versVXI11, dl); err == nil {
			abortPort = uint32(p)
		}
	}
	return &Client{host: host, core: core, lid: lid, abortPort: int(abortPort), maxRecv: maxRecv, opts: opts}, nil
}

func deadline(ctx context.Context, def time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(def)
}

// ioTimeoutMS returns the device io_timeout for the remaining context time,
// and the socket deadline that includes a margin.
//
// The device io_timeout is set slightly shorter than the context deadline so
// the instrument answers "I/O timeout" (code 15) before the context fires.
// That keeps the RPC stream in sync and the link usable after a query that
// gets no response (e.g. an unknown SDS query).
func ioTimeout(ctx context.Context) (uint32, time.Time) {
	dl := deadline(ctx, DefaultIOTimeout)
	rem := time.Until(dl)
	if rem > 2*deviceLead {
		rem -= deviceLead
	}
	if rem < time.Millisecond {
		rem = time.Millisecond
	}
	ms := rem.Milliseconds()
	if ms > math.MaxUint32 {
		ms = math.MaxUint32
	}
	return uint32(ms), dl.Add(netMargin)
}

// Name implements port.Transport.
func (c *Client) Name() string { return "vxi11" }

// MaxRecvSize is the largest write chunk the device accepts.
func (c *Client) MaxRecvSize() uint32 { return c.maxRecv }

// Write sends msg as one message, chunked to maxRecvSize, with END on the last chunk.
func (c *Client) Write(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		n := len(msg)
		flags := uint32(flagEnd)
		if uint32(n) > c.maxRecv {
			n = int(c.maxRecv)
			flags = 0
		}
		io, dl := ioTimeout(ctx)
		e := &xdrEnc{}
		e.i32(c.lid)
		e.u32(io)
		e.u32(0) // lock_timeout
		e.u32(flags)
		e.opaque(msg[:n])
		res, err := c.core.call(ctx, procDeviceWrite, e.b, dl)
		if err != nil {
			return fmt.Errorf("vxi11 device_write: %w", err)
		}
		d := &xdrDec{b: res}
		code := d.i32()
		size := d.u32()
		if d.err != nil {
			return fmt.Errorf("vxi11 device_write: %w", d.err)
		}
		if code != 0 {
			return &DeviceError{Op: "device_write", Code: code}
		}
		if int(size) > n {
			size = uint32(n)
		}
		msg = msg[size:]
		if len(msg) == 0 {
			return nil
		}
		if size == 0 {
			return fmt.Errorf("vxi11 device_write: device accepted 0 bytes")
		}
	}
}

// readChunk is the requestSize for each device_read call.
const readChunk = 1 << 20

// Read returns one complete response message (until the device sets END).
func (c *Client) Read(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []byte
	for {
		io, dl := ioTimeout(ctx)
		e := &xdrEnc{}
		e.i32(c.lid)
		e.u32(readChunk)
		e.u32(io)
		e.u32(0) // lock_timeout
		e.u32(0) // flags (no termchar)
		e.u32(0) // termChar
		res, err := c.core.call(ctx, procDeviceRead, e.b, dl)
		if err != nil {
			return nil, fmt.Errorf("vxi11 device_read: %w", err)
		}
		d := &xdrDec{b: res}
		code := d.i32()
		reason := d.u32()
		data := d.opaque()
		if d.err != nil {
			return nil, fmt.Errorf("vxi11 device_read: %w", d.err)
		}
		if code != 0 {
			return nil, &DeviceError{Op: "device_read", Code: code}
		}
		out = append(out, data...)
		if reason&(reasonEnd|reasonChr) != 0 {
			return out, nil
		}
		if len(data) == 0 && reason&reasonReqCnt == 0 {
			return nil, fmt.Errorf("vxi11 device_read: no data and no END (reason %#x)", reason)
		}
	}
}

func (c *Client) generic(ctx context.Context, proc uint32, op string, flags uint32, lockTimeout uint32) (*xdrDec, error) {
	io, dl := ioTimeout(ctx)
	e := &xdrEnc{}
	e.i32(c.lid)
	e.u32(flags)
	e.u32(lockTimeout)
	e.u32(io)
	res, err := c.core.call(ctx, proc, e.b, dl)
	if err != nil {
		return nil, fmt.Errorf("vxi11 %s: %w", op, err)
	}
	d := &xdrDec{b: res}
	if code := d.i32(); d.err == nil && code != 0 {
		return nil, &DeviceError{Op: op, Code: code}
	}
	if d.err != nil {
		return nil, fmt.Errorf("vxi11 %s: %w", op, d.err)
	}
	return d, nil
}

// ReadSTB returns the status byte via device_readstb.
func (c *Client) ReadSTB(ctx context.Context) (byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, err := c.generic(ctx, procDeviceReadSTB, "device_readstb", 0, 0)
	if err != nil {
		return 0, err
	}
	stb := d.u32()
	if d.err != nil {
		return 0, fmt.Errorf("vxi11 device_readstb: %w", d.err)
	}
	return byte(stb), nil
}

// Clear performs device_clear.
func (c *Client) Clear(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.generic(ctx, procDeviceClear, "device_clear", 0, 0)
	return err
}

// Lock acquires the device lock, waiting up to wait.
func (c *Client) Lock(ctx context.Context, wait time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var flags uint32
	if wait > 0 {
		flags = flagWaitLock
	}
	_, dl := ioTimeout(ctx)
	e := &xdrEnc{}
	e.i32(c.lid)
	e.u32(flags)
	e.u32(uint32(wait.Milliseconds()))
	res, err := c.core.call(ctx, procDeviceLock, e.b, dl.Add(wait))
	if err != nil {
		return fmt.Errorf("vxi11 device_lock: %w", err)
	}
	d := &xdrDec{b: res}
	if code := d.i32(); code != 0 {
		return &DeviceError{Op: "device_lock", Code: code}
	}
	return d.err
}

// Unlock releases the device lock.
func (c *Client) Unlock(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, dl := ioTimeout(ctx)
	e := &xdrEnc{}
	e.i32(c.lid)
	res, err := c.core.call(ctx, procDeviceUnlock, e.b, dl)
	if err != nil {
		return fmt.Errorf("vxi11 device_unlock: %w", err)
	}
	d := &xdrDec{b: res}
	if code := d.i32(); code != 0 {
		return &DeviceError{Op: "device_unlock", Code: code}
	}
	return d.err
}

// Abort sends device_abort on the abort channel, interrupting an in-progress
// core-channel call. It does not take the client mutex on purpose.
func (c *Client) Abort(ctx context.Context) error {
	if c.abortPort == 0 {
		return &DeviceError{Op: "device_abort", Code: 8}
	}
	ab, err := dialRPC(ctx, net.JoinHostPort(c.host, fmt.Sprint(c.abortPort)), progAbort, versVXI11)
	if err != nil {
		return fmt.Errorf("vxi11 abort channel: %w", err)
	}
	defer func() { _ = ab.close() }()
	e := &xdrEnc{}
	e.i32(c.lid)
	res, err := ab.call(ctx, procAbort, e.b, deadline(ctx, 5*time.Second))
	if err != nil {
		return fmt.Errorf("vxi11 device_abort: %w", err)
	}
	d := &xdrDec{b: res}
	if code := d.i32(); code != 0 {
		return &DeviceError{Op: "device_abort", Code: code}
	}
	return d.err
}

// Close destroys the link and closes the core channel.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.closeSRQLocked(ctx)
	e := &xdrEnc{}
	e.i32(c.lid)
	_, err := c.core.call(ctx, procDestroyLink, e.b, deadline(ctx, 2*time.Second))
	cerr := c.core.close()
	if err != nil {
		return nil // link teardown is best effort; the TCP close frees it on the device
	}
	return cerr
}
