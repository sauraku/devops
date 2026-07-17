package db

import (
	"database/sql"
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

func (db *DB) CreateDeployment(d *models.Deployment) error {
	_, err := db.Exec(`
		INSERT INTO deployments (id, project_id, kind, status, ref, sha, image_tag, branch,
		                         commit_message, started_at, finished_at, exit_code, log_path,
		                         github_run_id, github_run_number, github_actor,
		                         github_repository, github_workflow)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.ID, d.ProjectID, d.Kind, d.Status, d.Ref, d.SHA, d.ImageTag, d.Branch,
		d.CommitMessage, d.StartedAt.Format(time.RFC3339), nullTime(d.FinishedAt),
		d.ExitCode, d.LogPath,
		d.GitHubRunID, d.GitHubRunNumber, d.GitHubActor, d.GitHubRepo, d.GitHubWorkflow)
	return err
}

func (db *DB) UpdateDeploymentStatus(id string, status models.DeploymentStatus, exitCode *int, finishedAt *time.Time) error {
	_, err := db.Exec(`
		UPDATE deployments SET status = ?, exit_code = ?, finished_at = ? WHERE id = ?
	`, status, exitCode, nullTime(finishedAt), id)
	return err
}

func (db *DB) TransitionDeploymentStatus(id string, from, to models.DeploymentStatus) (bool, error) {
	result, err := db.Exec(`
		UPDATE deployments SET status = ? WHERE id = ? AND status = ?
	`, to, id, from)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (db *DB) CompleteActiveDeployment(id string, status models.DeploymentStatus, exitCode int, finishedAt time.Time) (bool, error) {
	result, err := db.Exec(`
		UPDATE deployments SET status = ?, exit_code = ?, finished_at = ?
		WHERE id = ? AND status IN ('pending', 'running')
	`, status, exitCode, finishedAt.Format(time.RFC3339), id)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (db *DB) ListDeployments(projectID string, limit int) ([]*models.Deployment, error) {
	rows, err := db.Query(`
		SELECT id, project_id, kind, status, ref, sha, image_tag, branch, commit_message,
		       started_at, finished_at, exit_code, log_path,
		       github_run_id, github_run_number, github_actor, github_repository, github_workflow
		FROM deployments WHERE project_id = ? ORDER BY started_at DESC LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []*models.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}

func (db *DB) GetActiveDeployments(projectID string) ([]*models.Deployment, error) {
	rows, err := db.Query(`
		SELECT id, project_id, kind, status, ref, sha, image_tag, branch, commit_message,
		       started_at, finished_at, exit_code, log_path,
		       github_run_id, github_run_number, github_actor, github_repository, github_workflow
		FROM deployments WHERE project_id = ? AND status IN ('pending', 'running') ORDER BY started_at DESC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []*models.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}

func (db *DB) GetDeployment(id string) (*models.Deployment, error) {
	row := db.QueryRow(`
		SELECT id, project_id, kind, status, ref, sha, image_tag, branch, commit_message,
		       started_at, finished_at, exit_code, log_path,
		       github_run_id, github_run_number, github_actor, github_repository, github_workflow
		FROM deployments WHERE id = ?
	`, id)
	return scanDeployment(row)
}

func scanDeployment(row scanner) (*models.Deployment, error) {
	var d models.Deployment
	var startedAt, finishedAt sql.NullString
	var exitCode sql.NullInt64
	err := row.Scan(&d.ID, &d.ProjectID, &d.Kind, &d.Status, &d.Ref, &d.SHA, &d.ImageTag, &d.Branch,
		&d.CommitMessage, &startedAt, &finishedAt, &exitCode, &d.LogPath,
		&d.GitHubRunID, &d.GitHubRunNumber, &d.GitHubActor, &d.GitHubRepo, &d.GitHubWorkflow)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		d.StartedAt, _ = time.Parse(time.RFC3339, startedAt.String)
	}
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339, finishedAt.String)
		d.FinishedAt = &t
	}
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		d.ExitCode = &ec
	}
	return &d, nil
}

func nullTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
