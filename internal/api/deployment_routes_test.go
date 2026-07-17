package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/models"
	"github.com/sauraku/devops-control/internal/services"
)

func TestProjectTokenCanOnlyReadItsDeploymentStatus(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "dev"}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	deployment := &models.Deployment{
		ID:        "deploy-medusa-123",
		ProjectID: project.ID,
		Kind:      models.DeploymentKindDeploy,
		Status:    models.DeploymentStatusPendingApproval,
		StartedAt: time.Now().UTC(),
		LogPath:   "/internal/path/that-must-not-be-returned.log",
	}
	if err := database.CreateDeployment(deployment); err != nil {
		t.Fatal(err)
	}
	cfg := &models.Config{Host: "127.0.0.1", BaseDir: root, DataDir: root, ProjectRoot: root}
	audit := services.NewAuditService(database)
	deploys := services.NewDeployService(database, nil, audit, cfg)
	auth := NewAuthenticator(strings.Repeat("m", 64), strings.Repeat("c", 64), false)
	router := NewRouter(&Handler{deploys: deploys}, auth, cfg)
	statusPath := "/api/projects/medusa/deployments/deploy-medusa-123/status"

	req := httptest.NewRequest(http.MethodGet, statusPath, nil)
	req.Header.Set("X-Deploy-Control-Token", auth.ProjectToken("medusa"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Operation models.DeploymentOperation `json:"operation"`
		StatusURL string                     `json:"status_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Operation.ID != deployment.ID || response.Operation.Phase != models.DeploymentPhaseManualApproval {
		t.Fatalf("unexpected status response: %#v", response)
	}
	if strings.Contains(recorder.Body.String(), deployment.LogPath) {
		t.Fatal("read-only operation response exposed its controller log path")
	}

	wrongProject := httptest.NewRequest(http.MethodGet, statusPath, nil)
	wrongProject.Header.Set("X-Deploy-Control-Token", auth.ProjectToken("another-project"))
	wrongRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongRecorder, wrongProject)
	if wrongRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("cross-project token status = %d, want %d", wrongRecorder.Code, http.StatusUnauthorized)
	}

	approve := httptest.NewRequest(http.MethodPost,
		"/api/projects/medusa/deployments/deploy-medusa-123/approve",
		strings.NewReader(`{"confirmation":"approve deploy-medusa-123"}`))
	approve.Header.Set("X-Deploy-Control-Token", auth.ProjectToken("medusa"))
	approveRecorder := httptest.NewRecorder()
	router.ServeHTTP(approveRecorder, approve)
	if approveRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("project token approval status = %d, want %d", approveRecorder.Code, http.StatusUnauthorized)
	}
}
