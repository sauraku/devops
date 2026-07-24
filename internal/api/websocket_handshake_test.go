package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestSensitiveWebSocketHandshakePolicy(t *testing.T) {
	type handlerFactory func(*testing.T, *RequestTrust) http.Handler
	handlers := map[string]handlerFactory{
		"deployment log": func(t *testing.T, trust *RequestTrust) http.Handler {
			streamer := NewLogStreamer()
			logPath := filepath.Join(t.TempDir(), "deployment.log")
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !trust.allowsSensitiveWebSocket(r) {
					writeError(w, http.StatusUpgradeRequired, "HTTPS is required for deployment logs")
					return
				}
				streamer.handleWebSocket(w, r, logPath, trust)
			})
		},
		"terminal": func(_ *testing.T, trust *RequestTrust) http.Handler {
			handler := &Handler{requestTrust: trust, terminalLimiter: newTerminalAttemptLimiter()}
			return http.HandlerFunc(handler.TerminalHandler)
		},
	}

	tests := []struct {
		name        string
		remoteAddr  string
		origin      string
		multiple    bool
		wantSuccess bool
		wantStatus  int
	}{
		{name: "trusted proxy HTTPS same origin", remoteAddr: "10.0.0.2:443", origin: "same", wantSuccess: true},
		{name: "foreign origin", remoteAddr: "10.0.0.2:443", origin: "https://foreign.example", wantStatus: http.StatusForbidden},
		{name: "missing origin", remoteAddr: "10.0.0.2:443", wantStatus: http.StatusForbidden},
		{name: "multiple origins", remoteAddr: "10.0.0.2:443", origin: "same", multiple: true, wantStatus: http.StatusForbidden},
		{name: "untrusted peer forged HTTPS", remoteAddr: "192.0.2.10:443", origin: "same", wantStatus: http.StatusUpgradeRequired},
	}

	for handlerName, factory := range handlers {
		for _, test := range tests {
			t.Run(handlerName+"/"+test.name, func(t *testing.T) {
				trust := testTrust("10.0.0.0/8")
				baseHandler := factory(t, trust)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.RemoteAddr = test.remoteAddr
					baseHandler.ServeHTTP(w, r)
				}))
				defer server.Close()

				parsed, err := url.Parse(server.URL)
				if err != nil {
					t.Fatalf("parse test server URL: %v", err)
				}
				headers := http.Header{"X-Forwarded-Proto": {"https"}}
				if test.origin != "" {
					origin := test.origin
					if origin == "same" {
						origin = "https://" + parsed.Host
					}
					headers["Origin"] = []string{origin}
					if test.multiple {
						headers["Origin"] = append(headers["Origin"], origin)
					}
				}

				wsURL := "ws://" + parsed.Host + "/ws"
				conn, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
				if test.wantSuccess {
					if err != nil {
						t.Fatalf("websocket handshake failed: %v", err)
					}
					if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
						t.Fatalf("handshake status = %v, want 101", responseStatus(response))
					}
					_ = conn.Close()
					return
				}
				if conn != nil {
					_ = conn.Close()
					t.Fatal("rejected websocket handshake returned a connection")
				}
				if err == nil {
					t.Fatal("websocket handshake unexpectedly succeeded")
				}
				if response == nil || response.StatusCode != test.wantStatus {
					t.Fatalf("handshake status = %v, want %d", responseStatus(response), test.wantStatus)
				}
			})
		}
	}
}

func responseStatus(response *http.Response) string {
	if response == nil {
		return "no response"
	}
	return strings.TrimSpace(response.Status)
}
