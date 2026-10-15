package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientIPTrust(t *testing.T) {
	old := caddyProxy
	t.Cleanup(func() { caddyProxy = old })
	caddyProxy = newProxyTrust()
	caddyProxy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("172.20.0.2")}, nil
	}
	for _, tc := range []struct{ peer, xff, xri, want string }{
		{"203.0.113.1:1234", "8.8.8.8", "1.1.1.1", "203.0.113.1"},
		{"172.20.0.3:1234", "8.8.8.8", "", "172.20.0.3"},
		{"172.20.0.2:1234", "8.8.8.8", "1.1.1.1", "8.8.8.8"},
		{"172.20.0.2:1234", "2001:db8::1", "", "2001:db8::1"},
		{"172.20.0.2:1234", "<script>alert(1)</script>", "8.8.8.8", "172.20.0.2"},
		{"172.20.0.2:1234", "8.8.8.8, 1.1.1.1", "", "172.20.0.2"},
		{"172.20.0.2:1234", "", "8.8.8.8", "172.20.0.2"},
		{"172.20.0.2:1234", "fe80::1%eth0", "", "172.20.0.2"},
		{"[2001:db8::2]:1234", "8.8.8.8", "", "2001:db8::2"},
	} {
		r := httptest.NewRequest("GET", "/ip", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.xff)
		r.Header.Set("X-Real-IP", tc.xri)
		if got := clientIP(r); got != tc.want {
			t.Errorf("peer=%q xff=%q: got %q want %q", tc.peer, tc.xff, got, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "/ip", nil)
	r.RemoteAddr = "172.20.0.2:1234"
	r.Header.Add("X-Forwarded-For", "8.8.8.8")
	r.Header.Add("X-Forwarded-For", "1.1.1.1")
	if got := clientIP(r); got != "172.20.0.2" {
		t.Fatal(got)
	}
}

func TestProxyDNSRefreshAndFailure(t *testing.T) {
	p := newProxyTrust()
	addr := netip.MustParseAddr("172.20.0.2")
	calls := 0
	fail := false
	p.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		calls++
		if host != "caddy" || network != "ip" {
			t.Fatal(network, host)
		}
		if fail {
			return nil, errors.New("DNS unavailable")
		}
		return []netip.Addr{addr}, nil
	}
	if !p.contains(addr) || !p.contains(addr) || calls != 1 {
		t.Fatal("DNS cache failed")
	}
	old := addr
	addr = netip.MustParseAddr("172.20.0.4")
	p.expires = time.Time{}
	if p.contains(old) || !p.contains(addr) {
		t.Fatal("old proxy is still trusted")
	}
	fail = true
	p.expires = time.Time{}
	if p.contains(addr) {
		t.Fatal("trusted proxy after DNS failure")
	}
}
