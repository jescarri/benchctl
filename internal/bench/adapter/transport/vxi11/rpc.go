package vxi11

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// RPC program numbers (VXI-11 spec B.6, RFC 1833).
const (
	progPortmap   = 100000
	versPortmap   = 2
	procGetPort   = 3
	protoTCP      = 6
	progCore      = 0x0607AF
	progAbort     = 0x0607B0
	versVXI11     = 1
	procAbort     = 1
	rpcCall       = 0
	rpcReply      = 1
	msgAccepted   = 0
	acceptSuccess = 0
)

// rpcConn is a synchronous ONC-RPC client over one TCP connection.
type rpcConn struct {
	mu     sync.Mutex
	c      net.Conn
	prog   uint32
	vers   uint32
	xid    uint32
	broken bool
}

func dialRPC(ctx context.Context, addr string, prog, vers uint32) (*rpcConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &rpcConn{c: c, prog: prog, vers: vers, xid: uint32(time.Now().UnixNano())}, nil
}

// call performs one RPC. deadline bounds the whole exchange.
func (r *rpcConn) call(ctx context.Context, proc uint32, args []byte, deadline time.Time) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.broken {
		return nil, fmt.Errorf("rpc: connection is broken after an earlier failure")
	}
	r.xid++
	xid := r.xid

	e := &xdrEnc{}
	e.u32(xid)
	e.u32(rpcCall)
	e.u32(2) // RPC version
	e.u32(r.prog)
	e.u32(r.vers)
	e.u32(proc)
	e.u32(0) // cred AUTH_NULL
	e.u32(0)
	e.u32(0) // verf AUTH_NULL
	e.u32(0)
	e.b = append(e.b, args...)

	if err := r.c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = r.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := writeRecord(r.c, e.b); err != nil {
		r.broken = true
		return nil, ctxErr(ctx, err)
	}
	for {
		rec, err := readRecord(r.c)
		if err != nil {
			r.broken = true
			return nil, ctxErr(ctx, err)
		}
		d := &xdrDec{b: rec}
		rxid := d.u32()
		if mt := d.u32(); mt != rpcReply {
			r.broken = true
			return nil, fmt.Errorf("rpc: unexpected message type %d", mt)
		}
		if rxid != xid {
			continue // stale reply from an aborted earlier call
		}
		if st := d.u32(); st != msgAccepted {
			return nil, fmt.Errorf("rpc: call denied (reply_stat %d)", st)
		}
		d.u32()       // verf flavor
		d.opaque()    // verf body
		as := d.u32() // accept_stat
		if d.err != nil {
			r.broken = true
			return nil, fmt.Errorf("rpc: malformed reply: %w", d.err)
		}
		if as != acceptSuccess {
			return nil, fmt.Errorf("rpc: call not accepted (accept_stat %d) for prog %#x proc %d", as, r.prog, proc)
		}
		return d.b, nil
	}
}

func (r *rpcConn) close() error { return r.c.Close() }

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// getPort asks the portmapper on host:pmPort for the TCP port of prog/vers.
func getPort(ctx context.Context, host string, pmPort int, prog, vers uint32, deadline time.Time) (int, error) {
	pm, err := dialRPC(ctx, net.JoinHostPort(host, fmt.Sprint(pmPort)), progPortmap, versPortmap)
	if err != nil {
		return 0, fmt.Errorf("portmapper: %w", err)
	}
	defer func() { _ = pm.close() }()
	e := &xdrEnc{}
	e.u32(prog)
	e.u32(vers)
	e.u32(protoTCP)
	e.u32(0)
	res, err := pm.call(ctx, procGetPort, e.b, deadline)
	if err != nil {
		return 0, fmt.Errorf("portmapper GETPORT: %w", err)
	}
	d := &xdrDec{b: res}
	port := d.u32()
	if d.err != nil {
		return 0, fmt.Errorf("portmapper GETPORT: %w", d.err)
	}
	if port == 0 {
		return 0, fmt.Errorf("portmapper: program %#x v%d not registered", prog, vers)
	}
	return int(port), nil
}
