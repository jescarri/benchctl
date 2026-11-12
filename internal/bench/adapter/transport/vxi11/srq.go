package vxi11

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// Interrupt channel (VXI-11 spec B.6.3): the controller runs an RPC server
// (program DEVICE_INTR) and the instrument calls device_intr_srq on it when
// it asserts a service request.
const (
	procDeviceEnableSRQ = 20
	procCreateIntrChan  = 25
	procDestroyIntrChan = 26
	progIntr            = 0x0607B1
	procIntrSRQ         = 30
	familyTCP           = 0
)

// srqHandle identifies this link in device_intr_srq calls.
var srqHandle = []byte("benchctl")

type srqState struct {
	mu      sync.Mutex
	ln      net.Listener
	ch      chan struct{}
	count   int
	accepts int
	closed  bool
}

// SRQSupported reports whether EnableSRQ succeeded on this link.
func (c *Client) SRQSupported() bool {
	c.srq.mu.Lock()
	defer c.srq.mu.Unlock()
	return c.srq.ln != nil && !c.srq.closed
}

// SRQConnections returns how many interrupt-channel connections the
// instrument opened back to us (0 means it never connected, e.g. firewalled).
func (c *Client) SRQConnections() int {
	c.srq.mu.Lock()
	defer c.srq.mu.Unlock()
	return c.srq.accepts
}

// SRQCount returns how many service requests have been received.
func (c *Client) SRQCount() int {
	c.srq.mu.Lock()
	defer c.srq.mu.Unlock()
	return c.srq.count
}

// EnableSRQ opens the interrupt channel: it starts a TCP RPC server on the
// local address used to reach the instrument, asks the instrument to connect
// back (create_intr_chan) and enables SRQ delivery on this link
// (device_enable_srq). Calling it again is a no-op.
func (c *Client) EnableSRQ(ctx context.Context) error {
	c.srq.mu.Lock()
	if c.srq.ln != nil {
		c.srq.mu.Unlock()
		return nil
	}
	c.srq.mu.Unlock()

	local, ok := c.core.c.LocalAddr().(*net.TCPAddr)
	if !ok || local.IP.To4() == nil {
		return &DeviceError{Op: "create_intr_chan", Code: 8}
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(local.IP.String(), "0"))
	if err != nil {
		return fmt.Errorf("vxi11 interrupt listener: %w", err)
	}
	c.srq.mu.Lock()
	c.srq.ln = ln
	c.srq.ch = make(chan struct{}, 1)
	c.srq.closed = false
	c.srq.mu.Unlock()
	go c.serveIntr(ln)

	fail := func(err error) error {
		c.srq.mu.Lock()
		c.srq.ln, c.srq.ch = nil, nil // WaitSRQ then reports "not established"
		c.srq.mu.Unlock()
		_ = ln.Close()
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	_, dl := ioTimeout(ctx)
	e := &xdrEnc{}
	e.u32(binary.BigEndian.Uint32(local.IP.To4()))
	e.u32(uint32(ln.Addr().(*net.TCPAddr).Port))
	e.u32(progIntr)
	e.u32(versVXI11)
	e.u32(familyTCP)
	res, err := c.core.call(ctx, procCreateIntrChan, e.b, dl)
	if err != nil {
		return fail(fmt.Errorf("vxi11 create_intr_chan: %w", err))
	}
	if code := (&xdrDec{b: res}).i32(); code != 0 {
		return fail(&DeviceError{Op: "create_intr_chan", Code: code})
	}
	if err := c.enableSRQLocked(ctx, true); err != nil {
		c.destroyIntrLocked(ctx)
		return fail(err)
	}
	return nil
}

func (c *Client) enableSRQLocked(ctx context.Context, on bool) error {
	_, dl := ioTimeout(ctx)
	e := &xdrEnc{}
	e.i32(c.lid)
	e.boolean(on)
	e.opaque(srqHandle)
	res, err := c.core.call(ctx, procDeviceEnableSRQ, e.b, dl)
	if err != nil {
		return fmt.Errorf("vxi11 device_enable_srq: %w", err)
	}
	if code := (&xdrDec{b: res}).i32(); code != 0 {
		return &DeviceError{Op: "device_enable_srq", Code: code}
	}
	return nil
}

func (c *Client) destroyIntrLocked(ctx context.Context) {
	_, dl := ioTimeout(ctx)
	_, _ = c.core.call(ctx, procDestroyIntrChan, nil, dl)
}

// DrainSRQ discards a pending (already delivered) service request.
func (c *Client) DrainSRQ() {
	c.srq.mu.Lock()
	ch := c.srq.ch
	c.srq.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// WaitSRQ blocks until the instrument asserts a service request or ctx ends.
func (c *Client) WaitSRQ(ctx context.Context) error {
	c.srq.mu.Lock()
	ch, ln, closed := c.srq.ch, c.srq.ln, c.srq.closed
	c.srq.mu.Unlock()
	if ch == nil || ln == nil || closed {
		return &DeviceError{Op: "wait_srq", Code: 6} // channel not established
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("vxi11 waiting for SRQ: %w", ctx.Err())
	}
}

// serveIntr accepts the instrument's interrupt-channel connection(s) and
// handles device_intr_srq calls.
func (c *Client) serveIntr(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		c.srq.mu.Lock()
		c.srq.accepts++
		c.srq.mu.Unlock()
		go c.handleIntrConn(conn)
	}
}

func (c *Client) handleIntrConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		rec, err := readRecord(conn)
		if err != nil {
			return
		}
		d := &xdrDec{b: rec}
		xid := d.u32()
		if d.u32() != rpcCall {
			continue
		}
		d.u32() // rpcvers
		prog, _, proc := d.u32(), d.u32(), d.u32()
		d.u32()
		d.opaque() // cred
		d.u32()
		d.opaque() // verf
		if d.err != nil || prog != progIntr || proc != procIntrSRQ {
			continue
		}
		c.srq.mu.Lock()
		c.srq.count++
		ch := c.srq.ch
		c.srq.mu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default: // one pending SRQ is enough
			}
		}
		// device_intr_srq returns void. Reply so instruments that wait for
		// it are not blocked; ignore failures from ones that don't.
		e := &xdrEnc{}
		e.u32(xid)
		e.u32(rpcReply)
		e.u32(msgAccepted)
		e.u32(0)
		e.u32(0)
		e.u32(acceptSuccess)
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = writeRecord(conn, e.b)
	}
}

// closeSRQ disables SRQ, destroys the interrupt channel and stops the
// listener. Caller holds c.mu.
func (c *Client) closeSRQLocked(ctx context.Context) {
	c.srq.mu.Lock()
	ln := c.srq.ln
	c.srq.closed = true
	c.srq.mu.Unlock()
	if ln == nil {
		return
	}
	_ = c.enableSRQLocked(ctx, false)
	c.destroyIntrLocked(ctx)
	_ = ln.Close()
}
