package services

import (
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/models"
)

type AuditService struct {
	db *db.DB
}

func NewAuditService(database *db.DB) *AuditService {
	return &AuditService{db: database}
}

func (s *AuditService) Log(action, status, projectID, message, metadata string) {
	entry := &models.AuditEntry{
		Timestamp: time.Now().UTC(),
		Action:    action,
		Status:    status,
		ProjectID: projectID,
		Message:   message,
		Metadata:  metadata,
	}
	_ = s.db.LogAudit(entry)
}

func (s *AuditService) Recent(projectID string, limit int) ([]*models.AuditEntry, error) {
	return s.db.ListAuditLogs(projectID, limit)
}
