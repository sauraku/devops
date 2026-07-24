package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDeploymentWebSocketRejectsPlaintextBeforeLookupOrUpgrade(t *testing.T) {
	h := &Handler{requestTrust: testTrust("10.0.0.0/8")}
	req := httptest.NewRequest(http.MethodGet, "http://control.example/api/projects/example/ws/deploy-1", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Origin", "http://control.example")
	recorder := httptest.NewRecorder()

	h.WebSocketHandler(recorder, req, "example", "deploy-1")
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext deployment websocket status = %d, want 426", recorder.Code)
	}
}

func TestDeploymentWebSocketTransportAllowsHTTPSAndDirectLoopback(t *testing.T) {
	trusted := testTrust("10.0.0.0/8")
	h := &Handler{requestTrust: trusted}
	secure := httptest.NewRequest(http.MethodGet, "http://control.example/ws", nil)
	secure.RemoteAddr = "10.0.0.2:443"
	secure.Header.Set("X-Forwarded-Proto", "https")
	if !h.deploymentWebSocketTransportAllowed(secure) {
		t.Fatal("trusted forwarded HTTPS was rejected")
	}

	direct := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/ws", nil)
	direct.RemoteAddr = "127.0.0.1:12345"
	direct.Host = "127.0.0.1:8787"
	localHandler := &Handler{requestTrust: NewRequestTrust(nil)}
	if !localHandler.deploymentWebSocketTransportAllowed(direct) {
		t.Fatal("direct loopback development websocket was rejected")
	}
}

func TestLogStreamerRemovesClientAfterFailedWrite(t *testing.T) {
	streamer := NewLogStreamer()
	logPath := filepath.Join(t.TempDir(), "deploy.log")
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trust := NewRequestTrust(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamer.handleWebSocket(w, r, logPath, trust)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Origin": []string{"http://" + parsed.Host}}
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+parsed.Host, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var serverClient *wsClient
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		streamer.mu.RLock()
		for client := range streamer.clients[logPath] {
			serverClient = client
		}
		streamer.mu.RUnlock()
		if serverClient != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if serverClient == nil {
		t.Fatal("websocket client was not registered")
	}

	if err := serverClient.conn.UnderlyingConn().Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeWSMessage(serverClient, map[string]string{"type": "log"}); err == nil ||
		!strings.Contains(err.Error(), "websocket") {
		t.Fatalf("closed websocket write error = %v", err)
	}
	streamer.StreamDeploymentLog("medusa", "deploy", filepath.Dir(logPath))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		streamer.mu.RLock()
		remaining := len(streamer.clients[logPath])
		streamer.mu.RUnlock()
		if remaining == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failed websocket client remained registered")
}
