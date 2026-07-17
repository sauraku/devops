package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sauraku/devops-control/internal/models"
)

func TestCredentialsAndEnvironmentAreEncryptedAndSeparated(t *testing.T) {
	InitCrypto(strings.Repeat("e", 64))
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	registryPassword := "registry-password-test-value"
	runnerToken := "runner-token-test-value"
	project := &models.Project{
		ID: "medusa", Name: "Medusa", BranchName: "dev",
		DeploymentMode: models.DeploymentModeComposeImage,
		RegistryHost:   "ghcr.io", RegistryUsername: "test-user",
	}
	if err := database.SaveProjectWithCredentials(project, &registryPassword, &runnerToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := database.GetRegistryPassword("medusa"); got != registryPassword {
		t.Fatalf("registry password = %q, want distinct saved value", got)
	}
	if got, _ := database.GetRunnerToken("medusa"); got != runnerToken {
		t.Fatalf("runner token = %q, want distinct saved value", got)
	}

	overrides := map[string]string{"JWT_SECRET": "environment-secret-test-value", "PORT": "9000"}
	if err := database.SaveProjectEnvOverrides("medusa", overrides); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := database.QueryRow("SELECT encrypted_overrides FROM project_env WHERE project_id = ?", "medusa").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, overrides["JWT_SECRET"]) {
		t.Fatal("project environment was stored in plaintext")
	}
	got, err := database.GetProjectEnvOverrides("medusa")
	if err != nil || got["JWT_SECRET"] != overrides["JWT_SECRET"] || got["PORT"] != "9000" {
		t.Fatalf("environment round trip = %#v, %v", got, err)
	}
}

func TestMigrateLegacyProjectEnvOverridesIsEncryptedAndAtomic(t *testing.T) {
	InitCrypto(strings.Repeat("m", 64))
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "main"}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	legacySecret := "legacy-plaintext-secret"
	metadata, _ := json.Marshal(map[string]any{
		"unrelated": "preserved",
		"env_overrides": map[string]any{
			"JWT_SECRET": legacySecret,
			"PORT":       float64(9000),
		},
	})
	if err := database.UpsertProjectState("medusa", map[string]any{"metadata": string(metadata)}); err != nil {
		t.Fatal(err)
	}

	if err := database.MigrateLegacyProjectEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	overrides, err := database.GetProjectEnvOverrides("medusa")
	if err != nil {
		t.Fatal(err)
	}
	if overrides["JWT_SECRET"] != legacySecret || overrides["PORT"] != "9000" {
		t.Fatalf("migrated overrides = %#v", overrides)
	}
	state, err := database.GetProjectState("medusa")
	if err != nil {
		t.Fatal(err)
	}
	rawMetadata, _ := state["metadata"].(string)
	if strings.Contains(rawMetadata, legacySecret) || strings.Contains(rawMetadata, "env_overrides") {
		t.Fatalf("legacy plaintext remained in metadata: %s", rawMetadata)
	}
	if !strings.Contains(rawMetadata, "preserved") {
		t.Fatalf("unrelated metadata was lost: %s", rawMetadata)
	}
	var encrypted string
	if err := database.QueryRow("SELECT encrypted_overrides FROM project_env WHERE project_id = ?", "medusa").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, legacySecret) {
		t.Fatal("migrated environment remained plaintext")
	}

	// The migration is idempotent and does not replace an encrypted value with
	// stale legacy metadata if both exist during an interrupted old upgrade.
	if err := database.SaveProjectEnvOverrides("medusa", map[string]string{"JWT_SECRET": "current-secret"}); err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(map[string]any{"env_overrides": map[string]any{"JWT_SECRET": "stale-secret"}})
	if _, err := database.Exec("UPDATE project_state SET metadata = ? WHERE project_id = ?", string(stale), "medusa"); err != nil {
		t.Fatal(err)
	}
	if err := database.MigrateLegacyProjectEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	overrides, err = database.GetProjectEnvOverrides("medusa")
	if err != nil || overrides["JWT_SECRET"] != "current-secret" {
		t.Fatalf("encrypted value was overwritten: %#v, %v", overrides, err)
	}
}

func TestMigrateLegacyProjectEnvOverridesRollsBackOnInvalidRow(t *testing.T) {
	InitCrypto(strings.Repeat("r", 64))
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	for _, id := range []string{"good", "bad"} {
		if err := database.UpsertProject(&models.Project{ID: id, Name: id, BranchName: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	good, _ := json.Marshal(map[string]any{"env_overrides": map[string]any{"SECRET": "plaintext"}})
	if err := database.UpsertProjectState("good", map[string]any{"metadata": string(good)}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertProjectState("bad", map[string]any{"metadata": "not-json"}); err != nil {
		t.Fatal(err)
	}

	if err := database.MigrateLegacyProjectEnvOverrides(); err == nil {
		t.Fatal("expected invalid metadata to fail the migration")
	}
	if got, err := database.GetProjectEnvOverrides("good"); err != nil || got != nil {
		t.Fatalf("partial encrypted migration was committed: %#v, %v", got, err)
	}
	state, err := database.GetProjectState("good")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state["metadata"].(string), "plaintext") {
		t.Fatal("plaintext source was removed despite transaction rollback")
	}
}
