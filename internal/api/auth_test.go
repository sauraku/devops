package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

type failOnRead struct{}

func (failOnRead) Read([]byte) (int, error) {
	panic("request body was read")
}

func (failOnRead) Close() error { return nil }

func TestSignedSessionAndCSRF(t *testing.T) {
	auth := NewAuthenticator(strings.Repeat("m", 64), strings.Repeat("c", 64), false, nil)
	form := url.Values{"token": {strings.Repeat("m", 64)}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	auth.LoginHandler(recorder, req)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags are not hardened: %#v", cookie)
	}
	if strings.Contains(cookie.Value, strings.Repeat("m", 16)) {
		t.Fatal("session cookie contains the master token")
	}

	authed := httptest.NewRequest(http.MethodGet, "/", nil)
	authed.AddCookie(cookie)
	if !auth.VerifyCookie(authed) {
		t.Fatal("valid signed session was rejected")
	}
	csrf := auth.CSRFToken(authed)
	authed.Header.Set("X-CSRF-Token", csrf)
	if csrf == "" || !auth.VerifyCSRF(authed) {
		t.Fatal("valid session CSRF token was rejected")
	}

	tampered := *cookie
	tampered.Value += "0"
	bad := httptest.NewRequest(http.MethodGet, "/", nil)
	bad.AddCookie(&tampered)
	if auth.VerifyCookie(bad) {
		t.Fatal("tampered session cookie was accepted")
	}
}

func TestProjectTokenIsScoped(t *testing.T) {
	auth := NewAuthenticator(strings.Repeat("m", 64), strings.Repeat("c", 64), false, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/medusa/deploy", nil)
	req.Header.Set("X-Deploy-Control-Token", auth.ProjectToken("medusa"))
	if !auth.VerifyProjectToken(req, "medusa") {
		t.Fatal("matching project token was rejected")
	}
	if auth.VerifyProjectToken(req, "another-project") {
		t.Fatal("project token authorized a different project")
	}
	if auth.VerifyBearerToken(req) {
		t.Fatal("project token was accepted as the master token")
	}
}

func TestMasterAndProjectTokenHeadersAreDisjoint(t *testing.T) {
	const master = "master-token-with-at-least-thirty-two-characters"
	auth := NewAuthenticator(master, "cookie-secret", false, nil)

	masterRequest := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	masterRequest.Header.Set(masterTokenHeader, master)
	if !auth.VerifyBearerToken(masterRequest) {
		t.Fatal("master token was rejected in the master-only header")
	}
	if auth.VerifyProjectToken(masterRequest, "medusa") {
		t.Fatal("master-only header authenticated as a project runner")
	}

	legacyMaster := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	legacyMaster.Header.Set(projectTokenHeader, master)
	if auth.VerifyBearerToken(legacyMaster) {
		t.Fatal("project-only header authenticated as the master token")
	}

	projectRequest := httptest.NewRequest(http.MethodPost, "/api/projects/medusa/deploy", nil)
	projectRequest.Header.Set(projectTokenHeader, auth.ProjectToken("medusa"))
	if !auth.VerifyProjectToken(projectRequest, "medusa") {
		t.Fatal("project token was rejected in the project-only header")
	}
	if auth.VerifyBearerToken(projectRequest) {
		t.Fatal("project-only header authenticated as the master token")
	}
}

func TestProjectTokenAuthContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/medusa/deploy", nil)
	if isProjectTokenAuth(req) {
		t.Fatal("ordinary request was marked as project-token authenticated")
	}
	if !isProjectTokenAuth(withProjectTokenAuth(req)) {
		t.Fatal("project-token authentication was not preserved in request context")
	}
}

func TestProjectActionPolicy(t *testing.T) {
	tests := []struct {
		action, method  string
		allowed, mutate bool
	}{
		{"status", http.MethodGet, true, false},
		{"status", http.MethodPost, false, false},
		{"config", http.MethodPost, true, true},
		{"config", http.MethodGet, false, true},
		{"containers/backend/restart", http.MethodPost, true, true},
		{"containers/backend/restart", http.MethodGet, false, true},
		{"containers/backend/logs", http.MethodGet, true, false},
		{"backups", http.MethodGet, true, false},
		{"backups", http.MethodPost, true, true},
		{"deployments/deploy-medusa-123/status", http.MethodGet, true, false},
		{"deployments/deploy-medusa-123/status", http.MethodPost, false, false},
		{"deployments/deploy-medusa-123/approve", http.MethodPost, true, true},
		{"deployments/deploy-medusa-123/approve", http.MethodGet, false, true},
		{"deployments/deploy-medusa-123/unknown", http.MethodGet, false, false},
	}
	for _, test := range tests {
		allowed, mutate := projectActionPolicy(test.action, test.method)
		if allowed != test.allowed || mutate != test.mutate {
			t.Errorf("policy(%q, %q) = (%v, %v), want (%v, %v)", test.action, test.method, allowed, mutate, test.allowed, test.mutate)
		}
	}
}

func TestProjectTokenActionsAreReadOnlyAndProjectScoped(t *testing.T) {
	if !projectTokenActionAllowed("deploy") {
		t.Fatal("project token cannot trigger its project deploy route")
	}
	if !projectTokenActionAllowed("deployments/deploy-medusa-123/status") {
		t.Fatal("project token cannot read its deployment status route")
	}
	for _, action := range []string{
		"status",
		"deployments/deploy-medusa-123",
		"deployments/deploy-medusa-123/approve",
	} {
		if projectTokenActionAllowed(action) {
			t.Fatalf("project token unexpectedly allowed action %q", action)
		}
	}
}

func TestLoginBackoffIsScopedToRemoteIP(t *testing.T) {
	auth := NewAuthenticator("correct-token", strings.Repeat("c", 64), false, nil)
	failed := func(remoteAddr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {"wrong-token"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = remoteAddr
		recorder := httptest.NewRecorder()
		auth.LoginHandler(recorder, req)
		return recorder
	}

	if recorder := failed("192.0.2.1:1234"); recorder.Code != http.StatusOK {
		t.Fatalf("first invalid login status = %d, want 200", recorder.Code)
	}
	if recorder := failed("192.0.2.1:1235"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited login status = %d, want 429", recorder.Code)
	}
	if recorder := failed("192.0.2.2:1234"); recorder.Code != http.StatusOK {
		t.Fatalf("different remote IP status = %d, want 200", recorder.Code)
	}
}

func TestLoginBackoffUsesTrustedForwardedClientIP(t *testing.T) {
	auth := NewAuthenticatorWithTrustedProxies(
		"correct-token",
		strings.Repeat("c", 64),
		false,
		nil,
		[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	)
	failed := func(clientIP string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {"wrong-token"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", clientIP)
		req.RemoteAddr = "10.0.0.2:443"
		recorder := httptest.NewRecorder()
		auth.LoginHandler(recorder, req)
		return recorder
	}

	if recorder := failed("198.51.100.10, 192.0.2.1"); recorder.Code != http.StatusOK {
		t.Fatalf("first client status = %d, want 200", recorder.Code)
	}
	if recorder := failed("198.51.100.11, 192.0.2.1"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("same first-untrusted-hop status = %d, want 429", recorder.Code)
	}
	if recorder := failed("198.51.100.10, 192.0.2.2"); recorder.Code != http.StatusOK {
		t.Fatalf("different forwarded client status = %d, want 200", recorder.Code)
	}
}

func TestSecureLoginRequiresEffectiveHTTPSBeforeReadingToken(t *testing.T) {
	auth := NewAuthenticatorWithTrustedProxies(
		"correct-token",
		strings.Repeat("c", 64),
		true,
		nil,
		[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	)

	plaintext := httptest.NewRequest(http.MethodPost, "/login", io.NopCloser(failOnRead{}))
	plaintext.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	plaintext.RemoteAddr = "10.0.0.2:443"
	plaintext.Header.Set("X-Forwarded-Proto", "http")
	plaintextRecorder := httptest.NewRecorder()
	auth.LoginHandler(plaintextRecorder, plaintext)
	if plaintextRecorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext login status = %d, want 426", plaintextRecorder.Code)
	}

	secure := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=correct-token"))
	secure.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	secure.RemoteAddr = "10.0.0.2:443"
	secure.Header.Set("X-Forwarded-Proto", "https")
	secureRecorder := httptest.NewRecorder()
	auth.LoginHandler(secureRecorder, secure)
	if secureRecorder.Code != http.StatusSeeOther {
		t.Fatalf("trusted HTTPS login status = %d, want 303", secureRecorder.Code)
	}

	forged := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=correct-token"))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forged.RemoteAddr = "192.0.2.10:443"
	forged.Header.Set("X-Forwarded-Proto", "https")
	forgedRecorder := httptest.NewRecorder()
	auth.LoginHandler(forgedRecorder, forged)
	if forgedRecorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("untrusted forged HTTPS login status = %d, want 426", forgedRecorder.Code)
	}
}

func TestSecureBrowserRoutesNeverRenderLoginFormOverPlaintext(t *testing.T) {
	auth := NewAuthenticatorWithTrustedProxies(
		"correct-token",
		strings.Repeat("c", 64),
		true,
		nil,
		[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	)

	tests := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{name: "login", path: "/login", handler: auth.LoginHandler},
		{name: "unauthenticated root", path: "/", handler: auth.RequireAuth(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unauthenticated request reached protected handler")
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plaintext := httptest.NewRequest(http.MethodGet, test.path, nil)
			plaintext.RemoteAddr = "10.0.0.2:443"
			plaintext.Header.Set("X-Forwarded-Proto", "http")
			plaintextRecorder := httptest.NewRecorder()
			test.handler(plaintextRecorder, plaintext)
			if plaintextRecorder.Code != http.StatusUpgradeRequired {
				t.Fatalf("plaintext status = %d, want 426", plaintextRecorder.Code)
			}
			if strings.Contains(plaintextRecorder.Body.String(), "name=\"token\"") {
				t.Fatal("plaintext response rendered the token form")
			}

			secure := httptest.NewRequest(http.MethodGet, test.path, nil)
			secure.RemoteAddr = "10.0.0.2:443"
			secure.Header.Set("X-Forwarded-Proto", "https")
			secureRecorder := httptest.NewRecorder()
			test.handler(secureRecorder, secure)
			if secureRecorder.Code != http.StatusOK {
				t.Fatalf("trusted HTTPS status = %d, want 200", secureRecorder.Code)
			}
			if !strings.Contains(secureRecorder.Body.String(), "name=\"token\"") {
				t.Fatal("trusted HTTPS response did not render the token form")
			}
		})
	}
}
