package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sessionLifetime = 12 * time.Hour

type Authenticator struct {
	token        string
	cookieSecret []byte
	cookieSecure bool
}

func NewAuthenticator(token, cookieSecret string, cookieSecure bool) *Authenticator {
	return &Authenticator{
		token:        token,
		cookieSecret: []byte(cookieSecret),
		cookieSecure: cookieSecure,
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

	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		var body map[string]string
		if err := readJSON(r, &body); err == nil {
			token = strings.TrimSpace(body["token"])
		}
	}

	if !secureEqual(token, a.token) {
		sendLoginPage(w)
		return
	}

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
