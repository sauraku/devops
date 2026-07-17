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
	DeploymentStatusPending         DeploymentStatus = "pending"
	DeploymentStatusPendingApproval DeploymentStatus = "pending_approval"
	DeploymentStatusRunning         DeploymentStatus = "running"
	DeploymentStatusSuccess         DeploymentStatus = "success"
	DeploymentStatusFailed          DeploymentStatus = "failed"
	DeploymentStatusAborted         DeploymentStatus = "aborted"
)

type DeploymentPhase string

const (
	DeploymentPhasePending        DeploymentPhase = "pending"
	DeploymentPhaseManualApproval DeploymentPhase = "manual_approval"
	DeploymentPhaseRunning        DeploymentPhase = "running"
	DeploymentPhaseTerminal       DeploymentPhase = "terminal"
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

type DeploymentApprovalRequest struct {
	Confirmation string `json:"confirmation"`
}

// DeploymentOperation is the stable, read-only completion contract returned to
// asynchronous callers. It intentionally omits server filesystem paths and
// other controller-only deployment details.
type DeploymentOperation struct {
	ID                     string           `json:"id"`
	ProjectID              string           `json:"project_id"`
	Kind                   DeploymentKind   `json:"kind"`
	Status                 DeploymentStatus `json:"status"`
	Phase                  DeploymentPhase  `json:"phase"`
	Terminal               bool             `json:"terminal"`
	Successful             bool             `json:"successful"`
	ManualApprovalRequired bool             `json:"manual_approval_required"`
	StartedAt              time.Time        `json:"started_at"`
	FinishedAt             *time.Time       `json:"finished_at,omitempty"`
	ExitCode               *int             `json:"exit_code,omitempty"`
}

func NewDeploymentOperation(d *Deployment) *DeploymentOperation {
	if d == nil {
		return nil
	}
	phase := DeploymentPhaseTerminal
	switch d.Status {
	case DeploymentStatusPending:
		phase = DeploymentPhasePending
	case DeploymentStatusPendingApproval:
		phase = DeploymentPhaseManualApproval
	case DeploymentStatusRunning:
		phase = DeploymentPhaseRunning
	}
	terminal := phase == DeploymentPhaseTerminal
	return &DeploymentOperation{
		ID:                     d.ID,
		ProjectID:              d.ProjectID,
		Kind:                   d.Kind,
		Status:                 d.Status,
		Phase:                  phase,
		Terminal:               terminal,
		Successful:             d.Status == DeploymentStatusSuccess,
		ManualApprovalRequired: d.Status == DeploymentStatusPendingApproval,
		StartedAt:              d.StartedAt,
		FinishedAt:             d.FinishedAt,
		ExitCode:               d.ExitCode,
	}
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
