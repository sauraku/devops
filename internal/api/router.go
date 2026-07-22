package api

import (
	"bufio"
	"context"
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

type authContextKey string

const projectTokenAuthKey authContextKey = "project-token-auth"

func withProjectTokenAuth(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), projectTokenAuthKey, true))
}

func isProjectTokenAuth(req *http.Request) bool {
	value, _ := req.Context().Value(projectTokenAuthKey).(bool)
	return value
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
	r.mux.HandleFunc("/api/projects/", r.handleProjectRoute)

	// Terminal — WebSocket SSH session
	if r.cfg.EnableTerminal {
		r.mux.HandleFunc("/api/terminal", r.auth.RequireAuth(r.h.TerminalHandler))
	}

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
	content = strings.ReplaceAll(content, "__CSRF_TOKEN__", r.auth.CSRFToken(req))
	content = strings.ReplaceAll(content, "__AUTH_TOKEN__", "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(content))
}

func (r *Router) serveStatic(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/")
	// Try the embedded UI first
	data, err := fs.ReadFile(uiFS, "ui/dist/"+path)
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

	allowed, mutation := projectActionPolicy(action, req.Method)
	if !allowed {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	masterAuth := r.auth.IsAuthed(req)
	projectAuth := projectTokenActionAllowed(action) && r.auth.VerifyProjectToken(req, projectID)
	if !masterAuth && !projectAuth {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if mutation && !r.auth.VerifyBearerToken(req) && !projectAuth && !r.auth.VerifyCSRF(req) {
		writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
		return
	}
	if projectAuth && !masterAuth {
		req = withProjectTokenAuth(req)
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
	case "container-log-files":
		r.h.ProjectContainerLogFiles(w, req, projectID)
	case "env-template":
		r.h.ProjectEnvTemplate(w, req, projectID)
	case "env-config":
		r.h.ProjectSaveEnvConfig(w, req, projectID)
	case "deployments":
		writeError(w, http.StatusNotImplemented, "deployments list not yet implemented")
	default:
		// Check for nested routes
		if strings.HasPrefix(action, "containers/") {
			sub := strings.TrimPrefix(action, "containers/")
			if strings.HasSuffix(sub, "/logs") {
				service := strings.TrimSuffix(sub, "/logs")
				r.h.ProjectContainerLogs(w, req, projectID, service)
				return
			}
			parts := strings.Split(sub, "/")
			if len(parts) == 2 {
				r.h.ProjectContainerAction(w, req, projectID, parts[0], parts[1])
				return
			}
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if strings.HasPrefix(action, "container-log-files/") {
			service := strings.TrimPrefix(action, "container-log-files/")
			r.h.ProjectContainerLogFileContent(w, req, projectID, service)
			return
		}
		if strings.HasPrefix(action, "logs/") {
			logName := strings.TrimPrefix(action, "logs/")
			r.h.ProjectLogContent(w, req, projectID, logName)
			return
		}
		if deployID, subAction, ok := deploymentSubAction(action); ok {
			switch subAction {
			case "status":
				r.h.ProjectDeploymentStatus(w, req, projectID, deployID)
			case "approve":
				r.h.ProjectApproveDeployment(w, req, projectID, deployID)
			case "stream":
				r.h.WebSocketHandler(w, req, projectID, deployID)
			case "":
				// Stream via websocket if upgrade requested
				if strings.Contains(strings.ToLower(req.Header.Get("Upgrade")), "websocket") {
					r.h.WebSocketHandler(w, req, projectID, deployID)
					return
				}
				r.h.ProjectDeploymentLog(w, req, projectID, deployID)
			}
			return
		}
		writeError(w, http.StatusNotFound, "not found")
	}
}

func projectActionPolicy(action, requestMethod string) (allowed bool, mutation bool) {
	switch action {
	case "", "status", "logs", "container-log-files", "env-template", "deployments":
		return requestMethod == http.MethodGet, false
	case "config", "deploy", "pause", "resume", "backups/verify", "restore/dry-run", "restore", "rollback", "abort", "runner", "delete", "env-config":
		return requestMethod == http.MethodPost, true
	case "backups":
		return requestMethod == http.MethodGet || requestMethod == http.MethodPost, requestMethod == http.MethodPost
	}
	if _, subAction, ok := deploymentSubAction(action); ok {
		switch subAction {
		case "", "status", "stream":
			return requestMethod == http.MethodGet, false
		case "approve":
			return requestMethod == http.MethodPost, true
		default:
			return false, false
		}
	}
	if strings.HasPrefix(action, "containers/") {
		if strings.HasSuffix(action, "/logs") {
			return requestMethod == http.MethodGet, false
		}
		return requestMethod == http.MethodPost, true
	}
	if strings.HasPrefix(action, "logs/") || strings.HasPrefix(action, "container-log-files/") {
		return requestMethod == http.MethodGet, false
	}
	return true, false
}

var deploymentIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func deploymentSubAction(action string) (deployID, subAction string, ok bool) {
	parts := strings.Split(action, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "deployments" || !deploymentIDRE.MatchString(parts[1]) {
		return "", "", false
	}
	if len(parts) == 3 {
		subAction = parts[2]
	}
	return parts[1], subAction, true
}

func projectTokenActionAllowed(action string) bool {
	if action == "deploy" {
		return true
	}
	_, subAction, ok := deploymentSubAction(action)
	return ok && subAction == "status"
}
