package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type LogStreamer struct {
	mu      sync.RWMutex
	clients map[string]map[*wsClient]bool
}

type wsClient struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func NewLogStreamer() *LogStreamer {
	return &LogStreamer{
		clients: make(map[string]map[*wsClient]bool),
	}
}

func (ls *LogStreamer) handleWebSocket(w http.ResponseWriter, r *http.Request, logPath string, trust *RequestTrust) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     trust.sameOrigin,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}
	defer client.close()

	ls.mu.Lock()
	if ls.clients[logPath] == nil {
		ls.clients[logPath] = make(map[*wsClient]bool)
	}
	ls.clients[logPath][client] = true
	ls.mu.Unlock()

	defer func() {
		ls.removeClient(logPath, client)
	}()

	// Start streaming the log file
	ls.streamLogFile(client, logPath)
}

func (ls *LogStreamer) streamLogFile(client *wsClient, logPath string) {
	conn := client.conn
	lastSize := int64(0)
	if info, err := os.Stat(logPath); err == nil {
		lastSize = info.Size()
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			info, err := os.Stat(logPath)
			if err != nil {
				if err := writeWSMessage(client, map[string]string{"type": "waiting", "message": "Waiting for log file..."}); err != nil {
					return
				}
				continue
			}

			if info.Size() < lastSize {
				lastSize = 0
			}

			if info.Size() == lastSize {
				continue
			}

			f, err := os.Open(logPath)
			if err != nil {
				continue
			}

			if lastSize > 0 {
				f.Seek(lastSize, io.SeekStart)
			}

			data := make([]byte, info.Size()-lastSize)
			n, _ := f.Read(data)
			f.Close()

			if n > 0 {
				lastSize = info.Size()
				if err := writeWSMessage(client, map[string]any{
					"type": "log",
					"data": string(data[:n]),
				}); err != nil {
					return
				}
			}
		}
	}
}

func (ls *LogStreamer) StreamDeploymentLog(projectID, deployID, logDir string) {
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	ls.mu.RLock()
	clients := make([]*wsClient, 0, len(ls.clients[logPath]))
	for client := range ls.clients[logPath] {
		clients = append(clients, client)
	}
	ls.mu.RUnlock()

	if len(clients) == 0 {
		return
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		return
	}

	msg := map[string]any{
		"type": "log",
		"data": string(data),
	}

	for _, client := range clients {
		if err := writeWSMessage(client, msg); err != nil {
			ls.removeClient(logPath, client)
			client.close()
		}
	}
}

func (ls *LogStreamer) removeClient(logPath string, client *wsClient) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.clients[logPath], client)
	if len(ls.clients[logPath]) == 0 {
		delete(ls.clients, logPath)
	}
}

func (client *wsClient) close() {
	client.closeOnce.Do(func() {
		_ = client.conn.Close()
	})
}

func writeWSMessage(client *wsClient, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode websocket message: %w", err)
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		client.close()
		return fmt.Errorf("set websocket write deadline: %w", err)
	}
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		client.close()
		return fmt.Errorf("write websocket message: %w", err)
	}
	return nil
}

func (h *Handler) WebSocketHandler(w http.ResponseWriter, r *http.Request, projectID, deployID string) {
	if !h.deploymentWebSocketTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, "HTTPS is required for deployment logs")
		return
	}
	if deployID == "" {
		writeError(w, http.StatusBadRequest, "deploy ID is required")
		return
	}

	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	logDir := h.projects.LogDir(p)
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	streamer := NewLogStreamer()
	streamer.handleWebSocket(w, r, logPath, h.requestTrust)
}

func (h *Handler) deploymentWebSocketTransportAllowed(r *http.Request) bool {
	return h.requestTrust.allowsSensitiveWebSocket(r)
}
