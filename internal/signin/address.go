package signin

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Proxies are the addresses of the proxies in front of the server, from the operator's `trustedProxies`. A nil *Proxies
// is the key absent: no address is counted.
type Proxies struct{ prefixes []netip.Prefix }

// ParseProxies reads addresses and CIDR prefixes. An empty list declares no proxy, which is not the key absent.
func ParseProxies(entries []string) (*Proxies, error) {
	p := &Proxies{}
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, addrErr := netip.ParseAddr(entry)
			if addrErr != nil {
				return nil, fmt.Errorf("trustedProxies: %q is no IP address or CIDR prefix", entry)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		p.prefixes = append(p.prefixes, prefix)
	}
	return p, nil
}

func (p *Proxies) has(addr netip.Addr) bool {
	for _, prefix := range p.prefixes {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// Peer is the address of the connection that sent the request.
func Peer(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr.Unmap()
}

// Client is the address the request came from: its peer, unless that is a declared proxy, and then the entry of
// X-Forwarded-For nearest the server that is no declared proxy (an entry the client sent ahead of it changes nothing).
// A header from any other peer is ignored. ok is false when a declared proxy forwarded no entry that counts: none, every
// one a declared proxy, or the nearest not an address. The proxy's own address is never returned for its clients, as one
// guesser would then refuse them all.
func (p *Proxies) Client(r *http.Request) (addr netip.Addr, ok bool) {
	peer := Peer(r)
	if !p.has(peer) {
		return peer, true
	}
	var entries []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(line, ",")...)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(entries[i]))
		if err != nil {
			return netip.Addr{}, false
		}
		if !p.has(addr) {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// UndeclaredIngress says that, with no proxy declared at all, a request from a private or loopback peer carries a
// forwarded header: likely an ingress the operator forgot to declare.
func (p *Proxies) UndeclaredIngress(r *http.Request) bool {
	peer := Peer(r)
	return len(p.prefixes) == 0 && (peer.IsLoopback() || peer.IsPrivate()) && r.Header.Get("X-Forwarded-For") != ""
}

// Key is what an address counts as: an IPv6 address as its /64, an IPv4-mapped one as its IPv4 address.
func Key(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().Addr().String()
	}
	return addr.String()
}
