package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// ReconcileLocks preserves operations that are still running after a control
// process restart. Dead owners are failed and released; live owners retain the
// lock until their process exits, then require explicit outcome verification.
func (s *DeployService) ReconcileLocks() {
	locks, err := s.db.ListLocks()
	if err != nil {
		log.Printf("reconcile locks: %v", err)
		return
	}
	for _, lock := range locks {
		pid := readLockPID(filepath.Join(s.cfg.DataDir, lock.ProjectID, "deploy-lock", "info"))
		if pid > 1 && processAlive(pid) {
			log.Printf("Preserving live %s lock for project %s (pid=%d)", lock.Operation, lock.ProjectID, pid)
			lockCopy := *lock
			safeGo("reconcileLock", func() {
				for processAlive(pid) {
					time.Sleep(2 * time.Second)
				}
				s.failInterruptedOperation(&lockCopy)
			})
			continue
		}
		s.failInterruptedOperation(lock)
	}
}

func (s *DeployService) failInterruptedOperation(lock *models.DeployLock) {
	now := time.Now().UTC()
	exitCode := -1
	_ = s.db.UpdateDeploymentStatus(lock.OperationID, models.DeploymentStatusFailed, &exitCode, &now)
	if err := s.db.ReleaseLock(lock.ProjectID, lock.OperationID); err != nil {
		log.Printf("reconcile lock release for project %s operation %s: %v", lock.ProjectID, lock.OperationID, err)
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.DataDir, lock.ProjectID, "deploy-lock"))
	s.audit.Log("operation_interrupted", "failed", lock.ProjectID,
		fmt.Sprintf("operation=%s id=%s controller ownership was interrupted; verify external effects", lock.Operation, lock.OperationID), "")
}

func readLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "pid=") {
			pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid=")))
			return pid
		}
	}
	return 0
}

func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func (s *DeployService) Deploy(projectID string, req *models.DeployRequest) (*models.Deployment, error) {
	p, normalized, err := s.prepareRequest(projectID, req)
	if err != nil {
		return nil, err
	}
	deployment, err := s.createDeployment(p, normalized, models.DeploymentStatusPending)
	if err != nil {
		return nil, err
	}
	if err := s.startDeployment(deployment, p, models.DeploymentStatusPending); err != nil {
		now := time.Now().UTC()
		exitCode := -1
		_ = s.db.UpdateDeploymentStatus(deployment.ID, models.DeploymentStatusFailed, &exitCode, &now)
		return nil, err
	}
	return deployment, nil
}

func (s *DeployService) RequestApproval(projectID string, req *models.DeployRequest) (*models.Deployment, error) {
	p, normalized, err := s.prepareRequest(projectID, req)
	if err != nil {
		return nil, err
	}
	deployment, err := s.createDeployment(p, normalized, models.DeploymentStatusPendingApproval)
	if err != nil {
		return nil, err
	}
	s.audit.Log("deploy_pending_approval", "pending", projectID,
		fmt.Sprintf("deploy=%s ref=%s sha=%s", deployment.ID, deployment.Ref, deployment.SHA), "")
	return deployment, nil
}

func (s *DeployService) Approve(projectID, deployID string) (*models.Deployment, error) {
	deployment, err := s.GetDeployment(projectID, deployID)
	if err != nil {
		return nil, err
	}
	if deployment.Kind != models.DeploymentKindDeploy {
		return nil, fmt.Errorf("operation is not a deployment")
	}
	if deployment.Status != models.DeploymentStatusPendingApproval {
		return nil, fmt.Errorf("deployment is not pending manual approval")
	}
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}
	request := &models.DeployRequest{
		Ref:             deployment.Ref,
		SHA:             deployment.SHA,
		Branch:          deployment.Branch,
		ImageTag:        deployment.ImageTag,
		CommitMessage:   deployment.CommitMessage,
		GitHubRunID:     deployment.GitHubRunID,
		GitHubRunNumber: deployment.GitHubRunNumber,
		GitHubActor:     deployment.GitHubActor,
		GitHubRepo:      deployment.GitHubRepo,
		GitHubWorkflow:  deployment.GitHubWorkflow,
	}
	if _, err := ValidateDeployRequest(p, request, deployment.GitHubRunID != ""); err != nil {
		return nil, fmt.Errorf("deployment no longer matches project configuration: %w", err)
	}
	if err := s.startDeployment(deployment, p, models.DeploymentStatusPendingApproval); err != nil {
		return nil, err
	}
	deployment.Status = models.DeploymentStatusPending
	s.audit.Log("deploy_approved", "pending", projectID, fmt.Sprintf("deploy=%s", deployment.ID), "")
	return deployment, nil
}

func (s *DeployService) GetDeployment(projectID, deployID string) (*models.Deployment, error) {
	deployment, err := s.db.GetDeployment(deployID)
	if err != nil {
		return nil, err
	}
	if deployment.ProjectID != projectID {
		return nil, fmt.Errorf("deployment does not belong to project")
	}
	return deployment, nil
}

func (s *DeployService) prepareRequest(projectID string, req *models.DeployRequest) (*models.Project, *models.DeployRequest, error) {
	if !regexp.MustCompile(`^[a-z0-9_.-]+$`).MatchString(projectID) {
		return nil, nil, fmt.Errorf("invalid project id format: %s", projectID)
	}
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("project not found: %s", projectID)
	}
	normalized, err := ValidateDeployRequest(p, req, false)
	if err != nil {
		return nil, nil, err
	}
	return p, normalized, nil
}

func (s *DeployService) createDeployment(p *models.Project, req *models.DeployRequest, status models.DeploymentStatus) (*models.Deployment, error) {
	deployID := fmt.Sprintf("deploy-%s-%d", p.ID, time.Now().UnixNano())
	logDir := filepath.Join(s.cfg.BaseDir, "Logs", p.ID)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, fmt.Errorf("create deployment log directory: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	deployment := &models.Deployment{
		ID:              deployID,
		ProjectID:       p.ID,
		Kind:            models.DeploymentKindDeploy,
		Status:          status,
		Ref:             req.Ref,
		SHA:             req.SHA,
		ImageTag:        req.ImageTag,
		Branch:          req.Branch,
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
	return deployment, nil
}

func (s *DeployService) startDeployment(deployment *models.Deployment, p *models.Project, expectedStatus models.DeploymentStatus) error {
	var registryAuth *docker.RegistryAuth
	registryAuthHandedOff := false
	defer func() {
		if registryAuth != nil && !registryAuthHandedOff {
			if err := registryAuth.Close(); err != nil {
				log.Printf("remove isolated registry credentials for %s: %v", p.ID, err)
			}
		}
	}()

	state, err := s.db.GetProjectState(p.ID)
	if err != nil {
		return fmt.Errorf("get project state: %w", err)
	}
	if paused, _ := state["paused"].(bool); paused {
		return fmt.Errorf("deployments are paused for project %s", p.ID)
	}

	pass, err := s.db.GetRegistryPassword(p.ID)
	if err != nil {
		return fmt.Errorf("load registry credentials: %w", err)
	}
	if pass != "" {
		var ok bool
		var message string
		registryAuth, ok, message = s.docker.RegistryLoginIsolated(p.RegistryHost, p.RegistryUsername, pass)
		if !ok {
			return fmt.Errorf("registry login failed: %s", message)
		}
	}

	controllerEnv, projectOverrides, err := s.deploymentEnv(deployment, p)
	if err != nil {
		return err
	}
	if registryAuth != nil {
		controllerEnv["DOCKER_CONFIG"] = registryAuth.ConfigDir()
	}
	lock := &models.DeployLock{
		ProjectID:   p.ID,
		OperationID: deployment.ID,
		Operation:   "deploy",
		StartedAt:   deployment.StartedAt.Format(time.RFC3339),
		SHA:         deployment.SHA,
		ImageTag:    deployment.ImageTag,
		Branch:      deployment.Branch,
	}
	if err := s.db.CreateLock(lock); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if expectedStatus == models.DeploymentStatusPendingApproval {
		transitioned, err := s.db.TransitionDeploymentStatus(deployment.ID, expectedStatus, models.DeploymentStatusPending)
		if err != nil || !transitioned {
			if releaseErr := s.db.ReleaseLock(p.ID, deployment.ID); releaseErr != nil {
				log.Printf("release unqueued deployment lock for project %s operation %s: %v", p.ID, deployment.ID, releaseErr)
			}
			if err != nil {
				return fmt.Errorf("queue approved deployment: %w", err)
			}
			return fmt.Errorf("deployment is no longer pending manual approval")
		}
	}

	s.audit.Log("deploy_started", "pending", p.ID,
		fmt.Sprintf("deploy=%s ref=%s sha=%s", deployment.ID, deployment.Ref, deployment.SHA), "")
	safeGo("runDeploy", func() {
		if registryAuth != nil {
			defer func() {
				if err := registryAuth.Close(); err != nil {
					log.Printf("remove isolated registry credentials for %s: %v", p.ID, err)
				}
			}()
		}
		s.runDeploy(deployment, p, controllerEnv, projectOverrides)
	})
	registryAuthHandedOff = true
	return nil
}

func (s *DeployService) deploymentEnv(deployment *models.Deployment, p *models.Project) (map[string]string, map[string]string, error) {
	controllerBinary, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve controller executable: %w", err)
	}
	controllerEnv := map[string]string{
		"DEPLOY_ID":          deployment.ID,
		"DEPLOY_REF":         deployment.Ref,
		"DEPLOY_SHA":         deployment.SHA,
		"DEPLOY_BRANCH":      deployment.Branch,
		"IMAGE_TAG":          deployment.ImageTag,
		"PROJECT_ID":         p.ID,
		"REPO_URL":           p.RepoURL,
		"REGISTRY_HOST":      p.RegistryHost,
		"BASE_DIR":           s.cfg.BaseDir,
		"PROJECT_LOG_DIR":    filepath.Join(s.cfg.BaseDir, "Logs", p.ID),
		"GITHUB_RUN_ID":      deployment.GitHubRunID,
		"GITHUB_RUN_NUMBER":  deployment.GitHubRunNumber,
		"GITHUB_ACTOR":       deployment.GitHubActor,
		"GITHUB_REPOSITORY":  deployment.GitHubRepo,
		"GITHUB_WORKFLOW":    deployment.GitHubWorkflow,
		"COMMIT_MESSAGE":     deployment.CommitMessage,
		"DEVOPS_CONTROL_BIN": controllerBinary,
	}
	overrides, err := s.db.GetProjectEnvOverrides(p.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("load project environment: %w", err)
	}
	return controllerEnv, overrides, nil
}

var (
	dockerTagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	commitSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	digitsRE    = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// ValidateDeployRequest binds a deployment to the branch configured for its
// project. Scoped runner credentials additionally require complete GitHub
// callback provenance, so they cannot be reused as general deployment tokens.
// The returned request is a normalized copy; the caller's request is unchanged.
func ValidateDeployRequest(p *models.Project, req *models.DeployRequest, requireGitHubProvenance bool) (*models.DeployRequest, error) {
	if p == nil || req == nil {
		return nil, fmt.Errorf("project and deployment request are required")
	}

	normalized := *req
	configuredBranch := normalizeRef(p.BranchName)
	if configuredBranch == "" {
		return nil, fmt.Errorf("project %s has no deployment branch configured", p.ID)
	}
	normalized.Branch = normalizeRef(normalized.Branch)
	if normalized.Branch == "" {
		normalized.Branch = configuredBranch
	}
	if normalized.Branch != configuredBranch {
		return nil, fmt.Errorf("deployment branch %q does not match configured branch %q", normalized.Branch, configuredBranch)
	}
	if normalized.Ref != "" && normalizeRef(normalized.Ref) != configuredBranch {
		return nil, fmt.Errorf("deployment ref %q does not match configured branch %q", normalized.Ref, configuredBranch)
	}

	hasGitHubMetadata := normalized.GitHubRunID != "" ||
		normalized.GitHubRunNumber != "" ||
		normalized.GitHubActor != "" ||
		normalized.GitHubRepo != "" ||
		normalized.GitHubWorkflow != ""
	if requireGitHubProvenance || hasGitHubMetadata {
		if err := validateGitHubDeployRequest(p, &normalized, configuredBranch); err != nil {
			return nil, err
		}
	}

	if normalized.ImageTag == "" && normalized.SHA != "" {
		normalized.ImageTag = "sha-" + normalized.SHA
	}
	if normalized.ImageTag == "" && normalized.Ref != "" {
		normalized.ImageTag = normalizeRef(normalized.Ref)
	}
	if normalized.ImageTag == "" {
		return nil, fmt.Errorf("image_tag, ref, or sha is required for deployment")
	}
	if !dockerTagRE.MatchString(normalized.ImageTag) {
		return nil, fmt.Errorf("image tag must match Docker tag syntax")
	}

	return &normalized, nil
}

func validateGitHubDeployRequest(p *models.Project, req *models.DeployRequest, configuredBranch string) error {
	expectedRepo := repoOwnerRepo(p.RepoURL)
	if expectedRepo == "" || !strings.EqualFold(strings.TrimSpace(req.GitHubRepo), expectedRepo) {
		return fmt.Errorf("GitHub repository %q does not match configured repository %q", req.GitHubRepo, expectedRepo)
	}
	if req.Ref != "refs/heads/"+configuredBranch {
		return fmt.Errorf("GitHub ref %q does not match configured branch %q", req.Ref, configuredBranch)
	}
	if !commitSHARe.MatchString(req.SHA) {
		return fmt.Errorf("GitHub deployment SHA must be a full 40-character hexadecimal commit SHA")
	}
	if req.ImageTag != "sha-"+req.SHA {
		return fmt.Errorf("GitHub image tag must exactly match sha-<commit SHA>")
	}
	if !digitsRE.MatchString(req.GitHubRunID) || !digitsRE.MatchString(req.GitHubRunNumber) {
		return fmt.Errorf("GitHub run ID and run number must be positive integers")
	}
	if strings.TrimSpace(req.GitHubActor) == "" || strings.TrimSpace(req.GitHubWorkflow) == "" {
		return fmt.Errorf("GitHub actor and workflow are required")
	}
	return nil
}

func (s *DeployService) runDeploy(d *models.Deployment, p *models.Project, controllerEnv, projectOverrides map[string]string) {
	script := filepath.Join(s.cfg.ProjectRoot, "deploy", "project.sh")

	appDir := p.AppDir
	args := []string{script, p.ID, d.Branch, d.ImageTag}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = appDir
	cmd.Env = deploymentProcessEnv(controllerEnv)

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
	overrideStream, err := startCommandWithProjectOverrides(cmd, projectOverrides)
	if err == nil {
		s.running.Store(d.ID, deployHandle{cancel: cancel, pid: cmd.Process.Pid})
		transitioned, transitionErr := s.db.TransitionDeploymentStatus(d.ID, models.DeploymentStatusPending, models.DeploymentStatusRunning)
		if transitionErr != nil || !transitioned {
			cancel()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			if transitionErr != nil {
				err = fmt.Errorf("mark deployment running: %w", transitionErr)
			} else {
				err = fmt.Errorf("deployment was cancelled before execution")
			}
		} else {
			err = cmd.Wait()
		}
		streamErr := overrideStream.finish()
		if err == nil && streamErr != nil {
			err = fmt.Errorf("stream project environment overrides: %w", streamErr)
		}
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

	completed, updateErr := s.db.CompleteActiveDeployment(d.ID, status, exitCode, finishedAt)
	if updateErr != nil {
		log.Printf("runDeploy: update deployment %s completion: %v", d.ID, updateErr)
	}
	if err := s.db.ReleaseLock(p.ID, d.ID); err != nil {
		log.Printf("release completed deployment lock for project %s operation %s: %v", p.ID, d.ID, err)
	}
	if !completed {
		return
	}

	if err := s.recordDeploymentCompletionState(p.ID, d, status, exitCode, finishedAt); err != nil {
		log.Printf("runDeploy: update project %s deployment state: %v", p.ID, err)
	}

	s.audit.Log("deploy_finished", string(status), p.ID, fmt.Sprintf("deploy=%s exit_code=%d", d.ID, exitCode), "")
}

// recordDeploymentCompletionState patches only fields owned by the deployment
// operation. In particular, pause and operator metadata may be changed while a
// deployment is running and must never be overwritten from a stale snapshot.
func (s *DeployService) recordDeploymentCompletionState(projectID string, d *models.Deployment, status models.DeploymentStatus, exitCode int, finishedAt time.Time) error {
	return s.db.UpsertProjectState(projectID, map[string]any{
		"last_deploy_status":      string(status),
		"last_deploy_message":     fmt.Sprintf("deploy %s completed with exit code %d", d.ID, exitCode),
		"last_run_at":             finishedAt.Format(time.RFC3339),
		"active_deploy_id":        "",
		"last_deployed_commit":    d.SHA,
		"last_deployed_image_tag": d.ImageTag,
	})
}

const projectEnvOverridesFDEnv = "DEVOPS_ENV_OVERRIDES_FD"

type projectEnvOverrideStream struct {
	writer *os.File
	done   <-chan error
}

// startCommandWithProjectOverrides starts cmd with project overrides on a
// private inherited descriptor. Writing starts only after the process exists,
// so payloads larger than the pipe buffer cannot deadlock cmd.Start.
func startCommandWithProjectOverrides(cmd *exec.Cmd, overrides map[string]string) (*projectEnvOverrideStream, error) {
	if overrides == nil {
		overrides = map[string]string{}
	}
	payload, err := json.Marshal(overrides)
	if err != nil {
		return nil, fmt.Errorf("encode project environment overrides: %w", err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create project environment override pipe: %w", err)
	}
	if err := writer.SetWriteDeadline(time.Time{}); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("configure project environment override pipe: %w", err)
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", projectEnvOverridesFDEnv, fd))
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	_ = reader.Close()

	done := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(writer, bytes.NewReader(payload))
		closeErr := writer.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		done <- writeErr
	}()
	return &projectEnvOverrideStream{writer: writer, done: done}, nil
}

// finish is called after cmd.Wait. Closing the writer here also releases a
// blocked write if a failed child left the inherited read descriptor unread.
func (s *projectEnvOverrideStream) finish() error {
	_ = s.writer.SetWriteDeadline(time.Now())
	_ = s.writer.Close()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-s.done:
		return err
	case <-timer.C:
		return fmt.Errorf("project environment override writer did not stop")
	}
}

var (
	deploymentControllerEnvKeys = envKeySet(
		"DEPLOY_ID", "DEPLOY_REF", "DEPLOY_SHA", "DEPLOY_BRANCH", "IMAGE_TAG",
		"PROJECT_ID", "REPO_URL", "REGISTRY_HOST", "BASE_DIR", "PROJECT_LOG_DIR",
		"GITHUB_RUN_ID", "GITHUB_RUN_NUMBER", "GITHUB_ACTOR", "GITHUB_REPOSITORY",
		"GITHUB_WORKFLOW", "COMMIT_MESSAGE", "DOCKER_CONFIG", "DEVOPS_CONTROL_BIN",
	)
	backupControllerEnvKeys = envKeySet(
		"DEPLOY_ID", "TARGET_BRANCH", "BACKUP_KIND", "BACKUP_REASON",
		"BACKUP_DIR_PATH", "BACKUP_MANIFEST_FILE", "ENV_FILE", "BASE_DIR",
		"PROJECT_ID", "PROJECT_DIR", "PROJECT_STATE_DIR", "PROJECT_LOG_DIR",
		"COMPOSE_PROJECT_NAME", "POSTGRES_CONTAINER",
	)
	restoreControllerEnvKeys = envKeySet(
		"DEPLOY_ID", "BACKUP_ID", "BACKUP_DIR_PATH", "BACKUP_MANIFEST_FILE",
		"ENV_FILE", "COMPOSE_FILE", "BASE_DIR", "PROJECT_ID", "PROJECT_DIR",
		"PROJECT_STATE_DIR", "PROJECT_LOG_DIR", "TARGET_BRANCH",
		"COMPOSE_PROJECT_NAME", "POSTGRES_CONTAINER", "RESTORE_COMMAND_JSON",
	)
	rollbackControllerEnvKeys = envKeySet(
		"DEPLOY_ID", "BASE_DIR", "PROJECT_ID", "REPO_URL", "REGISTRY_HOST",
		"PROJECT_LOG_DIR", "DOCKER_CONFIG",
	)
)

func envKeySet(keys ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}

// controllerProcessEnv is the single process-environment boundary for
// deploy/backup/restore/rollback scripts. Docker connection settings come only
// from the controller process. Operation maps are reduced to an exact
// per-operation allowlist so project-controlled command namespaces cannot be
// introduced accidentally.
func controllerProcessEnv(controllerEnv map[string]string, allowedKeys map[string]struct{}) []string {
	processEnv := map[string]string{
		"PATH": os.Getenv("PATH"),
		"HOME": os.Getenv("HOME"),
	}
	for key, value := range docker.TrustedDockerCommandEnv() {
		processEnv[key] = value
	}
	for key, value := range controllerEnv {
		if _, ok := allowedKeys[key]; ok {
			processEnv[key] = value
		}
	}

	keys := make([]string, 0, len(processEnv))
	for key := range processEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, fmt.Sprintf("%s=%s", key, processEnv[key]))
	}
	return result
}

func deploymentProcessEnv(controllerEnv map[string]string) []string {
	return controllerProcessEnv(controllerEnv, deploymentControllerEnvKeys)
}

func backupProcessEnv(controllerEnv map[string]string) []string {
	return controllerProcessEnv(controllerEnv, backupControllerEnvKeys)
}

func restoreProcessEnv(controllerEnv map[string]string) []string {
	return controllerProcessEnv(controllerEnv, restoreControllerEnvKeys)
}

func rollbackProcessEnv(controllerEnv map[string]string) []string {
	return controllerProcessEnv(controllerEnv, rollbackControllerEnvKeys)
}

func (s *DeployService) Abort(projectID string) error {
	deployments, err := s.db.GetActiveDeployments(projectID)
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
		if pid := readLockPID(filepath.Join(s.cfg.DataDir, projectID, "deploy-lock", "info")); pid > 1 && processAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		now := time.Now().UTC()
		ec := -9
		_ = s.db.UpdateDeploymentStatus(d.ID, models.DeploymentStatusAborted, &ec, &now)
	}
	if len(deployments) > 0 {
		if err := s.db.ReleaseLock(projectID, deployments[0].ID); err != nil {
			return fmt.Errorf("abort: release operation lock: %w", err)
		}
	}

	// Clean up the file lock after the owning process has been signalled.
	lockDir := filepath.Join(s.cfg.DataDir, projectID, "deploy-lock")
	os.RemoveAll(lockDir)

	if err := s.recordDeploymentAbortState(projectID); err != nil {
		return fmt.Errorf("abort: update project state: %w", err)
	}

	s.audit.Log("deploy_abort", "ok", projectID, fmt.Sprintf("aborted %d running deployments", len(deployments)), "")
	return nil
}

// recordDeploymentAbortState patches only fields owned by abort. Pause state
// belongs to the operator and may be updated concurrently with this operation.
func (s *DeployService) recordDeploymentAbortState(projectID string) error {
	return s.db.UpsertProjectState(projectID, map[string]any{
		"last_deploy_status":  "aborted",
		"last_deploy_message": "aborted by user",
		"active_deploy_id":    "",
	})
}

func (s *DeployService) GetLog(projectID, deployID string) (string, error) {
	deployment, err := s.GetDeployment(projectID, deployID)
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
