package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/models"
	"github.com/sauraku/devops-control/internal/services"
)

type Handler struct {
	projects   *services.ProjectService
	deploys    *services.DeployService
	backups    *services.BackupService
	audit      *services.AuditService
	auth       *Authenticator
	cfg        *models.Config
}

func NewHandler(
	projectService *services.ProjectService,
	deployService *services.DeployService,
	backupService *services.BackupService,
	auditService *services.AuditService,
	auth *Authenticator,
	cfg *models.Config,
) *Handler {
	return &Handler{
		projects: projectService,
		deploys:  deployService,
		backups:  backupService,
		audit:    auditService,
		auth:     auth,
		cfg:      cfg,
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

	svcNames := []string{"postgres", "redis", "backend", "storefront"}
	type projectDebug struct {
		ID            string
		Containers    map[string]string
		ServiceHealth map[string]*models.ServiceHealth
	}
	var projectDetails []projectDebug
	for _, p := range projects {
		pd := projectDebug{ID: p.ID, Containers: make(map[string]string), ServiceHealth: make(map[string]*models.ServiceHealth)}
		for _, svc := range svcNames {
			containerName := services.DeploymentContainerName(svc, p.BranchName, p.ID)
			summary := h.projects.Docker().ContainerSummary(containerName)
			pd.Containers[svc] = summary.State
			pd.ServiceHealth[svc] = h.projects.CheckServiceHealth(svc, containerName, summary)
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
		"projects":          projects,
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
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if status.Containers != nil {
		states := status.Containers["current"]
		log.Printf("Status %s containers: %v", projectID, states)
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
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Check auto_apply - if false and triggered by GitHub, mark as pending
	if req.GitHubRunID != "" && !p.AutoApply {
		pending := map[string]any{
			"ref":              req.Ref,
			"sha":              req.SHA,
			"branch":           req.Branch,
			"commit_message":   req.CommitMessage,
			"timestamp":        time.Now().UTC().Format(time.RFC3339),
			"github_run_id":    req.GitHubRunID,
			"github_run_number": req.GitHubRunNumber,
			"github_actor":     req.GitHubActor,
			"github_repository": req.GitHubRepo,
			"github_workflow":  req.GitHubWorkflow,
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":             true,
			"message":        "Deployment is pending manual approval in the DevOps portal.",
			"pending":        true,
			"pending_deploy": pending,
		})
		return
	}

	deployment, err := h.deploys.Deploy(projectID, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":        true,
		"operation": deployment,
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
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusNotFound, err.Error())
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
		writeError(w, http.StatusNotFound, err.Error())
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
		writeError(w, http.StatusBadRequest, err.Error())
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
		"tables":           result.TableList,
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
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": msg,
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
	if _, err := h.projects.Get(projectID); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	logDir := filepath.Join(h.cfg.BaseDir, "Logs", projectID)
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
	if _, err := h.projects.Get(projectID); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	logDir := filepath.Join(h.cfg.BaseDir, "Logs", projectID)
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

func (h *Handler) ProjectDeploymentLog(w http.ResponseWriter, r *http.Request, projectID, deployID string) {
	// Sanitize deployID to prevent path traversal
	safeDeployID := filepath.Base(deployID)
	logContent, err := h.deploys.GetLog(safeDeployID)
	if err != nil {
		logDir := filepath.Join(h.cfg.BaseDir, "Logs", projectID)
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
		writeError(w, http.StatusNotFound, err.Error())
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
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.projects.SaveEnvConfig(projectID, body.Overrides); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) updateProjectState(projectID string, state map[string]any) error {
	return h.projects.UpdateProjectState(projectID, state)
}


