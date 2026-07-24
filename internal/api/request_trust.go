package api

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

const (
	// Proxy chains should be short. These bounds keep attacker-controlled
	// forwarding metadata cheap even though the HTTP server accepts larger
	// aggregate headers.
	maxForwardedForBytes = 4096
	maxForwardedForHops  = 32
)

// RequestTrust is an immutable forwarding-header policy. It applies forwarded
// values only when the TCP peer is an explicitly trusted reverse proxy.
type RequestTrust struct {
	trustedProxyCIDRs []netip.Prefix
}

func NewRequestTrust(trustedProxyCIDRs []netip.Prefix) *RequestTrust {
	prefixes := make([]netip.Prefix, len(trustedProxyCIDRs))
	copy(prefixes, trustedProxyCIDRs)
	return &RequestTrust{trustedProxyCIDRs: prefixes}
}

func (t *RequestTrust) clientIP(r *http.Request) string {
	peer, ok := remoteIP(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if !t.isTrustedProxy(peer) {
		return peer.String()
	}

	forwardedValues := r.Header.Values("X-Forwarded-For")
	if len(forwardedValues) != 1 {
		return peer.String()
	}
	forwarded := forwardedValues[0]
	if len(forwarded) > maxForwardedForBytes || strings.TrimSpace(forwarded) == "" {
		return peer.String()
	}

	current := peer
	identity := peer
	foundUntrusted := false
	hops := 0
	for end := len(forwarded); ; {
		start := end - 1
		for start >= 0 && forwarded[start] != ',' {
			start--
		}
		hops++
		if hops > maxForwardedForHops {
			return peer.String()
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(forwarded[start+1 : end]))
		if err != nil {
			return peer.String()
		}
		if !foundUntrusted {
			if !t.isTrustedProxy(current) {
				identity = current
				foundUntrusted = true
			} else {
				current = addr.Unmap()
			}
		}
		if start < 0 {
			break
		}
		end = start
	}
	if !foundUntrusted {
		identity = current
	}
	return identity.String()
}

func (t *RequestTrust) scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	peer, ok := remoteIP(r.RemoteAddr)
	if !ok || !t.isTrustedProxy(peer) {
		return "http"
	}
	forwardedProtoValues := r.Header.Values("X-Forwarded-Proto")
	if len(forwardedProtoValues) != 1 {
		return "http"
	}
	forwardedProto := strings.TrimSpace(forwardedProtoValues[0])
	if strings.Contains(forwardedProto, ",") {
		return "http"
	}
	switch strings.ToLower(forwardedProto) {
	case "https":
		return "https"
	case "http":
		return "http"
	default:
		return "http"
	}
}

func (t *RequestTrust) isHTTPS(r *http.Request) bool {
	return t.scheme(r) == "https"
}

func (t *RequestTrust) allowsSensitiveWebSocket(r *http.Request) bool {
	return t.isHTTPS(r) || t.directLoopbackHTTP(r)
}

func (t *RequestTrust) sameOrigin(r *http.Request) bool {
	originValues := r.Header.Values("Origin")
	if len(originValues) != 1 {
		return false
	}
	origin := strings.TrimSpace(originValues[0])
	if origin == "" || strings.Contains(origin, ",") {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return strings.EqualFold(parsed.Scheme, t.scheme(r)) && strings.EqualFold(parsed.Host, r.Host)
}

func (t *RequestTrust) directLoopbackHTTP(r *http.Request) bool {
	peer, ok := remoteIP(r.RemoteAddr)
	if !ok || !peer.IsLoopback() || t.isTrustedProxy(peer) {
		return false
	}
	host := r.Host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func (t *RequestTrust) isTrustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range t.trustedProxyCIDRs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
