package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

func slugProjectID(raw string) string {
	slug := stringsMap(raw, func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			return r
		}
		return -1
	})
	slug = stringsTrim(slug, ".-")
	if slug == "" {
		return ""
	}
	if !(slug[0] >= 'a' && slug[0] <= 'z') && !(slug[0] >= '0' && slug[0] <= '9') {
		slug = "project-" + slug
	}
	return slug
}

func stringsMap(s string, fn func(rune) rune) string {
	result := make([]rune, 0, len(s))
	for _, r := range s {
		if mapped := fn(r); mapped >= 0 {
			result = append(result, mapped)
		} else if mapped == -1 {
			result = append(result, '-')
		}
	}
	return string(result)
}

func stringsTrim(s, cutset string) string {
	for len(s) > 0 && containsRune(cutset, rune(s[0])) {
		s = s[1:]
	}
	for len(s) > 0 && containsRune(cutset, rune(s[len(s)-1])) {
		s = s[:len(s)-1]
	}
	return s
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func (db *DB) ListProjects() ([]*models.Project, error) {
	rows, err := db.Query(`
		SELECT id, name, repo_url, branch_name, deployment_mode, auto_apply,
		       registry_host, registry_username, runner_container, runner_status, app_dir,
		       created_at, updated_at
		FROM projects ORDER BY name ASC LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []*models.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (db *DB) GetProject(id string) (*models.Project, error) {
	row := db.QueryRow(`
		SELECT id, name, repo_url, branch_name, deployment_mode, auto_apply,
		       registry_host, registry_username, runner_container, runner_status, app_dir,
		       created_at, updated_at
		FROM projects WHERE id = ?
	`, id)
	return scanProject(row)
}

func (db *DB) UpsertProject(p *models.Project) error {
	now := time.Now().UTC()
	p.UpdatedAt = now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	_, err := db.Exec(`
		INSERT INTO projects (id, name, repo_url, branch_name, deployment_mode, auto_apply,
		                      registry_host, registry_username, runner_container, runner_status,
		                      app_dir, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, repo_url=excluded.repo_url, branch_name=excluded.branch_name,
			deployment_mode=excluded.deployment_mode, auto_apply=excluded.auto_apply,
			registry_host=excluded.registry_host, registry_username=excluded.registry_username,
			runner_container=excluded.runner_container, runner_status=excluded.runner_status,
			app_dir=excluded.app_dir, updated_at=excluded.updated_at
	`, p.ID, p.Name, p.RepoURL, p.BranchName, p.DeploymentMode, boolToInt(p.AutoApply),
		p.RegistryHost, p.RegistryUsername, p.RunnerContainer, p.RunnerStatus,
		p.AppDir, p.CreatedAt.Format(time.RFC3339), p.UpdatedAt.Format(time.RFC3339))
	return err
}

// SaveProjectWithCredentials atomically persists project configuration and any
// newly supplied credentials. Nil credentials leave the existing values alone.
func (db *DB) SaveProjectWithCredentials(p *models.Project, registryPassword, runnerToken *string) error {
	var encryptedRegistry, encryptedRunner string
	var err error
	if registryPassword != nil {
		encryptedRegistry, err = encrypt(*registryPassword)
		if err != nil {
			return fmt.Errorf("encrypt registry password: %w", err)
		}
	}
	if runnerToken != nil {
		encryptedRunner, err = encrypt(*runnerToken)
		if err != nil {
			return fmt.Errorf("encrypt runner token: %w", err)
		}
	}

	now := time.Now().UTC()
	p.UpdatedAt = now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`
		INSERT INTO projects (id, name, repo_url, branch_name, deployment_mode, auto_apply,
		                      registry_host, registry_username, runner_container, runner_status,
		                      app_dir, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, repo_url=excluded.repo_url, branch_name=excluded.branch_name,
			deployment_mode=excluded.deployment_mode, auto_apply=excluded.auto_apply,
			registry_host=excluded.registry_host, registry_username=excluded.registry_username,
			runner_container=excluded.runner_container, runner_status=excluded.runner_status,
			app_dir=excluded.app_dir, updated_at=excluded.updated_at
	`, p.ID, p.Name, p.RepoURL, p.BranchName, p.DeploymentMode, boolToInt(p.AutoApply),
		p.RegistryHost, p.RegistryUsername, p.RunnerContainer, p.RunnerStatus,
		p.AppDir, p.CreatedAt.Format(time.RFC3339), p.UpdatedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	timestamp := now.Format(time.RFC3339)
	if registryPassword != nil {
		if _, err = tx.Exec(`
			INSERT INTO registry_auth (project_id, registry_password, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET registry_password=excluded.registry_password, updated_at=excluded.updated_at
		`, p.ID, encryptedRegistry, timestamp); err != nil {
			return err
		}
	}
	if runnerToken != nil {
		if _, err = tx.Exec(`
			INSERT INTO runner_auth (project_id, runner_token, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET runner_token=excluded.runner_token, updated_at=excluded.updated_at
		`, p.ID, encryptedRunner, timestamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) DeleteProject(id string) error {
	_, err := db.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

func (db *DB) GetProjectState(projectID string) (map[string]any, error) {
	row := db.QueryRow(`
		SELECT COALESCE(paused, 0), COALESCE(paused_reason, ''), COALESCE(paused_at, ''),
		       COALESCE(paused_by, ''), COALESCE(last_deploy_status, 'unknown'),
		       COALESCE(last_deploy_message, ''), COALESCE(last_run_at, ''),
		       COALESCE(active_deploy_id, ''),
		       COALESCE(last_deployed_commit, ''), COALESCE(last_deployed_image_tag, ''),
		       COALESCE(metadata, '{}')
		FROM project_state WHERE project_id = ?
	`, projectID)

	var paused int
	var pausedReason, pausedAt, pausedBy, lastStatus, lastMsg, lastRun, activeDeployID string
	var lastCommit, lastImageTag, metadata string
	err := row.Scan(&paused, &pausedReason, &pausedAt, &pausedBy, &lastStatus, &lastMsg, &lastRun, &activeDeployID, &lastCommit, &lastImageTag, &metadata)
	if err == sql.ErrNoRows {
		return map[string]any{
			"paused": false, "paused_reason": "", "last_deploy_status": "unknown",
			"last_deploy_message": "", "active_deploy_id": "",
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"paused":                  paused != 0,
		"paused_reason":           pausedReason,
		"paused_at":               pausedAt,
		"paused_by":               pausedBy,
		"last_deploy_status":      lastStatus,
		"last_deploy_message":     lastMsg,
		"last_run_at":             lastRun,
		"active_deploy_id":        activeDeployID,
		"last_deployed_commit":    lastCommit,
		"last_deployed_image_tag": lastImageTag,
		"metadata":                metadata,
	}, nil
}

func (db *DB) UpsertProjectState(projectID string, state map[string]any) error {
	paused := boolToInt(getBool(state, "paused"))
	pausedReason := getStr(state, "paused_reason")
	pausedAt := getStr(state, "paused_at")
	pausedBy := getStr(state, "paused_by")
	lastStatus := getStr(state, "last_deploy_status")
	lastMsg := getStr(state, "last_deploy_message")
	lastRun := getStr(state, "last_run_at")
	activeDeployID := getStr(state, "active_deploy_id")
	lastCommit := getStr(state, "last_deployed_commit")
	lastImageTag := getStr(state, "last_deployed_image_tag")
	metadata := getStr(state, "metadata")

	_, err := db.Exec(`
		INSERT INTO project_state (project_id, paused, paused_reason, paused_at, paused_by,
		                           last_deploy_status, last_deploy_message, last_run_at,
		                           active_deploy_id, last_deployed_commit, last_deployed_image_tag,
		                           metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			paused=excluded.paused, paused_reason=excluded.paused_reason,
			paused_at=excluded.paused_at, paused_by=excluded.paused_by,
			last_deploy_status=excluded.last_deploy_status,
			last_deploy_message=excluded.last_deploy_message,
			last_run_at=excluded.last_run_at,
			active_deploy_id=excluded.active_deploy_id,
			last_deployed_commit=excluded.last_deployed_commit,
			last_deployed_image_tag=excluded.last_deployed_image_tag,
			metadata=excluded.metadata
	`, projectID, paused, pausedReason, pausedAt, pausedBy, lastStatus, lastMsg, lastRun, activeDeployID, lastCommit, lastImageTag, metadata)
	return err
}

func (db *DB) SaveRunnerStatus(projectID, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`
		UPDATE projects SET runner_status = ?, updated_at = ? WHERE id = ?
	`, status, now, projectID)
	return err
}

func (db *DB) SaveRegistryPassword(projectID, password string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	encrypted, err := encrypt(password)
	if err != nil {
		return fmt.Errorf("encrypt registry password: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO registry_auth (project_id, registry_password, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET registry_password=excluded.registry_password, updated_at=excluded.updated_at
	`, projectID, encrypted, now)
	return err
}

func (db *DB) GetRegistryPassword(projectID string) (string, error) {
	var stored string
	err := db.QueryRow("SELECT registry_password FROM registry_auth WHERE project_id = ?", projectID).Scan(&stored)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	plaintext, err := decrypt(stored)
	if err != nil {
		return "", fmt.Errorf("decrypt registry password: %w", err)
	}
	return plaintext, nil
}

func (db *DB) SaveRunnerToken(projectID, token string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	encrypted, err := encrypt(token)
	if err != nil {
		return fmt.Errorf("encrypt runner token: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO runner_auth (project_id, runner_token, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET runner_token=excluded.runner_token, updated_at=excluded.updated_at
	`, projectID, encrypted, now)
	return err
}

func (db *DB) GetRunnerToken(projectID string) (string, error) {
	var stored string
	err := db.QueryRow("SELECT runner_token FROM runner_auth WHERE project_id = ?", projectID).Scan(&stored)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	plaintext, err := decrypt(stored)
	if err != nil {
		return "", fmt.Errorf("decrypt runner token: %w", err)
	}
	return plaintext, nil
}

// MigrateLegacyRunnerTokens copies only unmistakable GitHub PAT values from
// the old shared registry credential field. The registry copy is retained
// because the same PAT may also authorize GHCR pulls.
func (db *DB) MigrateLegacyRunnerTokens() error {
	rows, err := db.Query(`
		SELECT r.project_id, r.registry_password
		FROM registry_auth r
		LEFT JOIN runner_auth a ON a.project_id = r.project_id
		WHERE a.project_id IS NULL
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type legacyToken struct{ projectID, token string }
	var tokens []legacyToken
	for rows.Next() {
		var projectID, stored string
		if err := rows.Scan(&projectID, &stored); err != nil {
			return err
		}
		plain, err := decrypt(stored)
		if err != nil {
			return err
		}
		if strings.HasPrefix(plain, "ghp_") || strings.HasPrefix(plain, "github_pat_") {
			tokens = append(tokens, legacyToken{projectID: projectID, token: plain})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, token := range tokens {
		if err := db.SaveRunnerToken(token.projectID, token.token); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) SaveProjectEnvOverrides(projectID string, overrides map[string]string) error {
	plain, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("encode project environment: %w", err)
	}
	encrypted, err := encrypt(string(plain))
	if err != nil {
		return fmt.Errorf("encrypt project environment: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO project_env (project_id, encrypted_overrides, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET encrypted_overrides=excluded.encrypted_overrides, updated_at=excluded.updated_at
	`, projectID, encrypted, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (db *DB) GetProjectEnvOverrides(projectID string) (map[string]string, error) {
	var stored string
	err := db.QueryRow("SELECT encrypted_overrides FROM project_env WHERE project_id = ?", projectID).Scan(&stored)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := decrypt(stored)
	if err != nil {
		return nil, fmt.Errorf("decrypt project environment: %w", err)
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(plain), &overrides); err != nil {
		return nil, fmt.Errorf("decode project environment: %w", err)
	}
	return overrides, nil
}

// MigrateLegacyProjectEnvOverrides moves the old plaintext env_overrides field
// out of project_state.metadata and into the encrypted project_env table. The
// complete migration is one transaction so a failed startup cannot leave a
// project's only copy of its overrides half-migrated.
func (db *DB) MigrateLegacyProjectEnvOverrides() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT s.project_id, COALESCE(s.metadata, '{}'), e.project_id
		FROM project_state s
		LEFT JOIN project_env e ON e.project_id = s.project_id
	`)
	if err != nil {
		return err
	}
	type legacyEnvironment struct {
		projectID    string
		metadata     string
		hasEncrypted bool
	}
	var candidates []legacyEnvironment
	for rows.Next() {
		var candidate legacyEnvironment
		var encryptedProjectID sql.NullString
		if err := rows.Scan(&candidate.projectID, &candidate.metadata, &encryptedProjectID); err != nil {
			rows.Close()
			return err
		}
		candidate.hasEncrypted = encryptedProjectID.Valid
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, candidate := range candidates {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(candidate.metadata), &metadata); err != nil {
			return fmt.Errorf("decode project_state metadata for %s: %w", candidate.projectID, err)
		}
		raw, exists := metadata["env_overrides"]
		if !exists {
			continue
		}
		legacy, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("project_state metadata env_overrides for %s is not an object", candidate.projectID)
		}
		if !candidate.hasEncrypted {
			overrides := make(map[string]string, len(legacy))
			for key, value := range legacy {
				overrides[key] = fmt.Sprint(value)
			}
			plain, err := json.Marshal(overrides)
			if err != nil {
				return fmt.Errorf("encode legacy project environment for %s: %w", candidate.projectID, err)
			}
			encrypted, err := encrypt(string(plain))
			if err != nil {
				return fmt.Errorf("encrypt legacy project environment for %s: %w", candidate.projectID, err)
			}
			if _, err := tx.Exec(`
				INSERT INTO project_env (project_id, encrypted_overrides, updated_at)
				VALUES (?, ?, ?)
			`, candidate.projectID, encrypted, now); err != nil {
				return fmt.Errorf("save migrated project environment for %s: %w", candidate.projectID, err)
			}
		}
		delete(metadata, "env_overrides")
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode migrated project_state metadata for %s: %w", candidate.projectID, err)
		}
		if _, err := tx.Exec("UPDATE project_state SET metadata = ? WHERE project_id = ?", string(encoded), candidate.projectID); err != nil {
			return fmt.Errorf("remove plaintext project environment for %s: %w", candidate.projectID, err)
		}
	}

	return tx.Commit()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (*models.Project, error) {
	var p models.Project
	var createdAt, updatedAt string
	var autoApply int
	err := row.Scan(&p.ID, &p.Name, &p.RepoURL, &p.BranchName, &p.DeploymentMode,
		&autoApply, &p.RegistryHost, &p.RegistryUsername,
		&p.RunnerContainer, &p.RunnerStatus, &p.AppDir,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.AutoApply = autoApply != 0
	p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if p.RunnerContainer == "" {
		p.RunnerContainer = fmt.Sprintf("devops-runner-%s", p.ID)
	}
	return &p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if ok {
		return b
	}
	s, ok := v.(string)
	if ok {
		return s == "true" || s == "1"
	}
	return false
}

func getStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
