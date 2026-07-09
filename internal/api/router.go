package api

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

//go:embed ui/dist
var uiFS embed.FS

type Router struct {
	mux  *http.ServeMux
	h    *Handler
	auth *Authenticator
	cfg  *models.Config
}

func NewRouter(handler *Handler, auth *Authenticator, cfg *models.Config) *Router {
	r := &Router{
		mux:  http.NewServeMux(),
		h:    handler,
		auth: auth,
		cfg:  cfg,
	}
	r.registerRoutes()
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	lrw := &logResponseWriter{ResponseWriter: w, status: 200}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; script-src 'self'; connect-src 'self' ws://%s:* http://%s:* http://localhost:* ws://localhost:*", r.cfg.Host, r.cfg.Host))
	if strings.HasPrefix(req.URL.Path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
	}
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	r.mux.ServeHTTP(lrw, req)
	log.Printf("[%s] %s %s %d %s", req.Method, req.URL.Path, req.RemoteAddr, lrw.status, time.Since(start))
}

type logResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *logResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *logResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := l.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying writer does not support hijack")
}

func (r *Router) registerRoutes() {
	// Health (public)
	r.mux.HandleFunc("/api/health", r.h.Health)

	// Debug (public, for browser-side diagnostics)
	r.mux.HandleFunc("/api/debug", r.auth.RequireAuth(r.h.DebugInfo))

	// Auth
	r.mux.HandleFunc("/login", r.auth.LoginHandler)
	r.mux.HandleFunc("/logout", r.auth.LogoutHandler)

	// Static files (embedded UI)
	r.mux.Handle("/static/", r.auth.RequireAuth(http.HandlerFunc(r.serveStatic)))
	r.mux.Handle("/assets/", r.auth.RequireAuth(http.HandlerFunc(r.serveStatic)))

	// API - project list
	r.mux.HandleFunc("/api/projects", r.auth.RequireAuth(r.handleProjects))

	// API - project-specific routes
	r.mux.HandleFunc("/api/projects/", r.auth.RequireAuth(r.handleProjectRoute))

	// Terminal — WebSocket SSH session
	r.mux.HandleFunc("/api/terminal", r.auth.RequireAuth(r.h.TerminalHandler))

	// Root - serve UI or login
	r.mux.HandleFunc("/", r.auth.RequireAuth(r.serveRoot))
}

func (r *Router) serveRoot(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		r.serveStatic(w, req)
		return
	}
	html, err := fs.ReadFile(uiFS, "ui/dist/index.html")
	if err != nil {
		// Fallback to simple page
		htmlStr := `<!doctype html><html><head><meta charset="utf-8"><title>DevOps Control</title></head><body><h1>DevOps Control</h1><p>UI not built yet. Run the React frontend.</p></body></html>`
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlStr))
		return
	}
	content := string(html)
	content = strings.ReplaceAll(content, "__CSRF_TOKEN__", r.auth.CSRFSecret())
	content = strings.ReplaceAll(content, "__AUTH_TOKEN__", r.auth.Token())
	// Also inject as window variable for reliable access before module load
	content = strings.ReplaceAll(content, "</head>",
		fmt.Sprintf("<script>window.__AUTH_TOKEN__=%q;</script></head>", r.auth.Token()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(content))
}

func (r *Router) serveStatic(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/")
	// Try the embedded UI first
	data, err := fs.ReadFile(uiFS, "ui/dist/"+path)
	if err != nil {
		// Fallback for non-embedded paths
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.NotFound(w, req)
		return
	}
	contentType := "application/octet-stream"
	switch {
	case strings.HasSuffix(path, ".js"):
		contentType = "application/javascript"
	case strings.HasSuffix(path, ".css"):
		contentType = "text/css"
	case strings.HasSuffix(path, ".html"):
		contentType = "text/html"
	case strings.HasSuffix(path, ".png"):
		contentType = "image/png"
	case strings.HasSuffix(path, ".svg"):
		contentType = "image/svg+xml"
	case strings.HasSuffix(path, ".json"):
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

func (r *Router) handleProjects(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.h.ListProjects(w, req)
	case http.MethodPost:
		if !r.auth.VerifyBearerToken(req) && !r.auth.VerifyCSRF(req) {
			writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		r.h.CreateProject(w, req)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

var projectPathRE = regexp.MustCompile(`^/api/projects/([^/]+)(?:/(.*))?$`)

func (r *Router) handleProjectRoute(w http.ResponseWriter, req *http.Request) {
	matches := projectPathRE.FindStringSubmatch(req.URL.Path)
	if matches == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	projectID := matches[1]
	action := matches[2]

	// Validate projectID against path traversal and invalid characters
	if !regexp.MustCompile(`^[a-z0-9_.-]+$`).MatchString(projectID) {
		writeError(w, http.StatusBadRequest, "invalid project id format")
		return
	}

	// Mutations require CSRF/bearer token
	isMutation := req.Method == http.MethodPost || req.Method == http.MethodDelete
	if isMutation {
		if !r.auth.VerifyBearerToken(req) && !r.auth.VerifyCSRF(req) {
			writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
	}

	r.routeProjectAction(w, req, projectID, action)
}

func (r *Router) routeProjectAction(w http.ResponseWriter, req *http.Request, projectID, action string) {
	if action == "" || action == "status" {
		if req.Method == http.MethodGet {
			r.h.ProjectStatus(w, req, projectID)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Validate HTTP method per action
	mutationActions := map[string]bool{
		"deploy":     true,
		"pause":      true,
		"resume":     true,
		"restore":    true,
		"rollback":   true,
		"abort":      true,
		"runner":     true,
		"delete":     true,
		"env-config": true,
	}
	readActions := map[string]bool{
		"config":          true,
		"backups":         true,
		"backups/verify":  true,
		"restore/dry-run": true,
		"logs":            true,
		"env-template":    true,
		"deployments":     true,
	}
	if strings.HasPrefix(action, "logs/") || strings.HasPrefix(action, "deployments/") {
		readActions[action] = true
	}

	if mutationActions[action] && req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	switch action {
	case "config":
		r.h.ProjectConfig(w, req, projectID)
	case "deploy":
		r.h.ProjectDeploy(w, req, projectID)
	case "pause":
		r.h.ProjectPause(w, req, projectID)
	case "resume":
		r.h.ProjectResume(w, req, projectID)
	case "backups":
		if req.Method == http.MethodGet {
			r.h.ProjectBackups(w, req, projectID)
		} else {
			r.h.ProjectCreateBackup(w, req, projectID)
		}
	case "backups/verify":
		r.h.ProjectVerifyBackup(w, req, projectID)
	case "restore/dry-run":
		r.h.ProjectRestoreDryRun(w, req, projectID)
	case "restore":
		r.h.ProjectRestore(w, req, projectID)
	case "rollback":
		r.h.ProjectRollback(w, req, projectID)
	case "abort":
		r.h.ProjectAbort(w, req, projectID)
	case "runner":
		r.h.ProjectRunnerAction(w, req, projectID)
	case "delete":
		r.h.ProjectDelete(w, req, projectID)
	case "logs":
		r.h.ProjectLogs(w, req, projectID)
	case "env-template":
		r.h.ProjectEnvTemplate(w, req, projectID)
	case "env-config":
		r.h.ProjectSaveEnvConfig(w, req, projectID)
	case "deployments":
		r.h.ProjectStatus(w, req, projectID) // fallback for deployments list
	default:
		// Check for nested routes
		if strings.HasPrefix(action, "logs/") {
			logName := strings.TrimPrefix(action, "logs/")
			r.h.ProjectLogContent(w, req, projectID, logName)
			return
		}
		if strings.HasPrefix(action, "deployments/") {
			deployID := strings.TrimPrefix(action, "deployments/")
			// Stream via websocket if upgrade requested
			if strings.Contains(req.Header.Get("Upgrade"), "websocket") {
				r.h.WebSocketHandler(w, req, projectID)
				return
			}
			r.h.ProjectDeploymentLog(w, req, projectID, deployID)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
	}
}
