package connect_test

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/config"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/connect"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/discovery"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
)

// instrument starts a raw-socket SDM3045X stand-in on ip:port.
func instrument(t *testing.T, ip string, port int, serial string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		t.Skipf("cannot listen on %s:%d: %v", ip, port, err)
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
					switch strings.TrimSpace(line) {
					case "*IDN?":
						_, _ = c.Write([]byte("Siglent Technologies,SDM3045X," + serial + ",5.01\n"))
					case "SYST:ERR?":
						_, _ = c.Write([]byte("0,\"No error\"\n"))
					case "READ?":
						_, _ = c.Write([]byte("+4.99700000E+00\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func closedPort(t *testing.T) int {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

type env struct {
	conn  *connect.Connector
	store *config.FileStore
	log   *bytes.Buffer
	port  int
}

func setup(t *testing.T, lastIP string) env {
	t.Helper()
	port := instrument(t, "127.0.0.1", 0, "WRONG")
	instrument(t, "127.0.0.2", port, "SN-DMM")
	store := config.New(filepath.Join(t.TempDir(), "bench.json"))
	if err := store.Save(&domain.Bench{Subnet: "127.0.0.0/30", Instruments: map[string]domain.InstrumentRef{
		"dmm": {Serial: "SN-DMM", Model: "SDM3045X", LastIP: lastIP, Transport: "raw"},
	}}); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	return env{
		conn: &connect.Connector{
			Store: store, Discoverer: &discovery.Scanner{Port: port, Raw: true, VXI11Port: -1, ConnectTimeout: 200 * time.Millisecond},
			Log: &log, RawPort: port, LockDir: t.TempDir(), VXI11Port: closedPort(t),
			DialTimeout: 500 * time.Millisecond, IOTimeout: time.Second,
		},
		store: store, log: &log, port: port,
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestOpenPinnedBySerial(t *testing.T) {
	e := setup(t, "127.0.0.2")
	inst, err := e.conn.Open(ctx(t), "dmm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	if inst.Role() != domain.RoleDMM || inst.Identity().Serial != "SN-DMM" || inst.SCPI().TransportName() != "raw" {
		t.Fatalf("%+v %s", inst.Identity(), inst.Role())
	}
}

func TestRediscoveryAfterIPChange(t *testing.T) {
	e := setup(t, "127.0.0.1") // a different instrument now answers here
	inst, err := e.conn.Open(ctx(t), "dmm")
	if err != nil {
		t.Fatalf("%v\nlog: %s", err, e.log)
	}
	_ = inst.Close()
	b, _ := e.store.Load()
	if b.Instruments["dmm"].LastIP != "127.0.0.2" {
		t.Fatalf("inventory not updated: %+v", b.Instruments["dmm"])
	}
	if !strings.Contains(e.log.String(), "moved to 127.0.0.2") {
		t.Fatalf("log: %s", e.log)
	}
}

func TestAutoNeverFallsBackToRaw(t *testing.T) {
	// Only a raw-socket instrument is listening: auto must fail with a hint
	// to ask for raw explicitly, not quietly use it.
	e := setup(t, "127.0.0.2")
	e.conn.Transport = connect.TransportAuto
	inst, err := e.conn.Open(ctx(t), "dmm")
	if err == nil {
		_ = inst.Close()
		t.Fatal("auto connected over raw")
	}
	if domain.KindOf(err) != domain.KindUnreachable || !strings.Contains(err.Error(), "--transport raw") {
		t.Fatalf("want unreachable with raw hint, got %v", err)
	}
	e.conn.Transport = connect.TransportRaw
	inst, err = e.conn.Open(ctx(t), "dmm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	if inst.SCPI().TransportName() != "raw" {
		t.Fatalf("transport %s", inst.SCPI().TransportName())
	}
}

func TestBusyAndErrors(t *testing.T) {
	e := setup(t, "127.0.0.2")
	l, err := scpi.AcquireFileLock(ctx(t), e.conn.LockDir, "SN-DMM", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.conn.Open(ctx(t), "dmm"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("expected busy, got %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := e.conn.Open(ctx(t), "nope"); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("unknown alias: %v", err)
	}
	b, _ := e.store.Load()
	ref := b.Instruments["dmm"]
	ref.Serial = "GONE"
	b.Instruments["dmm"] = ref
	if err := e.store.Save(b); err != nil {
		t.Fatal(err)
	}
	if _, err := e.conn.Open(ctx(t), "dmm"); domain.KindOf(err) != domain.KindUnreachable {
		t.Fatalf("missing serial: %v", err)
	}
	e.conn.Transport = "carrier-pigeon"
	if _, err := e.conn.OpenAddr(ctx(t), "127.0.0.2", "carrier-pigeon", ""); err == nil {
		t.Fatal("unknown transport")
	}
}

func TestDiscoveryScan(t *testing.T) {
	e := setup(t, "127.0.0.2")
	s := &discovery.Scanner{Port: e.port, Raw: true, VXI11Port: -1, ConnectTimeout: 200 * time.Millisecond}
	found, err := s.Scan(ctx(t), "127.0.0.0/30")
	if err != nil || len(found) != 2 {
		t.Fatalf("%+v %v", found, err)
	}
	if found[0].IP != "127.0.0.1" || found[1].Identity.Serial != "SN-DMM" || found[1].Role != domain.RoleDMM {
		t.Fatalf("%+v", found)
	}
}

func TestHosts(t *testing.T) {
	for cidr, n := range map[string]int{"192.168.2.0/24": 254, "10.0.0.0/30": 2, "10.0.0.5/32": 1, "10.0.0.0/31": 2} {
		h, err := discovery.Hosts(cidr)
		if err != nil || len(h) != n {
			t.Errorf("%s: %d hosts, %v", cidr, len(h), err)
		}
	}
	for _, bad := range []string{"nope", "10.0.0.0/8", "::1/128"} {
		if _, err := discovery.Hosts(bad); domain.KindOf(err) != domain.KindUsage {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestConfigStore(t *testing.T) {
	dir := t.TempDir()
	s := config.New(filepath.Join(dir, "sub", "bench.json"))
	b, err := s.Load()
	if err != nil || len(b.Instruments) != 0 {
		t.Fatalf("missing file: %+v %v", b, err)
	}
	b.Subnet = "1.2.3.0/24"
	b.Limits = map[string]map[string]any{"fg": {"max_amp_vpp": 10.0}}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Subnet != "1.2.3.0/24" {
		t.Fatalf("%+v %v", got, err)
	}
	if v, ok := got.Limit("fg", "max_amp_vpp"); !ok || v != 10 {
		t.Fatal("limit lost in round trip")
	}
	if err := os.WriteFile(s.Path(), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("malformed: %v", err)
	}
	if !strings.HasSuffix(config.DefaultPath(), filepath.Join("benchctl", "bench.json")) {
		t.Fatal(config.DefaultPath())
	}
}
