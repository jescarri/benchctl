package vxi11

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer implements enough of a VXI-11 device (portmapper, core and
// abort channels) to exercise the client.
type fakeServer struct {
	mu             sync.Mutex
	pm, core, ab   net.Listener
	maxRecv        uint32
	advertiseAbort bool
	refuseIntr     bool // create_intr_chan fails (operation not supported)
	readChunk      int  // max bytes per device_read reply (0 = unlimited)
	fragment       bool // split replies into two RPC fragments
	respond        func(cmd string) []byte
	inbuf          []byte
	out            []byte
	writes         int
	nextLid        int32
	lockedBy       int32
	stb            byte
	aborts         int
	intrAddr       string // set by create_intr_chan
	srqEnabled     bool
	srqHandle      []byte
	silentSRQ      bool // Siglent behaviour: accept the channel, never call back
}

func newFake(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{maxRecv: 1 << 20}
	var err error
	for _, l := range []*net.Listener{&f.pm, &f.core, &f.ab} {
		if *l, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		go f.serve(*l)
	}
	t.Cleanup(func() { _ = f.pm.Close(); _ = f.core.Close(); _ = f.ab.Close() })
	return f
}

func port(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }

func (f *fakeServer) dial(t *testing.T, id int32) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return Dial(ctx, "127.0.0.1", Options{PortmapperPort: port(f.pm), ClientID: id})
}

func (f *fakeServer) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			for {
				rec, err := readRecord(c)
				if err != nil {
					return
				}
				d := &xdrDec{b: rec}
				xid := d.u32()
				d.u32() // CALL
				d.u32() // rpcvers
				prog, _, proc := d.u32(), d.u32(), d.u32()
				d.u32()
				d.opaque() // cred
				d.u32()
				d.opaque() // verf
				res := f.handle(prog, proc, d)
				e := &xdrEnc{}
				e.u32(xid)
				e.u32(rpcReply)
				e.u32(msgAccepted)
				e.u32(0)
				e.u32(0) // verf
				e.u32(acceptSuccess)
				e.b = append(e.b, res...)
				if f.fragment && len(e.b) > 8 {
					h := len(e.b) / 2
					var hdr [4]byte
					binary.BigEndian.PutUint32(hdr[:], uint32(h))
					_, _ = c.Write(append(hdr[:], e.b[:h]...))
					_ = writeRecord(c, e.b[h:])
				} else {
					_ = writeRecord(c, e.b)
				}
			}
		}()
	}
}

func (f *fakeServer) handle(prog, proc uint32, d *xdrDec) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := &xdrEnc{}
	switch {
	case prog == progPortmap && proc == procGetPort:
		p := d.u32()
		switch p {
		case progCore:
			e.u32(uint32(port(f.core)))
		case progAbort:
			e.u32(uint32(port(f.ab)))
		default:
			e.u32(0)
		}
	case prog == progAbort && proc == procAbort:
		f.aborts++
		e.i32(0)
	case prog == progCore:
		switch proc {
		case procCreateLink:
			f.nextLid++
			e.i32(0)
			e.i32(f.nextLid)
			if f.advertiseAbort {
				e.u32(uint32(port(f.ab)))
			} else {
				e.u32(0)
			}
			e.u32(f.maxRecv)
		case procDeviceWrite:
			d.i32()
			d.u32()
			d.u32()
			flags := d.u32()
			data := d.opaque()
			f.writes++
			f.inbuf = append(f.inbuf, data...)
			if flags&flagEnd != 0 {
				cmd := strings.TrimRight(string(f.inbuf), "\n")
				f.inbuf = nil
				if cmd == "*OPC" && f.srqEnabled && f.intrAddr != "" && !f.silentSRQ {
					go sendSRQ(f.intrAddr, f.srqHandle)
				}
				if f.respond != nil {
					f.out = append(f.out, f.respond(cmd)...)
				}
			}
			e.i32(0)
			e.u32(uint32(len(data)))
		case procDeviceRead:
			d.i32()
			req := int(d.u32())
			if len(f.out) == 0 {
				e.i32(ErrCodeTimeout)
				e.u32(0)
				e.opaque(nil)
				break
			}
			n := min(req, len(f.out))
			if f.readChunk > 0 {
				n = min(n, f.readChunk)
			}
			data := f.out[:n]
			f.out = f.out[n:]
			e.i32(0)
			if len(f.out) == 0 {
				e.u32(reasonEnd)
			} else {
				e.u32(reasonReqCnt)
			}
			e.opaque(data)
		case procDeviceReadSTB:
			e.i32(0)
			e.u32(uint32(f.stb))
		case procDeviceClear:
			f.out = nil
			e.i32(0)
		case procDeviceLock:
			lid := d.i32()
			if f.lockedBy != 0 && f.lockedBy != lid {
				e.i32(ErrCodeLocked)
			} else {
				f.lockedBy = lid
				e.i32(0)
			}
		case procDeviceUnlock:
			lid := d.i32()
			if f.lockedBy != lid {
				e.i32(12)
			} else {
				f.lockedBy = 0
				e.i32(0)
			}
		case procCreateIntrChan:
			ip := make(net.IP, 4)
			binary.BigEndian.PutUint32(ip, d.u32())
			p := d.u32()
			if d.u32() != progIntr {
				e.i32(5)
				break
			}
			if f.refuseIntr {
				e.i32(8)
				break
			}
			f.intrAddr = net.JoinHostPort(ip.String(), fmt.Sprint(p))
			e.i32(0)
		case procDeviceEnableSRQ:
			d.i32()
			f.srqEnabled = d.u32() == 1
			f.srqHandle = d.opaque()
			e.i32(0)
		case procDestroyIntrChan:
			f.intrAddr = ""
			e.i32(0)
		case procDestroyLink:
			lid := d.i32()
			if f.lockedBy == lid {
				f.lockedBy = 0
			}
			e.i32(0)
		default:
			e.i32(8)
		}
	}
	return e.b
}

func ctx2s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestQueryRoundTrip(t *testing.T) {
	f := newFake(t)
	f.fragment = true
	f.respond = func(cmd string) []byte {
		if cmd == "*IDN?" {
			return []byte("Siglent Technologies,SDM3045X,SN,1\n")
		}
		return nil
	}
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctx2s(t)
	if err := c.Write(ctx, []byte("*IDN?\n")); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "Siglent Technologies,SDM3045X,SN,1\n" {
		t.Fatalf("got %q", msg)
	}
	if c.Name() != "vxi11" {
		t.Error("name")
	}
}

func TestChunkedWriteAndRead(t *testing.T) {
	f := newFake(t)
	f.maxRecv = 16
	f.readChunk = 7
	long := "C1:BSWV " + strings.Repeat("X", 100)
	payload := bytes.Repeat([]byte{0, '\n', 0xff, '#'}, 50)
	var got string
	f.respond = func(cmd string) []byte {
		got = cmd
		return append([]byte("#3200"), append(payload, '\n')...)
	}
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctx2s(t)
	if err := c.Write(ctx, []byte(long+"\n")); err != nil {
		t.Fatal(err)
	}
	if got != long {
		t.Fatalf("server reassembled %q", got)
	}
	if f.writes < 7 {
		t.Errorf("expected chunked writes, got %d", f.writes)
	}
	msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(msg[5:205], payload) {
		t.Fatal("binary payload corrupted across reads")
	}
}

func TestReadTimeoutIsDeviceError(t *testing.T) {
	f := newFake(t)
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Read(ctx2s(t))
	if !IsCode(err, ErrCodeTimeout) {
		t.Fatalf("expected device timeout, got %v", err)
	}
	// The link stays usable after a device-level timeout.
	f.respond = func(string) []byte { return []byte("1\n") }
	if err := c.Write(ctx2s(t), []byte("*OPC?\n")); err != nil {
		t.Fatal(err)
	}
	if msg, err := c.Read(ctx2s(t)); err != nil || string(msg) != "1\n" {
		t.Fatalf("after timeout: %q %v", msg, err)
	}
}

func TestSTBClearLockAbort(t *testing.T) {
	f := newFake(t)
	f.stb = 0x44
	a, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := f.dial(t, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ctx := ctx2s(t)

	if stb, err := a.ReadSTB(ctx); err != nil || stb != 0x44 {
		t.Fatalf("stb %x %v", stb, err)
	}
	f.out = []byte("stale\n")
	if err := a.Clear(ctx); err != nil || len(f.out) != 0 {
		t.Fatalf("clear: %v, out=%q", err, f.out)
	}
	if err := a.Lock(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Lock(ctx, 0); !IsCode(err, ErrCodeLocked) {
		t.Fatalf("second link should see 'locked', got %v", err)
	}
	if err := a.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Lock(ctx, 0); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	// abortPort=0 from create_link: the port comes from the portmapper.
	if a.abortPort == 0 {
		t.Fatal("abort port not resolved via portmapper")
	}
	if err := a.Abort(ctx); err != nil || f.aborts != 1 {
		t.Fatalf("abort: %v (%d)", err, f.aborts)
	}
}

func TestPortmapperMissing(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	p := port(ln)
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Dial(ctx, "127.0.0.1", Options{PortmapperPort: p}); err == nil {
		t.Fatal("expected error with no portmapper")
	}
}

func TestDeviceErrorText(t *testing.T) {
	e := &DeviceError{Op: "device_lock", Code: 11}
	if !strings.Contains(e.Error(), "locked by another link") {
		t.Error(e.Error())
	}
	if !strings.Contains((&DeviceError{Op: "x", Code: 99}).Error(), "unknown") {
		t.Error("unknown code text")
	}
}

func TestXDRRoundTrip(t *testing.T) {
	e := &xdrEnc{}
	e.u32(7)
	e.i32(-3)
	e.str("inst0")
	e.opaque([]byte{1, 2, 3})
	e.boolean(true)
	if len(e.b)%4 != 0 {
		t.Fatal("not 4-byte aligned")
	}
	d := &xdrDec{b: e.b}
	if d.u32() != 7 || d.i32() != -3 || d.str() != "inst0" || !bytes.Equal(d.opaque(), []byte{1, 2, 3}) || d.u32() != 1 || d.err != nil {
		t.Fatal("round trip mismatch")
	}
	if d.u32(); d.err == nil {
		t.Fatal("expected short-buffer error")
	}
}

// sendSRQ plays the instrument side of the interrupt channel.
func sendSRQ(addr string, handle []byte) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	e := &xdrEnc{}
	e.u32(42)
	e.u32(rpcCall)
	e.u32(2)
	e.u32(progIntr)
	e.u32(versVXI11)
	e.u32(procIntrSRQ)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.opaque(handle)
	_ = writeRecord(c, e.b)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = readRecord(c) // the client replies
}

func TestSRQDelivered(t *testing.T) {
	f := newFake(t)
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctx2s(t)
	if err := c.EnableSRQ(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.srqEnabled || string(f.srqHandle) != "benchctl" || f.intrAddr == "" {
		t.Fatalf("server state: enabled=%v handle=%q addr=%q", f.srqEnabled, f.srqHandle, f.intrAddr)
	}
	if err := c.EnableSRQ(ctx); err != nil {
		t.Fatal("second EnableSRQ must be a no-op")
	}
	if err := c.Write(ctx, []byte("*OPC\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitSRQ(ctx); err != nil {
		t.Fatal(err)
	}
	if c.SRQCount() != 1 || c.SRQConnections() != 1 || !c.SRQSupported() {
		t.Fatalf("count=%d conns=%d", c.SRQCount(), c.SRQConnections())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if f.srqEnabled || f.intrAddr != "" {
		t.Fatal("Close must disable SRQ and destroy the interrupt channel")
	}
}

func TestSRQSilentInstrument(t *testing.T) {
	f := newFake(t)
	f.silentSRQ = true
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.WaitSRQ(ctx2s(t)); err == nil {
		t.Fatal("WaitSRQ before EnableSRQ must fail")
	}
	if err := c.EnableSRQ(ctx2s(t)); err != nil {
		t.Fatal(err)
	}
	_ = c.Write(ctx2s(t), []byte("*OPC\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := c.WaitSRQ(ctx); err == nil || c.SRQConnections() != 0 {
		t.Fatalf("expected timeout with no connection, got %v (%d conns)", err, c.SRQConnections())
	}
}

func TestWaitSRQAfterFailedEnable(t *testing.T) {
	f := newFake(t)
	f.refuseIntr = true
	c, err := f.dial(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.EnableSRQ(ctx2s(t)); err == nil {
		t.Fatal("EnableSRQ must fail when create_intr_chan is refused")
	}
	// Must report "not established" at once, not block until the deadline.
	start := time.Now()
	err = c.WaitSRQ(ctx2s(t))
	if !IsCode(err, 6) || time.Since(start) > time.Second {
		t.Fatalf("WaitSRQ after failed enable: %v after %v", err, time.Since(start))
	}
}
