package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

var (
	serviceNameRE     = regexp.MustCompile(`^[a-z0-9_.-]+$`)
	legacyGitHubPATRE = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)
)

type ProjectService struct {
	db                     *db.DB
	docker                 *docker.Client
	audit                  *AuditService
	cfg                    *models.Config
	githubAPIBaseURL       string
	githubHTTPClient       *http.Client
	runnerMonitorMu        sync.Mutex
	runnerMonitors         map[string]context.CancelFunc
	runnerLifecycleMu      sync.Mutex
	runnerLifecycleLocks   map[string]*sync.Mutex
	runnerRecoveryMu       sync.Mutex
	runnerRecoveryState    map[string]*runnerRecoveryAttemptState
	recoverRunnerFn        func(*models.Project, string) error
	preservedRunnerOpsFn   func(*models.Project) preservedRunnerRecoveryOps
	registrationRecoveryFn func(*models.Project, string) error
}

func NewProjectService(database *db.DB, dockerClient *docker.Client, audit *AuditService, cfg *models.Config) *ProjectService {
	return &ProjectService{
		db:                   database,
		docker:               dockerClient,
		audit:                audit,
		cfg:                  cfg,
		githubAPIBaseURL:     "https://api.github.com",
		githubHTTPClient:     &http.Client{Timeout: 15 * time.Second},
		runnerMonitors:       make(map[string]context.CancelFunc),
		runnerLifecycleLocks: make(map[string]*sync.Mutex),
		runnerRecoveryState:  make(map[string]*runnerRecoveryAttemptState),
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

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isAlphanumericStart(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func (s *ProjectService) CreateOrUpdate(req *models.ProjectRequest) (*models.Project, error) {
	if req.ID == nil || strings.TrimSpace(*req.ID) == "" {
		return nil, fmt.Errorf("project id is required")
	}
	projectID := s.SlugID(strings.TrimSpace(*req.ID))
	if projectID == "" {
		return nil, fmt.Errorf("project id must contain at least one letter or number")
	}

	existing, existingErr := s.db.GetProject(projectID)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("load project: %w", existingErr)
	}
	p := &models.Project{
		ID:              projectID,
		Name:            projectID,
		RepoURL:         "",
		BranchName:      envDefault("ENV_NAME", "dev"),
		DeploymentMode:  models.DeploymentModeComposeImage,
		AutoApply:       true,
		RegistryHost:    "ghcr.io",
		RunnerContainer: fmt.Sprintf("devops-runner-%s", projectID),
		RunnerStatus:    "unknown",
	}
	if existing != nil {
		p = existing
	}

	if req.Name != nil {
		p.Name = strings.TrimSpace(*req.Name)
		if p.Name == "" {
			return nil, fmt.Errorf("project name cannot be empty")
		}
	}
	if req.RepoURL != nil {
		p.RepoURL = strings.TrimSpace(*req.RepoURL)
	}
	if req.BranchName != nil {
		p.BranchName = normalizeRef(strings.TrimSpace(*req.BranchName))
		if p.BranchName == "" {
			return nil, fmt.Errorf("branch name cannot be empty")
		}
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
	// Resolve and clean the path to prevent traversal via ../.. or symlinks.
	validatedAppDir, err := validateProjectAppDir(s.cfg.BaseDir, p.AppDir)
	if err != nil {
		return nil, err
	}
	p.AppDir = validatedAppDir

	if err := s.validateRepoURL(p.RepoURL); err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?$`).MatchString(p.RegistryHost) {
		return nil, fmt.Errorf("registry host must be a hostname with no scheme or path")
	}

	var registryPassword *string
	if req.RegistryPassword != nil && *req.RegistryPassword != "" {
		if p.RegistryUsername == "" {
			return nil, fmt.Errorf("registry username is required when providing a password")
		}
		if err := validateSingleLineSecret(*req.RegistryPassword); err != nil {
			return nil, fmt.Errorf("invalid registry password: %w", err)
		}
		password := *req.RegistryPassword
		registryPassword = &password
	}

	var runnerTokenToSave *string
	runnerToken := ""
	if req.RunnerToken != nil && strings.TrimSpace(*req.RunnerToken) != "" {
		runnerToken = strings.TrimSpace(*req.RunnerToken)
		if err := validateSingleLineSecret(runnerToken); err != nil {
			return nil, fmt.Errorf("invalid runner token: %w", err)
		}
		runnerTokenToSave = &runnerToken
	} else if existing != nil {
		var err error
		runnerToken, err = s.db.GetRunnerToken(projectID)
		if err != nil {
			return nil, fmt.Errorf("load runner token: %w", err)
		}
	}
	if runnerToken == "" && s.cfg.GithubToken != "" {
		runnerToken = s.cfg.GithubToken
	}
	if req.ListenerActive != nil && *req.ListenerActive && runnerToken == "" {
		return nil, fmt.Errorf("no runner token configured; configure a token before enabling the listener")
	}
	if registryPassword != nil {
		registryAuth, ok, msg := s.docker.RegistryLoginIsolated(p.RegistryHost, p.RegistryUsername, *registryPassword)
		if !ok {
			return nil, fmt.Errorf("registry login failed: %s", msg)
		}
		if err := registryAuth.Close(); err != nil {
			return nil, fmt.Errorf("remove isolated registry credentials: %w", err)
		}
	}

	if err := s.db.SaveProjectWithCredentials(p, registryPassword, runnerTokenToSave); err != nil {
		return nil, fmt.Errorf("save project: %w", err)
	}
	s.ensureEnvTemplate(p)

	if req.ListenerActive != nil {
		if *req.ListenerActive {
			p.RunnerStatus = "starting"
			_ = s.db.SaveRunnerStatus(p.ID, p.RunnerStatus)
			safeGo("startRunner", func() {
				if err := s.startRunner(p, runnerToken); err != nil {
					s.audit.Log("runner_start", "error", p.ID, err.Error(), "")
					_ = s.db.SaveRunnerStatus(p.ID, "error")
				} else {
					_ = s.db.SaveRunnerStatus(p.ID, "active")
				}
			})
		} else {
			if err := s.stopRunner(p); err != nil {
				return nil, err
			}
			p.RunnerStatus = "stopped"
			_ = s.db.SaveRunnerStatus(p.ID, p.RunnerStatus)
		}
	}
	s.audit.Log("project_upsert", "ok", p.ID, fmt.Sprintf("repo=%s branch=%s", p.RepoURL, p.BranchName), "")
	return p, nil
}

func validateSingleLineSecret(value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("value must be a single line")
	}
	return nil
}

func resolveExistingProjectPath(path string) string {
	clean := filepath.Clean(path)
	ancestor := clean
	for {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			if relative, err := filepath.Rel(ancestor, clean); err == nil {
				return filepath.Clean(filepath.Join(resolved, relative))
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return clean
		}
		ancestor = parent
	}
}

func validateProjectAppDir(baseDir, appDir string) (string, error) {
	clean := filepath.Clean(appDir)
	projectsDir := filepath.Join(baseDir, "Projects")
	projectsRoot := filepath.Clean(projectsDir) + string(filepath.Separator)
	if clean == filepath.Clean(projectsDir) || !strings.HasPrefix(clean+string(filepath.Separator), projectsRoot) {
		return "", fmt.Errorf("app_dir must be within %s", projectsDir)
	}
	resolved := resolveExistingProjectPath(clean)
	resolvedRoot := resolveExistingProjectPath(projectsDir)
	if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("app_dir resolves outside %s", projectsDir)
	}
	return clean, nil
}

func (s *ProjectService) Delete(projectID string) error {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return fmt.Errorf("project not found: %s", projectID)
	}
	deleteID := fmt.Sprintf("delete-%s-%d", projectID, time.Now().UnixMilli())
	lock := &models.DeployLock{
		ProjectID: projectID, OperationID: deleteID, Operation: "delete",
		StartedAt: time.Now().UTC().Format(time.RFC3339), Branch: p.BranchName,
	}
	if err := s.db.CreateLock(lock); err != nil {
		return fmt.Errorf("cannot delete project while another operation is active: %w", err)
	}
	defer func() {
		if err := s.db.ReleaseLock(projectID, deleteID); err != nil {
			log.Printf("release project deletion lock for project %s operation %s: %v", projectID, deleteID, err)
		}
	}()

	projectDir := p.AppDir
	if projectDir == "" {
		projectDir = filepath.Join(s.cfg.BaseDir, "Projects", projectID)
	}
	projectDir, err = validateProjectAppDir(s.cfg.BaseDir, projectDir)
	if err != nil {
		return fmt.Errorf("refusing to delete project with unsafe app_dir: %w", err)
	}
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	registeredBranch := branchSlug(p.BranchName)
	composeProjects := map[string]string{
		fmt.Sprintf("%s-%s", p.ID, registeredBranch): filepath.Join(projectDir, ".env."+registeredBranch),
	}
	historicalProjects, err := s.docker.ListComposeProjects(p.ID+"-", projectDir)
	if err != nil {
		return fmt.Errorf("discover owned Compose projects: %w", err)
	}
	for _, composeProject := range historicalProjects {
		suffix := strings.TrimPrefix(composeProject, p.ID+"-")
		if suffix != "" {
			composeProjects[composeProject] = filepath.Join(projectDir, ".env."+suffix)
		}
	}
	if _, err := os.Stat(composeFile); err == nil {
		envFiles, _ := filepath.Glob(filepath.Join(projectDir, ".env.*"))
		for _, ef := range envFiles {
			if strings.HasSuffix(ef, ".env.template") {
				continue
			}
			suffix := strings.TrimPrefix(filepath.Base(ef), ".env.")
			if suffix != "" {
				composeProjects[fmt.Sprintf("%s-%s", p.ID, branchSlug(suffix))] = ef
			}
		}
	}
	if err := s.stopRunner(p); err != nil {
		return err
	}
	runnerComposeProject := fmt.Sprintf("devops-runner-%s", p.ID)
	if err := s.docker.RemoveComposeVolumes(runnerComposeProject); err != nil {
		return fmt.Errorf("remove runner volumes for %s: %w", runnerComposeProject, err)
	}
	for composeProject, envFile := range composeProjects {
		if _, err := os.Stat(composeFile); err == nil {
			if _, envErr := os.Stat(envFile); envErr != nil {
				containers, listErr := s.docker.ListComposeContainers(composeProject)
				if listErr != nil {
					return fmt.Errorf("inspect Compose project %s: %w", composeProject, listErr)
				}
				if len(containers) > 0 {
					// A project can have pre-existing Compose-owned containers but no
					// generated branch environment yet (for example when it was
					// registered before its first deploy). Compose still needs every
					// interpolation value to parse the file for `down`. Build a
					// short-lived, non-persistent environment from the committed
					// template and saved operator overrides. It is used only to tear
					// down the exact Compose project selected above; it never becomes
					// the project's deployment environment.
					teardownEnv, teardownErr := s.teardownEnvironment(p, composeProject)
					if teardownErr != nil {
						return fmt.Errorf("prepare teardown environment for Compose project %s: %w", composeProject, teardownErr)
					}
					if err := s.docker.ComposeDownWithEnvFile(composeFile, composeProject, filepath.Join(projectDir, ".env.template"), teardownEnv); err != nil {
						return fmt.Errorf("stop Compose project %s with temporary environment: %w", composeProject, err)
					}
					continue
				}
				continue
			}
			if err := s.docker.ComposeDownWithEnvFile(composeFile, composeProject, envFile, nil); err != nil {
				return fmt.Errorf("stop Compose project %s: %w", composeProject, err)
			}
		}
		remaining, err := s.docker.ListComposeContainers(composeProject)
		if err != nil {
			return fmt.Errorf("verify Compose project %s removal: %w", composeProject, err)
		}
		if len(remaining) > 0 {
			return fmt.Errorf("refusing to delete project data while owned containers remain for %s: %s", composeProject, strings.Join(remaining, ", "))
		}
		if err := s.docker.RemoveComposeVolumes(composeProject); err != nil {
			return fmt.Errorf("remove Compose volumes for %s: %w", composeProject, err)
		}
	}

	// Clean up data dirs
	for _, path := range []string{
		filepath.Join(s.cfg.BaseDir, "State", projectID),
		filepath.Join(s.cfg.BaseDir, "Releases", projectID),
		filepath.Join(s.cfg.BaseDir, "Logs", projectID),
		projectDir,
	} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove project data %s: %w", path, err)
		}
	}
	// Clean up legacy project dir at project root level
	legacyDir := filepath.Join(s.cfg.ProjectRoot, "Projects", projectID)
	if err := os.RemoveAll(legacyDir); err != nil {
		return fmt.Errorf("remove legacy project data %s: %w", legacyDir, err)
	}

	if err := s.db.DeleteProject(projectID); err != nil {
		return err
	}
	s.audit.Log("project_delete", "ok", projectID, "Project deleted and cleaned up.", "")
	return nil
}

// teardownEnvironment creates a complete process-only environment for
// `docker compose down` when a project's generated branch env file does not
// exist. Compose parses the model before it removes label-owned resources, so
// every interpolation must be non-empty. The returned values are never
// persisted; real saved overrides take precedence and blank variables receive
// type-safe inert placeholders solely for Compose interpolation.
func (s *ProjectService) teardownEnvironment(p *models.Project, composeProject string) (map[string]string, error) {
	templatePath := filepath.Join(p.AppDir, ".env.template")
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read environment template: %w", err)
	}

	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if !envKeyRE.MatchString(key) {
			return nil, fmt.Errorf("invalid environment key %q in template", key)
		}
		values[key] = strings.TrimSpace(parts[1])
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("environment template declares no variables")
	}

	for key, value := range s.loadEnvOverrides(p.ID) {
		if _, declared := values[key]; declared {
			values[key] = value
		}
	}
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			values[key] = teardownPlaceholder(key)
		}
	}
	values["COMPOSE_PROJECT_NAME"] = composeProject
	values["ENV_NAME"] = branchSlug(p.BranchName)
	return values, nil
}

func teardownPlaceholder(key string) string {
	switch {
	case strings.HasSuffix(key, "_PORT"):
		return "1"
	case strings.HasSuffix(key, "_URL") || strings.HasSuffix(key, "_CORS"):
		return "http://teardown.invalid"
	case strings.HasSuffix(key, "_SECURE"), strings.HasPrefix(key, "ALLOW_"), strings.HasPrefix(key, "RESET_"), strings.HasSuffix(key, "_CONFIRMED"), strings.HasSuffix(key, "_APPROVED"):
		return "false"
	default:
		return "teardown-placeholder"
	}
}

func (s *ProjectService) Status(projectID string) (*models.ProjectStatus, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	state, _ := s.db.GetProjectState(projectID)
	lock, _ := s.db.GetLock(projectID)

	runnerSummary := s.docker.ContainerSummary(p.RunnerContainer)
	if runnerSummary.Exists {
		projectName := fmt.Sprintf("devops-runner-%s", p.ID)
		if err := s.docker.VerifyComposeOwnership(p.RunnerContainer, projectName, "github-runner"); err != nil {
			runnerSummary = &models.ContainerState{Container: p.RunnerContainer, State: "unavailable"}
		}
	}

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
		containerName, ownershipErr := s.OwnedContainerName(p, svc)
		var summary *models.ContainerState
		if ownershipErr != nil {
			containerName = DeploymentContainerName(svc, p.BranchName, p.ID)
			summary = &models.ContainerState{Container: containerName, State: "unavailable"}
		} else {
			summary = s.docker.ContainerSummary(containerName)
		}
		containers["current"][svc] = summary.State
		health[svc] = s.CheckServiceHealth(p, svc, containerName, summary)
	}

	deployments, _ := s.db.ListDeployments(projectID, 10)
	backups, _ := s.db.ListBackups(projectID, 10)

	return &models.ProjectStatus{
		Project:      p,
		State:        state,
		Lock:         lock,
		Runner:       map[string]string{"container": p.RunnerContainer, "state": runnerSummary.State},
		Containers:   containers,
		Health:       health,
		Deployments:  deployments,
		Backups:      backups,
		Capabilities: s.capabilities(p),
		LogDir:       s.LogDir(p),
		ServerTime:   time.Now().UTC().Format(time.RFC3339),
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
		"terminal":        s.cfg.EnableTerminal,
	}
}

func (s *ProjectService) validateRepoURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("only GitHub HTTPS URLs are supported")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repository URL must identify one GitHub owner and repository")
	}
	if len(s.cfg.AllowedRepoPrefixes) > 0 {
		allowed := false
		for _, prefix := range s.cfg.AllowedRepoPrefixes {
			if strings.HasPrefix(rawURL, prefix) && (strings.HasSuffix(prefix, "/") || len(rawURL) == len(prefix) || rawURL[len(prefix)] == '/') {
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

const (
	runnerRegistrationRequestMarker = "DEVOPS_RUNNER_REGISTRATION_REQUIRED"
	runnerDeletionHint              = "The runner registration has been deleted from the server"
	runnerRegistrationTokenPath     = "/run/devops-runner-registration/token"
	runnerRegistrationStagingPath   = "/run/devops-runner-registration/.token.tmp"
	runnerReadyTimeout              = 5 * time.Minute
	runnerMonitorPollInterval       = 30 * time.Second
	runnerTransitionRecoveryDelay   = 90 * time.Second
	runnerRecoveryBackoffInitial    = 30 * time.Second
	runnerRecoveryBackoffMaximum    = 5 * time.Minute
	runnerRecoveryServerDelayMax    = time.Hour
)

type runnerRecoveryAttemptState struct {
	failures    int
	nextAttempt time.Time
	lastError   string
}

type githubRunnerVerificationError struct {
	message string
	retryAt time.Time
}

func (e *githubRunnerVerificationError) Error() string {
	return e.message
}

func githubRunnerRetryAt(headers http.Header, now time.Time) time.Time {
	var retryAt time.Time
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			retryAt = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(raw); err == nil && parsed.After(now) {
			retryAt = parsed
		}
	}
	if raw := strings.TrimSpace(headers.Get("X-RateLimit-Reset")); raw != "" {
		if unixSeconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
			resetAt := time.Unix(unixSeconds, 0)
			if resetAt.After(retryAt) && resetAt.After(now) {
				retryAt = resetAt
			}
		}
	}
	if maximum := now.Add(runnerRecoveryServerDelayMax); retryAt.After(maximum) {
		return maximum
	}
	return retryAt
}

func runnerVerificationRetryAt(err error) time.Time {
	var verificationErr *githubRunnerVerificationError
	if errors.As(err, &verificationErr) {
		return verificationErr.retryAt
	}
	return time.Time{}
}

func runnerRecoveryBackoff(projectID string, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := runnerRecoveryBackoffInitial
	for attempt := 1; attempt < failures && delay < runnerRecoveryBackoffMaximum; attempt++ {
		if delay > runnerRecoveryBackoffMaximum/2 {
			delay = runnerRecoveryBackoffMaximum
			break
		}
		delay *= 2
	}
	if delay > runnerRecoveryBackoffMaximum {
		delay = runnerRecoveryBackoffMaximum
	}

	// Stable per-project jitter prevents several failed runners from retrying
	// GitHub in lockstep while keeping tests and operational timing predictable.
	jitterRange := delay / 5
	if jitterRange <= 0 {
		return delay
	}
	var hash uint64 = 1469598103934665603
	for _, value := range fmt.Sprintf("%s:%d", projectID, failures) {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	width := uint64(2*jitterRange + 1)
	jitter := time.Duration(int64(hash%width) - int64(jitterRange))
	delay += jitter
	if delay < time.Second {
		return time.Second
	}
	if delay > runnerRecoveryBackoffMaximum {
		return runnerRecoveryBackoffMaximum
	}
	return delay
}

func runnerLogsReady(logs string) bool {
	lower := strings.ToLower(logs)
	for _, marker := range []string{"runner already configured.", "listening for jobs", "running job:"} {
		if strings.LastIndex(lower, marker) >= 0 {
			return true
		}
	}
	return false
}

func runnerRegistrationRequested(logs string) bool {
	requestIndex := strings.LastIndex(logs, runnerRegistrationRequestMarker)
	if requestIndex < 0 {
		return false
	}
	lower := strings.ToLower(logs)
	readyIndex := -1
	for _, marker := range []string{"runner already configured.", "listening for jobs", "running job:"} {
		if index := strings.LastIndex(lower, marker); index > readyIndex {
			readyIndex = index
		}
	}
	return requestIndex > readyIndex
}

func runnerTransitionStartedAt(previous time.Time, now time.Time, summary *models.ContainerState, ready bool) time.Time {
	if summary == nil || !summary.Exists || (summary.Running && ready) {
		return time.Time{}
	}
	if summary.State == "created" || summary.State == "restarting" {
		if previous.IsZero() {
			return now
		}
		return previous
	}
	// Docker restart policies briefly report a crash-looping container as
	// running between backoff intervals. Preserve the transition window until
	// this runner generation emits a readiness marker.
	if !previous.IsZero() && summary.Running {
		return previous
	}
	return time.Time{}
}

func (s *ProjectService) runnerRegistrationCredential(projectID string) (string, error) {
	credential, err := s.db.GetRunnerToken(projectID)
	if err != nil {
		return "", fmt.Errorf("load runner credential: %w", err)
	}
	if credential == "" {
		credential = s.cfg.GithubToken
	}
	if credential == "" {
		return "", fmt.Errorf("runner credential is not configured")
	}
	return credential, nil
}

func (s *ProjectService) runnerCredentials(projectID string) (registrationCredential, verificationCredential string, err error) {
	registrationCredential, err = s.runnerRegistrationCredential(projectID)
	if err != nil {
		return "", "", err
	}
	if isGitHubPersonalAccessToken(registrationCredential) {
		verificationCredential = registrationCredential
	} else if isGitHubPersonalAccessToken(s.cfg.GithubToken) {
		registrationCredential = s.cfg.GithubToken
		verificationCredential = s.cfg.GithubToken
	}
	if verificationCredential == "" {
		return "", "", fmt.Errorf("a GitHub personal access token is required to verify runner registration")
	}
	return registrationCredential, verificationCredential, nil
}

type runnerRegistrationState uint8

const (
	runnerRegistrationUnknown runnerRegistrationState = iota
	runnerRegistrationPresent
	runnerRegistrationAbsent
)

func (s *ProjectService) githubRunnerRegistrationState(ctx context.Context, p *models.Project, credential string) (runnerRegistrationState, error) {
	if p == nil {
		return runnerRegistrationUnknown, fmt.Errorf("project is required")
	}
	if !isGitHubPersonalAccessToken(credential) {
		return runnerRegistrationUnknown, fmt.Errorf("GitHub runner verification requires a personal access token")
	}
	if err := s.validateRepoURL(p.RepoURL); err != nil {
		return runnerRegistrationUnknown, fmt.Errorf("cannot verify runner registration: %w", err)
	}
	owner, repo, ok := strings.Cut(repoOwnerRepo(p.RepoURL), "/")
	if !ok || owner == "" || repo == "" {
		return runnerRegistrationUnknown, fmt.Errorf("cannot verify runner registration: repository URL is invalid")
	}
	expectedName := fmt.Sprintf("runner-%s-%s", p.ID, branchSlug(p.BranchName))
	baseURL := strings.TrimRight(s.githubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	client := s.githubHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	for page := 1; page <= 100; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runners?per_page=100&page=%d",
			baseURL, url.PathEscape(owner), url.PathEscape(repo), page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return runnerRegistrationUnknown, fmt.Errorf("create GitHub runner verification request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := client.Do(req)
		if err != nil {
			return runnerRegistrationUnknown, fmt.Errorf("verify GitHub runner registration: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			return runnerRegistrationUnknown, &githubRunnerVerificationError{
				message: fmt.Sprintf("verify GitHub runner registration: GitHub returned %s", resp.Status),
				retryAt: githubRunnerRetryAt(resp.Header, time.Now()),
			}
		}
		var result struct {
			TotalCount int `json:"total_count"`
			Runners    []struct {
				Name string `json:"name"`
			} `json:"runners"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
		resp.Body.Close()
		if decodeErr != nil {
			return runnerRegistrationUnknown, fmt.Errorf("decode GitHub runner list: %w", decodeErr)
		}
		for _, runner := range result.Runners {
			if runner.Name == expectedName {
				return runnerRegistrationPresent, nil
			}
		}
		if len(result.Runners) < 100 || page*100 >= result.TotalCount {
			return runnerRegistrationAbsent, nil
		}
	}
	return runnerRegistrationUnknown, fmt.Errorf("GitHub runner list exceeded the verification limit")
}

func (s *ProjectService) handleRunnerRecoveryHint(ctx context.Context, p *models.Project, localUnavailable bool) error {
	registrationCredential, verificationCredential, err := s.runnerCredentials(p.ID)
	if err != nil {
		return err
	}
	registrationState, err := s.githubRunnerRegistrationState(ctx, p, verificationCredential)
	if err != nil {
		return err
	}
	switch registrationState {
	case runnerRegistrationPresent:
		if !localUnavailable {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return s.restoreVerifiedPresentRunner(ctx, p)
	case runnerRegistrationAbsent:
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.recoverRunnerFn != nil {
			return s.recoverRunnerFn(p, registrationCredential)
		}
		return s.replaceVerifiedAbsentRunner(ctx, p, registrationCredential)
	default:
		return fmt.Errorf("GitHub runner registration state is unknown")
	}
}

type runnerRecoverySignal struct {
	registrationRequested bool
	localUnavailable      bool
}

func (s *ProjectService) handleRunnerRecoverySignal(ctx context.Context, p *models.Project, signal runnerRecoverySignal) error {
	if signal.registrationRequested {
		credential, err := s.runnerRegistrationCredential(p.ID)
		if err != nil {
			return err
		}
		if s.registrationRecoveryFn != nil {
			return s.registrationRecoveryFn(p, credential)
		}
		return s.recoverRunnerRegistrationHandoff(ctx, p, credential)
	}
	return s.handleRunnerRecoveryHint(ctx, p, signal.localUnavailable)
}

type registrationHandoffRecoveryOps struct {
	containerExists        bool
	saveStatus             func(string)
	verifyOwnership        func() error
	stopMonitor            func()
	restartPreservingState func() error
	installMonitor         func()
}

func restartRunnerRegistrationHandoff(ops registrationHandoffRecoveryOps) error {
	ops.saveStatus("starting")
	if ops.containerExists {
		if err := ops.verifyOwnership(); err != nil {
			ops.saveStatus("error")
			ops.installMonitor()
			return fmt.Errorf("verify runner registration handoff ownership: %w", err)
		}
	}
	ops.stopMonitor()
	if err := ops.restartPreservingState(); err != nil {
		ops.saveStatus("error")
		ops.installMonitor()
		return fmt.Errorf("restart runner registration handoff with preserved state: %w", err)
	}
	ops.saveStatus("active")
	// startRunnerContainerLocked installs the monitor after readiness. Keeping
	// this handoff explicit also makes injected/test implementations correct.
	ops.installMonitor()
	return nil
}

func (s *ProjectService) recoverRunnerRegistrationHandoff(ctx context.Context, p *models.Project, credential string) error {
	unlock := s.lockRunnerLifecycle(p.ID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	summary := s.docker.ContainerSummary(p.RunnerContainer)
	err := restartRunnerRegistrationHandoff(registrationHandoffRecoveryOps{
		containerExists: summary.Exists,
		saveStatus: func(status string) {
			_ = s.db.SaveRunnerStatus(p.ID, status)
		},
		verifyOwnership: func() error {
			return s.docker.VerifyComposeOwnership(
				p.RunnerContainer, fmt.Sprintf("devops-runner-%s", p.ID), "github-runner",
			)
		},
		stopMonitor:            func() { s.stopRunnerMonitor(p.ID) },
		restartPreservingState: func() error { return s.startRunnerContainerLocked(p, credential) },
		installMonitor:         func() { s.startRunnerMonitor(p) },
	})
	if err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.Log(
			"runner_recovery",
			"ok",
			p.ID,
			"registration handoff was interrupted; restarted runner with preserved state and completed registration",
			"",
		)
	}
	return nil
}

type preservedRunnerRecoveryOps struct {
	containerExists   bool
	forceRecreate     bool
	saveStatus        func(string)
	verifyOwnership   func() error
	stopMonitor       func()
	startExisting     func() error
	recreatePreserved func() error
	waitReady         func() (bool, string, string)
	installMonitor    func()
}

func restoreRunnerWithPreservedState(ops preservedRunnerRecoveryOps) error {
	ops.saveStatus("starting")
	if ops.containerExists {
		if err := ops.verifyOwnership(); err != nil {
			ops.saveStatus("error")
			ops.installMonitor()
			return fmt.Errorf("verify preserved runner ownership: %w", err)
		}
	}
	ops.stopMonitor()
	var err error
	if ops.containerExists && !ops.forceRecreate {
		err = ops.startExisting()
	} else {
		err = ops.recreatePreserved()
	}
	if err == nil {
		ready, state, logs := ops.waitReady()
		if !ready {
			err = fmt.Errorf("preserved runner did not become ready (state=%s): %s", state, truncateStr(logs, 200))
		}
	}
	if err != nil {
		ops.saveStatus("error")
		ops.installMonitor()
		return err
	}
	ops.saveStatus("active")
	ops.installMonitor()
	return nil
}

func (s *ProjectService) restoreVerifiedPresentRunner(ctx context.Context, p *models.Project) error {
	unlock := s.lockRunnerLifecycle(p.ID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var ops preservedRunnerRecoveryOps
	if s.preservedRunnerOpsFn != nil {
		ops = s.preservedRunnerOpsFn(p)
	} else {
		summary := s.docker.ContainerSummary(p.RunnerContainer)
		ops = preservedRunnerRecoveryOps{
			containerExists: summary.Exists,
			forceRecreate:   summary.State == "created" || summary.State == "restarting" || summary.Running,
			saveStatus: func(status string) {
				_ = s.db.SaveRunnerStatus(p.ID, status)
			},
			verifyOwnership: func() error {
				return s.docker.VerifyComposeOwnership(
					p.RunnerContainer, fmt.Sprintf("devops-runner-%s", p.ID), "github-runner",
				)
			},
			stopMonitor:   func() { s.stopRunnerMonitor(p.ID) },
			startExisting: func() error { return s.docker.StartContainer(p.RunnerContainer) },
			recreatePreserved: func() error {
				if summary.Exists {
					if err := s.stopRunnerResourcesLocked(p, false); err != nil {
						return err
					}
				}
				return s.recreateRunnerWithPreservedStateLocked(p)
			},
			waitReady: func() (bool, string, string) {
				return s.docker.WaitForRunnerReady(p.RunnerContainer, runnerReadyTimeout)
			},
			installMonitor: func() { s.startRunnerMonitor(p) },
		}
	}
	err := restoreRunnerWithPreservedState(ops)
	if err != nil {
		return fmt.Errorf("restore runner with preserved registration: %w", err)
	}
	if s.audit != nil {
		s.audit.Log("runner_recovery", "ok", p.ID, "GitHub registration present; restored local runner with preserved state", "")
	}
	return nil
}

func (s *ProjectService) replaceVerifiedAbsentRunner(ctx context.Context, p *models.Project, registrationCredential string) error {
	projectName := fmt.Sprintf("devops-runner-%s", p.ID)
	unlock := s.lockRunnerLifecycle(p.ID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = s.db.SaveRunnerStatus(p.ID, "starting")
	err := replaceRunnerResources(
		func() error { return s.stopRunnerForRecoveryLocked(p) },
		func() error { return s.docker.RemoveComposeVolumes(projectName) },
		func() error { return s.startRunnerLocked(p, registrationCredential) },
	)
	if err != nil {
		// If clean startup failed after cancelling the old monitor, install a
		// controller-owned monitor on the now-absent container so recovery is
		// retried. If the old monitor is still active this is a no-op.
		_ = s.db.SaveRunnerStatus(p.ID, "error")
		s.startRunnerMonitor(p)
		return err
	}
	_ = s.db.SaveRunnerStatus(p.ID, "active")
	if s.audit != nil {
		s.audit.Log("runner_recovery", "ok", p.ID, "GitHub absence verified; replaced runner container and state volume", "")
	}
	return nil
}

func replaceRunnerResources(stop, discardState, startClean func() error) error {
	if err := stop(); err != nil {
		return fmt.Errorf("stop compromised runner: %w", err)
	}
	// Remove only volumes carrying this exact Compose project label. A fresh
	// container cannot inherit state or tmpfs contents from the compromised job.
	if err := discardState(); err != nil {
		return fmt.Errorf("discard compromised runner state: %w", err)
	}
	if err := startClean(); err != nil {
		return fmt.Errorf("start clean runner: %w", err)
	}
	return nil
}

func (s *ProjectService) runnerRecoveryNextAttempt(projectID string) time.Time {
	s.runnerRecoveryMu.Lock()
	defer s.runnerRecoveryMu.Unlock()
	if state := s.runnerRecoveryState[projectID]; state != nil {
		return state.nextAttempt
	}
	return time.Time{}
}

func (s *ProjectService) recordRunnerRecoveryFailure(p *models.Project, recoveryErr error, now time.Time) time.Time {
	message := recoveryErr.Error()
	s.runnerRecoveryMu.Lock()
	if s.runnerRecoveryState == nil {
		s.runnerRecoveryState = make(map[string]*runnerRecoveryAttemptState)
	}
	state := s.runnerRecoveryState[p.ID]
	if state == nil {
		state = &runnerRecoveryAttemptState{}
		s.runnerRecoveryState[p.ID] = state
	}
	state.failures++
	state.nextAttempt = now.Add(runnerRecoveryBackoff(p.ID, state.failures))
	if retryAt := runnerVerificationRetryAt(recoveryErr); retryAt.After(state.nextAttempt) {
		state.nextAttempt = retryAt
	}
	report := state.lastError != message
	state.lastError = message
	nextAttempt := state.nextAttempt
	s.runnerRecoveryMu.Unlock()

	if report {
		_ = s.db.SaveRunnerStatus(p.ID, "recovery_error")
		log.Printf(
			"runner recovery for %s failed: %v; next attempt no earlier than %s",
			p.ID,
			recoveryErr,
			nextAttempt.UTC().Format(time.RFC3339),
		)
		if s.audit != nil {
			s.audit.Log(
				"runner_recovery",
				"failed",
				p.ID,
				fmt.Sprintf(
					"automatic recovery failed; next_attempt=%s reason=%s",
					nextAttempt.UTC().Format(time.RFC3339),
					truncateStr(message, 240),
				),
				"",
			)
		}
	}
	return nextAttempt
}

func (s *ProjectService) clearRunnerRecoveryFailure(p *models.Project) {
	s.runnerRecoveryMu.Lock()
	state := s.runnerRecoveryState[p.ID]
	delete(s.runnerRecoveryState, p.ID)
	s.runnerRecoveryMu.Unlock()
	if state == nil || state.lastError == "" {
		return
	}
	log.Printf("runner recovery for %s is healthy again", p.ID)
	if s.audit != nil {
		s.audit.Log("runner_recovery", "ok", p.ID, "automatic runner recovery is healthy again", "")
	}
}

func (s *ProjectService) startRunnerMonitor(p *models.Project) {
	if p == nil || p.ID == "" || p.RunnerContainer == "" {
		return
	}
	ctx, installed := s.installRunnerMonitor(p.ID)
	if !installed {
		return
	}

	safeGo("runnerMonitor", func() {
		initialLogs := s.docker.ContainerLogs(p.RunnerContainer, 200)
		since := time.Now().UTC()
		firstPoll := true
		pendingRegistration := runnerRegistrationRequested(initialLogs)
		pendingDeletionVerification := strings.Contains(initialLogs, runnerDeletionHint)
		readyObserved := runnerLogsReady(initialLogs) && !pendingRegistration
		var transitionStarted time.Time
		timer := time.NewTimer(0)
		defer timer.Stop()
		resetTimer := func(delay time.Duration) {
			if delay < 0 {
				delay = 0
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case pollStarted := <-timer.C:
				var logs string
				if firstPoll {
					logs = initialLogs
					firstPoll = false
				} else {
					logs = s.docker.ContainerLogsSince(p.RunnerContainer, 200, since.Format(time.RFC3339Nano))
				}
				since = pollStarted.UTC()
				summary := s.docker.ContainerSummary(p.RunnerContainer)
				if strings.Contains(logs, runnerRegistrationRequestMarker) {
					pendingRegistration = runnerRegistrationRequested(logs)
				} else if runnerLogsReady(logs) {
					pendingRegistration = false
				}
				if runnerLogsReady(logs) && !runnerRegistrationRequested(logs) {
					readyObserved = true
				}
				if summary.State == "created" || summary.State == "restarting" {
					readyObserved = false
				}
				transitionStarted = runnerTransitionStartedAt(
					transitionStarted, pollStarted, summary, readyObserved,
				)
				sustainedTransition := !transitionStarted.IsZero() &&
					pollStarted.Sub(transitionStarted) >= runnerTransitionRecoveryDelay
				localUnavailable := !summary.Exists ||
					(!summary.Running && summary.State != "created" && summary.State != "restarting") ||
					sustainedTransition
				if strings.Contains(logs, runnerDeletionHint) {
					pendingDeletionVerification = true
				}
				pendingVerification := pendingDeletionVerification || localUnavailable
				if !pendingRegistration && !pendingVerification {
					resetTimer(runnerMonitorPollInterval)
					continue
				}
				if nextAttempt := s.runnerRecoveryNextAttempt(p.ID); nextAttempt.After(pollStarted) {
					delay := time.Until(nextAttempt)
					if delay > runnerMonitorPollInterval {
						delay = runnerMonitorPollInterval
					}
					resetTimer(delay)
					continue
				}
				verifyCtx, verifyCancel := context.WithTimeout(ctx, 20*time.Second)
				err := s.handleRunnerRecoverySignal(verifyCtx, p, runnerRecoverySignal{
					registrationRequested: pendingRegistration,
					localUnavailable:      localUnavailable,
				})
				verifyCancel()
				if err != nil {
					nextAttempt := s.recordRunnerRecoveryFailure(p, err, time.Now())
					if ctx.Err() != nil {
						// A failed lifecycle recovery installs a replacement
						// monitor. Persistent backoff state transfers to it.
						return
					}
					delay := time.Until(nextAttempt)
					if delay > runnerMonitorPollInterval {
						delay = runnerMonitorPollInterval
					}
					resetTimer(delay)
					continue
				}
				s.clearRunnerRecoveryFailure(p)
				if ctx.Err() != nil {
					// Verified recovery cancels this monitor before replacing
					// the container; only the replacement monitor may proceed.
					return
				}
				if summary.Running {
					_ = s.db.SaveRunnerStatus(p.ID, "active")
				}
				pendingRegistration = false
				pendingDeletionVerification = false
				transitionStarted = time.Time{}
				resetTimer(runnerMonitorPollInterval)
			}
		}
	})
}

func (s *ProjectService) installRunnerMonitor(projectID string) (context.Context, bool) {
	s.runnerMonitorMu.Lock()
	defer s.runnerMonitorMu.Unlock()
	if s.runnerMonitors == nil {
		s.runnerMonitors = make(map[string]context.CancelFunc)
	}
	if _, exists := s.runnerMonitors[projectID]; exists {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.runnerMonitors[projectID] = cancel
	return ctx, true
}

func (s *ProjectService) stopRunnerMonitor(projectID string) {
	s.runnerMonitorMu.Lock()
	cancel := s.runnerMonitors[projectID]
	delete(s.runnerMonitors, projectID)
	s.runnerMonitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *ProjectService) lockRunnerLifecycle(projectID string) func() {
	s.runnerLifecycleMu.Lock()
	if s.runnerLifecycleLocks == nil {
		s.runnerLifecycleLocks = make(map[string]*sync.Mutex)
	}
	lock := s.runnerLifecycleLocks[projectID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.runnerLifecycleLocks[projectID] = lock
	}
	s.runnerLifecycleMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func isGitHubPersonalAccessToken(token string) bool {
	return strings.HasPrefix(token, "ghp_") ||
		strings.HasPrefix(token, "github_pat_") ||
		legacyGitHubPATRE.MatchString(token)
}

// runnerRegistrationToken exchanges a long-lived GitHub PAT in the controller.
// Registration tokens supplied directly by an operator remain supported.
func (s *ProjectService) runnerRegistrationToken(ctx context.Context, repoURL, credential string) (string, error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", fmt.Errorf("runner credential is empty")
	}
	if err := validateSingleLineSecret(credential); err != nil {
		return "", fmt.Errorf("invalid runner credential: %w", err)
	}
	if !isGitHubPersonalAccessToken(credential) {
		return credential, nil
	}

	if err := s.validateRepoURL(repoURL); err != nil {
		return "", fmt.Errorf("cannot request runner registration token: %w", err)
	}
	repository := repoOwnerRepo(repoURL)
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" {
		return "", fmt.Errorf("cannot request runner registration token: repository URL is invalid")
	}
	baseURL := strings.TrimRight(s.githubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runners/registration-token",
		baseURL, url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create GitHub runner registration request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := s.githubHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request GitHub runner registration token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return "", fmt.Errorf("request GitHub runner registration token: GitHub returned %s", resp.Status)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode GitHub runner registration token response: %w", err)
	}
	result.Token = strings.TrimSpace(result.Token)
	if result.Token == "" {
		return "", fmt.Errorf("GitHub runner registration token response did not contain a token")
	}
	if err := validateSingleLineSecret(result.Token); err != nil {
		return "", fmt.Errorf("GitHub returned an invalid runner registration token")
	}
	if isGitHubPersonalAccessToken(result.Token) {
		return "", fmt.Errorf("GitHub returned an unexpected long-lived credential")
	}
	return result.Token, nil
}

// waitForRunnerRegistrationRequest distinguishes an unconfigured runner from
// one whose persisted registration is already usable. No credential enters the
// container until the unconfigured entrypoint emits the request marker.
func (s *ProjectService) waitForRunnerRegistrationRequest(containerName string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		logs := s.docker.ContainerLogs(containerName, 50)
		if strings.Contains(logs, runnerRegistrationRequestMarker) {
			return true, nil
		}
		if strings.Contains(logs, "Runner already configured.") || strings.Contains(strings.ToLower(logs), "listening for jobs") {
			return false, nil
		}
		state := s.docker.ContainerSummary(containerName)
		if !state.Exists {
			return false, fmt.Errorf("runner container disappeared before registration")
		}
		if state.State != "running" && state.State != "created" && state.State != "restarting" {
			return false, fmt.Errorf("runner stopped before requesting registration (state=%s)", state.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, fmt.Errorf("runner did not request a registration credential")
}

// copyRunnerRegistrationToken streams the short-lived token through docker
// exec's standard input into the runner-owned registration tmpfs. Docker cp
// cannot target a tmpfs below a read-only container root filesystem. The token
// is never persisted on the controller, passed as a command argument, or made
// an environment variable. The runner observes it only after the atomic move.
func (s *ProjectService) copyRunnerRegistrationToken(ctx context.Context, containerName, token string) error {
	if err := validateSingleLineSecret(token); err != nil {
		return fmt.Errorf("invalid runner registration credential")
	}
	// The entrypoint runs as the unprivileged runner user, which owns the tmpfs.
	// Write a private staging file and rename it only after the complete stdin
	// stream arrives so it cannot consume a partially written credential.
	publish := exec.CommandContext(
		ctx,
		"docker", "exec", "-i", containerName,
		"sh", "-ceu",
		"umask 077; cat > \"$1\"; mv -f -- \"$1\" \"$2\"",
		"runner-registration-publisher", runnerRegistrationStagingPath, runnerRegistrationTokenPath,
	)
	publish.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	publish.Stdin = strings.NewReader(token)
	if output, err := publish.CombinedOutput(); err != nil {
		return fmt.Errorf("publish runner registration secret: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *ProjectService) startRunner(p *models.Project, runnerToken string) error {
	unlock := s.lockRunnerLifecycle(p.ID)
	defer unlock()
	return s.startRunnerLocked(p, runnerToken)
}

func (s *ProjectService) startRunnerLocked(p *models.Project, runnerToken string) error {
	return s.startRunnerContainerLocked(p, runnerToken)
}

func (s *ProjectService) runnerComposeEnvironment(p *models.Project) map[string]string {
	// Generate scoped runner token to avoid leaking the master deploy token
	mac := hmac.New(sha256.New, []byte(s.cfg.Token))
	mac.Write([]byte("runner:" + p.ID))
	scopedToken := hex.EncodeToString(mac.Sum(nil))
	return map[string]string{
		"REPO_URL":              p.RepoURL,
		"RUNNER_NAME":           fmt.Sprintf("runner-%s-%s", p.ID, branchSlug(p.BranchName)),
		"RUNNER_CONTAINER_NAME": p.RunnerContainer,
		"RUNNER_STATE_VOLUME":   fmt.Sprintf("%s-state", p.RunnerContainer),
		"RUNNER_LABELS":         runnerLabels(p),
		"DEPLOY_CONTROL_TOKEN":  scopedToken,
		"DEPLOY_CONTROL_URL":    s.cfg.RunnerControlURL,
		"RUNNER_NETWORK":        s.cfg.RunnerNetwork,
		"RUNNER_IMAGE":          s.cfg.RunnerImage,
	}
}

func (s *ProjectService) startRunnerContainerLocked(p *models.Project, runnerToken string) error {
	if err := s.stopRunnerLocked(p); err != nil {
		return err
	}
	composeFile := filepath.Join(s.cfg.ProjectRoot, "deploy", "runner", "docker-compose.runner.yml")
	if err := s.docker.ComposeUp(
		composeFile, fmt.Sprintf("devops-runner-%s", p.ID), s.runnerComposeEnvironment(p), "github-runner",
	); err != nil {
		return err
	}
	needsRegistration, err := s.waitForRunnerRegistrationRequest(p.RunnerContainer, 90*time.Second)
	if err != nil {
		s.docker.RemoveContainer(p.RunnerContainer)
		return err
	}
	if needsRegistration {
		registrationToken, err := s.runnerRegistrationToken(context.Background(), p.RepoURL, runnerToken)
		if err != nil {
			s.docker.RemoveContainer(p.RunnerContainer)
			return err
		}
		copyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = s.copyRunnerRegistrationToken(copyCtx, p.RunnerContainer, registrationToken)
		cancel()
		if err != nil {
			s.docker.RemoveContainer(p.RunnerContainer)
			return err
		}
	}

	// GitHub may retain a replaced runner's previous session briefly. The runner
	// client retries that transient conflict itself, so do not tear down a
	// correctly registered runner after only one retry interval.
	ready, state, logs := s.docker.WaitForRunnerReady(p.RunnerContainer, runnerReadyTimeout)
	if !ready {
		s.docker.RemoveContainer(p.RunnerContainer)
		return fmt.Errorf("runner did not become ready (state=%s): %s", state, truncateStr(logs, 200))
	}
	s.clearRunnerRecoveryFailure(p)
	s.startRunnerMonitor(p)
	return nil
}

func (s *ProjectService) recreateRunnerWithPreservedStateLocked(p *models.Project) error {
	composeFile := filepath.Join(s.cfg.ProjectRoot, "deploy", "runner", "docker-compose.runner.yml")
	if err := s.docker.ComposeUp(
		composeFile, fmt.Sprintf("devops-runner-%s", p.ID), s.runnerComposeEnvironment(p), "github-runner",
	); err != nil {
		return err
	}
	needsRegistration, err := s.waitForRunnerRegistrationRequest(p.RunnerContainer, 90*time.Second)
	if err != nil {
		s.docker.RemoveContainer(p.RunnerContainer)
		return err
	}
	if needsRegistration {
		s.docker.RemoveContainer(p.RunnerContainer)
		return fmt.Errorf("preserved runner state is unavailable while GitHub registration still exists")
	}
	return nil
}

func (s *ProjectService) stopRunner(p *models.Project) error {
	unlock := s.lockRunnerLifecycle(p.ID)
	defer unlock()
	return s.stopRunnerLocked(p)
}

func (s *ProjectService) stopRunnerLocked(p *models.Project) error {
	return s.stopRunnerResourcesLocked(p, true)
}

func (s *ProjectService) stopRunnerForRecoveryLocked(p *models.Project) error {
	return s.stopRunnerResourcesLocked(p, false)
}

func (s *ProjectService) stopRunnerResourcesLocked(p *models.Project, cancelMonitor bool) error {
	if cancelMonitor {
		s.stopRunnerMonitor(p.ID)
	}
	projectName := fmt.Sprintf("devops-runner-%s", p.ID)
	if summary := s.docker.ContainerSummary(p.RunnerContainer); !summary.Exists {
		return nil
	} else if err := s.docker.VerifyComposeOwnership(p.RunnerContainer, projectName, "github-runner"); err != nil {
		return fmt.Errorf("refusing to remove unowned runner container: %w", err)
	}
	if err := s.docker.ComposeDown(
		filepath.Join(s.cfg.ProjectRoot, "deploy", "runner", "docker-compose.runner.yml"),
		projectName,
		map[string]string{
			"RUNNER_IMAGE":          s.cfg.RunnerImage,
			"DEPLOY_CONTROL_URL":    s.cfg.RunnerControlURL,
			"RUNNER_NETWORK":        s.cfg.RunnerNetwork,
			"RUNNER_CONTAINER_NAME": p.RunnerContainer,
		},
	); err != nil {
		return fmt.Errorf("stop runner: %w", err)
	}
	return nil
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
			if err := s.stopRunner(p); err != nil {
				log.Printf("StopAllRunners: %v", err)
			}
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
			containerName, err := s.OwnedContainerName(p, svc)
			if err != nil {
				continue
			}
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
		runnerSummary := s.docker.ContainerSummary(p.RunnerContainer)
		if runnerSummary.Exists && (runnerSummary.Running || runnerSummary.State == "created" || runnerSummary.State == "restarting") {
			projectName := fmt.Sprintf("devops-runner-%s", p.ID)
			if err := s.docker.VerifyComposeOwnership(p.RunnerContainer, projectName, "github-runner"); err == nil {
				s.startRunnerMonitor(p)
			}
		} else if runnerSummary.Exists && (p.RunnerStatus == "active" || p.RunnerStatus == "starting") {
			projectName := fmt.Sprintf("devops-runner-%s", p.ID)
			if err := s.docker.VerifyComposeOwnership(p.RunnerContainer, projectName, "github-runner"); err != nil {
				log.Printf("Reconcile: refusing unowned runner %s: %v", p.ID, err)
			} else if err := s.docker.StartContainer(p.RunnerContainer); err != nil {
				log.Printf("Reconcile: failed to restart runner %s: %v", p.ID, err)
				_ = s.db.SaveRunnerStatus(p.ID, "error")
			} else {
				s.startRunnerMonitor(p)
			}
		} else if !runnerSummary.Exists && (p.RunnerStatus == "active" || p.RunnerStatus == "starting") {
			token, _ := s.db.GetRunnerToken(p.ID)
			if token == "" {
				token = s.cfg.GithubToken
			}
			if token == "" {
				log.Printf("Reconcile: runner %s should be active but has no configured credential", p.ID)
				_ = s.db.SaveRunnerStatus(p.ID, "error")
				continue
			}
			_ = s.db.SaveRunnerStatus(p.ID, "starting")
			project := p
			safeGo("reconcileRunner", func() {
				if err := s.startRunner(project, token); err != nil {
					log.Printf("Reconcile: failed to start runner %s: %v", project.ID, err)
					_ = s.db.SaveRunnerStatus(project.ID, "error")
					return
				}
				_ = s.db.SaveRunnerStatus(project.ID, "active")
			})
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

	existing, _ := s.db.GetRunnerToken(projectID)
	if existing != "" {
		log.Printf("SeedGithubToken: project %s already has a stored token, skipping", projectID)
		return
	}

	if err := s.db.SaveRunnerToken(projectID, token); err != nil {
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
	Key               string `json:"key"`
	Default           string `json:"default"`
	IsSecret          bool   `json:"is_secret"`
	OperatorRequired  bool   `json:"operator_required"`
	ControllerManaged bool   `json:"controller_managed"`
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
	repository := repoOwnerRepo(p.RepoURL)
	if repository == "" {
		return
	}
	dockerConfigDir := ""
	registryPassword, err := s.db.GetRegistryPassword(p.ID)
	if err != nil {
		log.Printf("ensureEnvTemplate: load registry credentials for %s: %v", p.ID, err)
		return
	}
	if registryPassword != "" {
		registryAuth, ok, message := s.docker.RegistryLoginIsolated(p.RegistryHost, p.RegistryUsername, registryPassword)
		if !ok {
			log.Printf("ensureEnvTemplate: registry login for %s failed: %s", p.ID, message)
			return
		}
		defer func() {
			if err := registryAuth.Close(); err != nil {
				log.Printf("ensureEnvTemplate: remove isolated registry credentials for %s: %v", p.ID, err)
			}
		}()
		dockerConfigDir = registryAuth.ConfigDir()
	}
	configImage := fmt.Sprintf("%s/%s-deploy-config:%s", p.RegistryHost, repository, p.BranchName)
	if out, err := s.extractFromConfigImage(configImage, p.AppDir, dockerConfigDir); err == nil {
		log.Printf("ensureEnvTemplate: extracted template from %s", configImage)
		return
	} else {
		log.Printf("ensureEnvTemplate: config image %s: %v (output: %s)", configImage, err, out)
	}

	// Fallback: a sibling checkout is supported for local development.
	sibling := filepath.Join(filepath.Dir(s.cfg.ProjectRoot), p.ID)
	localFiles := map[string]string{
		filepath.Join(sibling, ".env.template"):           templatePath,
		filepath.Join(sibling, "docker-compose.prod.yml"): filepath.Join(p.AppDir, "docker-compose.yml"),
		filepath.Join(sibling, "devops.json"):             filepath.Join(p.AppDir, "devops.json"),
	}
	for source, destination := range localFiles {
		if data, err := os.ReadFile(source); err == nil {
			_ = os.WriteFile(destination, data, 0o600)
		}
	}
}

func (s *ProjectService) extractFromConfigImage(image, destDir, dockerConfigDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pullCmd := exec.CommandContext(ctx, "docker", "pull", image)
	pullCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
	pullOut, pullErr := pullCmd.CombinedOutput()
	if pullErr != nil {
		return string(pullOut), pullErr
	}

	createCmd := exec.CommandContext(ctx, "docker", "create", image)
	createCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
	createOut, createErr := createCmd.CombinedOutput()
	if createErr != nil {
		return string(createOut), createErr
	}
	containerID := strings.TrimSpace(string(createOut))

	defer func() {
		removeCmd := exec.Command("docker", "rm", "-f", containerID)
		removeCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
		_ = removeCmd.Run()
	}()

	cpCmd := exec.CommandContext(ctx, "docker", "cp",
		fmt.Sprintf("%s:/app/.env.template", containerID),
		filepath.Join(destDir, ".env.template"))
	cpCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
	cpOut, cpErr := cpCmd.CombinedOutput()

	// Also extract the remaining project contract files. The configuration image
	// is the source of truth for a registry-backed project; without devops.json
	// the portal cannot apply its declared environment metadata.
	composePath := filepath.Join(destDir, "docker-compose.yml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		composeCmd := exec.CommandContext(ctx, "docker", "cp",
			fmt.Sprintf("%s:/app/docker-compose.yml", containerID),
			composePath)
		composeCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
		composeOut, composeErr := composeCmd.CombinedOutput()
		if composeErr != nil {
			return string(composeOut), composeErr
		}
	}
	devopsCmd := exec.CommandContext(ctx, "docker", "cp",
		fmt.Sprintf("%s:/app/devops.json", containerID),
		filepath.Join(destDir, "devops.json"))
	devopsCmd.Env = docker.CommandEnvForConfig(dockerConfigDir)
	devopsOut, devopsErr := devopsCmd.CombinedOutput()
	if devopsErr != nil {
		return string(devopsOut), devopsErr
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

	contract := projectEnvironmentContract(s.loadDevopsConfig(filepath.Join(p.AppDir, "devops.json")))
	secretKeys := map[string]bool{"password": true, "secret": true, "token": true, "key": true, "pass": true}
	var vars []EnvVar
	declared := make(map[string]bool)
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
		if !envKeyRE.MatchString(key) {
			continue
		}
		declared[key] = true
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
		if contract.nonSecret[key] {
			isSecret = false
		}
		if isSecret && def != "" {
			def = "********"
		}
		controllerManaged := contract.controllerManaged[key] || isReservedProjectEnvOverrideKey(key)
		operatorRequired := true // legacy projects retain strict blank-template gating
		if contract.present {
			operatorRequired = contract.operatorRequired[key] && !controllerManaged
		} else if controllerManaged {
			operatorRequired = false
		}
		vars = append(vars, EnvVar{
			Key:               key,
			Default:           def,
			IsSecret:          isSecret,
			OperatorRequired:  operatorRequired,
			ControllerManaged: controllerManaged,
		})
	}

	overrides := s.loadEnvOverrides(projectID)
	for key := range overrides {
		if !declared[key] || contract.controllerManaged[key] || isReservedProjectEnvOverrideKey(key) {
			delete(overrides, key)
		}
	}
	// Mask secret override values
	for k, v := range overrides {
		if v == "" {
			continue
		}
		if contract.nonSecret[k] {
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

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SaveEnvConfig validates and atomically patches the project's encrypted
// overrides. Omitted keys are preserved; deletion requires an explicit clear.
func (s *ProjectService) SaveEnvConfig(projectID string, overrides map[string]string, clearKeys []string) error {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return fmt.Errorf("project not found: %s", projectID)
	}
	data, err := os.ReadFile(filepath.Join(p.AppDir, ".env.template"))
	if err != nil {
		return fmt.Errorf("read environment template: %w", err)
	}
	allowed := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := strings.TrimSpace(strings.SplitN(line, "=", 2)[0])
		if envKeyRE.MatchString(key) {
			allowed[key] = true
		}
	}
	existing, err := s.db.GetProjectEnvOverrides(projectID)
	if err != nil {
		return fmt.Errorf("read saved project environment: %w", err)
	}
	if existing == nil {
		existing = make(map[string]string)
	}
	contract := projectEnvironmentContract(s.loadDevopsConfig(filepath.Join(p.AppDir, "devops.json")))
	clearSet := make(map[string]bool, len(clearKeys))
	explicitClearSet := make(map[string]bool, len(clearKeys))
	for _, key := range clearKeys {
		if !envKeyRE.MatchString(key) {
			return fmt.Errorf("invalid environment key %q in clear list", key)
		}
		clearSet[key] = true
		explicitClearSet[key] = true
	}
	// Remove legacy values that predate controller-owned namespace enforcement.
	// They must never survive merely because the UI no longer sends them.
	for key := range existing {
		if isReservedProjectEnvOverrideKey(key) || contract.controllerManaged[key] {
			clearSet[key] = true
		}
	}

	envPatch := make(map[string]string, len(overrides))
	for k, v := range overrides {
		if explicitClearSet[k] {
			return fmt.Errorf("environment key %q cannot be both set and cleared", k)
		}
		if isReservedProjectEnvOverrideKey(k) || contract.controllerManaged[k] {
			oldValue, existed := existing[k]
			if v == "" || v == "********" || (existed && v == oldValue) {
				clearSet[k] = true
				continue
			}
			return fmt.Errorf("environment key %q is controlled by the deployment system", k)
		}
		if !allowed[k] {
			return fmt.Errorf("environment key %q is not declared in .env.template", k)
		}
		if v == "********" {
			// A masked value is a UI sentinel, not a replacement secret.
			continue
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("environment value for %s must be a single line", k)
		}
		envPatch[k] = v
	}

	cleared := make([]string, 0, len(clearSet))
	for key := range clearSet {
		cleared = append(cleared, key)
	}
	sort.Strings(cleared)
	result, err := s.db.PatchProjectEnvOverrides(projectID, envPatch, cleared)
	if err != nil {
		return fmt.Errorf("patch project environment: %w", err)
	}
	if s.audit != nil {
		s.audit.Log(
			"project_env_patch",
			"ok",
			projectID,
			fmt.Sprintf(
				"added=%s updated=%s cleared=%s",
				strings.Join(result.Added, ","),
				strings.Join(result.Updated, ","),
				strings.Join(result.Cleared, ","),
			),
			"",
		)
	}
	return nil
}

func isDeploymentControllerEnvKey(key string) bool {
	switch key {
	case "DEPLOY_ID", "DEPLOY_REF", "DEPLOY_SHA", "DEPLOY_BRANCH", "IMAGE_TAG",
		"PROJECT_ID", "REPO_URL", "REGISTRY_HOST", "BASE_DIR", "PROJECT_LOG_DIR",
		"GITHUB_RUN_ID", "GITHUB_RUN_NUMBER", "GITHUB_ACTOR", "GITHUB_REPOSITORY",
		"GITHUB_WORKFLOW", "COMMIT_MESSAGE":
		return true
	default:
		return false
	}
}

func isReservedProjectEnvOverrideKey(key string) bool {
	if isDeploymentControllerEnvKey(key) {
		return true
	}
	for _, prefix := range [...]string{"DOCKER_", "COMPOSE_", "BUILDKIT_", "BUILDX_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case projectEnvOverridesFDEnv, "ENV_NAME",
		"PROJECT_DIR", "PROJECT_ENV_FILE", "PROJECT_COMPOSE_FILE", "PROJECT_DEVOPS_FILE", "DEVOPS_JSON",
		"PROJECT_STATE_DIR", "PROJECT_RELEASE_DIR", "DEPLOY_PROCESS_PID", "DEPLOY_ACTOR":
		return true
	default:
		return false
	}
}

func (s *ProjectService) loadEnvOverrides(projectID string) map[string]string {
	overrides, err := s.db.GetProjectEnvOverrides(projectID)
	if err == nil && overrides != nil {
		return overrides
	}
	if err != nil {
		log.Printf("load project environment %s: %v", projectID, err)
		return nil
	}

	return nil
}

// LoadEnvOverrides returns saved env var overrides for deploy injection
func (s *ProjectService) LoadEnvOverrides(projectID string) map[string]string {
	return s.loadEnvOverrides(projectID)
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
		token, _ := s.db.GetRunnerToken(projectID)
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
		if err := s.stopRunner(p); err != nil {
			return "", err
		}
		p.RunnerStatus = "stopped"
		_ = s.db.SaveRunnerStatus(p.ID, p.RunnerStatus)
		msg = "Runner container stopped."
	case "restart":
		if err := s.stopRunner(p); err != nil {
			return "", err
		}
		token, _ := s.db.GetRunnerToken(projectID)
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

func (s *ProjectService) getProjectPaths(p *models.Project) (string, string, string) {
	slug := branchSlug(p.BranchName)
	projectDir := filepath.Join(s.cfg.BaseDir, "Projects", p.ID)
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	envFile := filepath.Join(projectDir, ".env."+slug)
	projectName := fmt.Sprintf("%s-%s", p.ID, slug)
	return composeFile, envFile, projectName
}

func (s *ProjectService) ContainerAction(projectID, service, action string) error {
	if !serviceNameRE.MatchString(service) {
		return fmt.Errorf("invalid service name format")
	}

	p, err := s.db.GetProject(projectID)
	if err != nil {
		return err
	}

	containerName := s.FindContainerName(p.ID, service, p.BranchName)
	projectName := fmt.Sprintf("%s-%s", p.ID, branchSlug(p.BranchName))
	if err := s.docker.VerifyComposeOwnership(containerName, projectName, service); err != nil {
		return err
	}

	if action == "recreate" {
		s.audit.Log("container_recreate", "running", projectID, fmt.Sprintf("Recreating container for service %s", service), "")
		composeFile, envFile, projectName := s.getProjectPaths(p)
		slug := branchSlug(p.BranchName)

		overrides := s.loadEnvOverrides(projectID)
		if err := updateEnvFileWithOverrides(envFile, overrides, projectID, slug); err != nil {
			s.audit.Log("container_recreate", "failed", projectID, fmt.Sprintf("Failed to sync env overrides: %s", err.Error()), "")
			return err
		}
		if err := s.docker.ValidateComposeConfig(composeFile, projectName, envFile, []string{p.AppDir, s.LogDir(p)}); err != nil {
			s.audit.Log("container_recreate", "failed", projectID, err.Error(), "")
			return err
		}

		if err := s.docker.ComposeRecreate(composeFile, projectName, envFile, service); err != nil {
			s.audit.Log("container_recreate", "failed", projectID, err.Error(), "")
			return err
		}
		s.audit.Log("container_recreate", "ok", projectID, fmt.Sprintf("Recreated container for service %s", service), "")
		return nil
	}

	validActions := map[string]bool{
		"start":   true,
		"stop":    true,
		"restart": true,
		"pause":   true,
		"resume":  true,
	}
	if !validActions[action] {
		return fmt.Errorf("invalid action: %s", action)
	}

	auditKey := "container_" + action
	presentTense := action + "ing"
	if action == "stop" {
		presentTense = "stopping"
	} else if action == "resume" {
		presentTense = "resuming"
	}
	pastTense := action + "ed"
	if action == "resume" {
		pastTense = "resumed"
	}

	s.audit.Log(auditKey, "running", projectID, fmt.Sprintf("%s container for service %s", strings.Title(presentTense), service), "")
	if err := s.docker.ContainerAction(action, containerName); err != nil {
		s.audit.Log(auditKey, "failed", projectID, err.Error(), "")
		return err
	}
	s.audit.Log(auditKey, "ok", projectID, fmt.Sprintf("%s container for service %s", strings.Title(pastTense), service), "")
	return nil
}

// FindContainerName resolves a project service through exact Compose labels.
// The deterministic fallback is only used to report unavailable state.
func (s *ProjectService) FindContainerName(projectID, service, branch string) string {
	projectName := fmt.Sprintf("%s-%s", projectID, branchSlug(branch))
	if name, err := s.docker.FindComposeContainer(projectName, service); err == nil && name != "" {
		return name
	}
	return DeploymentContainerName(service, branch, projectID)
}

func (s *ProjectService) OwnedContainerName(p *models.Project, service string) (string, error) {
	if !serviceNameRE.MatchString(service) {
		return "", fmt.Errorf("invalid service name format")
	}
	projectName := fmt.Sprintf("%s-%s", p.ID, branchSlug(p.BranchName))
	name, err := s.docker.FindComposeContainer(projectName, service)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("service %s has no container owned by Compose project %s", service, projectName)
	}
	return name, nil
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

func (s *ProjectService) CheckServiceHealth(p *models.Project, service, containerName string, summary *models.ContainerState) *models.ServiceHealth {
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
	composeFile := filepath.Join(p.AppDir, "devops.json")
	if devopsCfg := s.loadDevopsConfig(composeFile); devopsCfg != nil && devopsCfg.Services != nil {
		if svcCfg, ok := devopsCfg.Services[service].(map[string]interface{}); ok {
			if healthCfg, ok := svcCfg["health"].(map[string]interface{}); ok {
				var port int
				if pFloat, ok := healthCfg["port"].(float64); ok {
					port = int(pFloat)
				} else if pInt, ok := healthCfg["port"].(int); ok {
					port = pInt
				}
				path, _ := healthCfg["path"].(string)
				if port > 0 && port <= 65535 && strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "\r\n\x00") {
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

type DevopsConfig struct {
	Version     string                   `json:"version"`
	ProjectName string                   `json:"project_name"`
	ComposeFile string                   `json:"compose_file"`
	EnvTemplate string                   `json:"env_template"`
	Services    map[string]interface{}   `json:"services"`
	Environment *DevopsEnvironmentConfig `json:"environment"`
	Logs        *DevopsLogsConfig        `json:"logs"`
}

// DevopsEnvironmentConfig separates operator input from values deploy-control
// generates or derives. Its keys must also be declared in .env.template.
type DevopsEnvironmentConfig struct {
	OperatorRequired  []string `json:"operator_required"`
	ControllerManaged []string `json:"controller_managed"`
	GeneratedSecrets  []string `json:"generated_secrets"`
	NonSecret         []string `json:"non_secret"`
}

type environmentContract struct {
	present           bool
	operatorRequired  map[string]bool
	controllerManaged map[string]bool
	nonSecret         map[string]bool
}

func projectEnvironmentContract(cfg *DevopsConfig) environmentContract {
	contract := environmentContract{
		operatorRequired:  make(map[string]bool),
		controllerManaged: make(map[string]bool),
		nonSecret:         make(map[string]bool),
	}
	if cfg == nil || cfg.Environment == nil {
		return contract
	}
	contract.present = true
	for _, key := range cfg.Environment.OperatorRequired {
		if envKeyRE.MatchString(key) {
			contract.operatorRequired[key] = true
		}
	}
	for _, key := range append(cfg.Environment.ControllerManaged, cfg.Environment.GeneratedSecrets...) {
		if envKeyRE.MatchString(key) {
			contract.controllerManaged[key] = true
		}
	}
	for _, key := range cfg.Environment.NonSecret {
		if envKeyRE.MatchString(key) {
			contract.nonSecret[key] = true
		}
	}
	return contract
}

type DevopsLogsConfig struct {
	Directory         string            `json:"directory"`
	ContainerInternal map[string]string `json:"container_internal"`
}

func (s *ProjectService) LogDir(p *models.Project) string {
	defaultDir := filepath.Join(s.cfg.BaseDir, "Logs", p.ID)
	devopsPath := filepath.Join(p.AppDir, "devops.json")
	cfg := s.loadDevopsConfig(devopsPath)
	if cfg != nil && cfg.Logs != nil && cfg.Logs.Directory != "" {
		candidate := filepath.Clean(filepath.Join(defaultDir, cfg.Logs.Directory))
		if candidate == defaultDir || strings.HasPrefix(candidate, defaultDir+string(filepath.Separator)) {
			return candidate
		}
		log.Printf("Ignoring unsafe logs.directory %q for project %s", cfg.Logs.Directory, p.ID)
	}
	return defaultDir
}

func (s *ProjectService) ContainerLogFiles(p *models.Project) map[string]string {
	devopsPath := filepath.Join(p.AppDir, "devops.json")
	cfg := s.loadDevopsConfig(devopsPath)
	if cfg == nil || cfg.Logs == nil || cfg.Logs.ContainerInternal == nil {
		return nil
	}
	result := make(map[string]string)
	for svc, path := range cfg.Logs.ContainerInternal {
		clean := filepath.Clean(path)
		if serviceNameRE.MatchString(svc) && strings.HasPrefix(clean, "/logs/") {
			result[svc] = clean
		}
	}
	return result
}

func (s *ProjectService) loadDevopsConfig(path string) *DevopsConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg DevopsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// composeServices returns service names for a project by reading docker-compose.yml
func (s *ProjectService) composeServices(p *models.Project) []string {
	// devops.json is the project's monitoring contract. Compose files can also
	// contain one-shot migration or initialization services that should not be
	// reported as continuously unhealthy after they complete successfully.
	devopsPath := filepath.Join(p.AppDir, "devops.json")
	if cfg := s.loadDevopsConfig(devopsPath); cfg != nil && len(cfg.Services) > 0 {
		services := make([]string, 0, len(cfg.Services))
		for name := range cfg.Services {
			if serviceNameRE.MatchString(name) {
				services = append(services, name)
			}
		}
		if len(services) > 0 {
			sort.Strings(services)
			return services
		}
	}

	composeFile := filepath.Join(p.AppDir, "docker-compose.yml")
	svcNames, err := s.docker.ComposeServiceNames(composeFile)
	if err != nil || len(svcNames) == 0 {
		return nil
	}
	sort.Strings(svcNames)
	return svcNames
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func updateEnvFileWithOverrides(envFile string, overrides map[string]string, projectID, branchSlug string) error {
	data, err := os.ReadFile(envFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = nil
	}

	lines := strings.Split(string(data), "\n")
	envMap := make(map[string]string)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	for k, v := range overrides {
		envMap[k] = v
	}

	if projectID != "" && branchSlug != "" {
		envMap["COMPOSE_PROJECT_NAME"] = fmt.Sprintf("%s-%s", projectID, branchSlug)
		envMap["ENV_NAME"] = branchSlug
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	newLines := make([]string, 0, len(keys))
	for _, key := range keys {
		newLines = append(newLines, fmt.Sprintf("%s=%s", key, envMap[key]))
	}
	return writeEnvFileAtomically(envFile, []byte(strings.Join(newLines, "\n")+"\n"))
}

func writeEnvFileAtomically(envFile string, content []byte) error {
	dir := filepath.Dir(envFile)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(envFile)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, envFile); err != nil {
		return err
	}
	keepTemp = false
	return nil
}
