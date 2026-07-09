package models

import "time"

type AuditEntry struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Status    string    `json:"status"`
	ProjectID string    `json:"project_id,omitempty"`
	Message   string    `json:"message,omitempty"`
	Metadata  string    `json:"metadata,omitempty"`
}
