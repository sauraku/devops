package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var terminalUpgrader = websocket.Upgrader{
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
		return origin == scheme+"://"+r.Host
	},
}

type TerminalSession struct {
	sshConn *ssh.Client
	sshSess *ssh.Session
	cancel  context.CancelFunc
}

var (
	activeTerminal    *TerminalSession
	terminalSessionMu sync.Mutex
	terminalWriteMu   sync.Mutex
)

const (
	terminalIdleTimeout = 60 * time.Second
	terminalReadLimit   = 4096
	terminalMaxCols     = 500
	terminalMaxRows     = 500
	terminalRateLimit   = 5 * time.Second
)

var (
	lastTerminalAttempt   = make(map[string]time.Time)
	terminalAttemptMu     sync.Mutex
	terminalLastPruneTime time.Time
)

func terminalAttemptAllowed(ip string, now time.Time) bool {
	terminalAttemptMu.Lock()
	defer terminalAttemptMu.Unlock()

	if terminalLastPruneTime.IsZero() || now.Sub(terminalLastPruneTime) >= terminalRateLimit {
		cutoff := now.Add(-terminalRateLimit)
		for attemptedIP, attemptedAt := range lastTerminalAttempt {
			if attemptedAt.Before(cutoff) {
				delete(lastTerminalAttempt, attemptedIP)
			}
		}
		terminalLastPruneTime = now
	}
	if last, exists := lastTerminalAttempt[ip]; exists && now.Sub(last) < terminalRateLimit {
		return false
	}
	lastTerminalAttempt[ip] = now
	return true
}

func writeTerminalMessage(conn *websocket.Conn, msg string) {
	terminalWriteMu.Lock()
	defer terminalWriteMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		log.Printf("Terminal: WriteMessage failed: %v", err)
	}
}

func (h *Handler) TerminalHandler(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}

	if !terminalAttemptAllowed(ip, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "too many terminal attempts; wait 5 seconds")
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalReadLimit)

	_, rawMsg, err := conn.ReadMessage()
	if err != nil {
		return
	}

	var auth struct {
		User string `json:"ssh_user"`
		Pass string `json:"ssh_pass"`
	}
	if err := json.Unmarshal(rawMsg, &auth); err != nil || auth.User == "" {
		writeTerminalMessage(conn, "ERROR: ssh_user required")
		return
	}

	passBytes := []byte(auth.Pass)
	auth.Pass = ""
	defer func() {
		for i := range passBytes {
			passBytes[i] = 0
		}
	}()

	var authMethods []ssh.AuthMethod
	home, _ := os.UserHomeDir()
	for _, keyName := range []string{"id_ed25519", "id_rsa"} {
		keyPath := filepath.Join(home, ".ssh", keyName)
		if keyData, err := os.ReadFile(keyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(keyData); err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}
	if len(passBytes) > 0 {
		authMethods = append(authMethods, ssh.Password(string(passBytes)))
	}

	hostKeyCallback, err := h.knownHostKeyCallback()
	if err != nil {
		log.Printf("Terminal: configure known_hosts: %v", err)
		writeTerminalMessage(conn, "ERROR: SSH host verification is not configured")
		return
	}
	sshConfig := &ssh.ClientConfig{
		User:            auth.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	sshHost := os.Getenv("SSH_HOST")
	if sshHost == "" {
		sshHost = "127.0.0.1"
	}
	sshConn, err := ssh.Dial("tcp", sshHost+":22", sshConfig)
	if err != nil {
		log.Printf("Terminal: SSH connection to %s failed: %v", sshHost, err)
		writeTerminalMessage(conn, "ERROR: SSH connection failed")
		return
	}
	defer sshConn.Close()

	session, err := sshConn.NewSession()
	if err != nil {
		writeTerminalMessage(conn, "ERROR: cannot create SSH session")
		return
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 80, 24, modes); err != nil {
		writeTerminalMessage(conn, "ERROR: cannot request PTY")
		return
	}

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		writeTerminalMessage(conn, "ERROR: cannot request stdin pipe")
		return
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		writeTerminalMessage(conn, "ERROR: cannot request stdout pipe")
		return
	}

	if err := session.Shell(); err != nil {
		writeTerminalMessage(conn, "ERROR: cannot start shell")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	thisSession := &TerminalSession{
		sshConn: sshConn,
		sshSess: session,
		cancel:  cancel,
	}

	terminalSessionMu.Lock()
	if activeTerminal != nil {
		activeTerminal.cancel()
		activeTerminal.sshSess.Close()
		activeTerminal.sshConn.Close()
	}
	activeTerminal = thisSession
	terminalSessionMu.Unlock()

	defer func() {
		terminalSessionMu.Lock()
		if activeTerminal == thisSession {
			activeTerminal = nil
		}
		terminalSessionMu.Unlock()
	}()

	if auth.User == "root" {
		writeTerminalMessage(conn, "\x1b[1;33mWARNING: connected as root\x1b[0m\r\n")
	}

	done := make(chan struct{})
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				terminalWriteMu.Lock()
				if werr := conn.WriteMessage(websocket.TextMessage, buf[:n]); werr != nil {
					terminalWriteMu.Unlock()
					return
				}
				terminalWriteMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			if len(msg) > 0 && msg[0] == '{' {
				var resize struct {
					Cols int `json:"cols"`
					Rows int `json:"rows"`
				}
				if json.Unmarshal(msg, &resize) == nil && resize.Cols > 0 && resize.Cols <= terminalMaxCols && resize.Rows > 0 && resize.Rows <= terminalMaxRows {
					session.WindowChange(resize.Rows, resize.Cols)
					continue
				}
			}
			stdinPipe.Write(msg)
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Since(time.Unix(0, lastActivity.Load())) > terminalIdleTimeout {
				writeTerminalMessage(conn, "\r\n\x1b[1;31mSession timed out after 60 seconds of inactivity.\x1b[0m\r\n")
				cancel()
				return
			}
		case <-ctx.Done():
			return
		case <-done:
			return
		}
	}
}

func (h *Handler) knownHostKeyCallback() (ssh.HostKeyCallback, error) {
	knownHostsPath := h.cfg.SSHKnownHosts
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	return knownhosts.New(knownHostsPath)
}
