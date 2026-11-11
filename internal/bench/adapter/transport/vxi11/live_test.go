//go:build bench

package vxi11

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Run with: BENCH_VXI11_HOST=192.168.2.76 go test -tags bench ./internal/bench/adapter/transport/vxi11/
func TestLiveVXI11(t *testing.T) {
	host := os.Getenv("BENCH_VXI11_HOST")
	if host == "" {
		t.Skip("BENCH_VXI11_HOST not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, Options{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	t.Logf("link ok: maxRecvSize=%d abortPort=%d", c.maxRecv, c.abortPort)

	if err := c.Write(ctx, []byte("*IDN?\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(resp), "Siglent") {
		t.Fatalf("unexpected IDN %q", resp)
	}
	t.Logf("IDN: %q", resp)

	stb, err := c.ReadSTB(ctx)
	t.Logf("device_readstb: stb=%d err=%v", stb, err)
	t.Logf("device_clear: err=%v", c.Clear(ctx))
	t.Logf("device_lock: err=%v", c.Lock(ctx, time.Second))
	t.Logf("device_unlock: err=%v", c.Unlock(ctx))

	// A second link must be refused while the first holds the lock.
	if err := c.Lock(ctx, 0); err == nil {
		c2, err := Dial(ctx, host, Options{ClientID: 2})
		if err == nil {
			err2 := c2.Lock(ctx, 0)
			t.Logf("second link lock while held: err=%v (locked=%v)", err2, IsCode(err2, ErrCodeLocked))
			_ = c2.Close()
		}
		_ = c.Unlock(ctx)
	}
	t.Logf("device_abort: err=%v", c.Abort(ctx))
}

// TestLiveSRQ: BENCH_VXI11_HOST=... go test -tags bench -run TestLiveSRQ ./internal/bench/adapter/transport/vxi11/
func TestLiveSRQ(t *testing.T) {
	host := os.Getenv("BENCH_VXI11_HOST")
	if host == "" {
		t.Skip("BENCH_VXI11_HOST not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.EnableSRQ(ctx); err != nil {
		t.Skipf("interrupt channel not supported: %v", err)
	}
	for _, cmd := range []string{"*CLS", "*ESE 1", "*SRE 32", "*OPC"} {
		if err := c.Write(ctx, []byte(cmd+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
	defer wcancel()
	err = c.WaitSRQ(wctx)
	stb, _ := c.ReadSTB(ctx)
	t.Logf("SRQ err=%v after %s, stb=%d, srq calls=%d, intr connections=%d, listener=%s",
		err, time.Since(start).Round(time.Millisecond), stb, c.SRQCount(), c.SRQConnections(), c.srq.ln.Addr())
	_ = c.Write(ctx, []byte("*STB?\n"))
	q, _ := c.Read(ctx)
	t.Logf("device_readstb=%d vs *STB?=%q", stb, q)
	_ = c.Write(ctx, []byte("*SRE 0\n"))
	_ = c.Write(ctx, []byte("*ESR?\n"))
	_, _ = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
}
