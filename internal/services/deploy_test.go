package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

func githubDeployRequest() *models.DeployRequest {
	sha := strings.Repeat("a", 40)
	return &models.DeployRequest{
		Ref:             "refs/heads/dev",
		SHA:             sha,
		Branch:          "dev",
		ImageTag:        "sha-" + sha,
		GitHubRunID:     "1234",
		GitHubRunNumber: "42",
		GitHubActor:     "octocat",
		GitHubRepo:      "sauraku/medusa",
		GitHubWorkflow:  "Multi-Branch CI/CD Deployment",
	}
}

func TestValidateDeployRequestAcceptsMatchingGitHubCallback(t *testing.T) {
	project := &models.Project{ID: "medusa", RepoURL: "https://github.com/sauraku/medusa.git", BranchName: "dev"}
	request := githubDeployRequest()

	normalized, err := ValidateDeployRequest(project, request, true)
	if err != nil {
		t.Fatalf("valid callback rejected: %v", err)
	}
	if normalized.Branch != "dev" || normalized.ImageTag != request.ImageTag {
		t.Fatalf("unexpected normalized request: %#v", normalized)
	}
}

func TestValidateDeployRequestRejectsCrossBranchAndForgedProvenance(t *testing.T) {
	project := &models.Project{ID: "medusa", RepoURL: "https://github.com/sauraku/medusa", BranchName: "dev"}
	tests := map[string]func(*models.DeployRequest){
		"branch":     func(r *models.DeployRequest) { r.Branch = "main" },
		"ref":        func(r *models.DeployRequest) { r.Ref = "refs/heads/main" },
		"repository": func(r *models.DeployRequest) { r.GitHubRepo = "sauraku/other" },
		"short sha":  func(r *models.DeployRequest) { r.SHA = "abc123" },
		"image tag":  func(r *models.DeployRequest) { r.ImageTag = "dev" },
		"run id":     func(r *models.DeployRequest) { r.GitHubRunID = "0" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := githubDeployRequest()
			mutate(request)
			if _, err := ValidateDeployRequest(project, request, true); err == nil {
				t.Fatal("unsafe callback was accepted")
			}
		})
	}
}

func TestValidateDeployRequestRequiresGitHubMetadataForScopedRunner(t *testing.T) {
	project := &models.Project{ID: "medusa", RepoURL: "https://github.com/sauraku/medusa", BranchName: "dev"}
	request := &models.DeployRequest{Branch: "dev", ImageTag: "manual-release"}
	if _, err := ValidateDeployRequest(project, request, true); err == nil {
		t.Fatal("scoped runner request without GitHub provenance was accepted")
	}

	normalized, err := ValidateDeployRequest(project, request, false)
	if err != nil {
		t.Fatalf("trusted manual deployment rejected: %v", err)
	}
	if normalized.Branch != "dev" || normalized.ImageTag != "manual-release" {
		t.Fatalf("unexpected normalized manual deployment: %#v", normalized)
	}
}

func TestValidateDeployRequestDoesNotMutateCaller(t *testing.T) {
	project := &models.Project{ID: "medusa", BranchName: "dev"}
	request := &models.DeployRequest{SHA: strings.Repeat("b", 40)}
	normalized, err := ValidateDeployRequest(project, request, false)
	if err != nil {
		t.Fatal(err)
	}
	if request.Branch != "" || request.ImageTag != "" {
		t.Fatalf("caller request was mutated: %#v", request)
	}
	if normalized.Branch != "dev" || normalized.ImageTag != "sha-"+request.SHA {
		t.Fatalf("request was not normalized: %#v", normalized)
	}
}

func TestDeploymentProcessEnvPreservesControllerDockerAuthAndBlocksProjectControlOverrides(t *testing.T) {
	t.Setenv("PATH", "/controller/bin")
	t.Setenv("HOME", "/controller/home")
	t.Setenv("DOCKER_CONFIG", "/tmp/controller-docker-config")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "controller")

	values := map[string]string{}
	for _, entry := range deploymentProcessEnv(map[string]string{
		"NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY": "pk_test",
		"DEPLOY_ID":                          "deploy-123",
		"PATH":                               "/project/bin",
		"HOME":                               "/project/home",
		"DOCKER_CONFIG":                      "/project/docker-config",
		"DOCKER_HOST":                        "tcp://untrusted:2375",
		"PROJECT_ENV_FILE":                   "/tmp/untrusted.env",
		"BASH_ENV":                           "/tmp/untrusted-bashrc",
	}) {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[parts[0]] = parts[1]
	}

	for key, want := range map[string]string{
		"PATH":                               "/controller/bin",
		"HOME":                               "/controller/home",
		"DOCKER_CONFIG":                      "/tmp/controller-docker-config",
		"DOCKER_HOST":                        "unix:///var/run/docker.sock",
		"DOCKER_CONTEXT":                     "controller",
		"DEPLOY_ID":                          "deploy-123",
		"NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY": "pk_test",
	} {
		if got := values[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"PROJECT_ENV_FILE", "BASH_ENV"} {
		if _, found := values[key]; found {
			t.Fatalf("project override for reserved key %s was passed to deployment", key)
		}
	}
}

func TestPendingApprovalRunsSameDeploymentToTerminalSuccess(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	appDir := filepath.Join(root, "app")
	scriptDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scriptDir, 0o750); err != nil {
		t.Fatal(err)
	}
	controllerDockerConfig := filepath.Join(root, "docker-config")
	t.Setenv("DOCKER_CONFIG", controllerDockerConfig)
	script := filepath.Join(scriptDir, "project.sh")
	scriptBody := fmt.Sprintf("#!/usr/bin/env bash\nset -euo pipefail\ntest -n \"${DEPLOY_ID:-}\"\ntest \"${DOCKER_CONFIG:-}\" = %q\nsleep 0.1\n", controllerDockerConfig)
	if err := os.WriteFile(script, []byte(scriptBody), 0o750); err != nil {
		t.Fatal(err)
	}

	project := &models.Project{
		ID:             "medusa",
		Name:           "Medusa",
		RepoURL:        "https://github.com/sauraku/medusa",
		BranchName:     "dev",
		DeploymentMode: models.DeploymentModeComposeImage,
		AutoApply:      false,
		RegistryHost:   "ghcr.io",
		AppDir:         appDir,
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	cfg := &models.Config{
		BaseDir:     root,
		DataDir:     filepath.Join(root, "data"),
		ProjectRoot: root,
	}
	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), cfg)

	pending, err := service.RequestApproval(project.ID, githubDeployRequest())
	if err != nil {
		t.Fatalf("request approval: %v", err)
	}
	operation := models.NewDeploymentOperation(pending)
	if operation.Phase != models.DeploymentPhaseManualApproval || operation.Terminal || !operation.ManualApprovalRequired {
		t.Fatalf("unexpected pending approval contract: %#v", operation)
	}

	approved, err := service.Approve(project.ID, pending.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.ID != pending.ID || approved.Status != models.DeploymentStatusPending {
		t.Fatalf("approval did not queue the same operation: %#v", approved)
	}

	deadline := time.Now().Add(5 * time.Second)
	var completed *models.Deployment
	for time.Now().Before(deadline) {
		completed, err = service.GetDeployment(project.ID, pending.ID)
		if err != nil {
			t.Fatal(err)
		}
		if models.NewDeploymentOperation(completed).Terminal {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := models.NewDeploymentOperation(completed)
	if !result.Terminal || !result.Successful || result.Status != models.DeploymentStatusSuccess {
		t.Fatalf("deployment did not reach terminal success: %#v", result)
	}
	if _, err := service.Approve(project.ID, pending.ID); err == nil {
		t.Fatal("terminal deployment was approved twice")
	}
	if _, err := service.GetDeployment("another-project", pending.ID); err == nil {
		t.Fatal("deployment status crossed the project boundary")
	}
}
