package raw

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// server is a tiny SCPI raw-socket server: each received line is answered by
// respond (nil = no answer).
func server(t *testing.T, respond func(cmd string) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if out := respond(strings.TrimRight(line, "\n")); out != nil {
						_, _ = c.Write(out)
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func block(header string, data []byte, trailer string) []byte {
	l := []byte(header)
	n := len(data)
	s := itoa(n)
	pad := strings.Repeat("0", 9-len(s)) + s
	l = append(l, '#', '9')
	l = append(l, pad...)
	l = append(l, data...)
	return append(l, trailer...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestLineAndBlockFraming(t *testing.T) {
	payload := []byte{0x00, '\n', '#', 0xff, '\n', 'x'} // binary with embedded terminators
	addr := server(t, func(cmd string) []byte {
		switch cmd {
		case "*IDN?":
			return []byte("Siglent,SDS1104X-E,SN1,1.0\n")
		case "C1:WF? DESC":
			// header + block + the SDS "\n\n" trailer, then a following line
			return block("DESC,", payload, "\n\n")
		case "HASH?":
			return []byte("value#1\n") // '#' not after a separator: plain line
		}
		return nil
	})
	c, err := Dial(ctxT(t), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctxT(t)

	for i := 0; i < 2; i++ { // twice: the trailer must not leak into the next read
		if err := c.Write(ctx, []byte("C1:WF? DESC")); err != nil {
			t.Fatal(err)
		}
		msg, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := block("DESC,", payload, "\n\n")
		if string(msg) != string(want) {
			t.Fatalf("block message = %q, want %q", msg, want)
		}
	}
	if err := c.Write(ctx, []byte("*IDN?\n")); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Read(ctx)
	if err != nil || string(msg) != "Siglent,SDS1104X-E,SN1,1.0\n" {
		t.Fatalf("line after block = %q, %v", msg, err)
	}
	if err := c.Write(ctx, []byte("HASH?")); err != nil {
		t.Fatal(err)
	}
	msg, _ = c.Read(ctx)
	if string(msg) != "value#1\n" {
		t.Fatalf("hash line = %q", msg)
	}
}

func TestBMPFraming(t *testing.T) {
	bmp := make([]byte, 1000)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[2:], uint32(len(bmp)))
	for i := 6; i < len(bmp); i++ {
		bmp[i] = byte(i) // includes '\n' (10) and '#' (35)
	}
	addr := server(t, func(cmd string) []byte {
		switch cmd {
		case "SCDP":
			return append(append([]byte{}, bmp...), '\n')
		case "*STB?":
			return []byte("4\n")
		}
		return nil
	})
	c, err := Dial(ctxT(t), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := ctxT(t)
	if err := c.Write(ctx, []byte("SCDP")); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg) != len(bmp)+1 || string(msg[:len(bmp)]) != string(bmp) {
		t.Fatalf("bmp len %d", len(msg))
	}
	stb, err := c.ReadSTB(ctx)
	if err != nil || stb != 4 {
		t.Fatalf("STB = %d, %v", stb, err)
	}
}

func TestTimeoutAndClear(t *testing.T) {
	addr := server(t, func(cmd string) []byte {
		if cmd == "*IDN?" {
			return []byte("A,B,C,D\n")
		}
		return nil // unknown query: silence, like the SDS
	})
	c, err := Dial(ctxT(t), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Write(ctx, []byte("C2:SANU?")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if err := c.Clear(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctxT(t), []byte("*IDN?")); err != nil {
		t.Fatal(err)
	}
	if msg, err := c.Read(ctxT(t)); err != nil || string(msg) != "A,B,C,D\n" {
		t.Fatalf("after clear: %q %v", msg, err)
	}
	if c.Name() != "raw" || c.Lock(ctxT(t), 0) != nil || c.Unlock(ctxT(t)) != nil {
		t.Error("raw lock should be a no-op")
	}
}

func TestDialDefaultPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Dial(ctx, "127.0.0.1") // port 5025 is almost certainly closed here
	if err == nil {
		t.Skip("something listens on 5025 locally")
	}
}
