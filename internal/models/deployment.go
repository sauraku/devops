package models

import "time"

type DeploymentKind string

const (
	DeploymentKindDeploy   DeploymentKind = "deploy"
	DeploymentKindRollback DeploymentKind = "rollback"
	DeploymentKindBackup   DeploymentKind = "backup"
	DeploymentKindRestore  DeploymentKind = "restore"
)

type DeploymentStatus string

const (
	DeploymentStatusRunning  DeploymentStatus = "running"
	DeploymentStatusSuccess  DeploymentStatus = "success"
	DeploymentStatusFailed   DeploymentStatus = "failed"
	DeploymentStatusAborted  DeploymentStatus = "aborted"
)

type Deployment struct {
	ID              string           `json:"id"`
	ProjectID       string           `json:"project_id"`
	Kind            DeploymentKind   `json:"kind"`
	Status          DeploymentStatus `json:"status"`
	Ref             string           `json:"ref"`
	SHA             string           `json:"sha"`
	ImageTag        string           `json:"image_tag"`
	Branch          string           `json:"branch"`
	CommitMessage   string           `json:"commit_message"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      *time.Time       `json:"finished_at,omitempty"`
	ExitCode        *int             `json:"exit_code,omitempty"`
	LogPath         string           `json:"log_path"`
	GitHubRunID     string           `json:"github_run_id"`
	GitHubRunNumber string           `json:"github_run_number"`
	GitHubActor     string           `json:"github_actor"`
	GitHubRepo      string           `json:"github_repository"`
	GitHubWorkflow  string           `json:"github_workflow"`
}

type DeployRequest struct {
	Ref             string `json:"ref"`
	SHA             string `json:"sha"`
	Branch          string `json:"branch"`
	ImageTag        string `json:"image_tag"`
	CommitMessage   string `json:"commit_message"`
	Confirmation    string `json:"confirmation"`
	GitHubRunID     string `json:"github_run_id"`
	GitHubRunNumber string `json:"github_run_number"`
	GitHubActor     string `json:"github_actor"`
	GitHubRepo      string `json:"github_repository"`
	GitHubWorkflow  string `json:"github_workflow"`
}

type DeployLock struct {
	ProjectID   string `json:"project_id"`
	OperationID string `json:"operation_id"`
	Operation   string `json:"operation"`
	StartedAt   string `json:"started_at"`
	SHA         string `json:"sha,omitempty"`
	ImageTag    string `json:"image_tag,omitempty"`
	Branch      string `json:"branch,omitempty"`
}
