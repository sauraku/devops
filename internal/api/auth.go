package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sauraku/devops-control/internal/services"
)

const sessionLifetime = 12 * time.Hour

const (
	loginInitialBackoff   = time.Second
	loginMaxBackoff       = time.Minute
	loginAttemptRetention = 10 * time.Minute
)

type loginAttempt struct {
	failures    int
	retryAfter  time.Time
	lastAttempt time.Time
}

type Authenticator struct {
	token         string
	cookieSecret  []byte
	cookieSecure  bool
	audit         *services.AuditService
	loginMu       sync.Mutex
	loginAttempts map[string]loginAttempt
}

func NewAuthenticator(token, cookieSecret string, cookieSecure bool, audit *services.AuditService) *Authenticator {
	return &Authenticator{
		token:         token,
		cookieSecret:  []byte(cookieSecret),
		cookieSecure:  cookieSecure,
		audit:         audit,
		loginAttempts: make(map[string]loginAttempt),
	}
}

func (a *Authenticator) VerifyBearerToken(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && secureEqual(auth[7:], a.token) {
		return true
	}
	headerToken := r.Header.Get("X-Deploy-Control-Token")
	return headerToken != "" && secureEqual(headerToken, a.token)
}

func (a *Authenticator) ProjectToken(projectID string) string {
	mac := hmac.New(sha256.New, []byte(a.token))
	_, _ = mac.Write([]byte("runner:" + projectID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) VerifyProjectToken(r *http.Request, projectID string) bool {
	token := strings.TrimSpace(r.Header.Get("X-Deploy-Control-Token"))
	return token != "" && secureEqual(token, a.ProjectToken(projectID))
}

func (a *Authenticator) VerifyCookie(r *http.Request) bool {
	_, ok := a.sessionID(r)
	return ok
}

func (a *Authenticator) CSRFToken(r *http.Request) string {
	sessionID, ok := a.sessionID(r)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, a.cookieSecret)
	_, _ = mac.Write([]byte("csrf:" + sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) VerifyCSRF(r *http.Request) bool {
	want := a.CSRFToken(r)
	got := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	return want != "" && got != "" && secureEqual(got, want)
}

func (a *Authenticator) IsAuthed(r *http.Request) bool {
	return a.VerifyBearerToken(r) || a.VerifyCookie(r)
}

func (a *Authenticator) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.IsAuthed(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			} else {
				sendLoginPage(w)
			}
			return
		}
		next(w, r)
	}
}

func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if r.Method == http.MethodGet {
			sendLoginPage(w)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := loginRemoteIP(r.RemoteAddr)
	if retryAfter, blocked := a.loginBlocked(ip, time.Now()); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		var body map[string]string
		if err := readJSON(r, &body); err == nil {
			token = strings.TrimSpace(body["token"])
		}
	}

	if !secureEqual(token, a.token) {
		retryAfter := a.recordLoginFailure(ip, time.Now())
		if a.audit != nil {
			a.audit.Log("login_failed", "denied", "", "remote_ip="+ip, "")
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		sendLoginPage(w)
		return
	}
	a.clearLoginFailures(ip)

	sessionIDBytes := make([]byte, 32)
	if _, err := rand.Read(sessionIDBytes); err != nil {
		internalError(w, fmt.Errorf("generate session: %w", err))
		return
	}
	sessionID := hex.EncodeToString(sessionIDBytes)
	expires := time.Now().Add(sessionLifetime)
	payload := sessionID + "." + strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, a.cookieSecret)
	_, _ = mac.Write([]byte(payload))
	value := payload + "." + hex.EncodeToString(mac.Sum(nil))

	http.SetCookie(w, &http.Cookie{
		Name:     "deploy_control",
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func loginRemoteIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || ip == "" {
		return remoteAddr
	}
	return ip
}

func (a *Authenticator) loginBlocked(ip string, now time.Time) (time.Duration, bool) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	a.pruneLoginAttemptsLocked(now)
	attempt, ok := a.loginAttempts[ip]
	if !ok || !now.Before(attempt.retryAfter) {
		return 0, false
	}
	return time.Until(attempt.retryAfter), true
}

func (a *Authenticator) recordLoginFailure(ip string, now time.Time) time.Duration {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	a.pruneLoginAttemptsLocked(now)
	attempt := a.loginAttempts[ip]
	attempt.failures++
	if attempt.failures > 7 {
		attempt.failures = 7
	}
	delay := loginInitialBackoff << (attempt.failures - 1)
	if delay > loginMaxBackoff {
		delay = loginMaxBackoff
	}
	attempt.retryAfter = now.Add(delay)
	attempt.lastAttempt = now
	a.loginAttempts[ip] = attempt
	return delay
}

func (a *Authenticator) clearLoginFailures(ip string) {
	a.loginMu.Lock()
	delete(a.loginAttempts, ip)
	a.loginMu.Unlock()
}

func (a *Authenticator) pruneLoginAttemptsLocked(now time.Time) {
	cutoff := now.Add(-loginAttemptRetention)
	for ip, attempt := range a.loginAttempts {
		if attempt.lastAttempt.Before(cutoff) {
			delete(a.loginAttempts, ip)
		}
	}
}

func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "deploy_control",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Authenticator) sessionID(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("deploy_control")
	if err != nil {
		return "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 || len(parts[0]) != 64 {
		return "", false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, a.cookieSecret)
	_, _ = mac.Write([]byte(payload))
	if !secureEqual(parts[2], hex.EncodeToString(mac.Sum(nil))) {
		return "", false
	}
	return parts[0], true
}

func secureEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func sendLoginPage(w http.ResponseWriter) {
	html := `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Deploy Control Login</title>
<style>
body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#08090b;color:#f4f7fb;}
.login-panel{background:#11161d;padding:2rem;border-radius:8px;box-shadow:0 20px 60px rgba(0,0,0,.36);text-align:center;}
input{display:block;width:100%;box-sizing:border-box;margin:1rem 0;padding:.5rem;background:#1b222d;border:1px solid rgba(212,221,236,.12);color:#f4f7fb;border-radius:4px;}
button{background:#2dd4bf;color:#08090b;border:none;padding:.5rem 2rem;border-radius:4px;cursor:pointer;font-weight:600;}
</style></head>
<body><main class="login-panel"><h1>Deploy Control</h1>
<p>Enter the deployment control token to continue.</p>
<form method="post" action="/login">
<input name="token" type="password" autocomplete="current-password" autofocus>
<button type="submit">Sign in</button>
</form></main></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
