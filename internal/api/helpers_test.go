package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceErrorDoesNotExposeInternalDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	serviceError(
		recorder,
		http.StatusBadRequest,
		"deployment could not be started",
		errors.New(`compose failed for /opt/devops-control/Projects/medusa: token=secret-value`),
	)
	body := recorder.Body.String()
	if !strings.Contains(body, "deployment could not be started") {
		t.Fatalf("public error missing: %s", body)
	}
	for _, secret := range []string{"/opt/devops-control", "secret-value", "compose failed"} {
		if strings.Contains(body, secret) {
			t.Fatalf("internal detail %q leaked: %s", secret, body)
		}
	}
}
