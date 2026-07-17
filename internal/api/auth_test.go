package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSignedSessionAndCSRF(t *testing.T) {
	auth := NewAuthenticator(strings.Repeat("m", 64), strings.Repeat("c", 64), false)
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
	auth := NewAuthenticator(strings.Repeat("m", 64), strings.Repeat("c", 64), false)
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
