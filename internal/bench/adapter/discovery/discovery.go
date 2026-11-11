// Package discovery finds SCPI instruments by scanning a subnet for VXI-11
// (and, only when asked, the raw socket on TCP 5025) and asking *IDN?. MAC OUIs are not usable (network modules are third-party)
// and the instruments do not advertise over mDNS.
package discovery

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/transport/raw"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/transport/vxi11"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// Scanner implements port.Discoverer.
type Scanner struct {
	Port           int           // raw socket port, default 5025
	Raw            bool          // also probe the raw socket when VXI-11 fails (last resort; off by default)
	VXI11Port      int           // portmapper port, default 111; -1 disables VXI-11 probing
	ConnectTimeout time.Duration // default 400ms
	IDNTimeout     time.Duration // default 2s
	Parallel       int           // default 64
}

var _ port.Discoverer = (*Scanner)(nil)

// Hosts lists the usable host addresses in cidr (at most a /16).
func Hosts(cidr string) ([]netip.Addr, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, domain.Usagef("invalid subnet %q: %v", cidr, err)
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return nil, domain.Usagef("only IPv4 subnets are supported")
	}
	if p.Bits() < 16 {
		return nil, domain.Usagef("subnet %s too large to scan (max /16)", cidr)
	}
	var out []netip.Addr
	for a := p.Addr(); p.Contains(a); a = a.Next() {
		out = append(out, a)
	}
	if p.Bits() <= 30 && len(out) > 2 {
		out = out[1 : len(out)-1] // drop network and broadcast
	}
	return out, nil
}

// Scan implements port.Discoverer.
func (s *Scanner) Scan(ctx context.Context, cidr string) ([]port.Found, error) {
	hosts, err := Hosts(cidr)
	if err != nil {
		return nil, err
	}
	pnum := s.Port
	if pnum == 0 {
		pnum = raw.DefaultPort
	}
	par := s.Parallel
	if par <= 0 {
		par = 64
	}
	var (
		mu    sync.Mutex
		found []port.Found
		wg    sync.WaitGroup
		sem   = make(chan struct{}, par)
	)
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h netip.Addr) {
			defer wg.Done()
			defer func() { <-sem }()
			if f, ok := s.probe(ctx, h.String(), pnum); ok {
				mu.Lock()
				found = append(found, f)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool {
		a, _ := netip.ParseAddr(found[i].IP)
		b, _ := netip.ParseAddr(found[j].IP)
		return a.Less(b)
	})
	return found, ctx.Err()
}

func (s *Scanner) probe(ctx context.Context, ip string, pnum int) (port.Found, bool) {
	ct := s.ConnectTimeout
	if ct == 0 {
		ct = 400 * time.Millisecond
	}
	it := s.IDNTimeout
	if it == 0 {
		it = 2 * time.Second
	}
	// VXI-11 only, unless the raw socket was asked for: VXI-11 links are
	// torn down cleanly, while the SDS1000X-E raw socket server never closes
	// its side of a session, so every raw probe leaks a session on the scope
	// until its socket server stops answering.
	if f, ok := s.probeVXI11(ctx, ip, ct, it); ok || !s.Raw {
		return f, ok
	}
	cctx, cancel := context.WithTimeout(ctx, ct)
	conn, err := raw.Dial(cctx, net.JoinHostPort(ip, fmt.Sprint(pnum)))
	cancel()
	if err != nil {
		return port.Found{}, false
	}
	defer func() { _ = conn.Close() }()
	return identify(ctx, scpi.NewSession(conn, nil), ip, "raw", it)
}

func (s *Scanner) probeVXI11(ctx context.Context, ip string, ct, it time.Duration) (port.Found, bool) {
	pm := s.VXI11Port
	if pm < 0 {
		return port.Found{}, false
	}
	if pm == 0 {
		pm = 111
	}
	// A plain TCP connect to the portmapper is harmless and fails fast.
	var d net.Dialer
	cctx, cancel := context.WithTimeout(ctx, ct)
	c, err := d.DialContext(cctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(pm)))
	cancel()
	if err != nil {
		return port.Found{}, false
	}
	_ = c.Close()
	vctx, cancel := context.WithTimeout(ctx, it)
	defer cancel()
	link, err := vxi11.Dial(vctx, ip, vxi11.Options{PortmapperPort: pm})
	if err != nil {
		return port.Found{}, false
	}
	defer func() { _ = link.Close() }()
	return identify(ctx, scpi.NewSession(link, nil), ip, "vxi11", it)
}

func identify(ctx context.Context, s *scpi.Session, ip, via string, timeout time.Duration) (port.Found, bool) {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := s.Query(qctx, "*IDN?")
	if err != nil {
		return port.Found{}, false
	}
	id, err := domain.ParseIDN(resp)
	if err != nil {
		return port.Found{}, false
	}
	return port.Found{IP: ip, Identity: id, Role: siglent.RoleFor(id.Model), Via: via}, true
}
