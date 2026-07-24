package api

import (
	"crypto/tls"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func testTrust(prefixes ...string) *RequestTrust {
	parsed := make([]netip.Prefix, 0, len(prefixes))
	for _, raw := range prefixes {
		parsed = append(parsed, netip.MustParsePrefix(raw))
	}
	return NewRequestTrust(parsed)
}

func TestAuthenticatorAndHandlerShareRequestTrust(t *testing.T) {
	trust := testTrust("10.0.0.0/8")
	auth := NewAuthenticatorWithRequestTrust("token", strings.Repeat("c", 64), true, nil, trust)
	handler := NewHandler(nil, nil, nil, nil, auth, nil, trust)
	if auth.requestTrust != trust || handler.requestTrust != trust {
		t.Fatal("composition did not preserve the shared request-trust policy")
	}
}

func TestHandlerRejectsDifferentRequestTrustPolicy(t *testing.T) {
	authTrust := testTrust("10.0.0.0/8")
	auth := NewAuthenticatorWithRequestTrust("token", strings.Repeat("c", 64), true, nil, authTrust)
	defer func() {
		if recover() == nil {
			t.Fatal("handler accepted a request-trust policy different from the authenticator")
		}
	}()
	NewHandler(nil, nil, nil, nil, auth, nil, testTrust("192.0.2.0/24"))
}

func TestRequestTrustIgnoresForwardingHeadersFromUntrustedPeer(t *testing.T) {
	trust := testTrust("10.0.0.0/8")
	req := httptest.NewRequest("GET", "http://control.example/api", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := trust.clientIP(req); got != "192.0.2.10" {
		t.Fatalf("client IP = %q, want direct peer", got)
	}
	if got := trust.scheme(req); got != "http" {
		t.Fatalf("scheme = %q, want http", got)
	}
}

func TestRequestTrustWalksTrustedChainRightToLeft(t *testing.T) {
	trust := testTrust("10.0.0.0/8", "192.168.0.0/16")
	req := httptest.NewRequest("GET", "http://control.example/api", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 192.168.1.5")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := trust.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("client IP = %q, want first untrusted hop", got)
	}
	if got := trust.scheme(req); got != "https" {
		t.Fatalf("scheme = %q, want https", got)
	}

	// The immediate proxy is trusted, but the right-most forwarded address is
	// not. It is therefore the client identity; a spoofed value to its left must
	// never become the rate-limit identity.
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.7")
	if got := trust.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("spoofed chain client IP = %q, want first untrusted hop", got)
	}
}

func TestRequestTrustNormalizesMappedIPv4AndFailsClosed(t *testing.T) {
	trust := testTrust("10.0.0.0/8")
	req := httptest.NewRequest("GET", "http://control.example/api", nil)
	req.RemoteAddr = "[::ffff:10.0.0.2]:443"
	req.Header.Set("X-Forwarded-For", "::ffff:203.0.113.7")
	if got := trust.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("mapped client IP = %q, want normalized IPv4", got)
	}

	req.Header.Set("X-Forwarded-For", "203.0.113.7, not-an-ip")
	req.Header.Set("X-Forwarded-Proto", "https,http")
	if got := trust.clientIP(req); got != "10.0.0.2" {
		t.Fatalf("malformed XFF client IP = %q, want trusted peer fallback", got)
	}
	if got := trust.scheme(req); got != "http" {
		t.Fatalf("malformed XFP scheme = %q, want fail-closed http", got)
	}
}

func TestRequestTrustBoundsForwardedChain(t *testing.T) {
	trust := testTrust("10.0.0.0/8")
	req := httptest.NewRequest("GET", "http://control.example/api", nil)
	req.RemoteAddr = "10.0.0.2:443"

	oversized := strings.Repeat("1", maxForwardedForBytes+1)
	req.Header.Set("X-Forwarded-For", oversized)
	if got := trust.clientIP(req); got != "10.0.0.2" {
		t.Fatalf("oversized XFF client IP = %q, want peer fallback", got)
	}

	hops := make([]string, maxForwardedForHops+1)
	for i := range hops {
		hops[i] = "192.0.2.1"
	}
	req.Header.Set("X-Forwarded-For", strings.Join(hops, ","))
	if got := trust.clientIP(req); got != "10.0.0.2" {
		t.Fatalf("over-hop-limit XFF client IP = %q, want peer fallback", got)
	}

	req.Header.Set("X-Forwarded-For", strings.Repeat(",", maxForwardedForHops))
	if got := trust.clientIP(req); got != "10.0.0.2" {
		t.Fatalf("comma-heavy malformed XFF client IP = %q, want peer fallback", got)
	}
}

func TestRequestTrustSameOriginUsesEffectiveScheme(t *testing.T) {
	trust := testTrust("10.0.0.0/8")
	req := httptest.NewRequest("GET", "http://control.example/api/logs", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Host = "control.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://control.example")
	if !trust.sameOrigin(req) {
		t.Fatal("same HTTPS origin behind trusted proxy was rejected")
	}
	req.Header.Set("Origin", "http://control.example")
	if trust.sameOrigin(req) {
		t.Fatal("origin with wrong effective scheme was accepted")
	}

	directTLS := httptest.NewRequest("GET", "https://control.example/api/logs", nil)
	directTLS.Host = "control.example"
	directTLS.RemoteAddr = "192.0.2.1:443"
	directTLS.TLS = &tls.ConnectionState{}
	directTLS.Header.Set("Origin", "https://control.example")
	if !trust.sameOrigin(directTLS) {
		t.Fatal("direct TLS same origin was rejected")
	}
}
