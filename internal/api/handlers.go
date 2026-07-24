package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dockerclient "github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
	"github.com/sauraku/devops-control/internal/services"
)

type Handler struct {
	projects        *services.ProjectService
	deploys         *services.DeployService
	backups         *services.BackupService
	audit           *services.AuditService
	auth            *Authenticator
	cfg             *models.Config
	requestTrust    *RequestTrust
	terminalLimiter *terminalAttemptLimiter
}

func NewHandler(
	projectService *services.ProjectService,
	deployService *services.DeployService,
	backupService *services.BackupService,
	auditService *services.AuditService,
	auth *Authenticator,
	cfg *models.Config,
	requestTrust *RequestTrust,
) *Handler {
	if requestTrust == nil {
		panic("request trust policy is required")
	}
	if auth != nil && auth.requestTrust != requestTrust {
		panic("authenticator and handler must share the request trust policy")
	}
	return &Handler{
		projects:        projectService,
		deploys:         deployService,
		backups:         backupService,
		audit:           auditService,
		auth:            auth,
		cfg:             cfg,
		requestTrust:    requestTrust,
		terminalLimiter: newTerminalAttemptLimiter(),
	}
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) DebugInfo(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.projects.List()

	type projectDebug struct {
		ID            string
		Containers    map[string]string
		ServiceHealth map[string]*models.ServiceHealth
	}
	var projectDetails []projectDebug
	for _, p := range projects {
		pd := projectDebug{ID: p.ID, Containers: make(map[string]string), ServiceHealth: make(map[string]*models.ServiceHealth)}
		status, err := h.projects.Status(p.ID)
		if err == nil {
			pd.Containers = status.Containers["current"]
			pd.ServiceHealth = status.Health
		}
		projectDetails = append(projectDetails, pd)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": time.Now().UTC().Format(time.RFC3339),
		"details":     projectDetails,
	})
}

func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := h.projects.List()
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects":           projects,
		"default_project_id": h.cfg.DefaultProjectID,
	})
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	var req models.ProjectRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	project, err := h.projects.CreateOrUpdate(&req)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "project configuration is invalid", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"project": project,
	})
}

func (h *Handler) ProjectStatus(w http.ResponseWriter, r *http.Request, projectID string) {
	status, err := h.projects.Status(projectID)
	if err != nil {
		serviceError(w, http.StatusNotFound, "project not found", err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) ProjectDeploy(w http.ResponseWriter, r *http.Request, projectID string) {
	var req models.DeployRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if strings.TrimSpace(req.Confirmation) != "deploy" {
		writeError(w, http.StatusBadRequest, "deploy requires typing 'deploy' as confirmation")
		return
	}

	p, err := h.projects.Get(projectID)
	if err != nil {
		serviceError(w, http.StatusNotFound, "project not found", err)
		return
	}
	normalized, err := services.ValidateDeployRequest(p, &req, isProjectTokenAuth(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req = *normalized

	// Check auto_apply - if false and triggered by GitHub, mark as pending
	if req.GitHubRunID != "" && !p.AutoApply {
		deployment, err := h.deploys.RequestApproval(projectID, &req)
		if err != nil {
			serviceError(w, http.StatusBadRequest, "deployment approval request failed", err)
			return
		}
		writeDeploymentOperation(w, http.StatusAccepted, projectID, deployment)
		return
	}

	deployment, err := h.deploys.Deploy(projectID, &req)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "deployment could not be started", err)
		return
	}

	writeDeploymentOperation(w, http.StatusAccepted, projectID, deployment)
}

func (h *Handler) ProjectDeploymentStatus(w http.ResponseWriter, r *http.Request, projectID, deployID string) {
	deployment, err := h.deploys.GetDeployment(projectID, deployID)
	if err != nil {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	writeDeploymentOperation(w, http.StatusOK, projectID, deployment)
}

func (h *Handler) ProjectApproveDeployment(w http.ResponseWriter, r *http.Request, projectID, deployID string) {
	var req models.DeploymentApprovalRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	expected := "approve " + deployID
	if strings.TrimSpace(req.Confirmation) != expected {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("approval requires the exact confirmation phrase: %q", expected))
		return
	}
	deployment, err := h.deploys.Approve(projectID, deployID)
	if err != nil {
		serviceError(w, http.StatusConflict, "deployment approval failed", err)
		return
	}
	writeDeploymentOperation(w, http.StatusAccepted, projectID, deployment)
}

func writeDeploymentOperation(w http.ResponseWriter, status int, projectID string, deployment *models.Deployment) {
	writeJSON(w, status, map[string]any{
		"ok":         true,
		"operation":  models.NewDeploymentOperation(deployment),
		"status_url": fmt.Sprintf("/api/projects/%s/deployments/%s/status", projectID, deployment.ID),
	})
}

func (h *Handler) ProjectConfig(w http.ResponseWriter, r *http.Request, projectID string) {
	var req models.ProjectRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.ID = &projectID

	project, err := h.projects.CreateOrUpdate(&req)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "project configuration is invalid", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"project": project,
	})
}

func (h *Handler) ProjectPause(w http.ResponseWriter, r *http.Request, projectID string) {
	state, err := h.projects.Status(projectID)
	if err != nil {
		serviceError(w, http.StatusNotFound, "project not found", err)
		return
	}
	if !state.Capabilities["pause"] {
		writeError(w, http.StatusForbidden, "pause is not available for this project")
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	readJSON(r, &body)
	if strings.TrimSpace(body.Reason) == "" {
		writeError(w, http.StatusBadRequest, "pause reason is required")
		return
	}

	state.State["paused"] = true
	state.State["paused_reason"] = body.Reason
	state.State["paused_at"] = time.Now().UTC().Format(time.RFC3339)
	state.State["paused_by"] = "deploy-control"

	dbState := map[string]any{
		"paused": true, "paused_reason": body.Reason,
		"paused_at": state.State["paused_at"], "paused_by": "deploy-control",
	}
	if err := h.updateProjectState(projectID, dbState); err != nil {
		internalError(w, err)
		return
	}

	h.audit.Log("project_pause", "ok", projectID, fmt.Sprintf("reason=%s", body.Reason), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": dbState})
}

func (h *Handler) ProjectResume(w http.ResponseWriter, r *http.Request, projectID string) {
	state, err := h.projects.Status(projectID)
	if err != nil {
		serviceError(w, http.StatusNotFound, "project not found", err)
		return
	}
	if !state.Capabilities["resume"] {
		writeError(w, http.StatusForbidden, "resume is not available for this project")
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	readJSON(r, &body)

	dbState := map[string]any{
		"paused": false, "paused_reason": "",
		"resumed_at": time.Now().UTC().Format(time.RFC3339), "resume_reason": body.Reason,
	}
	if err := h.updateProjectState(projectID, dbState); err != nil {
		internalError(w, err)
		return
	}

	h.audit.Log("project_resume", "ok", projectID, fmt.Sprintf("reason=%s", body.Reason), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": dbState})
}

func (h *Handler) ProjectBackups(w http.ResponseWriter, r *http.Request, projectID string) {
	backups, err := h.backups.ListBackups(projectID, 100)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

func (h *Handler) ProjectCreateBackup(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		Branch string `json:"branch"`
		Reason string `json:"reason"`
	}
	readJSON(r, &body)
	if body.Branch == "" {
		body.Branch = "main"
	}
	if body.Reason == "" {
		body.Reason = "manual backup"
	}

	deployment, err := h.backups.Create(projectID, body.Branch, body.Reason)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "backup could not be started", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "operation": deployment})
}

func (h *Handler) ProjectVerifyBackup(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		BackupID string `json:"backup_id"`
	}
	if err := readJSON(r, &body); err != nil || body.BackupID == "" {
		writeError(w, http.StatusBadRequest, "backup_id is required")
		return
	}

	result, err := h.backups.Verify(projectID, body.BackupID)
	if err != nil {
		internalError(w, err)
		return
	}

	status := http.StatusOK
	if !result.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, result)
}

func (h *Handler) ProjectRestoreDryRun(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		BackupID string `json:"backup_id"`
	}
	if err := readJSON(r, &body); err != nil || body.BackupID == "" {
		writeError(w, http.StatusBadRequest, "backup_id is required")
		return
	}

	result, err := h.backups.DryRunRestore(projectID, body.BackupID)
	if err != nil {
		internalError(w, err)
		return
	}

	plan := []string{
		"Acquire deploy-control lock",
		"Create pre-restore backup",
		"Stop write-capable app containers",
		"Restore selected custom-format dump",
		"Run migrations and health checks",
		"Restart storefront/backend",
	}

	status := http.StatusOK
	if !result.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"ok":              result.OK,
		"message":         result.Message,
		"backup":          result.Backup,
		"restore_enabled": true,
		"plan":            plan,
		"tables":          result.TableList,
		"actual_restore":  "disabled in this build; no database mutation was attempted",
	})
}

func (h *Handler) ProjectRestore(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.cfg.EnableRestore {
		writeError(w, http.StatusForbidden, "restore is disabled; set ENABLE_RESTORE=true to allow")
		return
	}

	var body struct {
		BackupID     string `json:"backup_id"`
		Confirmation string `json:"confirmation"`
	}
	if err := readJSON(r, &body); err != nil || body.BackupID == "" {
		writeError(w, http.StatusBadRequest, "backup_id is required")
		return
	}

	expectedPhrase := fmt.Sprintf("restore %s", body.BackupID)
	if body.Confirmation != expectedPhrase {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("restore requires typing the exact confirmation phrase: '%s'", expectedPhrase))
		return
	}

	result, err := h.backups.Verify(projectID, body.BackupID)
	if err != nil {
		internalError(w, err)
		return
	}
	if !result.OK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("backup verification failed: %s", result.Message))
		return
	}

	deployment, err := h.backups.Restore(projectID, body.BackupID)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "restore could not be started", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "operation": deployment})
}

func (h *Handler) ProjectRollback(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		Commit       string `json:"commit"`
		Confirmation string `json:"confirmation"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Commit == "" || body.Confirmation != body.Commit {
		writeError(w, http.StatusBadRequest, "rollback requires typing the exact commit SHA")
		return
	}

	deployment, err := h.backups.Rollback(projectID, body.Commit)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "rollback could not be started", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "operation": deployment})
}

func (h *Handler) ProjectAbort(w http.ResponseWriter, r *http.Request, projectID string) {
	if err := h.deploys.Abort(projectID); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Deployment aborted."})
}

func (h *Handler) ProjectRunnerAction(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch body.Action {
	case "start", "stop", "restart":
	default:
		writeError(w, http.StatusBadRequest, "invalid runner action; must be start, stop, or restart")
		return
	}

	msg, err := h.projects.RunnerAction(projectID, body.Action)
	if err != nil {
		serviceError(w, http.StatusBadRequest, "runner action failed", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": msg,
	})
}

func (h *Handler) ProjectContainerAction(w http.ResponseWriter, r *http.Request, projectID, service, action string) {
	if err := h.projects.ContainerAction(projectID, service, action); err != nil {
		if strings.Contains(err.Error(), "invalid service name format") || strings.Contains(err.Error(), "invalid action:") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Container %s of project %s: %s successful", service, projectID, action),
	})
}

func (h *Handler) ProjectDelete(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		Confirmation string `json:"confirmation"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	expectedPhrase := fmt.Sprintf("delete %s", projectID)
	if body.Confirmation != expectedPhrase {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("type the exact phrase: '%s'", expectedPhrase))
		return
	}

	if err := h.projects.Delete(projectID); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Project '%s' deleted and cleaned up.", projectID),
	})
}

func (h *Handler) ProjectLogs(w http.ResponseWriter, r *http.Request, projectID string) {
	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	logDir := h.projects.LogDir(p)
	entries, _ := filepath.Glob(filepath.Join(logDir, "*"))
	var logs []map[string]string
	for _, entry := range entries {
		info, err := os.Stat(entry)
		if err != nil {
			continue
		}
		logs = append(logs, map[string]string{
			"name": filepath.Base(entry),
			"size": fmt.Sprintf("%d", info.Size()),
			"mod":  info.ModTime().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (h *Handler) ProjectLogContent(w http.ResponseWriter, r *http.Request, projectID, logName string) {
	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	logDir := h.projects.LogDir(p)
	safeName := filepath.Base(logName)
	logPath := filepath.Join(logDir, safeName)
	info, err := os.Stat(logPath)
	if err != nil {
		writeText(w, http.StatusOK, "Log file not available.\n")
		return
	}
	if info.Size() > 10*1024*1024 {
		writeText(w, http.StatusOK, "Log file exceeds 10MB limit.\n")
		return
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		writeText(w, http.StatusOK, "Log file not available.\n")
		return
	}
	writeText(w, http.StatusOK, string(data))
}

func (h *Handler) ProjectContainerLogFiles(w http.ResponseWriter, r *http.Request, projectID string) {
	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	containerLogs := h.projects.ContainerLogFiles(p)
	if containerLogs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"logs": []map[string]string{}})
		return
	}
	var logs []map[string]string
	for svc, path := range containerLogs {
		containerName, ownerErr := h.projects.OwnedContainerName(p, svc)
		if ownerErr != nil {
			continue
		}
		content, readErr := h.projects.Docker().ContainerReadFile(containerName, path)
		size := 0
		if readErr == nil {
			size = len(content)
		}
		logs = append(logs, map[string]string{
			"service": svc,
			"path":    path,
			"size":    fmt.Sprintf("%d", size),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (h *Handler) ProjectContainerLogFileContent(w http.ResponseWriter, r *http.Request, projectID, service string) {
	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	containerLogs := h.projects.ContainerLogFiles(p)
	if containerLogs == nil {
		writeText(w, http.StatusOK, "No container log files configured.\n")
		return
	}
	filePath, ok := containerLogs[service]
	if !ok {
		writeText(w, http.StatusOK, "No log file configured for this service.\n")
		return
	}
	containerName, err := h.projects.OwnedContainerName(p, service)
	if err != nil {
		writeText(w, http.StatusNotFound, "Container is not available.\n")
		return
	}
	content, err := h.projects.Docker().ContainerReadFile(containerName, filePath)
	if err != nil {
		if errors.Is(err, dockerclient.ErrContainerFileTooLarge) {
			writeText(w, http.StatusOK, "Log file exceeds 10MB limit.\n")
			return
		}
		writeText(w, http.StatusOK, "Log file not available.\n")
		return
	}
	if len(content) > 10*1024*1024 {
		writeText(w, http.StatusOK, "Log file exceeds 10MB limit.\n")
		return
	}
	writeText(w, http.StatusOK, content)
}

func (h *Handler) ProjectContainerLogs(w http.ResponseWriter, r *http.Request, projectID, service string) {
	p, err := h.projects.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	containerName, err := h.projects.OwnedContainerName(p, service)
	if err != nil {
		writeText(w, http.StatusNotFound, "Container is not available.\n")
		return
	}
	tail := 100
	if t := queryParam(r, "tail"); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 && v <= 10000 {
			tail = v
		}
	}
	since := queryParam(r, "since")

	logs := h.projects.Docker().ContainerLogsSince(containerName, tail, since)
	writeText(w, http.StatusOK, logs)
}

func (h *Handler) ProjectDeploymentLog(w http.ResponseWriter, r *http.Request, projectID, deployID string) {
	// Sanitize deployID to prevent path traversal
	safeDeployID := filepath.Base(deployID)
	logContent, err := h.deploys.GetLog(projectID, safeDeployID)
	if err != nil {
		p, pErr := h.projects.Get(projectID)
		if pErr != nil {
			writeText(w, http.StatusOK, "Waiting for deployment log...\n")
			return
		}
		logDir := h.projects.LogDir(p)
		logPath := filepath.Join(logDir, safeDeployID+".log")
		info, statErr := os.Stat(logPath)
		if statErr != nil {
			writeText(w, http.StatusOK, "Waiting for deployment log...\n")
			return
		}
		if info.Size() > 10*1024*1024 {
			writeText(w, http.StatusOK, "Log file exceeds 10MB limit.\n")
			return
		}
		data, readErr := os.ReadFile(logPath)
		if readErr != nil {
			writeText(w, http.StatusOK, "Waiting for deployment log...\n")
			return
		}
		writeText(w, http.StatusOK, string(data))
		return
	}
	writeText(w, http.StatusOK, logContent)
}

func (h *Handler) ProjectEnvTemplate(w http.ResponseWriter, r *http.Request, projectID string) {
	vars, overrides, err := h.projects.ReadEnvTemplate(projectID)
	if err != nil {
		serviceError(w, http.StatusNotFound, "environment template not found", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"variables": vars,
		"overrides": overrides,
	})
}

func (h *Handler) ProjectSaveEnvConfig(w http.ResponseWriter, r *http.Request, projectID string) {
	var body struct {
		Overrides map[string]string `json:"overrides"`
		ClearKeys []string          `json:"clear_keys"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.projects.SaveEnvConfig(projectID, body.Overrides, body.ClearKeys); err != nil {
		serviceError(w, http.StatusBadRequest, "environment configuration is invalid", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) updateProjectState(projectID string, state map[string]any) error {
	return h.projects.UpdateProjectState(projectID, state)
}
