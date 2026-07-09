package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

type Authenticator struct {
	token           string
	tokenDigest     string
	jwtSecret       []byte
	cookieSecret    []byte
	csrfSecret      string
	cookieSecure    bool
}

func NewAuthenticator(token, jwtSecret, cookieSecret string, cookieSecure bool) *Authenticator {
	digest := sha256.Sum256([]byte(token))
	csrfHmac := hmac.New(sha256.New, []byte(token))
	csrfHmac.Write([]byte("deploy-control-csrf"))
	csrfSecret := hex.EncodeToString(csrfHmac.Sum(nil))

	return &Authenticator{
		token:        token,
		tokenDigest:  hex.EncodeToString(digest[:]),
		jwtSecret:    []byte(jwtSecret),
		cookieSecret: []byte(cookieSecret),
		csrfSecret:   csrfSecret,
		cookieSecure: cookieSecure,
	}
}

func (a *Authenticator) VerifyBearerToken(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && hmac.Equal([]byte(auth[7:]), []byte(a.token)) {
		return true
	}
	headerToken := r.Header.Get("X-Deploy-Control-Token")
	if headerToken != "" && hmac.Equal([]byte(headerToken), []byte(a.token)) {
		return true
	}
	return false
}

func (a *Authenticator) VerifyCookie(r *http.Request) bool {
	cookie, err := r.Cookie("deploy_control")
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(a.tokenDigest))
}

func (a *Authenticator) VerifyCSRF(r *http.Request) bool {
	return hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(a.csrfSecret))
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
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		var body map[string]string
		if err := readJSON(r, &body); err == nil {
			token = strings.TrimSpace(body["token"])
		}
	}

	if !hmac.Equal([]byte(token), []byte(a.token)) {
		sendLoginPage(w)
		return
	}

	// Clear any old Secure cookie first, then set compatible one
	http.SetCookie(w, &http.Cookie{
		Name:   "deploy_control",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "deploy_control",
		Value:    a.tokenDigest,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
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
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Authenticator) CSRFSecret() string {
	return a.csrfSecret
}

func (a *Authenticator) Token() string {
	return a.token
}

func sendLoginPage(w http.ResponseWriter) {
	html := `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Deploy Control Login</title>
<style>
body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#08090b;color:#f4f7fb;}
.login-panel{background:#11161d;padding:2rem;border-radius:8px;box-shadow:0 20px 60px rgba(0,0,0,.36);text-align:center;}
input{display:block;width:100%;margin:1rem 0;padding:.5rem;background:#1b222d;border:1px solid rgba(212,221,236,.12);color:#f4f7fb;border-radius:4px;}
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
	w.Write([]byte(html))
}
