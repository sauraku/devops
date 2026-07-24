package db

import (
	"database/sql"
	"fmt"

	"github.com/sauraku/devops-control/internal/models"
)

// LockOwnershipError means the requested operation no longer owns the
// project's lock. Callers must not silently treat this as a successful release:
// another operation may now own the project.
type LockOwnershipError struct {
	ProjectID   string
	OperationID string
}

func (e *LockOwnershipError) Error() string {
	return fmt.Sprintf("operation %s does not own the deploy lock for project %s", e.OperationID, e.ProjectID)
}

func (db *DB) ListLocks() ([]*models.DeployLock, error) {
	rows, err := db.Query(`
		SELECT project_id, operation_id, operation, started_at, sha, image_tag, branch
		FROM deploy_locks ORDER BY started_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []*models.DeployLock
	for rows.Next() {
		var l models.DeployLock
		if err := rows.Scan(&l.ProjectID, &l.OperationID, &l.Operation, &l.StartedAt, &l.SHA, &l.ImageTag, &l.Branch); err != nil {
			return nil, err
		}
		locks = append(locks, &l)
	}
	return locks, rows.Err()
}

func (db *DB) GetLock(projectID string) (*models.DeployLock, error) {
	row := db.QueryRow(`
		SELECT project_id, operation_id, operation, started_at, sha, image_tag, branch
		FROM deploy_locks WHERE project_id = ?
	`, projectID)

	var l models.DeployLock
	err := row.Scan(&l.ProjectID, &l.OperationID, &l.Operation, &l.StartedAt, &l.SHA, &l.ImageTag, &l.Branch)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (db *DB) CreateLock(l *models.DeployLock) error {
	res, err := db.Exec(`
		INSERT INTO deploy_locks (project_id, operation_id, operation, started_at, sha, image_tag, branch)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO NOTHING
	`, l.ProjectID, l.OperationID, l.Operation, l.StartedAt, l.SHA, l.ImageTag, l.Branch)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("a lock already exists for project %s", l.ProjectID)
	}
	return nil
}

func (db *DB) ReleaseLock(projectID, operationID string) error {
	result, err := db.Exec("DELETE FROM deploy_locks WHERE project_id = ? AND operation_id = ?", projectID, operationID)
	if err != nil {
		return err
	}
	released, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if released != 1 {
		return &LockOwnershipError{ProjectID: projectID, OperationID: operationID}
	}
	return nil
}

func (db *DB) ReleaseAllLocks() error {
	_, err := db.Exec("DELETE FROM deploy_locks")
	return err
}
