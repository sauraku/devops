package db

import (
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

func (db *DB) CreateBackup(b *models.Backup) error {
	_, err := db.Exec(`
		INSERT INTO backups (id, project_id, file_path, sha256, size_bytes, timestamp,
		                     verification_status, env_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, b.ID, b.ProjectID, b.FilePath, b.SHA256, b.SizeBytes, b.Timestamp,
		b.VerificationStatus, b.EnvName, b.CreatedAt.Format(time.RFC3339))
	return err
}

func (db *DB) UpdateBackupVerification(id, status string) error {
	_, err := db.Exec("UPDATE backups SET verification_status = ? WHERE id = ?", status, id)
	return err
}

func (db *DB) ListBackups(projectID string, limit int) ([]*models.Backup, error) {
	query := `SELECT id, project_id, file_path, sha256, size_bytes, timestamp,
		       verification_status, env_name, created_at
		FROM backups WHERE project_id = ? ORDER BY created_at DESC`
	args := []any{projectID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backups []*models.Backup
	for rows.Next() {
		var b models.Backup
		var createdAt string
		err := rows.Scan(&b.ID, &b.ProjectID, &b.FilePath, &b.SHA256, &b.SizeBytes,
			&b.Timestamp, &b.VerificationStatus, &b.EnvName, &createdAt)
		if err != nil {
			return nil, err
		}
		b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		backups = append(backups, &b)
	}
	return backups, rows.Err()
}

func (db *DB) GetBackup(id, projectID string) (*models.Backup, error) {
	row := db.QueryRow(`
		SELECT id, project_id, file_path, sha256, size_bytes, timestamp,
		       verification_status, env_name, created_at
		FROM backups WHERE id = ? AND project_id = ?
	`, id, projectID)
	var b models.Backup
	var createdAt string
	err := row.Scan(&b.ID, &b.ProjectID, &b.FilePath, &b.SHA256, &b.SizeBytes,
		&b.Timestamp, &b.VerificationStatus, &b.EnvName, &createdAt)
	if err != nil {
		return nil, err
	}
	b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &b, nil
}

func (db *DB) DeleteBackup(id, projectID string) error {
	_, err := db.Exec("DELETE FROM backups WHERE id = ? AND project_id = ?", id, projectID)
	return err
}
