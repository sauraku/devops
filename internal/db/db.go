package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	DataDir string
	stopCh  chan struct{}
	stop    sync.Once
	wg      sync.WaitGroup
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
	conn.SetMaxOpenConns(1)
	db := &DB{DB: conn, DataDir: dataDir, stopCh: make(chan struct{})}
	if err := db.migrate(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db.wg.Add(1)
	go db.walCheckpointer()
	return db, nil
}

func (db *DB) walCheckpointer() {
	defer db.wg.Done()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-db.stopCh:
			return
		case <-ticker.C:
			db.checkpointWAL()
		}
	}
}

func (db *DB) checkpointWAL() {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("PANIC in WAL checkpointer: %v", recovered)
		}
	}()
	if _, err := db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		log.Printf("WAL checkpoint error: %v", err)
	}
}

func (db *DB) Close() error {
	db.stop.Do(func() { close(db.stopCh) })
	db.wg.Wait()
	return db.DB.Close()
}

type schemaMigration struct {
	version int
	name    string
	apply   func(*sql.Tx) error
}

var schemaMigrations = []schemaMigration{
	{version: 1, name: "initial schema", apply: createInitialSchema},
	{version: 2, name: "deployment result columns", apply: addDeploymentResultColumns},
}

func (db *DB) migrate() error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var newest int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&newest); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	latest := schemaMigrations[len(schemaMigrations)-1].version
	if newest > latest {
		return fmt.Errorf("database schema version %d is newer than supported version %d", newest, latest)
	}

	for _, migration := range schemaMigrations {
		var applied int
		if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.version, err)
		}
		if err := migration.apply(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", migration.version, migration.name, err)
		}
		if _, err := tx.Exec(
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
			migration.version, migration.name, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func createInitialSchema(tx *sql.Tx) error {
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
		status TEXT NOT NULL DEFAULT 'pending',
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
		metadata TEXT NOT NULL DEFAULT '{}'
	);

	CREATE TABLE IF NOT EXISTS registry_auth (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		registry_password TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS runner_auth (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		runner_token TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS project_env (
		project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
		encrypted_overrides TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	`
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("create initial schema: %w", err)
	}
	return nil
}

func addDeploymentResultColumns(tx *sql.Tx) error {
	for _, column := range []string{"last_deployed_commit", "last_deployed_image_tag"} {
		exists, err := tableHasColumn(tx, "project_state", column)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(
			"ALTER TABLE project_state ADD COLUMN %s TEXT NOT NULL DEFAULT ''",
			column,
		)); err != nil {
			return fmt.Errorf("add project_state.%s: %w", column, err)
		}
	}
	return nil
}

func tableHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("inspect table %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
