package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

var caddyProxy = newProxyTrust()

type proxyTrust struct {
	mu           sync.RWMutex
	refreshMu    sync.Mutex
	lookup       func(context.Context, string, string) ([]netip.Addr, error)
	addresses    []netip.Addr
	expires      time.Time
	recheckAfter time.Time
}

func newProxyTrust() *proxyTrust {
	return &proxyTrust{lookup: net.DefaultResolver.LookupNetIP}
}

func (p *proxyTrust) contains(peer netip.Addr) bool {
	p.mu.RLock()
	known := p.matches(peer)
	expired := !time.Now().Before(p.expires)
	p.mu.RUnlock()
	if known && !expired {
		return true
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.RLock()
	now := time.Now()
	known = p.matches(peer)
	expired = !now.Before(p.expires)
	refresh := expired || (!known && !now.Before(p.recheckAfter))
	p.mu.RUnlock()
	if refresh {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		addresses, err := p.lookup(ctx, "ip", "caddy")
		p.mu.Lock()
		if !expired {
			p.recheckAfter = time.Now().Add(time.Second)
		}
		p.addresses = nil
		if err == nil && len(addresses) > 0 {
			p.addresses = addresses
			p.expires = time.Now().Add(30 * time.Second)
		} else {
			// Fail closed, but retry soon if Caddy is starting or being recreated.
			p.expires = time.Now().Add(time.Second)
			p.recheckAfter = p.expires
		}
		p.mu.Unlock()
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.matches(peer)
}

// Caller holds mu for reading or writing.
func (p *proxyTrust) matches(peer netip.Addr) bool {
	for _, addr := range p.addresses {
		if addr.Unmap() == peer {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	peer := stripPort(strings.TrimSpace(r.RemoteAddr))
	if addr, err := netip.ParseAddr(peer); err == nil && caddyProxy.contains(addr.Unmap()) {
		// Caddy supplies one address, never a client-provided forwarding chain.
		values := r.Header.Values("X-Forwarded-For")
		if len(values) == 1 {
			if forwarded, err := netip.ParseAddr(strings.TrimSpace(values[0])); err == nil && forwarded.Zone() == "" {
				return forwarded.Unmap().String()
			}
		}
	}
	return peer
}

func stripPort(v string) string {
	if parsedIP, err := netip.ParseAddr(v); err == nil {
		return parsedIP.String()
	}
	if parsedAddrPort, err := netip.ParseAddrPort(v); err == nil {
		return parsedAddrPort.Addr().String()
	}
	host, _, err := net.SplitHostPort(v)
	if err == nil && host != "" {
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		return host
	}
	return v
}
