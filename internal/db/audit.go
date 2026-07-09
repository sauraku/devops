package db

import (
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

func (db *DB) LogAudit(entry *models.AuditEntry) error {
	_, err := db.Exec(`
		INSERT INTO audit_log (timestamp, action, status, project_id, message, metadata)
		VALUES (?, ?, ?, ?, ?, ?)
	`, entry.Timestamp.Format(time.RFC3339), entry.Action, entry.Status, entry.ProjectID, entry.Message, entry.Metadata)
	return err
}

func (db *DB) ListAuditLogs(projectID string, limit int) ([]*models.AuditEntry, error) {
	if projectID != "" {
		return db.listAuditLogsFiltered("WHERE project_id = ? ORDER BY id DESC LIMIT ?", projectID, limit)
	}
	return db.listAuditLogsFiltered("ORDER BY id DESC LIMIT ?", limit)
}

func (db *DB) listAuditLogsFiltered(where string, args ...any) ([]*models.AuditEntry, error) {
	query := `SELECT id, timestamp, action, status, project_id, message, metadata FROM audit_log ` + where
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*models.AuditEntry
	for rows.Next() {
		var e models.AuditEntry
		var ts string
		err := rows.Scan(&e.ID, &ts, &e.Action, &e.Status, &e.ProjectID, &e.Message, &e.Metadata)
		if err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
