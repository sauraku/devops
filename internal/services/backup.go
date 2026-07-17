package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

type BackupService struct {
	db     *db.DB
	docker *docker.Client
	audit  *AuditService
	cfg    *models.Config
}

type composeServiceTarget struct {
	service string
	envKey  string
}

var (
	backupComposeTargets = []composeServiceTarget{
		{service: "postgres", envKey: "POSTGRES_CONTAINER"},
	}
	restoreComposeTargets = []composeServiceTarget{
		{service: "postgres", envKey: "POSTGRES_CONTAINER"},
	}
)

func restoreCommandJSON(appDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(appDir, "devops.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read devops.json: %w", err)
	}
	var config struct {
		Services map[string]struct {
			Restore *struct {
				Command json.RawMessage `json:"command"`
			} `json:"restore"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("parse devops.json: %w", err)
	}
	backend, ok := config.Services["backend"]
	if !ok || backend.Restore == nil || len(backend.Restore.Command) == 0 {
		return "", nil
	}
	var command []string
	if err := json.Unmarshal(backend.Restore.Command, &command); err != nil || len(command) == 0 {
		return "", fmt.Errorf("services.backend.restore.command must be a non-empty JSON string array")
	}
	for _, arg := range command {
		if arg == "" || strings.ContainsRune(arg, '\x00') {
			return "", fmt.Errorf("services.backend.restore.command contains an invalid argument")
		}
	}
	encoded, _ := json.Marshal(command)
	return string(encoded), nil
}

func NewBackupService(database *db.DB, dockerClient *docker.Client, audit *AuditService, cfg *models.Config) *BackupService {
	return &BackupService{
		db:     database,
		docker: dockerClient,
		audit:  audit,
		cfg:    cfg,
	}
}

func (s *BackupService) ownedComposeTargetEnv(projectID, branch string, targets []composeServiceTarget) (map[string]string, error) {
	composeProject := fmt.Sprintf("%s-%s", projectID, branchSlug(branch))
	env := map[string]string{"COMPOSE_PROJECT_NAME": composeProject}
	for _, target := range targets {
		containerID, err := s.ownedComposeContainerID(composeProject, target.service)
		if err != nil {
			return nil, err
		}
		env[target.envKey] = containerID
	}
	return env, nil
}

func (s *BackupService) ownedComposeContainerID(composeProject, service string) (string, error) {
	containerName, err := s.docker.FindComposeContainer(composeProject, service)
	if err != nil {
		return "", fmt.Errorf("resolve Compose service %s/%s: %w", composeProject, service, err)
	}
	if containerName == "" {
		return "", fmt.Errorf("Compose service %s/%s has no owned container", composeProject, service)
	}
	info, err := s.docker.InspectContainer(containerName)
	if err != nil {
		return "", fmt.Errorf("inspect Compose service %s/%s: %w", composeProject, service, err)
	}
	containerID, _ := info["Id"].(string)
	if containerID == "" {
		return "", fmt.Errorf("inspect Compose service %s/%s: container id is missing", composeProject, service)
	}
	if err := s.docker.VerifyComposeOwnership(containerID, composeProject, service); err != nil {
		return "", fmt.Errorf("verify Compose service %s/%s: %w", composeProject, service, err)
	}
	return containerID, nil
}

func (s *BackupService) Create(projectID, branch, reason string) (*models.Deployment, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}
	branch = normalizeRef(branch)
	if branch == "" {
		branch = p.BranchName
	}
	branchEnv := branchSlug(branch)
	targetEnv, err := s.ownedComposeTargetEnv(projectID, branch, backupComposeTargets)
	if err != nil {
		return nil, err
	}

	deployID := fmt.Sprintf("backup-%s-%d", projectID, time.Now().UnixMilli())
	logDir := filepath.Join(s.cfg.BaseDir, "Logs", projectID)
	os.MkdirAll(logDir, 0o750)
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	backupDir := filepath.Join(s.cfg.BaseDir, "Backups", projectID)
	os.MkdirAll(backupDir, 0o750)
	manifestFile := filepath.Join(backupDir, "manifest.jsonl")

	env := map[string]string{
		"DEPLOY_ID":            deployID,
		"TARGET_BRANCH":        branch,
		"BACKUP_KIND":          "manual",
		"BACKUP_REASON":        reason,
		"BACKUP_DIR_PATH":      backupDir,
		"BACKUP_MANIFEST_FILE": manifestFile,
		"ENV_FILE":             filepath.Join(s.cfg.BaseDir, "Projects", projectID, ".env."+branchEnv),
		"BASE_DIR":             s.cfg.BaseDir,
		"PROJECT_ID":           projectID,
		"PROJECT_DIR":          p.AppDir,
		"PROJECT_STATE_DIR":    filepath.Join(s.cfg.DataDir, projectID),
	}
	for key, value := range targetEnv {
		env[key] = value
	}

	deployment := &models.Deployment{
		ID:        deployID,
		ProjectID: projectID,
		Kind:      models.DeploymentKindBackup,
		Status:    models.DeploymentStatusRunning,
		Branch:    branch,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}

	if err := s.db.CreateDeployment(deployment); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	lock := &models.DeployLock{
		ProjectID:   projectID,
		OperationID: deployID,
		Operation:   "backup",
		StartedAt:   deployment.StartedAt.Format(time.RFC3339),
		Branch:      branch,
	}
	if err := s.db.CreateLock(lock); err != nil {
		_ = s.db.UpdateDeploymentStatus(deployID, models.DeploymentStatusFailed, nil, nil)
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	s.audit.Log("backup_started", "running", projectID, fmt.Sprintf("backup=%s branch=%s", deployID, branch), "")

	safeGo("runBackup", func() { s.runBackup(deployment, p, env) })

	return deployment, nil
}

func (s *BackupService) runBackup(d *models.Deployment, p *models.Project, env map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	script := filepath.Join(s.cfg.ProjectRoot, "scripts", "backup-db.sh")
	appDir := p.AppDir
	args := []string{script}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = appDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	logFile, err := os.Create(d.LogPath)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	err = cmd.Run()
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
	s.audit.Log("backup_finished", string(status), p.ID, fmt.Sprintf("backup=%s exit_code=%d", d.ID, exitCode), "")

	// Sync manifest entries into the DB so restore/verify can find them
	if exitCode == 0 {
		manifestFile := filepath.Join(s.cfg.BaseDir, "Backups", p.ID, "manifest.jsonl")
		s.syncManifestToDB(p.ID, manifestFile)
		s.CleanupOldBackups(p.ID)
	}

	if logFile != nil {
		logFile.Close()
	}
}

func (s *BackupService) syncManifestToDB(projectID, manifestFile string) {
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			BackupID     string `json:"backup_id"`
			Kind         string `json:"kind"`
			Reason       string `json:"reason"`
			SHA256       string `json:"sha256"`
			SizeBytes    int64  `json:"size_bytes"`
			Timestamp    string `json:"timestamp"`
			FinishedAt   string `json:"finished_at"`
			EnvName      string `json:"env_name"`
			Storage      string `json:"storage"`
			FirebaseURL  string `json:"firebase_url"`
			LocalPath    string `json:"local_path"`
			Verification string `json:"verification_status,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		filePath := entry.LocalPath
		if entry.FirebaseURL != "" {
			filePath = entry.FirebaseURL
		}
		if filePath == "" {
			filePath = fmt.Sprintf("%s.dump.gz", entry.BackupID)
		}
		b := &models.Backup{
			ID:                 entry.BackupID,
			ProjectID:          projectID,
			FilePath:           filePath,
			SHA256:             entry.SHA256,
			SizeBytes:          entry.SizeBytes,
			Timestamp:          entry.Timestamp,
			VerificationStatus: "verified",
			EnvName:            entry.EnvName,
		}
		if entry.Verification != "" {
			b.VerificationStatus = entry.Verification
		}
		_ = s.db.CreateBackup(b)
	}
}

func (s *BackupService) Verify(projectID, backupID string) (*models.BackupVerifyResult, error) {
	backup, err := s.db.GetBackup(backupID, projectID)
	if err != nil {
		return &models.BackupVerifyResult{OK: false, Message: "backup id not found"}, nil
	}

	backupDir := filepath.Join(s.cfg.BaseDir, "Backups", projectID)
	filePath, err := safeBackupPath(backupDir, backup.FilePath)
	if err != nil {
		return &models.BackupVerifyResult{OK: false, Message: err.Error(), Backup: backup}, nil
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return &models.BackupVerifyResult{OK: false, Message: "backup archive is missing", Backup: backup}, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return &models.BackupVerifyResult{OK: false, Message: fmt.Sprintf("cannot open backup: %s", err), Backup: backup}, nil
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return &models.BackupVerifyResult{OK: false, Message: fmt.Sprintf("checksum failed: %s", err), Backup: backup}, nil
	}
	digest := fmt.Sprintf("%x", h.Sum(nil))
	if digest != backup.SHA256 {
		_ = s.db.UpdateBackupVerification(backupID, "checksum_mismatch")
		return &models.BackupVerifyResult{OK: false, Message: "sha256 mismatch", Backup: backup}, nil
	}

	_ = s.db.UpdateBackupVerification(backupID, "verified")

	if _, err := exec.LookPath("pg_restore"); err == nil && strings.HasSuffix(filePath, ".gz") {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		gzipCmd := exec.CommandContext(ctx, "gzip", "-dc", filePath)
		restoreCmd := exec.CommandContext(ctx, "pg_restore", "--list")
		pipe, _ := gzipCmd.StdoutPipe()
		restoreCmd.Stdin = pipe
		gzipCmd.Stderr = nil
		restoreCmd.Stderr = nil
		if err := gzipCmd.Start(); err == nil {
			if err := restoreCmd.Run(); err != nil {
				return &models.BackupVerifyResult{OK: false, Message: "pg_restore --list failed", Backup: backup}, nil
			}
			if err := gzipCmd.Wait(); err != nil {
				return &models.BackupVerifyResult{OK: false, Message: "backup decompression failed", Backup: backup}, nil
			}
		}
	}

	return &models.BackupVerifyResult{OK: true, Message: "backup verified", Backup: backup}, nil
}

func safeBackupPath(backupDir, storedPath string) (string, error) {
	if storedPath == "" || strings.Contains(storedPath, "://") {
		return "", fmt.Errorf("backup is not available in local storage")
	}
	path := storedPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(backupDir, path)
	}
	path = filepath.Clean(path)
	root := filepath.Clean(backupDir)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("backup path escapes the project backup directory")
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			return "", fmt.Errorf("backup path resolves outside the project backup directory")
		}
		path = resolved
	}
	return path, nil
}

func (s *BackupService) DryRunRestore(projectID, backupID string) (*models.BackupVerifyResult, error) {
	result, err := s.Verify(projectID, backupID)
	if err != nil || !result.OK {
		return result, err
	}

	backupDir := filepath.Join(s.cfg.BaseDir, "Backups", projectID)
	filePath, pathErr := safeBackupPath(backupDir, result.Backup.FilePath)
	if pathErr != nil {
		result.OK = false
		result.Message = pathErr.Error()
		return result, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gzip", "-dc", filePath)
	listCmd := exec.CommandContext(ctx, "pg_restore", "--list")
	pipe, _ := cmd.StdoutPipe()
	listCmd.Stdin = pipe
	var out bytes.Buffer
	listCmd.Stdout = &out

	if err := cmd.Start(); err != nil {
		result.Message = fmt.Sprintf("cannot decompress backup: %v", err)
		return result, nil
	}
	listErr := listCmd.Run()
	gzipErr := cmd.Wait()

	if listErr != nil || gzipErr != nil {
		result.OK = false
		result.Message = fmt.Sprintf("backup inspection failed: pg_restore=%v gzip=%v", listErr, gzipErr)
		return result, nil
	}

	var tables []models.BackupTableInfo
	backupTableSet := make(map[string]bool)
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		semiParts := strings.SplitN(line, ";", 2)
		if len(semiParts) < 2 {
			continue
		}
		rest := strings.Fields(semiParts[1])
		if len(rest) < 4 {
			continue
		}
		if rest[2] != "TABLE" || rest[3] == "DATA" {
			continue
		}
		name := rest[3]
		if len(rest) > 4 {
			name = rest[3] + "." + rest[4]
		}
		key := strings.TrimPrefix(name, "public.")
		if backupTableSet[key] {
			continue
		}
		backupTableSet[key] = true
	}

	currentRows := make(map[string]int64)
	if p, err := s.db.GetProject(projectID); err == nil {
		dbName, dbUser := s.getBackupConfig(p)
		composeProject := fmt.Sprintf("%s-%s", p.ID, branchSlug(p.BranchName))
		pgContainer, ownershipErr := s.ownedComposeContainerID(composeProject, "postgres")
		if ownershipErr != nil {
			result.OK = false
			result.Message = ownershipErr.Error()
			return result, nil
		}
		queryCmd := exec.CommandContext(ctx, "docker", "exec", pgContainer,
			"psql", "-U", dbUser, "-d", dbName, "-t", "-c",
			"SELECT schemaname || '.' || relname, n_live_tup FROM pg_stat_user_tables ORDER BY relname")
		var out2 bytes.Buffer
		queryCmd.Stdout = &out2
		if err := queryCmd.Run(); err == nil {
			for _, line := range strings.Split(out2.String(), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					var rows int64
					fmt.Sscanf(parts[1], "%d", &rows)
					name := strings.TrimPrefix(parts[0], "public.")
					if strings.Contains(name, "|") {
						name = strings.TrimSpace(strings.Split(name, "|")[0])
					}
					currentRows[name] = rows
				}
			}
		}
	}

	for name := range backupTableSet {
		rows, exists := currentRows[name]
		delete(currentRows, name)
		action := "overwrite"
		if !exists {
			action = "new"
		}
		tables = append(tables, models.BackupTableInfo{
			Name: name,
			Kind: action,
			Rows: fmt.Sprintf("%d → ?", rows),
		})
	}
	for name := range currentRows {
		tables = append(tables, models.BackupTableInfo{
			Name: name,
			Kind: "dropped",
			Rows: fmt.Sprintf("%d → 0", currentRows[name]),
		})
	}
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})

	result.TableList = tables
	return result, nil
}

func (s *BackupService) Restore(projectID, backupID string) (*models.Deployment, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}
	targetEnv, err := s.ownedComposeTargetEnv(projectID, p.BranchName, restoreComposeTargets)
	if err != nil {
		return nil, err
	}

	deployID := fmt.Sprintf("restore-%s-%d", projectID, time.Now().UnixMilli())
	logDir := filepath.Join(s.cfg.BaseDir, "Logs", projectID)
	os.MkdirAll(logDir, 0o750)
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	backupDir := filepath.Join(s.cfg.BaseDir, "Backups", projectID)
	manifestFile := filepath.Join(backupDir, "manifest.jsonl")

	env := map[string]string{
		"DEPLOY_ID":            deployID,
		"BACKUP_ID":            backupID,
		"BACKUP_DIR_PATH":      backupDir,
		"BACKUP_MANIFEST_FILE": manifestFile,
		"ENV_FILE":             filepath.Join(s.cfg.BaseDir, "Projects", projectID, ".env."+branchSlug(p.BranchName)),
		"COMPOSE_FILE":         filepath.Join(s.cfg.BaseDir, "Projects", projectID, "docker-compose.yml"),
		"BASE_DIR":             s.cfg.BaseDir,
		"PROJECT_ID":           projectID,
		"PROJECT_DIR":          p.AppDir,
		"PROJECT_STATE_DIR":    filepath.Join(s.cfg.DataDir, projectID),
		"TARGET_BRANCH":        p.BranchName,
	}
	for key, value := range targetEnv {
		env[key] = value
	}
	if commandJSON, err := restoreCommandJSON(p.AppDir); err != nil {
		return nil, err
	} else if commandJSON != "" {
		env["RESTORE_COMMAND_JSON"] = commandJSON
	}

	deployment := &models.Deployment{
		ID:        deployID,
		ProjectID: projectID,
		Kind:      models.DeploymentKindRestore,
		Status:    models.DeploymentStatusRunning,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}

	if err := s.db.CreateDeployment(deployment); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	lock := &models.DeployLock{
		ProjectID:   projectID,
		OperationID: deployID,
		Operation:   "restore",
		StartedAt:   deployment.StartedAt.Format(time.RFC3339),
	}
	if err := s.db.CreateLock(lock); err != nil {
		_ = s.db.UpdateDeploymentStatus(deployID, models.DeploymentStatusFailed, nil, nil)
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	s.audit.Log("restore_started", "running", projectID, fmt.Sprintf("restore=%s backup=%s", deployID, backupID), "")

	safeGo("runRestore", func() { s.runRestore(deployment, p, env, backupID) })

	return deployment, nil
}

func (s *BackupService) runRestore(d *models.Deployment, p *models.Project, env map[string]string, backupID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	script := filepath.Join(s.cfg.ProjectRoot, "scripts", "restore-db.sh")
	args := []string{script, backupID}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = p.AppDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	logFile, err := os.Create(d.LogPath)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	err = cmd.Run()
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
	s.audit.Log("restore_finished", string(status), p.ID, fmt.Sprintf("restore=%s exit_code=%d", d.ID, exitCode), "")

	if logFile != nil {
		logFile.Close()
	}
}

func (s *BackupService) ListBackups(projectID string, limit int) ([]*models.Backup, error) {
	return s.db.ListBackups(projectID, limit)
}

func (s *BackupService) Rollback(projectID, commit string) (*models.Deployment, error) {
	p, err := s.db.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}
	if p.DeploymentMode != models.DeploymentModeLocalRepo {
		return nil, fmt.Errorf("rollback is only available for local_repo projects")
	}
	if password, err := s.db.GetRegistryPassword(projectID); err != nil {
		return nil, fmt.Errorf("load registry credentials: %w", err)
	} else if password != "" {
		if ok, message := s.docker.RegistryLogin(p.RegistryHost, p.RegistryUsername, password); !ok {
			return nil, fmt.Errorf("registry login failed: %s", message)
		}
	}

	deployID := fmt.Sprintf("rollback-%s-%d", projectID, time.Now().UnixMilli())
	logDir := filepath.Join(s.cfg.BaseDir, "Logs", projectID)
	os.MkdirAll(logDir, 0o750)
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", deployID))

	env := map[string]string{
		"DEPLOY_ID":       deployID,
		"BASE_DIR":        s.cfg.BaseDir,
		"PROJECT_ID":      projectID,
		"REPO_URL":        p.RepoURL,
		"REGISTRY_HOST":   p.RegistryHost,
		"PROJECT_LOG_DIR": filepath.Join(s.cfg.BaseDir, "Logs", projectID),
	}

	deployment := &models.Deployment{
		ID:        deployID,
		ProjectID: projectID,
		Kind:      models.DeploymentKindRollback,
		Status:    models.DeploymentStatusRunning,
		SHA:       commit,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}

	if err := s.db.CreateDeployment(deployment); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	lock := &models.DeployLock{
		ProjectID:   projectID,
		OperationID: deployID,
		Operation:   "rollback",
		StartedAt:   deployment.StartedAt.Format(time.RFC3339),
		SHA:         commit,
	}
	if err := s.db.CreateLock(lock); err != nil {
		_ = s.db.UpdateDeploymentStatus(deployID, models.DeploymentStatusFailed, nil, nil)
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	s.audit.Log("rollback_started", "running", projectID, fmt.Sprintf("rollback=%s commit=%s", deployID, commit), "")

	safeGo("runRollback", func() { s.runRollback(deployment, p, env, commit) })

	return deployment, nil
}

func (s *BackupService) runRollback(d *models.Deployment, p *models.Project, env map[string]string, commit string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	script := filepath.Join(s.cfg.ProjectRoot, "scripts", "rollback.sh")
	args := []string{script, commit}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = p.AppDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	logFile, err := os.Create(d.LogPath)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	err = cmd.Run()
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
	s.audit.Log("rollback_finished", string(status), p.ID, fmt.Sprintf("rollback=%s exit_code=%d", d.ID, exitCode), "")

	if logFile != nil {
		logFile.Close()
	}
}

func (s *BackupService) StartScheduler() {
	if s.cfg.BackupSchedule == "" || s.cfg.BackupSchedule == "off" {
		return
	}
	log.Printf("Backup scheduler: daily at %s UTC, retaining last %d backups per project", s.cfg.BackupSchedule, s.cfg.BackupRetention)
	safeGo("backupScheduler", func() {
		for {
			now := time.Now().UTC()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			if sched := s.cfg.BackupSchedule; len(sched) == 5 {
				if h, m, err := parseHM(sched); err == nil {
					target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
					if now.After(target) {
						target = target.Add(24 * time.Hour)
					}
					next = target
				}
			}
			wait := next.Sub(now)
			log.Printf("Backup scheduler: next run at %s (in %s)", next.Format(time.RFC3339), wait.Round(time.Second))
			time.Sleep(wait)

			log.Printf("Backup scheduler: running scheduled backups")
			projects, err := s.db.ListProjects()
			if err != nil {
				log.Printf("Backup scheduler: failed to list projects: %v", err)
				continue
			}
			for _, p := range projects {
				if _, err := s.Create(p.ID, p.BranchName, "scheduled"); err != nil {
					log.Printf("Backup scheduler: failed for %s: %v", p.ID, err)
				}
			}
		}
	})
}

func parseHM(s string) (int, int, error) {
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err == nil && (n != 2 || h < 0 || h > 23 || m < 0 || m > 59) {
		err = fmt.Errorf("time must be HH:MM in UTC")
	}
	return h, m, err
}

func (s *BackupService) CleanupOldBackups(projectID string) {
	if s.cfg.BackupRetention < 1 {
		log.Printf("Backup cleanup disabled: BACKUP_RETENTION must be at least 1")
		return
	}
	backups, err := s.db.ListBackups(projectID, 0)
	if err != nil || len(backups) <= s.cfg.BackupRetention {
		return
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Timestamp > backups[j].Timestamp
	})
	for _, b := range backups[s.cfg.BackupRetention:] {
		log.Printf("Backup cleanup: removing old backup %s (%s)", b.ID, b.Timestamp)
		if b.FilePath != "" && !strings.Contains(b.FilePath, "://") {
			if backupPath, err := safeBackupPath(filepath.Join(s.cfg.BaseDir, "Backups", projectID), b.FilePath); err == nil {
				_ = os.Remove(backupPath)
			} else {
				log.Printf("Backup cleanup: refusing unsafe path for %s: %v", b.ID, err)
				continue
			}
		}
		_ = s.db.DeleteBackup(b.ID, projectID)
	}
}

func (s *BackupService) RunRetentionForAll() {
	projects, err := s.db.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		s.CleanupOldBackups(p.ID)
	}
}

// getBackupConfig reads database name and user from devops.json, falling back
// to sensible defaults.
func (s *BackupService) getBackupConfig(p *models.Project) (dbName, dbUser string) {
	dbName = fmt.Sprintf("%s-db", p.ID)
	dbUser = "postgres"

	devopsPath := filepath.Join(p.AppDir, "devops.json")
	data, err := os.ReadFile(devopsPath)
	if err != nil {
		return
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	svcs, _ := cfg["services"].(map[string]interface{})
	if svcs == nil {
		return
	}
	if pg, ok := svcs["postgres"].(map[string]interface{}); ok {
		if backup, ok := pg["backup"].(map[string]interface{}); ok {
			if d, ok := backup["database"].(string); ok && d != "" {
				dbName = d
			}
			if u, ok := backup["user"].(string); ok && u != "" {
				dbUser = u
			}
		}
	}
	return
}
