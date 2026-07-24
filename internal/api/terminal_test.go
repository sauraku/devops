package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTerminalAttemptRateLimitPrunesExpiredAddresses(t *testing.T) {
	limiter := newTerminalAttemptLimiter()
	now := time.Now()
	if !limiter.allow("192.0.2.1", now) {
		t.Fatal("first terminal attempt was rejected")
	}
	if limiter.allow("192.0.2.1", now.Add(time.Second)) {
		t.Fatal("rate-limited terminal attempt was accepted")
	}
	if !limiter.allow("192.0.2.2", now.Add(terminalRateLimit+time.Second)) {
		t.Fatal("new terminal attempt was rejected")
	}
	_, firstIPPresent := limiter.lastAttempt["192.0.2.1"]
	if firstIPPresent {
		t.Fatal("expired terminal attempt was not pruned")
	}
}

func TestTerminalRejectsPlaintextBeforeWebSocketUpgrade(t *testing.T) {
	h := &Handler{requestTrust: testTrust("10.0.0.0/8"), terminalLimiter: newTerminalAttemptLimiter()}
	req := httptest.NewRequest(http.MethodGet, "http://control.example/api/terminal", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Origin", "http://control.example")
	recorder := httptest.NewRecorder()

	h.TerminalHandler(recorder, req)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext terminal status = %d, want 426", recorder.Code)
	}
}

func TestTerminalAllowsDirectLoopbackHTTPToReachUpgrade(t *testing.T) {
	h := &Handler{requestTrust: NewRequestTrust(nil), terminalLimiter: newTerminalAttemptLimiter()}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/terminal", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Origin", "http://127.0.0.1:8787")
	recorder := httptest.NewRecorder()

	h.TerminalHandler(recorder, req)
	if recorder.Code == http.StatusUpgradeRequired {
		t.Fatal("direct loopback HTTP terminal request was rejected before upgrade")
	}
}

func TestTerminalUsesForwardedClientForRateLimit(t *testing.T) {
	h := &Handler{requestTrust: testTrust("10.0.0.0/8"), terminalLimiter: newTerminalAttemptLimiter()}
	request := func(clientIP string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://control.example/api/terminal", nil)
		req.RemoteAddr = "10.0.0.2:443"
		req.Host = "control.example"
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "https://control.example")
		recorder := httptest.NewRecorder()
		h.TerminalHandler(recorder, req)
		return recorder
	}

	if recorder := request("192.0.2.1"); recorder.Code == http.StatusUpgradeRequired {
		t.Fatal("trusted HTTPS terminal request was rejected as plaintext")
	}
	if recorder := request("192.0.2.1"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded client status = %d, want 429", recorder.Code)
	}
	if recorder := request("192.0.2.2"); recorder.Code == http.StatusTooManyRequests {
		t.Fatal("different forwarded client shared the terminal rate-limit bucket")
	}
}

func TestTerminalResizeRequiresTypedControlFrame(t *testing.T) {
	cols, rows, ok := parseTerminalResizeFrame([]byte(`{"type":"resize","cols":120,"rows":40}`))
	if !ok || cols != 120 || rows != 40 {
		t.Fatalf("typed resize parsed as cols=%d rows=%d ok=%v", cols, rows, ok)
	}
	for _, pastedInput := range []string{
		`{"cols":120,"rows":40}`,
		`{"type":"application-data","cols":120,"rows":40}`,
		`{"type":"resize","cols":0,"rows":40}`,
		`{"type":"resize","cols":120,"rows":501}`,
	} {
		if _, _, ok := parseTerminalResizeFrame([]byte(pastedInput)); ok {
			t.Fatalf("terminal input was swallowed as resize control: %s", pastedInput)
		}
	}
}
