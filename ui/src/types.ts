export interface Project {
  id: string;
  name: string;
  repo_url: string;
  branch_name: string;
  deployment_mode: 'local_repo' | 'compose_image';
  auto_apply: boolean;
  registry_host: string;
  registry_username: string;
  runner_container: string;
  runner_status: string;
  app_dir: string;
  created_at: string;
  updated_at: string;
}

export interface Deployment {
  id: string;
  project_id: string;
  kind: string;
  status: 'running' | 'success' | 'failed' | 'aborted' | 'pending';
  ref: string;
  sha: string;
  image_tag: string;
  branch: string;
  commit_message: string;
  started_at: string;
  finished_at?: string;
  exit_code?: number;
  log_path: string;
  github_run_id: string;
  github_run_number: string;
  github_actor: string;
  github_repository: string;
  github_workflow: string;
}

export interface ServiceHealth {
  service: string;
  container: string;
  container_state: string;
  status: string;
  detail: string;
}

export interface Backup {
  id: string;
  project_id: string;
  file_path: string;
  sha256: string;
  size_bytes: number;
  timestamp: string;
  verification_status?: string;
  env_name: string;
}

export interface DeployLock {
  project_id: string;
  operation_id: string;
  operation: string;
  started_at: string;
  sha?: string;
  image_tag?: string;
  branch?: string;
}

export interface ProjectStatus {
  project: Project;
  state: Record<string, any>;
  lock: DeployLock | null;
  runner: { container: string; state: string };
  containers: { current: Record<string, string> };
  service_health: Record<string, ServiceHealth>;
  recent_deployments: Deployment[];
  recent_backups: Backup[];
  capabilities: Record<string, boolean>;
  server_time: string;
}

export interface ProjectListResponse {
  projects: Project[];
  default_project_id: string;
}
