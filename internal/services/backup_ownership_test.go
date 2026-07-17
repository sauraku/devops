package services

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	dockerclient "github.com/sauraku/devops-control/internal/docker"
)

func installFakeOwnershipDocker(t *testing.T, projectLabel string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/usr/bin/env bash
set -eu
if [ "${1:-}" = "compose" ] && [ "${2:-}" = "version" ]; then
  exit 0
fi
if [ "${1:-}" = "ps" ]; then
  case "$*" in
    *com.docker.compose.service=postgres*) printf '%s\n' postgres-container ;;
    *com.docker.compose.service=backend*) printf '%s\n' backend-container ;;
    *com.docker.compose.service=storefront*) printf '%s\n' storefront-container ;;
  esac
  exit 0
fi
if [ "${1:-}" = "inspect" ]; then
  container="${@: -1}"
  service="${container%%-*}"
  printf '[{"Id":"%s-id","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"%s"}},"State":{"Running":true}}]\n' \
    "${service}" "${FAKE_PROJECT_LABEL}" "${service}"
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PROJECT_LABEL", projectLabel)
}

func TestOwnedComposeTargetEnvReturnsVerifiedContainerIDs(t *testing.T) {
	installFakeOwnershipDocker(t, "medusa-main")
	service := &BackupService{docker: dockerclient.NewClient()}

	env, err := service.ownedComposeTargetEnv("medusa", "main", []composeServiceTarget{
		{service: "postgres", envKey: "POSTGRES_CONTAINER"},
		{service: "backend", envKey: "BACKEND_CONTAINER"},
		{service: "storefront", envKey: "STOREFRONT_CONTAINER"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if env["COMPOSE_PROJECT_NAME"] != "medusa-main" {
		t.Fatalf("COMPOSE_PROJECT_NAME = %q", env["COMPOSE_PROJECT_NAME"])
	}
	for key, want := range map[string]string{
		"POSTGRES_CONTAINER":   "postgres-id",
		"BACKEND_CONTAINER":    "backend-id",
		"STOREFRONT_CONTAINER": "storefront-id",
	} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestOwnedComposeTargetEnvRejectsWrongProjectLabel(t *testing.T) {
	installFakeOwnershipDocker(t, "other-main")
	service := &BackupService{docker: dockerclient.NewClient()}

	_, err := service.ownedComposeTargetEnv("medusa", "main", backupComposeTargets)
	if err == nil || !strings.Contains(err.Error(), "not owned by Compose project medusa-main service postgres") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBackupRestoreScriptsIgnoreDotenvContainerTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		args   []string
	}{
		{name: "backup", script: "backup-db.sh"},
		{name: "restore", script: "restore-db.sh", args: []string{"selected-backup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			projectDir := filepath.Join(root, "project")
			backupDir := filepath.Join(root, "backups")
			stateDir := filepath.Join(root, "state")
			logDir := filepath.Join(root, "logs")
			for _, dir := range []string{projectDir, backupDir, stateDir, logDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "restore" {
				if err := os.WriteFile(filepath.Join(backupDir, "selected-backup.dump.gz"), []byte("archive"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			envFile := filepath.Join(root, ".env.main")
			dotenv := strings.Join([]string{
				"PROJECT_ID=other",
				"TARGET_BRANCH=other",
				"COMPOSE_PROJECT_NAME=other-main",
				"POSTGRES_CONTAINER=unrelated-postgres",
				"BACKEND_CONTAINER=unrelated-backend",
				"STOREFRONT_CONTAINER=unrelated-storefront",
			}, "\n") + "\n"
			if err := os.WriteFile(envFile, []byte(dotenv), 0o600); err != nil {
				t.Fatal(err)
			}

			fakeBin := filepath.Join(root, "bin")
			if err := os.Mkdir(fakeBin, 0o700); err != nil {
				t.Fatal(err)
			}
			dockerLog := filepath.Join(root, "docker.log")
			fakeDocker := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "${FAKE_DOCKER_LOG}"
if [ "${1:-}" = "compose" ] && [ "${2:-}" = "version" ]; then
  exit 0
fi
if [ "${1:-}" = "ps" ]; then
  exit 0
fi
exit 1
`
			if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(fakeDocker), 0o700); err != nil {
				t.Fatal(err)
			}

			scriptPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", tc.script))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", append([]string{scriptPath}, tc.args...)...)
			cmd.Dir = projectDir
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_DOCKER_LOG="+dockerLog,
				"PROJECT_ID=medusa",
				"TARGET_BRANCH=main",
				"COMPOSE_PROJECT_NAME=medusa-main",
				"ENV_FILE="+envFile,
				"PROJECT_DIR="+projectDir,
				"PROJECT_STATE_DIR="+stateDir,
				"PROJECT_LOG_DIR="+logDir,
				"BACKUP_DIR_PATH="+backupDir,
				"BACKUP_SKIP_LOCK=true",
			)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("script unexpectedly succeeded:\n%s", output)
			}
			logData, readErr := os.ReadFile(dockerLog)
			if readErr != nil {
				t.Fatal(readErr)
			}
			logText := string(logData)
			if !strings.Contains(logText, "label=com.docker.compose.project=medusa-main") ||
				!strings.Contains(logText, "label=com.docker.compose.service=postgres") {
				t.Fatalf("trusted ownership filters missing from Docker calls:\n%s", logText)
			}
			if strings.Contains(logText, "other") || strings.Contains(logText, "unrelated") {
				t.Fatalf("dotenv-selected target reached Docker:\n%s", logText)
			}
		})
	}
}
