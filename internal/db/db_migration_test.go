package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestFreshDatabaseAppliesVersionedMigrations(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var versions int
	if err := database.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != len(schemaMigrations) {
		t.Fatalf("applied migrations = %d, want %d", versions, len(schemaMigrations))
	}
	for _, column := range []string{"last_deployed_commit", "last_deployed_image_tag"} {
		tx, err := database.Begin()
		if err != nil {
			t.Fatal(err)
		}
		exists, inspectErr := tableHasColumn(tx, "project_state", column)
		_ = tx.Rollback()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if !exists {
			t.Fatalf("fresh schema is missing project_state.%s", column)
		}
	}
}

func TestExistingDatabaseMigrationPreservesStateAndIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "devops-control.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
		CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			repo_url TEXT NOT NULL DEFAULT '',
			branch_name TEXT NOT NULL DEFAULT 'rc',
			deployment_mode TEXT NOT NULL DEFAULT 'compose_image',
			auto_apply INTEGER NOT NULL DEFAULT 1,
			registry_host TEXT NOT NULL DEFAULT 'ghcr.io',
			registry_username TEXT NOT NULL DEFAULT '',
			runner_container TEXT NOT NULL DEFAULT '',
			runner_status TEXT NOT NULL DEFAULT 'unknown',
			app_dir TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE project_state (
			project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
			paused INTEGER NOT NULL DEFAULT 0,
			paused_reason TEXT NOT NULL DEFAULT '',
			paused_at TEXT,
			paused_by TEXT NOT NULL DEFAULT '',
			last_deploy_status TEXT NOT NULL DEFAULT 'unknown',
			last_deploy_message TEXT NOT NULL DEFAULT '',
			last_run_at TEXT,
			active_deploy_id TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '{}'
		);
		INSERT INTO projects(id, name, created_at, updated_at)
			VALUES ('medusa', 'Medusa', 'before', 'before');
		INSERT INTO project_state(project_id, paused, paused_reason)
			VALUES ('medusa', 1, 'maintenance');
	`)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var paused bool
	var reason, commit, image string
	if err := database.QueryRow(`
		SELECT paused, paused_reason, last_deployed_commit, last_deployed_image_tag
		FROM project_state WHERE project_id = 'medusa'
	`).Scan(&paused, &reason, &commit, &image); err != nil {
		t.Fatal(err)
	}
	if !paused || reason != "maintenance" || commit != "" || image != "" {
		t.Fatalf("migrated state = paused:%v reason:%q commit:%q image:%q", paused, reason, commit, image)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatalf("idempotent reopen failed: %v", err)
	}
	defer reopened.Close()
}

func TestDatabaseRefusesUnknownFutureSchema(t *testing.T) {
	dataDir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dataDir, "devops-control.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		INSERT INTO schema_migrations(version, name, applied_at)
			VALUES (999, 'future', 'later');
	`)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_ = raw.Close()

	_, err = Open(dataDir)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future schema error = %v", err)
	}
}
