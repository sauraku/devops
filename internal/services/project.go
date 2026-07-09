package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

type ProjectService struct {
	db     *db.DB
	docker *docker.Client
	audit  *AuditService
	cfg    *models.Config
}

func NewProjectService(database *db.DB, dockerClient *docker.Client, audit *AuditService, cfg *models.Config) *ProjectService {
	return &ProjectService{
		db:     database,
		docker: dockerClient,
		audit:  audit,
		cfg:    cfg,
	}
}

func (s *ProjectService) Docker() *docker.Client { return s.docker }

func (s *ProjectService) List() ([]*models.Project, error) {
	return s.db.ListProjects()
}

func (s *ProjectService) Get(id string) (*models.Project, error) {
	return s.db.GetProject(id)
}

func (s *ProjectService) SlugID(raw string) string {
	slug := strings.ToLower(raw)
	re := regexp.MustCompile(`[^a-z0-9_.-]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, ".-")
	if slug == "" {
		return ""
	}
	if !isAlphanumericStart(slug) {
		slug = "project-" + slug
	}
	return slug
}

func isAlphanumericStart(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func (s *ProjectService) CreateOrUpdate(req *models.ProjectRequest) (*models.Project, error) {
	if req.ID == nil || *req.ID == "" {
		return nil, fmt.Errorf("project id is required")
	}
	projectID := s.SlugID(*req.ID)
	if projectID == "" {
		return nil, fmt.Errorf("project id must contain at least one letter or number")
	}

	existing, _ := s.db.GetProject(projectID)
	p := &models.Project{
		ID:               projectID,
		Name:             projectID,
		RepoURL:          "",
		BranchName:       "rc",
		DeploymentMode:   models.DeploymentModeComposeImage,
		AutoApply:        true,
		RegistryHost:     "ghcr.io",
		RunnerContainer:  fmt.Sprintf("devops-runner-%s", projectID),
		RunnerStatus:     "unknown",
	}
	if existing != nil {
		p = existing
	}

	if req.Name != nil {
		p.Name = strings.TrimSpace(*req.Name)
	}
	if req.RepoURL != nil {
		p.RepoURL = strings.TrimSpace(*req.RepoURL)
	}
	if req.BranchName != nil {
		p.BranchName = normalizeRef(strings.TrimSpace(*req.BranchName))
	}
	if req.DeploymentMode != nil {
		mode := models.DeploymentMode(strings.TrimSpace(*req.DeploymentMode))
		if mode != models.DeploymentModeLocalRepo && mode != models.DeploymentModeComposeImage {
			return nil, fmt.Errorf("deployment_mode must be one of: local_repo, compose_image")
		}
		p.DeploymentMode = mode
	}
	if req.AutoApply != nil {
		p.AutoApply = *req.AutoApply
	}
	if req.RegistryHost != nil {
		p.RegistryHost = strings.TrimSpace(*req.RegistryHost)
	}
	if req.RegistryUsername != nil {
		p.RegistryUsername = strings.TrimSpace(*req.RegistryUsername)
	}
	if req.AppDir != nil {
		p.AppDir = strings.TrimSpace(*req.AppDir)
	}
	if p.AppDir == "" {
		p.AppDir = filepath.Join(s.cfg.BaseDir, "Projects", p.ID)
	}
	// Resolve and clean the path to prevent traversal via ../..
	p.AppDir = filepath.Clean(p.AppDir)
	projectsRoot := filepath.Clean(filepath.Join(s.cfg.BaseDir, "Projects")) + string(filepath.Separator)
	if !strings.HasPrefix(p.AppDir+string(filepath.Separator), projectsRoot) {
		return nil, fmt.Errorf("app_dir must be within %s", filepath.Join(s.cfg.BaseDir, "Projects"))
	}

	// Auto-copy .env.template from common source locations
	s.ensureEnvTemplate(p)

	if err := s.validateRepoURL(p.RepoURL); err != nil {
		return nil, err
	}
	if err := s.db.UpsertProject(p); err != nil {
		return nil, fmt.Errorf("save project: %w", err)
	}

	if req.RegistryPassword != nil && *req.RegistryPassword != "" {
		if p.RegistryUsername == "" {
			return nil, fmt.Errorf("registry username is required when providing a password")
		}
		_ = s.db.SaveRegistryPassword(p.ID, *req.RegistryPassword)
		// Best-effort registry login — failure is non-fatal, can be retried from Settings
		ok, msg := s.docker.RegistryLogin(p.RegistryHost, p.RegistryUsername, *req.RegistryPassword)
		if !ok {
			s.audit.Log("registry_login_warn", "warning", p.ID,
				fmt.Sprintf("registry login failed, password saved: %s", msg), "")
		}
	}

	runnerToken := ""
	if req.RunnerToken != nil {
		runnerToken = strings.TrimSpace(*req.RunnerToken)
	}
	if runnerToken == "" && s.cfg.GithubToken != "" {
		runnerToken = s.cfg.GithubToken
	}
	listenerActive := false
	if req.ListenerActive != nil {
		listenerActive = *req.ListenerActive
	}

	if runnerToken != "" && listenerActive {
		p.RunnerStatus = "starting"
		safeGo("startRunner", func() {
			if err := s.startRunner(p, runnerToken); err != nil {
				s.audit.Log("runner_start", "error", p.ID, err.Error(), "")
				_ = s.db.SaveRunnerStatus(p.ID, "error")
			} else {
				_ = s.db.SaveRunnerStatus(p.ID, "active")
			}
		})
	} else if !listenerActive {
		s.stopRunner(p)
		p.RunnerStatus = "stopped"
	}

	if err := s.db.UpsertProject(p); err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	s.audit.Log("project_upsert", "ok", p.ID, fmt.Sprintf("repo=%s branch=%s", p.RepoURL, p.BranchName), "")
	return p, nil
}

func (s *ProjectService) Delete(projectID string) error {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return fmt.Errorf("project not found: %s", projectID)
	}
	s.stopRunner(p)

	projectDir := filepath.Join(s.cfg.BaseDir, "Projects", projectID)
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	if _, err := os.Stat(composeFile); err == nil {
		envFiles, _ := filepath.Glob(filepath.Join(projectDir, ".env.*"))
		for _, ef := range envFiles {
			if strings.HasSuffix(ef, ".env.template") {
				continue
			}
			suffix := strings.TrimPrefix(filepath.Base(ef), ".env.")
			if suffix != "" {
				composeProject := fmt.Sprintf("%s-%s", p.ID, suffix)
				_ = s.docker.ComposeDown(composeFile, composeProject, nil)
			}
		}
	}

	// Force-remove any remaining app containers for this project
	containers, _ := s.docker.ListContainers(fmt.Sprintf("name=%s-", p.ID))
	for _, c := range containers {
		s.docker.RemoveContainer(c)
	}

	s.audit.Log("project_delete", "ok", projectID, "Project deleted and cleaned up.", "")

	// Clean up data dirs
	os.RemoveAll(filepath.Join(s.cfg.BaseDir, "State", projectID))
	os.RemoveAll(filepath.Join(s.cfg.BaseDir, "Releases", projectID))
	os.RemoveAll(filepath.Join(s.cfg.BaseDir, "Logs", projectID))
	os.RemoveAll(filepath.Join(s.cfg.BaseDir, "Projects", projectID))
	// Clean up legacy project dir at project root level
	os.RemoveAll(filepath.Join(s.cfg.ProjectRoot, "Projects", projectID))

	return s.db.DeleteProject(projectID)
}

func (s *ProjectService) Status(projectID string) (*models.ProjectStatus, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	state, _ := s.db.GetProjectState(projectID)
	lock, _ := s.db.GetLock(projectID)

	runnerSummary := s.docker.ContainerSummary(p.RunnerContainer)

	// Sync runner_status with actual Docker state
	if runnerSummary.Running {
		p.RunnerStatus = "active"
	} else if runnerSummary.Exists {
		p.RunnerStatus = runnerSummary.State
	} else {
		p.RunnerStatus = "unavailable"
	}
	_ = s.db.SaveRunnerStatus(p.ID, p.RunnerStatus)

	services := s.composeServices(p)
	containers := map[string]map[string]string{
		"current": make(map[string]string),
	}
	health := make(map[string]*models.ServiceHealth)

	for _, svc := range services {
		containerName := DeploymentContainerName(svc, p.BranchName, p.ID)
		summary := s.docker.ContainerSummary(containerName)
		containers["current"][svc] = summary.State
		health[svc] = s.CheckServiceHealth(svc, containerName, summary)
	}

	deployments, _ := s.db.ListDeployments(projectID, 10)
	backups, _ := s.db.ListBackups(projectID, 10)

	return &models.ProjectStatus{
		Project:     p,
		State:       state,
		Lock:        lock,
		Runner:      map[string]string{"container": p.RunnerContainer, "state": runnerSummary.State},
		Containers:  containers,
		Health:      health,
		Deployments: deployments,
		Backups:     backups,
		Capabilities: s.capabilities(p),
		ServerTime:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (s *ProjectService) capabilities(p *models.Project) map[string]bool {
	isLocal := p.DeploymentMode == models.DeploymentModeLocalRepo
	return map[string]bool{
		"deploy":          true,
		"config":          true,
		"logs":            true,
		"backup_verify":   true,
		"rollback":        isLocal,
		"pause":           isLocal,
		"resume":          isLocal,
		"backup_create":   true,
		"restore_dry_run": true,
	}
}

func (s *ProjectService) validateRepoURL(url string) error {
	if url == "" {
		return nil
	}
	if !strings.HasPrefix(url, "https://github.com/") {
		return fmt.Errorf("only GitHub HTTPS URLs are supported")
	}
	if len(s.cfg.AllowedRepoPrefixes) > 0 {
		allowed := false
		for _, prefix := range s.cfg.AllowedRepoPrefixes {
			if strings.HasPrefix(url, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("repository URL is not in the allowed prefixes list")
		}
	}
	return nil
}

func (s *ProjectService) startRunner(p *models.Project, runnerToken string) error {
	homeDir, _ := os.UserHomeDir()
	sshDir := filepath.Join(homeDir, ".ssh")
	// Generate scoped runner token to avoid leaking the master deploy token
	mac := hmac.New(sha256.New, []byte(s.cfg.Token))
	mac.Write([]byte("runner:" + p.ID))
	scopedToken := hex.EncodeToString(mac.Sum(nil))
	env := map[string]string{
		"REPO_URL":              p.RepoURL,
		"RUNNER_TOKEN":          runnerToken,
		"RUNNER_NAME":           fmt.Sprintf("runner-%s-%s", p.ID, branchSlug(p.BranchName)),
		"RUNNER_CONTAINER_NAME": p.RunnerContainer,
		"RUNNER_STATE_VOLUME":   fmt.Sprintf("%s-state", p.RunnerContainer),
		"RUNNER_LABELS":         runnerLabels(p),
		"DEPLOY_BRANCH":         p.BranchName,
		"DEPLOY_BRANCH_SLUG":    branchSlug(p.BranchName),
		"REPO_HOST_DIR":         s.cfg.BaseDir,
		"BASE_DIR":              s.cfg.BaseDir,
		"SSH_DIR":               sshDir,
		"DEPLOY_CONTROL_TOKEN":  scopedToken,
	}

	s.docker.RemoveContainer(p.RunnerContainer)
	composeFile := filepath.Join(s.cfg.ProjectRoot, "deploy", "runner", "docker-compose.runner.yml")
	if err := s.docker.ComposeUp(composeFile, fmt.Sprintf("devops-runner-%s", p.ID), env, "github-runner"); err != nil {
		return err
	}

	ready, state, logs := s.docker.WaitForRunnerReady(p.RunnerContainer, 90*time.Second)
	if !ready {
		s.docker.RemoveContainer(p.RunnerContainer)
		return fmt.Errorf("runner did not become ready (state=%s): %s", state, truncateStr(logs, 200))
	}
	return nil
}

func (s *ProjectService) stopRunner(p *models.Project) {
	s.docker.RemoveContainer(p.RunnerContainer)
	_ = s.docker.ComposeDown(
		filepath.Join(s.cfg.ProjectRoot, "deploy", "runner", "docker-compose.runner.yml"),
		fmt.Sprintf("devops-runner-%s", p.ID),
		nil,
	)
}

// StopAllRunners stops all runner containers (called on server shutdown).
// Does NOT touch application containers — they persist across restarts.
func (s *ProjectService) StopAllRunners() {
	projects, err := s.db.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		if p.RunnerContainer != "" {
			s.docker.RemoveContainer(p.RunnerContainer)
		}
	}
}

func (s *ProjectService) UpdateProjectState(projectID string, state map[string]any) error {
	return s.db.UpsertProjectState(projectID, state)
}

// ReconcileContainers checks all projects and restarts containers that are
// in a stopped state but should be running. Runs once on server startup.
func (s *ProjectService) ReconcileContainers() {
	projects, err := s.db.ListProjects()
	if err != nil {
		log.Printf("Reconcile: failed to list projects: %v", err)
		return
	}
	for _, p := range projects {
		services := s.composeServices(p)
		for _, svc := range services {
			containerName := DeploymentContainerName(svc, p.BranchName, p.ID)
			summary := s.docker.ContainerSummary(containerName)
			if !summary.Exists {
				log.Printf("Reconcile: container %s does not exist (project %s, service %s) — skipping", containerName, p.ID, svc)
				continue
			}
			if summary.Running {
				continue
			}
			log.Printf("Reconcile: restarting stopped container %s (state=%s)", containerName, summary.State)
			if err := s.docker.StartContainer(containerName); err != nil {
				log.Printf("Reconcile: failed to restart %s: %v", containerName, err)
			}
		}
	}
}

// SeedGithubToken stores GITHUB_TOKEN as the runner token for a project
// if the project exists and doesn't already have a stored token.
// Starts the runner if AutoApply is enabled and the runner is not already active.
func (s *ProjectService) SeedGithubToken(projectID, token string) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		log.Printf("SeedGithubToken: project %s not found, skipping", projectID)
		return
	}

	existing, _ := s.db.GetRegistryPassword(projectID)
	if existing != "" {
		log.Printf("SeedGithubToken: project %s already has a stored token, skipping", projectID)
		return
	}

	if err := s.db.SaveRegistryPassword(projectID, token); err != nil {
		log.Printf("SeedGithubToken: failed to save token for %s: %v", projectID, err)
		return
	}
	log.Printf("SeedGithubToken: saved GITHUB_TOKEN for project %s", projectID)

	if p.AutoApply && p.RunnerStatus != "active" {
		log.Printf("SeedGithubToken: auto-starting runner for project %s", projectID)
		if err := s.startRunner(p, token); err != nil {
			log.Printf("SeedGithubToken: failed to start runner for %s: %v", projectID, err)
			p.RunnerStatus = "error"
		} else {
			p.RunnerStatus = "active"
		}
		_ = s.db.UpsertProject(p)
	}
}

func (s *ProjectService) SaveRegistryPassword(projectID, password string) error {
	return s.db.SaveRegistryPassword(projectID, password)
}

func (s *ProjectService) GetRegistryPassword(projectID string) (string, error) {
	return s.db.GetRegistryPassword(projectID)
}

// EnvVar describes a single variable from .env.template
type EnvVar struct {
	Key      string `json:"key"`
	Default  string `json:"default"`
	IsSecret bool   `json:"is_secret"`
}

// ensureEnvTemplate attempts to place .env.template in the project directory.
// Tries config image extraction first, falls back to local checkout.
func (s *ProjectService) ensureEnvTemplate(p *models.Project) {
	os.MkdirAll(p.AppDir, 0o750)

	templatePath := filepath.Join(p.AppDir, ".env.template")
	if _, err := os.Stat(templatePath); err == nil {
		return // already exists
	}

	// Try extracting from config image (works in both dev and production)
	configImage := fmt.Sprintf("%s/%s-deploy-config:%s",
		p.RegistryHost, repoOwnerRepo(p.RepoURL), p.BranchName)
	if out, err := s.extractFromConfigImage(configImage, p.AppDir); err == nil {
		log.Printf("ensureEnvTemplate: extracted template from %s", configImage)
		return
	} else {
		log.Printf("ensureEnvTemplate: config image %s: %v (output: %s)", configImage, err, out)
	}

	// Fallback: local checkout (dev only)
	home, _ := os.UserHomeDir()
	src := filepath.Join(home, "Documents", p.ID, ".env.template")
	if data, err := os.ReadFile(src); err == nil {
		os.WriteFile(templatePath, data, 0o644)
	}
}

func (s *ProjectService) extractFromConfigImage(image, destDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pullCmd := exec.CommandContext(ctx, "docker", "pull", image)
	pullOut, pullErr := pullCmd.CombinedOutput()
	if pullErr != nil {
		return string(pullOut), pullErr
	}

	createCmd := exec.CommandContext(ctx, "docker", "create", image)
	createOut, createErr := createCmd.CombinedOutput()
	if createErr != nil {
		return string(createOut), createErr
	}
	containerID := strings.TrimSpace(string(createOut))

	defer exec.Command("docker", "rm", "-f", containerID).Run()

	cpCmd := exec.CommandContext(ctx, "docker", "cp",
		fmt.Sprintf("%s:/app/.env.template", containerID),
		filepath.Join(destDir, ".env.template"))
	cpOut, cpErr := cpCmd.CombinedOutput()

	// Also extract compose file if missing
	composePath := filepath.Join(destDir, "docker-compose.yml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		exec.CommandContext(ctx, "docker", "cp",
			fmt.Sprintf("%s:/app/docker-compose.yml", containerID),
			composePath).Run()
	}

	return string(cpOut), cpErr
}

func repoOwnerRepo(repoURL string) string {
	cleaned := strings.TrimSuffix(repoURL, ".git")
	cleaned = strings.TrimSuffix(cleaned, "/")
	parts := strings.Split(cleaned, "/")
	if len(parts) >= 2 {
		return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
	}
	return ""
}

// ReadEnvTemplate parses .env.template and returns variables + saved overrides
func (s *ProjectService) ReadEnvTemplate(projectID string) ([]EnvVar, map[string]string, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("project not found: %s", projectID)
	}
	templatePath := filepath.Join(p.AppDir, ".env.template")
	data, err := os.ReadFile(templatePath)
	if err != nil {
		// Try to auto-copy from source location, then retry
		s.ensureEnvTemplate(p)
		data, err = os.ReadFile(templatePath)
		if err != nil {
			return nil, nil, nil // no template — not an error
		}
	}

	secretKeys := map[string]bool{"password": true, "secret": true, "token": true, "key": true, "pass": true}
	var vars []EnvVar
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) < 1 || parts[0] == "" {
			continue
		}
		key := strings.TrimSpace(parts[0])
		def := ""
		if len(parts) == 2 {
			def = strings.TrimSpace(parts[1])
		}
		isSecret := false
		lower := strings.ToLower(key)
		for sk := range secretKeys {
			if strings.Contains(lower, sk) {
				isSecret = true
				break
			}
		}
		if isSecret && def != "" {
			def = "********"
		}
		vars = append(vars, EnvVar{Key: key, Default: def, IsSecret: isSecret})
	}

	// Load saved overrides from metadata JSON field
	overrides := s.loadEnvOverridesFromState(projectID)
	// Mask secret override values
	for k, v := range overrides {
		if v == "" {
			continue
		}
		lower := strings.ToLower(k)
		for sk := range secretKeys {
			if strings.Contains(lower, sk) && v != "" {
				overrides[k] = "********"
				break
			}
		}
	}
	return vars, overrides, nil
}

// SaveEnvConfig saves env var overrides into project state metadata field
func (s *ProjectService) SaveEnvConfig(projectID string, overrides map[string]string) error {
	state, _ := s.db.GetProjectState(projectID)
	if state == nil {
		state = map[string]any{}
	}
	// Merge with existing metadata
	var metadata map[string]any
	rawMeta, _ := state["metadata"].(string)
	if rawMeta != "" {
		json.Unmarshal([]byte(rawMeta), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	envMap := make(map[string]any, len(overrides))
	for k, v := range overrides {
		envMap[k] = v
	}
	metadata["env_overrides"] = envMap
	metaBytes, _ := json.Marshal(metadata)
	state["metadata"] = string(metaBytes)
	return s.db.UpsertProjectState(projectID, state)
}

// loadEnvOverridesFromState reads env_overrides from the metadata JSON field
func (s *ProjectService) loadEnvOverridesFromState(projectID string) map[string]string {
	state, _ := s.db.GetProjectState(projectID)
	if state == nil {
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

// LoadEnvOverrides returns saved env var overrides for deploy injection
func (s *ProjectService) LoadEnvOverrides(projectID string) map[string]string {
	return s.loadEnvOverridesFromState(projectID)
}

func (s *ProjectService) RunnerAction(projectID, action string) (string, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return "", fmt.Errorf("project not found: %s", projectID)
	}
	container := p.RunnerContainer
	if container == "" {
		return "", fmt.Errorf("no runner container configured")
	}

	var msg string
	switch action {
	case "start":
		token, _ := s.db.GetRegistryPassword(projectID)
		if token == "" {
			token = s.cfg.GithubToken
		}
		if token == "" {
			return "", fmt.Errorf("no runner token configured; configure a token in Settings first")
		}
		safeGo("runnerStart", func() {
			if err := s.startRunner(p, token); err != nil {
				s.audit.Log("runner_start", "error", p.ID, err.Error(), "")
				_ = s.db.SaveRunnerStatus(p.ID, "error")
			} else {
				_ = s.db.SaveRunnerStatus(p.ID, "active")
			}
		})
		p.RunnerStatus = "starting"
		_ = s.db.UpsertProject(p)
		msg = "Runner start initiated."
	case "stop":
		s.stopRunner(p)
		msg = "Runner container stopped."
	case "restart":
		s.stopRunner(p)
		token, _ := s.db.GetRegistryPassword(projectID)
		if token == "" {
			token = s.cfg.GithubToken
		}
		if token == "" {
			return "", fmt.Errorf("no runner token configured; configure a token in Settings first")
		}
		safeGo("runnerRestart", func() {
			if err := s.startRunner(p, token); err != nil {
				s.audit.Log("runner_start", "error", p.ID, err.Error(), "")
				_ = s.db.SaveRunnerStatus(p.ID, "error")
			} else {
				_ = s.db.SaveRunnerStatus(p.ID, "active")
			}
		})
		p.RunnerStatus = "starting"
		_ = s.db.UpsertProject(p)
		msg = "Runner restart initiated."
	}
	return msg, nil
}

func DeploymentContainerName(service, branch, projectID string) string {
	return fmt.Sprintf("%s-%s-%s", projectID, branchSlug(branch), service)
}

func runnerLabels(p *models.Project) string {
	labels := []string{fmt.Sprintf("project-%s", p.ID), fmt.Sprintf("branch-%s", branchSlug(p.BranchName))}
	if p.BranchName == "main" {
		labels = append(labels, "production")
	} else {
		labels = append(labels, "development")
	}
	return strings.Join(labels, ",")
}

func branchSlug(branch string) string {
	slug := strings.ToLower(branch)
	re := regexp.MustCompile(`[^a-z0-9_.-]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, ".-")
	if slug == "" {
		slug = "rc"
	}
	if !isAlphanumericStart(slug) {
		slug = "branch-" + slug
	}
	return slug
}

func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "refs/heads/") {
		ref = strings.TrimPrefix(ref, "refs/heads/")
	}
	if strings.HasPrefix(ref, "origin/") {
		ref = strings.TrimPrefix(ref, "origin/")
	}
	return ref
}

func (s *ProjectService) CheckServiceHealth(service, containerName string, summary *models.ContainerState) *models.ServiceHealth {
	h := &models.ServiceHealth{
		Service:        service,
		Container:      containerName,
		ContainerState: summary.State,
		Status:         "unavailable",
		Detail:         "Container is not available.",
	}
	if !summary.Exists {
		h.Detail = "Container was not found."
		return h
	}
	if !summary.Running {
		h.Detail = fmt.Sprintf("Container exists but is %s.", summary.State)
		return h
	}
	if summary.Health == "healthy" {
		h.Status = "healthy"
		h.Detail = "Docker healthcheck reports healthy."
		return h
	}
	if summary.Health == "unhealthy" {
		h.Status = "unhealthy"
		h.Detail = "Docker healthcheck reports unhealthy."
		return h
	}

	// If the project has a devops.json, look up health config for this service
	composeFile := filepath.Join(s.cfg.BaseDir, "Projects", containerName[:strings.Index(containerName, "-")], "devops.json")
	if devopsCfg := s.loadDevopsConfig(composeFile); devopsCfg != nil {
		if svcCfg, ok := devopsCfg["services"].(map[string]interface{})[service]; ok {
			if svcMap, ok := svcCfg.(map[string]interface{}); ok {
				if healthCfg, ok := svcMap["health"].(map[string]interface{}); ok {
					port := int(healthCfg["port"].(float64))
					path, _ := healthCfg["path"].(string)
					execStatus, execDetail := s.docker.ExecHTTPHealth(containerName, port, path)
					h.Status = execStatus
					h.Detail = execDetail
					return h
				}
			}
		}
	}

	h.Status = "unknown"
	h.Detail = "Container is running, but no health check is configured."
	return h
}

func (s *ProjectService) loadDevopsConfig(path string) map[string]interface{} {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return cfg
}

// composeServices returns service names for a project by reading docker-compose.yml
func (s *ProjectService) composeServices(p *models.Project) []string {
	composeFile := filepath.Join(s.cfg.BaseDir, "Projects", p.ID, "docker-compose.yml")
	svcNames, err := s.docker.ComposeServiceNames(composeFile)
	if err != nil || len(svcNames) == 0 {
		// Fallback: read from devops.json services section
		devopsPath := filepath.Join(s.cfg.BaseDir, "Projects", p.ID, "devops.json")
		if cfg := s.loadDevopsConfig(devopsPath); cfg != nil {
			if svcs, ok := cfg["services"].(map[string]interface{}); ok {
				for name := range svcs {
					svcNames = append(svcNames, name)
				}
			}
		}
	}
	if len(svcNames) == 0 {
		return []string{"postgres", "redis", "backend", "storefront"}
	}
	return svcNames
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
