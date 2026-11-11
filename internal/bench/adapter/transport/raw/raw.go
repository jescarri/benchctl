// Package raw implements the SCPI raw-socket transport (LXI "SCPI-RAW", TCP 5025).
//
// A raw socket has no end-of-message flag, so message framing is inferred:
//   - an IEEE 488.2 definite-length block (#<n><len><bytes>) is read by length,
//     whether it starts the message or follows a header such as "C1:WF DAT2,";
//   - a Windows BMP (Siglent SCDP screenshots) is read by the size in its header;
//   - anything else is read up to '\n'.
//
// Trailing terminators after binary payloads are drained so they do not leak
// into the next response.
package raw

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultPort is the SCPI raw socket port.
const DefaultPort = 5025

// DefaultIOTimeout is used when the context carries no deadline.
const DefaultIOTimeout = 10 * time.Second

// drainWindow is how long to wait for stray terminators after a binary payload.
const drainWindow = 60 * time.Millisecond

// maxMessage bounds a single response.
const maxMessage = 512 << 20

// Conn is a raw socket connection to one instrument.
type Conn struct {
	mu   sync.Mutex
	addr string
	c    net.Conn
	r    *bufio.Reader
}

// Dial connects to addr ("host:port"; port defaults to 5025).
func Dial(ctx context.Context, addr string) (*Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, strconv.Itoa(DefaultPort))
	}
	c := &Conn{addr: addr}
	if err := c.dial(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Conn) dial(ctx context.Context) error {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	c.c = nc
	c.r = bufio.NewReaderSize(nc, 64<<10)
	return nil
}

// Name implements port.Transport.
func (c *Conn) Name() string { return "raw" }

func (c *Conn) arm(ctx context.Context) (func() bool, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(DefaultIOTimeout)
	}
	nc := c.c // captured: Clear may replace c.c while the callback is pending
	if err := nc.SetDeadline(dl); err != nil {
		return nil, err
	}
	return context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) }), nil
}

// wrap reports context errors in preference to the socket error. The socket
// deadline equals the context deadline, so the socket can time out a moment
// before ctx.Err() is set; that is still the caller's deadline.
func wrap(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("raw %s: %w", op, ctx.Err())
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl.Add(-10*time.Millisecond)) {
			return fmt.Errorf("raw %s: %w", op, context.DeadlineExceeded)
		}
	}
	return fmt.Errorf("raw %s: %w", op, err)
}

// Write sends msg, appending '\n' if missing.
func (c *Conn) Write(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop, err := c.arm(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg = append(append([]byte(nil), msg...), '\n')
	}
	if _, err := c.c.Write(msg); err != nil {
		return wrap(ctx, "write", err)
	}
	return nil
}

// Read returns one response message, including its terminator.
func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop, err := c.arm(ctx)
	if err != nil {
		return nil, err
	}
	defer stop()
	msg, err := c.readMessage()
	if err != nil {
		return nil, wrap(ctx, "read", err)
	}
	return msg, nil
}

func (c *Conn) readMessage() ([]byte, error) {
	if p, err := c.r.Peek(2); err == nil && string(p) == "BM" {
		return c.readBMP()
	}
	var buf []byte
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '#' && blockMayStart(buf) {
			nb, err := c.r.Peek(1)
			if err != nil {
				return nil, err
			}
			if nb[0] >= '1' && nb[0] <= '9' {
				return c.readBlock(append(buf, b))
			}
		}
		buf = append(buf, b)
		if b == '\n' {
			return buf, nil
		}
		if len(buf) > maxMessage {
			return nil, fmt.Errorf("line response exceeds %d bytes", maxMessage)
		}
	}
}

// blockMayStart reports whether a '#' after prefix can begin an IEEE 488.2 block:
// at message start or after a header separator.
func blockMayStart(prefix []byte) bool {
	if len(prefix) == 0 {
		return true
	}
	switch prefix[len(prefix)-1] {
	case ' ', ',', '\t':
		return true
	}
	return false
}

func (c *Conn) readBlock(buf []byte) ([]byte, error) {
	nd, err := c.r.ReadByte()
	if err != nil {
		return nil, err
	}
	n := int(nd - '0')
	digits := make([]byte, n)
	if _, err := io.ReadFull(c.r, digits); err != nil {
		return nil, err
	}
	size, err := strconv.Atoi(string(digits))
	if err != nil || size < 0 {
		return nil, fmt.Errorf("invalid block length %q", digits)
	}
	if size > maxMessage {
		return nil, fmt.Errorf("block of %d bytes exceeds limit", size)
	}
	buf = append(buf, nd)
	buf = append(buf, digits...)
	start := len(buf)
	buf = append(buf, make([]byte, size)...)
	if n, err := io.ReadFull(c.r, buf[start:]); err != nil {
		return nil, fmt.Errorf("block truncated after %d/%d bytes: %w", n, size, err)
	}
	return append(buf, c.drainTerminators()...), nil
}

func (c *Conn) readBMP() ([]byte, error) {
	hdr := make([]byte, 6)
	if _, err := io.ReadFull(c.r, hdr); err != nil {
		return nil, err
	}
	size := int(binary.LittleEndian.Uint32(hdr[2:6]))
	if size < 6 || size > maxMessage {
		return nil, fmt.Errorf("invalid BMP size %d", size)
	}
	buf := make([]byte, size)
	copy(buf, hdr)
	if _, err := io.ReadFull(c.r, buf[6:]); err != nil {
		return nil, err
	}
	return append(buf, c.drainTerminators()...), nil
}

// drainTerminators consumes '\r'/'\n' bytes that follow a binary payload.
// Some firmware (e.g. SDS1000X-E WF?) sends "\n\n" after a block.
func (c *Conn) drainTerminators() []byte {
	var out []byte
	saved := time.Now().Add(drainWindow)
	_ = c.c.SetReadDeadline(saved)
	for {
		p, err := c.r.Peek(1)
		if err != nil || (p[0] != '\n' && p[0] != '\r') {
			return out
		}
		b, _ := c.r.ReadByte()
		out = append(out, b)
	}
}

// Clear emulates a device clear by reconnecting, which discards any pending output.
func (c *Conn) Clear(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.c.Close()
	if err := c.dial(ctx); err != nil {
		return wrap(ctx, "clear (reconnect)", err)
	}
	return nil
}

// Lock is a no-op: raw sockets have no instrument-side lock. The SCPI layer
// still takes the local file lock.
func (c *Conn) Lock(context.Context, time.Duration) error { return nil }

// Unlock is a no-op.
func (c *Conn) Unlock(context.Context) error { return nil }

// ReadSTB queries *STB? over the message channel.
func (c *Conn) ReadSTB(ctx context.Context) (byte, error) {
	if err := c.Write(ctx, []byte("*STB?\n")); err != nil {
		return 0, err
	}
	resp, err := c.Read(ctx)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(resp))
	if i := strings.LastIndexAny(s, " ,"); i >= 0 {
		s = s[i+1:]
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || v > 255 {
		return 0, fmt.Errorf("raw *STB?: unexpected response %q", resp)
	}
	return byte(v), nil
}

// Close closes the socket.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Close()
}
