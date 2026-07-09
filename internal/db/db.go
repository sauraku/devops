package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	DataDir string
}

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "devops-control.db")
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	conn.SetMaxOpenConns(4) // WAL mode supports concurrent readers
	db := &DB{DB: conn, DataDir: dataDir}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Mark deployments left "running" after a crash as failed
	if _, err := db.Exec(`UPDATE deployments SET status='failed', finished_at=?, exit_code=-1 WHERE status='running'`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("Failed to mark stale deployments as failed: %v", err)
	}
	// Start periodic WAL checkpointing
	go db.walCheckpointer()
	return db, nil
}

func (db *DB) walCheckpointer() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in WAL checkpointer: %v", r)
		}
	}()
	for {
		time.Sleep(1 * time.Hour)
		if _, err := db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			log.Printf("WAL checkpoint error: %v", err)
		}
	}
}

func (db *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS projects (
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

	CREATE TABLE IF NOT EXISTS deployments (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		kind TEXT NOT NULL DEFAULT 'deploy',
		status TEXT NOT NULL DEFAULT 'running',
		ref TEXT NOT NULL DEFAULT '',
		sha TEXT NOT NULL DEFAULT '',
		image_tag TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT '',
		commit_message TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		finished_at TEXT,
		exit_code INTEGER,
		log_path TEXT NOT NULL DEFAULT '',
		github_run_id TEXT NOT NULL DEFAULT '',
		github_run_number TEXT NOT NULL DEFAULT '',
		github_actor TEXT NOT NULL DEFAULT '',
		github_repository TEXT NOT NULL DEFAULT '',
		github_workflow TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_deployments_project ON deployments(project_id);
	CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status);

	CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		action TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'ok',
		project_id TEXT,
		message TEXT,
		metadata TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_audit_project ON audit_log(project_id);
	CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);

	CREATE TABLE IF NOT EXISTS backups (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		file_path TEXT NOT NULL,
		sha256 TEXT NOT NULL DEFAULT '',
		size_bytes INTEGER NOT NULL DEFAULT 0,
		timestamp TEXT NOT NULL,
		verification_status TEXT,
		env_name TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_backups_project ON backups(project_id);

	CREATE TABLE IF NOT EXISTS deploy_locks (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		operation_id TEXT NOT NULL,
		operation TEXT NOT NULL,
		started_at TEXT NOT NULL,
		sha TEXT NOT NULL DEFAULT '',
		image_tag TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS project_state (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		paused INTEGER NOT NULL DEFAULT 0,
		paused_reason TEXT NOT NULL DEFAULT '',
		paused_at TEXT,
		paused_by TEXT NOT NULL DEFAULT '',
		last_deploy_status TEXT NOT NULL DEFAULT 'unknown',
		last_deploy_message TEXT NOT NULL DEFAULT '',
		last_run_at TEXT,
		active_deploy_id TEXT NOT NULL DEFAULT '',
		last_deployed_commit TEXT NOT NULL DEFAULT '',
		last_deployed_image_tag TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}'
	);

	CREATE TABLE IF NOT EXISTS registry_auth (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		registry_password TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Migrate: add last_deployed_commit and last_deployed_image_tag columns
	// (added 2026-06-27 — safe to run on schema that already has them)
	db.Exec("ALTER TABLE project_state ADD COLUMN last_deployed_commit TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE project_state ADD COLUMN last_deployed_image_tag TEXT NOT NULL DEFAULT ''")

	return nil
}
