package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

type deployHandle struct {
	cancel context.CancelFunc
	pid    int
}

type DeployService struct {
	db      *db.DB
	docker  *docker.Client
	audit   *AuditService
	cfg     *models.Config
	running sync.Map // map[string]deployHandle
}

func NewDeployService(database *db.DB, dockerClient *docker.Client, audit *AuditService, cfg *models.Config) *DeployService {
	return &DeployService{
		db:     database,
		docker: dockerClient,
		audit:  audit,
		cfg:    cfg,
	}
}

func (s *DeployService) Deploy(projectID string, req *models.DeployRequest) (*models.Deployment, error) {
	if !regexp.MustCompile(`^[a-z0-9_.-]+$`).MatchString(projectID) {
		return nil, fmt.Errorf("invalid project id format: %s", projectID)
	}
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}

	state, err := s.db.GetProjectState(projectID)
	if err != nil {
		return nil, fmt.Errorf("get project state: %w", err)
	}
	if paused, _ := state["paused"].(bool); paused {
		return nil, fmt.Errorf("deployments are paused for project %s", projectID)
	}

	// Best-effort registry login before deploy
	if pass, err := s.db.GetRegistryPassword(projectID); err == nil && pass != "" {
		s.docker.RegistryLogin(p.RegistryHost, p.RegistryUsername, pass)
	}

	existingLock, _ := s.db.GetLock(projectID)
	if existingLock != nil {
		return nil, fmt.Errorf("a %s operation is already in progress (operation: %s)", existingLock.Operation, existingLock.OperationID)
	}

	branch := normalizeRef(req.Branch)
	if branch == "" {
		branch = p.BranchName
	}
	ref := req.Ref
	sha := req.SHA
	imageTag := req.ImageTag
	if imageTag == "" && sha != "" {
		imageTag = fmt.Sprintf("sha-%s", sha)
	}
	if imageTag == "" && ref != "" {
		imageTag = normalizeRef(ref)
	}
	if imageTag == "" {
		return nil, fmt.Errorf("image_tag, ref, or sha is required for deployment")
	}

	tagRe := regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	if !tagRe.MatchString(imageTag) {
		return nil, fmt.Errorf("image tag must match Docker tag syntax")
	}

	deployID := fmt.Sprintf("deploy-%s-%d", projectID, time.Now().UnixMilli())
	logDir := filepath.Join(s.cfg.BaseDir, "Logs", projectID)
	os.MkdirAll(logDir, 0o750)
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	env := map[string]string{
		"DEPLOY_ID":           deployID,
		"DEPLOY_REF":          ref,
		"DEPLOY_SHA":          sha,
		"DEPLOY_BRANCH":       branch,
		"IMAGE_TAG":           imageTag,
		"PROJECT_ID":          projectID,
		"REPO_URL":            p.RepoURL,
		"BASE_DIR":            s.cfg.BaseDir,
		"GITHUB_RUN_ID":       req.GitHubRunID,
		"GITHUB_RUN_NUMBER":   req.GitHubRunNumber,
		"GITHUB_ACTOR":        req.GitHubActor,
		"GITHUB_REPOSITORY":   req.GitHubRepo,
		"GITHUB_WORKFLOW":     req.GitHubWorkflow,
		"COMMIT_MESSAGE":      req.CommitMessage,
	}

	// Inject user-configured env overrides from .env.template
	overrides := loadEnvOverrides(s.db, projectID)
	for k, v := range overrides {
		if _, exists := env[k]; !exists {
			env[k] = v
		}
	}

	deployment := &models.Deployment{
		ID:              deployID,
		ProjectID:       projectID,
		Kind:            models.DeploymentKindDeploy,
		Status:          models.DeploymentStatusRunning,
		Ref:             ref,
		SHA:             sha,
		ImageTag:        imageTag,
		Branch:          branch,
		CommitMessage:   req.CommitMessage,
		StartedAt:       time.Now().UTC(),
		LogPath:         logPath,
		GitHubRunID:     req.GitHubRunID,
		GitHubRunNumber: req.GitHubRunNumber,
		GitHubActor:     req.GitHubActor,
		GitHubRepo:      req.GitHubRepo,
		GitHubWorkflow:  req.GitHubWorkflow,
	}

	if err := s.db.CreateDeployment(deployment); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	lock := &models.DeployLock{
		ProjectID:   projectID,
		OperationID: deployID,
		Operation:   "deploy",
		StartedAt:   deployment.StartedAt.Format(time.RFC3339),
		SHA:         sha,
		ImageTag:    imageTag,
		Branch:      branch,
	}
	if err := s.db.CreateLock(lock); err != nil {
		// Lock acquisition failed — clean up deployment record
		_ = s.db.UpdateDeploymentStatus(deployID, models.DeploymentStatusFailed, nil, nil)
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	s.audit.Log("deploy_started", "running", projectID, fmt.Sprintf("deploy=%s ref=%s sha=%s", deployID, ref, sha), "")

	safeGo("runDeploy", func() { s.runDeploy(deployment, p, env) })

	return deployment, nil
}

func (s *DeployService) runDeploy(d *models.Deployment, p *models.Project, env map[string]string) {
	script := filepath.Join(s.cfg.ProjectRoot, "deploy", "project.sh")

	appDir := p.AppDir
	args := []string{script, p.ID, d.Branch, d.ImageTag}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = appDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	logFile, err := os.Create(d.LogPath)
	if err != nil {
		log.Printf("runDeploy: failed to create log file %s: %v — output captured in stderr", d.LogPath, err)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	} else {
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	s.running.Store(d.ID, deployHandle{cancel: cancel, pid: 0})
	err = cmd.Start()
	if err == nil {
		s.running.Store(d.ID, deployHandle{cancel: cancel, pid: cmd.Process.Pid})
		err = cmd.Wait()
	} else {
		cancel()
	}
	s.running.Delete(d.ID)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	finishedAt := time.Now().UTC()
	status := models.DeploymentStatusSuccess
	if exitCode != 0 {
		status = models.DeploymentStatusFailed
	}

	_ = s.db.UpdateDeploymentStatus(d.ID, status, &exitCode, &finishedAt)
	_ = s.db.ReleaseLock(p.ID)

	stateFile, err := s.db.GetProjectState(p.ID)
	if err != nil || stateFile == nil {
		stateFile = map[string]any{}
	}
	stateFile["last_deploy_status"] = string(status)
	stateFile["last_deploy_message"] = fmt.Sprintf("deploy %s completed with exit code %d", d.ID, exitCode)
	stateFile["last_run_at"] = finishedAt.Format(time.RFC3339)
	stateFile["active_deploy_id"] = ""
	stateFile["last_deployed_commit"] = d.SHA
	stateFile["last_deployed_image_tag"] = d.ImageTag
	_ = s.db.UpsertProjectState(p.ID, stateFile)

	s.audit.Log("deploy_finished", string(status), p.ID, fmt.Sprintf("deploy=%s exit_code=%d", d.ID, exitCode), "")
}

func (s *DeployService) Abort(projectID string) error {
	deployments, err := s.db.GetRunningDeployments(projectID)
	if err != nil {
		return err
	}
	for _, d := range deployments {
		if h, ok := s.running.Load(d.ID); ok {
			handle, ok := h.(deployHandle)
			if !ok {
				continue
			}
			handle.cancel()
			if handle.pid > 0 {
				syscall.Kill(-handle.pid, syscall.SIGKILL)
			}
		}
		now := time.Now().UTC()
		ec := -9
		_ = s.db.UpdateDeploymentStatus(d.ID, models.DeploymentStatusAborted, &ec, &now)
	}
	_ = s.db.ReleaseLock(projectID)

	// Also clean up the file-based lock used by deploy scripts
	lockDir := filepath.Join(s.cfg.DataDir, projectID, "deploy-lock")
	os.RemoveAll(lockDir)

	state, err := s.db.GetProjectState(projectID)
	if err != nil {
		return fmt.Errorf("abort: get project state: %w", err)
	}
	state["last_deploy_status"] = "aborted"
	state["last_deploy_message"] = "aborted by user"
	state["active_deploy_id"] = ""
	_ = s.db.UpsertProjectState(projectID, state)

	s.audit.Log("deploy_abort", "ok", projectID, fmt.Sprintf("aborted %d running deployments", len(deployments)), "")
	return nil
}

func (s *DeployService) GetLog(deployID string) (string, error) {
	deployment, err := s.db.GetDeployment(deployID)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(deployment.LogPath)
	if err != nil {
		return "", err
	}
	if info.Size() > 10*1024*1024 {
		return "", fmt.Errorf("log file exceeds 10MB limit")
	}
	data, err := os.ReadFile(deployment.LogPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// loadEnvOverrides reads user-configured env vars from project state
func loadEnvOverrides(database *db.DB, projectID string) map[string]string {
	state, err := database.GetProjectState(projectID)
	if err != nil || state == nil {
		return nil
	}
	raw, _ := state["metadata"].(string)
	if raw == "" {
		return nil
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil
	}
	env, _ := metadata["env_overrides"].(map[string]any)
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// SafeGo is the exported wrapper for safeGo so main can call it.
func SafeGo(name string, fn func()) { safeGo(name, fn) }

// safeGo runs fn in a goroutine with panic recovery.
// Panics are logged and the goroutine exits cleanly, preventing process crashes.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC in goroutine %s: %v", name, r)
			}
		}()
		fn()
	}()
}
