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

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		expected := fmt.Sprintf("%s://%s", scheme, r.Host)
		return origin == expected
	},
}

type LogStreamer struct {
	mu      sync.RWMutex
	clients map[string]map[*websocket.Conn]bool
}

func NewLogStreamer() *LogStreamer {
	return &LogStreamer{
		clients: make(map[string]map[*websocket.Conn]bool),
	}
}

func (ls *LogStreamer) HandleWebSocket(w http.ResponseWriter, r *http.Request, logPath string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ls.mu.Lock()
	if ls.clients[logPath] == nil {
		ls.clients[logPath] = make(map[*websocket.Conn]bool)
	}
	ls.clients[logPath][conn] = true
	ls.mu.Unlock()

	defer func() {
		ls.mu.Lock()
		delete(ls.clients[logPath], conn)
		ls.mu.Unlock()
	}()

	// Start streaming the log file
	ls.streamLogFile(conn, logPath)
}

func (ls *LogStreamer) streamLogFile(conn *websocket.Conn, logPath string) {
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
				writeWSMessage(conn, map[string]string{"type": "waiting", "message": "Waiting for log file..."})
				continue
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
				writeWSMessage(conn, map[string]any{
					"type": "log",
					"data": string(data[:n]),
				})
			}
		}
	}
}

func (ls *LogStreamer) StreamDeploymentLog(projectID, deployID, logDir string) {
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	ls.mu.RLock()
	clients := ls.clients[logPath]
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

	ls.mu.RLock()
	defer ls.mu.RUnlock()
	for conn := range ls.clients[logPath] {
		writeWSMessage(conn, msg)
	}
}

func writeWSMessage(conn *websocket.Conn, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	wsMu.RLock()
	conn.WriteMessage(websocket.TextMessage, data)
	wsMu.RUnlock()
}

var wsMu sync.RWMutex

func (h *Handler) WebSocketHandler(w http.ResponseWriter, r *http.Request, projectID string) {
	logName := queryParam(r, "name")
	if logName == "" {
		writeError(w, http.StatusBadRequest, "log name is required")
		return
	}

	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	logDir := h.projects.LogDir(p)
	logPath := filepath.Join(logDir, filepath.Base(logName))

	streamer := NewLogStreamer()
	streamer.HandleWebSocket(w, r, logPath)
}
