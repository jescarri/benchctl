package discovery

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// rawOnly serves *IDN? on a raw socket, like an instrument without VXI-11.
func rawOnly(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					_, _ = c.Write([]byte("Siglent Technologies,SDM3045X,SN-RAW,1.01\n"))
				}
			}(c)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRawProbedOnlyWhenAsked(t *testing.T) {
	p := rawOnly(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := &Scanner{Port: p, VXI11Port: -1, ConnectTimeout: 200 * time.Millisecond}
	found, err := s.Scan(ctx, "127.0.0.1/32")
	if err != nil || len(found) != 0 {
		t.Fatalf("default scan used raw: %v %v", found, err)
	}
	s.Raw = true
	found, err = s.Scan(ctx, "127.0.0.1/32")
	if err != nil || len(found) != 1 || found[0].Via != "raw" || found[0].Identity.Serial != "SN-RAW" {
		t.Fatalf("raw scan: %+v %v", found, err)
	}
}
