package main

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

var caddyProxy = newProxyTrust()

type proxyTrust struct {
	mu           sync.Mutex
	lookup       func(context.Context, string, string) ([]netip.Addr, error)
	addresses    []netip.Addr
	expires      time.Time
	recheckAfter time.Time
}

func newProxyTrust() *proxyTrust {
	return &proxyTrust{lookup: net.DefaultResolver.LookupNetIP}
}

func (p *proxyTrust) contains(peer netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	known := false
	for _, addr := range p.addresses {
		if addr.Unmap() == peer {
			known = true
			break
		}
	}
	expired := !now.Before(p.expires)
	if expired || (!known && !now.Before(p.recheckAfter)) {
		if !expired {
			// Unknown peers can trigger early refresh, at most once per second.
			p.recheckAfter = now.Add(time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		addresses, err := p.lookup(ctx, "ip", "caddy")
		p.addresses = nil
		if err == nil && len(addresses) > 0 {
			p.addresses = addresses
			p.expires = time.Now().Add(30 * time.Second)
		} else {
			// Fail closed, but retry soon if Caddy is starting or being recreated.
			p.expires = time.Now().Add(time.Second)
			p.recheckAfter = p.expires
		}
	}
	for _, addr := range p.addresses {
		if addr.Unmap() == peer {
			return true
		}
	}
	return false
}
