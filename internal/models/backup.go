package models

import "time"

type Backup struct {
	ID                 string `json:"id"`
	ProjectID          string `json:"project_id"`
	FilePath           string `json:"file_path"`
	SHA256             string `json:"sha256"`
	SizeBytes          int64  `json:"size_bytes"`
	Timestamp          string `json:"timestamp"`
	VerificationStatus string `json:"verification_status"`
	EnvName            string `json:"env_name"`
	CreatedAt          time.Time `json:"created_at"`
}

type BackupVerifyResult struct {
	OK        bool                 `json:"ok"`
	Message   string               `json:"message"`
	Backup    *Backup              `json:"backup,omitempty"`
	TableList []BackupTableInfo    `json:"tables,omitempty"`
}

type BackupTableInfo struct {
	Name string `json:"name"`
	Rows string `json:"rows"`
	Kind string `json:"kind"`
}
