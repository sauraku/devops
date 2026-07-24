package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

func TestDeploymentStatePatchesPreserveConcurrentPauseFields(t *testing.T) {
	db.InitCrypto(strings.Repeat("p", 64))
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "main"}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{})
	sha := strings.Repeat("a", 40)
	deployment := &models.Deployment{
		ID:        "deploy-medusa-1",
		ProjectID: project.ID,
		SHA:       sha,
		ImageTag:  "sha-" + sha,
	}

	tests := []struct {
		name       string
		wantStatus string
		update     func() error
	}{
		{
			name:       "completion",
			wantStatus: string(models.DeploymentStatusSuccess),
			update: func() error {
				return service.recordDeploymentCompletionState(
					project.ID, deployment, models.DeploymentStatusSuccess, 0, time.Now().UTC(),
				)
			},
		},
		{
			name:       "abort",
			wantStatus: "aborted",
			update: func() error {
				return service.recordDeploymentAbortState(project.ID)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := database.UpsertProjectState(project.ID, map[string]any{
				"paused":             false,
				"paused_reason":      "",
				"paused_at":          "",
				"paused_by":          "",
				"last_deploy_status": "running",
				"active_deploy_id":   deployment.ID,
			}); err != nil {
				t.Fatal(err)
			}

			start := make(chan struct{})
			errs := make(chan error, 2)
			go func() {
				<-start
				errs <- database.UpsertProjectState(project.ID, map[string]any{
					"paused":        true,
					"paused_reason": "maintenance",
					"paused_at":     "2026-07-23T12:00:00Z",
					"paused_by":     "operator",
				})
			}()
			go func() {
				<-start
				errs <- tc.update()
			}()
			close(start)
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}

			state, err := database.GetProjectState(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]any{
				"paused":        true,
				"paused_reason": "maintenance",
				"paused_at":     "2026-07-23T12:00:00Z",
				"paused_by":     "operator",
			} {
				if got := state[key]; got != want {
					t.Fatalf("%s = %#v, want %#v after concurrent %s", key, got, want, tc.name)
				}
			}
			if got := state["last_deploy_status"]; got != tc.wantStatus {
				t.Fatalf("last_deploy_status = %#v, want %q", got, tc.wantStatus)
			}
			if got := state["active_deploy_id"]; got != "" {
				t.Fatalf("active_deploy_id = %#v, want empty", got)
			}
		})
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
	t.Setenv("DOCKER_TLS", "1")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/tmp/controller-docker-certificates")
	t.Setenv("DOCKER_API_VERSION", "1.46")

	values := map[string]string{}
	for _, entry := range deploymentProcessEnv(map[string]string{
		"DEPLOY_ID":              "deploy-123",
		"PROJECT_ID":             "medusa",
		"GITHUB_RUN_ID":          "1234",
		"PATH":                   "/project/bin",
		"HOME":                   "/project/home",
		"LD_PRELOAD":             "/project/evil.so",
		"LD_LIBRARY_PATH":        "/project/libraries",
		"GCONV_PATH":             "/project/gconv",
		"BASH_ENV":               "/project/bash-env",
		"PYTHONPATH":             "/project/python",
		"PYTHONINSPECT":          "1",
		"NODE_OPTIONS":           "--require=/project/hook.js",
		"APP_DOCKER_MODE":        "allowed-app-docker",
		"MY_COMPOSE_VALUE":       "allowed-app-compose",
		"DOCKER_CONFIG":          "/tmp/project-operation-docker-config",
		"DOCKER_TLS":             "0",
		"DOCKER_TLS_VERIFY":      "0",
		"COMPOSE_FILE":           "/project/compose.yml",
		projectEnvOverridesFDEnv: "99",
	}) {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[parts[0]] = parts[1]
	}

	for key, want := range map[string]string{
		"PATH":               "/controller/bin",
		"HOME":               "/controller/home",
		"DOCKER_CONFIG":      "/tmp/project-operation-docker-config",
		"DOCKER_HOST":        "unix:///var/run/docker.sock",
		"DOCKER_CONTEXT":     "controller",
		"DOCKER_TLS":         "1",
		"DOCKER_TLS_VERIFY":  "1",
		"DOCKER_CERT_PATH":   "/tmp/controller-docker-certificates",
		"DOCKER_API_VERSION": "1.46",
		"DEPLOY_ID":          "deploy-123",
		"PROJECT_ID":         "medusa",
		"GITHUB_RUN_ID":      "1234",
	} {
		if got := values[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "GCONV_PATH", "BASH_ENV", "PYTHONPATH", "PYTHONINSPECT",
		"NODE_OPTIONS", "APP_DOCKER_MODE", "MY_COMPOSE_VALUE", "COMPOSE_FILE", projectEnvOverridesFDEnv,
	} {
		if _, found := values[key]; found {
			t.Fatalf("non-controller key %s was passed to deployment", key)
		}
	}
}

func TestProjectOverridesTravelOnlyThroughPrivateDescriptor(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	scriptPath := filepath.Join(dir, "read-overrides.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
for key in LD_PRELOAD LD_LIBRARY_PATH GCONV_PATH BASH_ENV PYTHONPATH PYTHONINSPECT NODE_OPTIONS APP_VALUE; do
  if [[ "${!key+x}" == x ]]; then
    echo "project value reached child environment: ${key}" >&2
    exit 40
  fi
done
python3 - "${DEVOPS_ENV_OVERRIDES_FD}" "$1" <<'PY'
import json
import os
import sys

fd, result_path = int(sys.argv[1]), sys.argv[2]
with os.fdopen(fd, encoding="utf-8") as handle:
    values = json.load(handle)
assert values["APP_VALUE"] == "legitimate-app-value"
assert len(values["LARGE_VALUE"]) == 1024 * 1024
with open(result_path, "w", encoding="utf-8") as handle:
    json.dump({"APP_VALUE": values["APP_VALUE"]}, handle)
PY
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	controllerEnv := map[string]string{
		"DEPLOY_ID":       "deploy-123",
		"LD_PRELOAD":      "/project/evil.so",
		"LD_LIBRARY_PATH": "/project/libraries",
		"GCONV_PATH":      "/project/gconv",
		"BASH_ENV":        "/project/bash-env",
		"PYTHONPATH":      "/project/python",
		"PYTHONINSPECT":   "1",
		"NODE_OPTIONS":    "--require=/project/hook.js",
		"APP_VALUE":       "must-not-be-process-env",
	}
	overrides := map[string]string{
		"APP_VALUE":       "legitimate-app-value",
		"LARGE_VALUE":     strings.Repeat("x", 1024*1024),
		"LD_PRELOAD":      "/container/loader.so",
		"LD_LIBRARY_PATH": "/container/libraries",
		"GCONV_PATH":      "/container/gconv",
		"BASH_ENV":        "/container/bash-env",
		"PYTHONPATH":      "/container/python",
		"PYTHONINSPECT":   "1",
		"NODE_OPTIONS":    "--require=/container/hook.js",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath, resultPath)
	cmd.Env = deploymentProcessEnv(controllerEnv)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	stream, err := startCommandWithProjectOverrides(cmd, overrides)
	if err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	streamErr := stream.finish()
	if waitErr != nil {
		t.Fatalf("child failed: %v: %s", waitErr, output.String())
	}
	if streamErr != nil {
		t.Fatalf("override stream failed: %v", streamErr)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["APP_VALUE"] != "legitimate-app-value" {
		t.Fatalf("private override result = %q", result["APP_VALUE"])
	}
}

func TestProjectOverrideWriterStopsWhenDescendantRetainsUnreadDescriptor(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	scriptPath := filepath.Join(dir, "retain-override-fd.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
python3 - "$1" <<'PY'
import os
import sys
import time

pid = os.fork()
if pid == 0:
    time.sleep(30)
    os._exit(0)
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    handle.write(str(pid))
PY
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath, pidPath)
	cmd.Env = deploymentProcessEnv(map[string]string{"DEPLOY_ID": "deploy-unread-fd"})
	stream, err := startCommandWithProjectOverrides(cmd, map[string]string{
		"LARGE_VALUE": strings.Repeat("x", 4*1024*1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child failed before liveness check: %v", err)
	}
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &descendantPID); err != nil || descendantPID <= 1 {
		t.Fatalf("invalid descendant pid %q: %v", pidData, err)
	}
	descendant, err := os.FindProcess(descendantPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = descendant.Kill() })

	started := time.Now()
	streamErr := stream.finish()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("finish took %s with retained unread descriptor", elapsed)
	}
	if streamErr == nil {
		t.Fatal("unread override payload unexpectedly reported success")
	}
	if strings.Contains(streamErr.Error(), "did not stop") {
		t.Fatalf("override writer goroutine did not exit: %v", streamErr)
	}
}

func TestProjectScriptRendersPrivateOverridesWithoutExportingThem(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(repoRoot, "deploy", "project.sh")
	baseDir := t.TempDir()
	projectDir := filepath.Join(baseDir, "Projects", "medusa")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	template := `APP_VALUE=template-value
EMPTY_VALUE=template-empty
MASKED_SECRET=template-secret
LD_PRELOAD=
LD_LIBRARY_PATH=
GCONV_PATH=
BASH_ENV=
PYTHONPATH=
PYTHONINSPECT=
NODE_OPTIONS=
DOCKER_TLS_VERIFY=template-docker-value
COMPOSE_PROFILES=template-compose-value
BUILDKIT_HOST=template-buildkit-value
BUILDX_CONFIG=template-buildx-value
BASE_DIR=template-base
IMAGE_TAG=
COMPOSE_PROJECT_NAME=
ENV_NAME=
`
	for path, content := range map[string]string{
		filepath.Join(projectDir, ".env.template"): template,
		filepath.Join(projectDir, ".env.main"): `APP_VALUE=old-app-value
REMOVED_KEY=stale-removed-value
DEPLOY_ID=stale-deploy-id
GITHUB_RUN_ID=stale-run-id
IMAGE_TAG=stale-image
COMPOSE_PROJECT_NAME=stale-project
COMPOSE_PROFILES=stale-profile
DOCKER_TLS_VERIFY=stale-docker-value
BUILDKIT_HOST=stale-buildkit-value
BUILDX_CONFIG=stale-buildx-value
ENV_NAME=stale-environment
`,
		filepath.Join(projectDir, "docker-compose.yml"): "services: {}\n",
		filepath.Join(projectDir, "devops.json"):        `{"environment":{"generated_secrets":[]}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerStub := `#!/usr/bin/env bash
set -euo pipefail
for key in LD_PRELOAD LD_LIBRARY_PATH GCONV_PATH BASH_ENV PYTHONPATH PYTHONINSPECT NODE_OPTIONS APP_VALUE COMPOSE_PROFILES BUILDKIT_HOST BUILDX_CONFIG; do
  if [[ "${!key+x}" == x ]]; then
    echo "project value reached Docker process: ${key}" >&2
    exit 41
  fi
done
if [[ "${1:-}" == "compose" ]]; then
  for argument in "$@"; do
    if [[ "${argument}" == "config" ]]; then
      printf '%s\n' '{"volumes":{},"networks":{},"services":{}}'
      exit 0
    fi
  done
  exit 0
fi
if [[ "${1:-}" == "ps" ]]; then
  exit 0
fi
exit 0
`
	timeoutStub := `#!/usr/bin/env bash
set -euo pipefail
shift
exec "$@"
`
	validatorStub := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  snapshot-compose-input)
    [[ "${2:-}" == */Projects/medusa/docker-compose.yml ]]
    [[ "${3:-}" == */Projects/medusa/.env.main ]]
    [[ "${4:-}" == */Projects/medusa ]]
    [[ "${5:-}" == */State/medusa/compose-input.* ]]
    cp "${2}" "${5}/compose.yml"
    cp "${3}" "${5}/project.env"
    ;;
  validate-compose-rendered)
    [[ "${2:-}" == */State/medusa/compose-config.* ]]
    [[ "${3:-}" == medusa-main ]]
    [[ "${4:-}" == ghcr.io/sauraku/medusa ]]
    [[ "${5:-}" == release-test ]]
    ;;
  *)
    exit 42
    ;;
esac
`
	for path, content := range map[string]string{
		filepath.Join(binDir, "docker"):         dockerStub,
		filepath.Join(binDir, "timeout"):        timeoutStub,
		filepath.Join(binDir, "devops-control"): validatorStub,
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", baseDir)
	for _, key := range []string{"DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION"} {
		t.Setenv(key, "")
	}
	controllerEnv := map[string]string{
		"BASE_DIR":                      baseDir,
		"DEPLOY_ID":                     "deploy-private-env-test",
		"DEPLOY_SHA":                    strings.Repeat("a", 40),
		"IMAGE_TAG":                     "release-test",
		"PROJECT_ID":                    "medusa",
		"DEVOPS_CONTROL_BIN":            filepath.Join(binDir, "devops-control"),
		"AUTHENTICATED_GHCR_REPOSITORY": "ghcr.io/sauraku/medusa",
	}
	overrides := map[string]string{
		"APP_VALUE":              "operator-value",
		"EMPTY_VALUE":            "",
		"MASKED_SECRET":          "resolved-secret",
		"LD_PRELOAD":             "/container/loader.so",
		"LD_LIBRARY_PATH":        "/container/libraries",
		"GCONV_PATH":             "/container/gconv",
		"BASH_ENV":               "/container/bash-env",
		"PYTHONPATH":             "/container/python",
		"PYTHONINSPECT":          "1",
		"NODE_OPTIONS":           "--require=/container/hook.js",
		"DOCKER_TLS_VERIFY":      "container-docker-value",
		"COMPOSE_PROFILES":       "container-compose-value",
		"BUILDKIT_HOST":          "tcp://container-buildkit:1234",
		"BUILDX_CONFIG":          "/container/buildx",
		"BASE_DIR":               "/attacker/base",
		"COMPOSE_PROJECT_NAME":   "attacker-project",
		"ENV_NAME":               "attacker-environment",
		"UNDECLARED_APPLICATION": "must-not-be-rendered",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath, "medusa", "main", "release-test")
	cmd.Dir = projectDir
	cmd.Env = deploymentProcessEnv(controllerEnv)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	stream, err := startCommandWithProjectOverrides(cmd, overrides)
	if err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	streamErr := stream.finish()
	if waitErr != nil {
		t.Fatalf("project script failed: %v: %s", waitErr, output.String())
	}
	if streamErr != nil {
		t.Fatalf("override stream failed: %v", streamErr)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".env.main"))
	if err != nil {
		t.Fatal(err)
	}
	rendered := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			rendered[parts[0]] = parts[1]
		}
	}
	for key, want := range map[string]string{
		"APP_VALUE":            "operator-value",
		"EMPTY_VALUE":          "template-empty",
		"MASKED_SECRET":        "resolved-secret",
		"LD_PRELOAD":           "/container/loader.so",
		"LD_LIBRARY_PATH":      "/container/libraries",
		"GCONV_PATH":           "/container/gconv",
		"BASH_ENV":             "/container/bash-env",
		"PYTHONPATH":           "/container/python",
		"PYTHONINSPECT":        "1",
		"NODE_OPTIONS":         "--require=/container/hook.js",
		"BASE_DIR":             baseDir,
		"COMPOSE_PROJECT_NAME": "medusa-main",
		"ENV_NAME":             "main",
		"IMAGE_TAG":            "release-test",
	} {
		if got := rendered[key]; got != want {
			t.Fatalf("rendered %s = %q, want %q; output: %s", key, got, want, output.String())
		}
	}
	for _, key := range []string{
		"UNDECLARED_APPLICATION", "REMOVED_KEY", "DEPLOY_ID", "GITHUB_RUN_ID",
		"DOCKER_TLS_VERIFY", "COMPOSE_PROFILES", "BUILDKIT_HOST", "BUILDX_CONFIG",
	} {
		if _, exists := rendered[key]; exists {
			t.Fatalf("stale or undeclared key %s was rendered", key)
		}
	}
	for _, value := range []string{"operator-value", "/container/loader.so", "container-docker-value"} {
		if strings.Contains(output.String(), value) {
			t.Fatalf("project override leaked to deployment output: %q", value)
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

func TestAsyncDeployKeepsIsolatedRegistryAuthUntilChildFinishes(t *testing.T) {
	root := t.TempDir()
	db.InitCrypto(strings.Repeat("i", 64))
	database, err := db.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	appDir := filepath.Join(root, "app")
	scriptDir := filepath.Join(root, "deploy")
	binDir := filepath.Join(root, "bin")
	logDir := filepath.Join(root, "Logs", "medusa")
	globalConfigDir := filepath.Join(root, "global-docker-config")
	for _, dir := range []string{appDir, scriptDir, binDir, logDir, globalConfigDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(globalConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}

	globalSentinel := filepath.Join(globalConfigDir, "sentinel")
	if err := os.WriteFile(globalSentinel, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedPath := filepath.Join(root, "deploy-child-started")
	releasePath := filepath.Join(root, "release-deploy-child")
	observedConfigPath := filepath.Join(root, "observed-docker-config")
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })

	fakeDocker := filepath.Join(binDir, "docker")
	fakeDockerBody := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" != "login" ]]; then
	exit 1
fi
test -n "${DOCKER_CONFIG:-}"
test "${DOCKER_CONFIG}" != %q
test -d "${DOCKER_CONFIG}"
cat >/dev/null
printf 'authenticated\n' >"${DOCKER_CONFIG}/authenticated"
`, globalConfigDir)
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerBody), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
	t.Setenv("DOCKER_CONFIG", globalConfigDir)

	projectScript := filepath.Join(scriptDir, "project.sh")
	projectScriptBody := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
test -n "${DOCKER_CONFIG:-}"
test "${DOCKER_CONFIG}" != %q
test -f "${DOCKER_CONFIG}/authenticated"
printf '%%s\n' "${DOCKER_CONFIG}" >%q
: >%q
for ((i = 0; i < 500; i++)); do
	if [[ -f %q ]]; then
		exit 0
	fi
	sleep 0.01
done
exit 91
`, globalConfigDir, observedConfigPath, startedPath, releasePath)
	if err := os.WriteFile(projectScript, []byte(projectScriptBody), 0o700); err != nil {
		t.Fatal(err)
	}

	project := &models.Project{
		ID:               "medusa",
		Name:             "Medusa",
		RepoURL:          "https://github.com/sauraku/medusa",
		BranchName:       "main",
		DeploymentMode:   models.DeploymentModeComposeImage,
		RegistryHost:     "registry.example",
		RegistryUsername: "operator",
		AppDir:           appDir,
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveRegistryPassword(project.ID, "registry-password-test-value"); err != nil {
		t.Fatal(err)
	}

	deployment := &models.Deployment{
		ID:        "deploy-medusa-isolated-auth",
		ProjectID: project.ID,
		Kind:      models.DeploymentKindDeploy,
		Status:    models.DeploymentStatusPending,
		Branch:    "main",
		ImageTag:  "release-test",
		StartedAt: time.Now().UTC(),
		LogPath:   filepath.Join(logDir, "deploy-medusa-isolated-auth.log"),
	}
	if err := database.CreateDeployment(deployment); err != nil {
		t.Fatal(err)
	}
	cfg := &models.Config{
		BaseDir:     root,
		DataDir:     filepath.Join(root, "data"),
		ProjectRoot: root,
	}
	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), cfg)
	if err := service.startDeployment(deployment, project, models.DeploymentStatusPending); err != nil {
		t.Fatalf("start deployment: %v", err)
	}

	waitForPath := func(path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return
			} else if !os.IsNotExist(err) {
				t.Fatalf("stat %s: %v", path, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", path)
	}
	waitForPath(startedPath)

	observedBytes, err := os.ReadFile(observedConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	observedConfigDir := strings.TrimSpace(string(observedBytes))
	if observedConfigDir == "" || observedConfigDir == globalConfigDir {
		t.Fatalf("deployment received non-isolated Docker config %q", observedConfigDir)
	}
	if info, err := os.Stat(observedConfigDir); err != nil {
		t.Fatalf("isolated Docker config was removed while deployment was running: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("isolated Docker config mode = %o, want 700", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(observedConfigDir, "authenticated")); err != nil {
		t.Fatalf("deployment could not read isolated registry auth: %v", err)
	}
	if got, err := os.ReadFile(globalSentinel); err != nil {
		t.Fatal(err)
	} else if string(got) != "untouched\n" {
		t.Fatalf("global Docker config sentinel was mutated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(globalConfigDir, "authenticated")); !os.IsNotExist(err) {
		t.Fatalf("registry login mutated global Docker config: %v", err)
	}

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var completed *models.Deployment
	for time.Now().Before(deadline) {
		completed, err = service.GetDeployment(project.ID, deployment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if models.NewDeploymentOperation(completed).Terminal {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := models.NewDeploymentOperation(completed)
	if !result.Terminal || !result.Successful {
		t.Fatalf("deployment did not reach terminal success: %#v", result)
	}

	cleanupDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(cleanupDeadline) {
		_, err = os.Stat(observedConfigDir)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatalf("stat isolated Docker config after deployment: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("isolated Docker config was not removed after deployment: %s", observedConfigDir)
	}
	if got, err := os.ReadFile(globalSentinel); err != nil {
		t.Fatal(err)
	} else if string(got) != "untouched\n" {
		t.Fatalf("global Docker config sentinel was mutated after deployment: %q", got)
	}
	if _, err := os.Stat(filepath.Join(globalConfigDir, "authenticated")); !os.IsNotExist(err) {
		t.Fatalf("registry auth remained in global Docker config: %v", err)
	}
}

func TestDeploymentRegistryAuthFallsBackToStoredGitHubPATForGHCR(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "docker-login")
	writeRegistryLoginStub(t, root, capturePath, false)

	database := openDeployTestDB(t, root)
	project := &models.Project{
		ID:           "medusa",
		RepoURL:      "https://github.com/sauraku/medusa.git",
		RegistryHost: "ghcr.io",
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	pat := "ghp_" + strings.Repeat("a", 36)
	if err := database.SaveRunnerToken(project.ID, pat); err != nil {
		t.Fatal(err)
	}

	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{GithubUser: "pat-owner"})
	auth, authenticatedRepository, err := service.registryAuthForDeployment(project)
	if err != nil {
		t.Fatalf("registry auth: %v", err)
	}
	if auth == nil {
		t.Fatal("stored GitHub PAT did not produce isolated GHCR auth")
	}
	if authenticatedRepository != "ghcr.io/sauraku/medusa" {
		t.Fatalf("authenticated repository = %q, want ghcr.io/sauraku/medusa", authenticatedRepository)
	}
	t.Cleanup(func() { _ = auth.Close() })

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ghcr.io|pat-owner|" + pat; strings.TrimSpace(string(got)) != want {
		t.Fatalf("registry login = %q, want %q", strings.TrimSpace(string(got)), want)
	}
	if info, err := os.Stat(auth.ConfigDir()); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("isolated Docker config mode = %o, want 700", info.Mode().Perm())
	}
}

func TestDeploymentRegistryAuthExplicitCredentialTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "docker-login")
	writeRegistryLoginStub(t, root, capturePath, false)

	database := openDeployTestDB(t, root)
	project := &models.Project{
		ID:               "medusa",
		RepoURL:          "https://github.com/sauraku/medusa",
		RegistryHost:     "ghcr.io",
		RegistryUsername: "registry-operator",
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveRunnerToken(project.ID, "ghp_"+strings.Repeat("a", 36)); err != nil {
		t.Fatal(err)
	}
	const explicitPassword = "explicit-registry-password"
	if err := database.SaveRegistryPassword(project.ID, explicitPassword); err != nil {
		t.Fatal(err)
	}

	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{GithubUser: "pat-owner"})
	auth, authenticatedRepository, err := service.registryAuthForDeployment(project)
	if err != nil {
		t.Fatalf("registry auth: %v", err)
	}
	if auth == nil {
		t.Fatal("explicit registry credential did not produce isolated auth")
	}
	if authenticatedRepository != "" {
		t.Fatalf("explicit credential unexpectedly constrained to fallback repository %q", authenticatedRepository)
	}
	t.Cleanup(func() { _ = auth.Close() })

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ghcr.io|registry-operator|" + explicitPassword; strings.TrimSpace(string(got)) != want {
		t.Fatalf("registry login = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestDeploymentRegistryAuthDoesNotReuseRunnerCredentialOutsideBoundedFallback(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		repoURL string
		token   string
	}{
		{
			name:    "different registry",
			host:    "registry.example",
			repoURL: "https://github.com/sauraku/medusa",
			token:   "ghp_" + strings.Repeat("a", 36),
		},
		{
			name:    "non GitHub repository",
			host:    "ghcr.io",
			repoURL: "https://github.com.evil.example/sauraku/medusa",
			token:   "ghp_" + strings.Repeat("a", 36),
		},
		{
			name:    "short lived runner token",
			host:    "ghcr.io",
			repoURL: "https://github.com/sauraku/medusa",
			token:   "short-lived-registration-token",
		},
		{
			name:    "short PAT shaped token",
			host:    "ghcr.io",
			repoURL: "https://github.com/sauraku/medusa",
			token:   "ghp_not-a-long-lived-pat",
		},
		{
			name:    "registry host is not exact",
			host:    "GHCR.IO",
			repoURL: "https://github.com/sauraku/medusa",
			token:   "ghp_" + strings.Repeat("a", 36),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			capturePath := filepath.Join(root, "docker-login")
			writeRegistryLoginStub(t, root, capturePath, false)
			database := openDeployTestDB(t, root)
			project := &models.Project{
				ID:           "medusa",
				RepoURL:      tc.repoURL,
				RegistryHost: tc.host,
			}
			if err := database.UpsertProject(project); err != nil {
				t.Fatal(err)
			}
			if err := database.SaveRunnerToken(project.ID, tc.token); err != nil {
				t.Fatal(err)
			}

			service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{GithubUser: "pat-owner"})
			auth, authenticatedRepository, err := service.registryAuthForDeployment(project)
			if err != nil {
				t.Fatalf("registry auth: %v", err)
			}
			if auth != nil {
				_ = auth.Close()
				t.Fatal("runner credential was reused outside the bounded GHCR fallback")
			}
			if authenticatedRepository != "" {
				t.Fatalf("unexpected authenticated repository %q", authenticatedRepository)
			}
			if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
				t.Fatalf("Docker login was invoked unexpectedly: %v", err)
			}
		})
	}
}

func TestDeploymentRegistryAuthDoesNotExposeFallbackPATOnLoginFailure(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "docker-login")
	writeRegistryLoginStub(t, root, capturePath, true)

	database := openDeployTestDB(t, root)
	project := &models.Project{
		ID:           "medusa",
		RepoURL:      "https://github.com/sauraku/medusa",
		RegistryHost: "ghcr.io",
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	pat := "ghp_" + strings.Repeat("s", 36)
	if err := database.SaveRunnerToken(project.ID, pat); err != nil {
		t.Fatal(err)
	}

	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{GithubUser: "pat-owner"})
	auth, _, err := service.registryAuthForDeployment(project)
	if auth != nil {
		_ = auth.Close()
		t.Fatal("failed registry login returned an auth handle")
	}
	if err == nil || !strings.Contains(err.Error(), "GHCR login with stored GitHub runner credential failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), pat) {
		t.Fatal("registry login failure exposed the stored GitHub PAT")
	}
}

func TestDeploymentRegistryAuthDoesNotExposeExplicitCredentialOnLoginFailure(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "docker-login")
	writeRegistryLoginStub(t, root, capturePath, true)

	database := openDeployTestDB(t, root)
	project := &models.Project{
		ID:               "medusa",
		RepoURL:          "https://github.com/sauraku/medusa",
		RegistryHost:     "ghcr.io",
		RegistryUsername: "registry-operator",
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	password := "explicit-registry-secret"
	if err := database.SaveRegistryPassword(project.ID, password); err != nil {
		t.Fatal(err)
	}

	service := NewDeployService(database, docker.NewClient(), NewAuditService(database), &models.Config{})
	auth, _, err := service.registryAuthForDeployment(project)
	if auth != nil {
		_ = auth.Close()
		t.Fatal("failed registry login returned an auth handle")
	}
	if err == nil || !strings.Contains(err.Error(), "registry login failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("registry login failure exposed the explicit registry credential")
	}
}

func openDeployTestDB(t *testing.T, root string) *db.DB {
	t.Helper()
	db.InitCrypto(strings.Repeat("r", 64))
	database, err := db.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func writeRegistryLoginStub(t *testing.T, root, capturePath string, fail bool) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	failure := ""
	if fail {
		// Deliberately mimic a hostile helper echoing stdin. The service must
		// not copy this output into its error.
		failure = `printf 'login rejected for %s\n' "$password"
exit 1`
	}
	body := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" != "login" ]]; then
	exit 1
fi
host="${2:-}"
test "${3:-}" = "-u"
username="${4:-}"
test "${5:-}" = "--password-stdin"
IFS= read -r password || true
printf '%%s|%%s|%%s\n' "$host" "$username" "$password" >%q
%s
`, capturePath, failure)
	dockerPath := filepath.Join(binDir, "docker")
	if err := os.WriteFile(dockerPath, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
}
