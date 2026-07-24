package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
	"github.com/sauraku/devops-control/internal/services"
)

func TestProjectSaveEnvConfigSupportsPatchAndExplicitClear(t *testing.T) {
	db.InitCrypto(strings.Repeat("a", 64))
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, ".env.template"), []byte(
		"SMTP_HOST=\nSMTP_PORT=\nSMTP_USER=\nSMTP_PASS=\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "main", AppDir: appDir}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	projectService := services.NewProjectService(
		database,
		docker.NewClient(),
		services.NewAuditService(database),
		&models.Config{},
	)
	handler := &Handler{projects: projectService}

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/projects/medusa/env-config",
			strings.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ProjectSaveEnvConfig(recorder, req, project.ID)
		if recorder.Code != http.StatusOK {
			t.Fatalf("save env status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	post(`{"overrides":{"SMTP_HOST":"smtp.example.test","SMTP_PORT":"587","SMTP_USER":"mailer","SMTP_PASS":"smtp-secret"}}`)
	// Legacy clients send only the flat overrides object. It is now a patch, so
	// omitted SMTP fields must survive.
	post(`{"overrides":{"SMTP_HOST":"smtp2.example.test"}}`)
	post(`{"overrides":{},"clear_keys":["SMTP_USER"]}`)

	got, err := database.GetProjectEnvOverrides(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got["SMTP_USER"]; exists {
		t.Fatalf("explicitly cleared SMTP_USER remains: %#v", got)
	}
	if len(got) != 3 || got["SMTP_HOST"] != "smtp2.example.test" ||
		got["SMTP_PORT"] != "587" || got["SMTP_PASS"] != "smtp-secret" {
		t.Fatalf("API patch lost or changed an omitted SMTP value: %#v", got)
	}
}
