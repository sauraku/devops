package models

import "time"

type DeploymentMode string

const (
	DeploymentModeLocalRepo    DeploymentMode = "local_repo"
	DeploymentModeComposeImage DeploymentMode = "compose_image"
)

type Project struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	RepoURL          string         `json:"repo_url"`
	BranchName       string         `json:"branch_name"`
	DeploymentMode   DeploymentMode `json:"deployment_mode"`
	AutoApply        bool           `json:"auto_apply"`
	RegistryHost     string         `json:"registry_host"`
	RegistryUsername string         `json:"registry_username"`
	RunnerContainer  string         `json:"runner_container"`
	RunnerStatus     string         `json:"runner_status"`
	AppDir           string         `json:"app_dir"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

type ProjectRequest struct {
	ID               *string `json:"id"`
	Name             *string `json:"name"`
	RepoURL          *string `json:"repo_url"`
	BranchName       *string `json:"branch_name"`
	DeploymentMode   *string `json:"deployment_mode"`
	AutoApply        *bool   `json:"auto_apply"`
	RunnerToken      *string `json:"runner_token"`
	ListenerActive   *bool   `json:"listener_active"`
	RegistryHost     *string `json:"registry_host"`
	RegistryUsername *string `json:"registry_username"`
	RegistryPassword *string `json:"registry_password"`
	AppDir           *string `json:"app_dir"`
}
