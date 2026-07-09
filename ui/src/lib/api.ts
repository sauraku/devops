const CSRF_TOKEN =
  document.querySelector<HTMLMetaElement>('meta[name="csrf-token"]')?.content ?? '';
const AUTH_TOKEN =
  document.querySelector<HTMLMetaElement>('meta[name="auth-token"]')?.content ?? '';

const REQUEST_TIMEOUT = 30000;

function redirectToLogin() {
  window.location.href = '/';
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT);
  try {
    const res = await fetch(path, {
      ...options,
      signal: controller.signal,
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': CSRF_TOKEN,
        'X-Deploy-Control-Token': AUTH_TOKEN,
        ...options.headers,
      },
    });
    if (res.status === 401) {
      redirectToLogin();
      throw new Error('session expired');
    }
    const type = res.headers.get('Content-Type') ?? '';
    if (type.includes('application/json')) {
      const data = await res.json();
      if (!res.ok) throw new Error(data.error ?? 'request failed');
      return data as T;
    }
    const text = await res.text();
    if (!res.ok) throw new Error(text || 'request failed');
    return text as unknown as T;
  } finally {
    clearTimeout(timeout);
  }
}

import type { Project, ProjectListResponse, ProjectStatus, Deployment, Backup } from '../types';

export function listProjects() {
  return request<ProjectListResponse>('/api/projects');
}

export function getProjectStatus(projectId: string) {
  return request<ProjectStatus>(`/api/projects/${encodeURIComponent(projectId)}/status`);
}

export function createProject(body: Record<string, any>) {
  return request<{ ok: boolean; project: Project }>('/api/projects', {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

export function deployProject(projectId: string, body: Record<string, any>) {
  return request<{ ok: boolean; operation: Deployment }>(
    `/api/projects/${encodeURIComponent(projectId)}/deploy`,
    { method: 'POST', body: JSON.stringify(body) }
  );
}

export function updateProjectConfig(projectId: string, body: Record<string, any>) {
  return request<{ ok: boolean; project: Project }>(
    `/api/projects/${encodeURIComponent(projectId)}/config`,
    { method: 'POST', body: JSON.stringify(body) }
  );
}

export function pauseProject(projectId: string, reason: string) {
  return request<{ ok: boolean }>(
    `/api/projects/${encodeURIComponent(projectId)}/pause`,
    { method: 'POST', body: JSON.stringify({ reason }) }
  );
}

export function resumeProject(projectId: string, reason: string) {
  return request<{ ok: boolean }>(
    `/api/projects/${encodeURIComponent(projectId)}/resume`,
    { method: 'POST', body: JSON.stringify({ reason }) }
  );
}

export function deleteProject(projectId: string, confirmation: string) {
  return request<{ ok: boolean; message: string }>(
    `/api/projects/${encodeURIComponent(projectId)}/delete`,
    { method: 'POST', body: JSON.stringify({ confirmation }) }
  );
}

export function abortDeploy(projectId: string) {
  return request<{ ok: boolean; message: string }>(
    `/api/projects/${encodeURIComponent(projectId)}/abort`,
    { method: 'POST' }
  );
}

export function verifyBackup(projectId: string, backupId: string) {
  return request<{ ok: boolean; message: string }>(
    `/api/projects/${encodeURIComponent(projectId)}/backups/verify`,
    { method: 'POST', body: JSON.stringify({ backup_id: backupId }) }
  );
}

export function createBackup(projectId: string, branch: string, reason: string) {
  return request<{ ok: boolean; operation: Deployment }>(
    `/api/projects/${encodeURIComponent(projectId)}/backups`,
    { method: 'POST', body: JSON.stringify({ branch, reason }) }
  );
}

export function rollbackProject(projectId: string, commit: string) {
  return request<{ ok: boolean; operation: Deployment }>(
    `/api/projects/${encodeURIComponent(projectId)}/rollback`,
    { method: 'POST', body: JSON.stringify({ commit, confirmation: commit }) }
  );
}

export function getDeploymentLog(projectId: string, deployId: string) {
  return request<string>(
    `/api/projects/${encodeURIComponent(projectId)}/deployments/${encodeURIComponent(deployId)}`
  );
}

export function runnerAction(projectId: string, action: string) {
  return request<{ ok: boolean; message: string }>(
    `/api/projects/${encodeURIComponent(projectId)}/runner`,
    { method: 'POST', body: JSON.stringify({ action }) }
  );
}

export function getBackups(projectId: string) {
  return request<{ backups: Backup[] }>(
    `/api/projects/${encodeURIComponent(projectId)}/backups`
  );
}

export function restoreBackup(projectId: string, backupId: string) {
  return request<{ ok: boolean; operation: Deployment }>(
    `/api/projects/${encodeURIComponent(projectId)}/restore`,
    { method: 'POST', body: JSON.stringify({ backup_id: backupId, confirmation: `restore ${backupId}` }) }
  );
}

export function dryRunRestore(projectId: string, backupId: string) {
  return request<{ ok: boolean; message: string; tables: { name: string; rows: string; kind: string }[] }>(
    `/api/projects/${encodeURIComponent(projectId)}/restore/dry-run`,
    { method: 'POST', body: JSON.stringify({ backup_id: backupId }) }
  );
}

export function getEnvTemplate(projectId: string) {
  return request<{ variables: { key: string; default: string; is_secret: boolean }[]; overrides: Record<string, string> }>(
    `/api/projects/${encodeURIComponent(projectId)}/env-template`
  );
}

export function saveEnvConfig(projectId: string, overrides: Record<string, string>) {
  return request<{ ok: boolean }>(
    `/api/projects/${encodeURIComponent(projectId)}/env-config`,
    { method: 'POST', body: JSON.stringify({ overrides }) }
  );
}
