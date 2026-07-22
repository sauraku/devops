package services

import (
	"context"
	"fmt"
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
	_ = s.db.ReleaseLock(lock.ProjectID, lock.OperationID)
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
		if ok, message := s.docker.RegistryLogin(p.RegistryHost, p.RegistryUsername, pass); !ok {
			return fmt.Errorf("registry login failed: %s", message)
		}
	}

	env, err := s.deploymentEnv(deployment, p)
	if err != nil {
		return err
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
			_ = s.db.ReleaseLock(p.ID, deployment.ID)
			if err != nil {
				return fmt.Errorf("queue approved deployment: %w", err)
			}
			return fmt.Errorf("deployment is no longer pending manual approval")
		}
	}

	s.audit.Log("deploy_started", "pending", p.ID,
		fmt.Sprintf("deploy=%s ref=%s sha=%s", deployment.ID, deployment.Ref, deployment.SHA), "")
	safeGo("runDeploy", func() { s.runDeploy(deployment, p, env) })
	return nil
}

func (s *DeployService) deploymentEnv(deployment *models.Deployment, p *models.Project) (map[string]string, error) {
	env := map[string]string{
		"DEPLOY_ID":         deployment.ID,
		"DEPLOY_REF":        deployment.Ref,
		"DEPLOY_SHA":        deployment.SHA,
		"DEPLOY_BRANCH":     deployment.Branch,
		"IMAGE_TAG":         deployment.ImageTag,
		"PROJECT_ID":        p.ID,
		"REPO_URL":          p.RepoURL,
		"REGISTRY_HOST":     p.RegistryHost,
		"BASE_DIR":          s.cfg.BaseDir,
		"PROJECT_LOG_DIR":   filepath.Join(s.cfg.BaseDir, "Logs", p.ID),
		"GITHUB_RUN_ID":     deployment.GitHubRunID,
		"GITHUB_RUN_NUMBER": deployment.GitHubRunNumber,
		"GITHUB_ACTOR":      deployment.GitHubActor,
		"GITHUB_REPOSITORY": deployment.GitHubRepo,
		"GITHUB_WORKFLOW":   deployment.GitHubWorkflow,
		"COMMIT_MESSAGE":    deployment.CommitMessage,
	}
	overrides, err := s.db.GetProjectEnvOverrides(p.ID)
	if err != nil {
		return nil, fmt.Errorf("load project environment: %w", err)
	}
	for key, value := range overrides {
		if _, exists := env[key]; !exists {
			env[key] = value
		}
	}
	return env, nil
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

func (s *DeployService) runDeploy(d *models.Deployment, p *models.Project, env map[string]string) {
	script := filepath.Join(s.cfg.ProjectRoot, "deploy", "project.sh")

	appDir := p.AppDir
	args := []string{script, p.ID, d.Branch, d.ImageTag}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = appDir
	cmd.Env = deploymentProcessEnv(env)

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
	_ = s.db.ReleaseLock(p.ID, d.ID)
	if !completed {
		return
	}

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

// deploymentProcessEnv starts deployments with a deliberately narrow process
// environment. The controller's Docker auth location is required for private
// registry pulls, so it must survive that narrowing. Project environment
// overrides must never replace command resolution, Docker selection, or
// project-control paths used by deploy/project.sh.
func deploymentProcessEnv(deploymentEnv map[string]string) []string {
	processEnv := map[string]string{
		"PATH": os.Getenv("PATH"),
		"HOME": os.Getenv("HOME"),
	}
	for _, key := range []string{"DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT"} {
		if value := os.Getenv(key); value != "" {
			processEnv[key] = value
		}
	}
	for key, value := range deploymentEnv {
		if docker.IsReservedCommandEnvKey(key) {
			continue
		}
		processEnv[key] = value
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
		_ = s.db.ReleaseLock(projectID, deployments[0].ID)
	}

	// Clean up the file lock after the owning process has been signalled.
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
