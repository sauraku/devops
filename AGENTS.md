# AGENTS.md — DevOps Control Plane

> **Critical rule:** Never commit, push, or otherwise modify git remotes unless explicitly told to. Wait for instruction.

> **Prod-readiness mandate:** Every single change — no matter how small — MUST be vetted for production security and reliability before committing or pushing. Ask yourself:
>   1. Does this leak secrets, tokens, or internal paths to logs, error messages, HTTP responses, or the UI?
>   2. Does this change operate on the correct Docker resources (containers, volumes, networks) and could it accidentally affect non-devops containers?
>   3. Does this introduce a crash, hang, or unhandled error path that could take the control plane or a project offline?
>   4. Does this change work correctly when the env file contains session vars (HOME, PATH, PWD, SHLVL, etc.) that override critical shell state?
>   5. Does this change rely on a file, directory, or service that may not exist or may have restrictive permissions in production?
>   6. Does this change handle the case where the server has zero devops projects, or the Projects directory is empty, missing, or permission-denied?
>   7. For any docker command — are the right filters in place? Never use `docker rm -f $(docker ps -aq)` or equivalent blanket operations.
>   8. For any bash script sourcing an env file — are HOME/PATH/PWD restored afterward to avoid breaking subsequent commands?
>   9. Does this regenerate or expose the devops token, JWT_SECRET, or registry passwords anywhere they shouldn't be?
>   10. If this modifies a deploy/backup/teardown script — does it work when curl-piped into bash (non-interactive, no tty)?
>   
>   **If any answer is "no" or "I'm not sure," STOP and fix before pushing.**

## Quick Reference

| What | Command |
|------|---------|
| **Local dev** | `./deploy/local.sh start` (generated secrets in ignored `.env.local`, BASE_DIR defaults to `./data`) |
| **Prod deploy** | `bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)` |
| **Prod teardown** | `bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/teardown.sh)` |
| **Devops UI** | `http://localhost:8787` (local) / `https://{devops-domain}` through the trusted TLS proxy (prod) |
| **Prod token** | Set in `.env.prod` on server — never committed |
| **GitHub PAT** | Set via `GITHUB_TOKEN` env var — never committed |

## Architecture

```
Go binary (cmd/devops-control) — embeds React UI via //go:embed ui/dist
├── internal/api/        HTTP handlers, router, auth (token+cookie+CSRF), WebSocket
├── internal/services/   Business logic (deploy, backup, project, audit)
├── internal/docker/     Docker client (compose, container management, registry login)
├── internal/db/         SQLite at {BASE_DIR}/State/devops-control.db
└── internal/models/     Data structures (Config, Project, Deployment, Backup)
```

- **Frontend**: React + TypeScript + Tailwind v4 + TanStack Query + Vite
- **Docker**: Multi-stage build (node → go → docker:24-cli) → GHCR
- **Database**: SQLite (no PostgreSQL needed for the control plane itself)
- **Auth**: Master token login → signed expiring HttpOnly cookie (`deploy_control`) + per-session CSRF token

## How to Start

### Local Development
```bash
./deploy/local.sh start
# Generates strong secrets when absent; BASE_DIR defaults to ./data
# Builds UI + Go binary, starts on http://localhost:8787
# Keeps the old process through successful builds; stops only its exact recorded PID
# Secrets persisted in .env.local (gitignored)
```

Use `./deploy/local.sh status` for a read-only, timeout-bounded health check and `./deploy/local.sh stop` for a serialized, ownership-checked stop. These commands do not rebuild or rewrite `.env.local`, and unknown port owners are never killed.

### Production Bootstrap
```bash
DEPLOY_CONTROL_PUBLIC_URL=https://<devops-domain> \
  TRUSTED_PROXY_CIDRS=<proxy-ip-or-cidr> GITHUB_TOKEN=<your_github_pat> \
  bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
```
- Pulls `ghcr.io/sauraku/devops:main` Docker image
- Auto-generates env file with random secrets at `~/.devops-control/.env.prod`
- Mounts `/var/run/docker.sock` for container management
- Data persists in `~/.devops-control/` (mounted to `/opt/devops-control` in container)
- `BASE_DIR=/opt/devops-control` forced via `-e` flag
- With `COOKIE_SECURE=true`, configure the exact proxy addresses in `TRUSTED_PROXY_CIDRS` and the external HTTPS origin in `DEPLOY_CONTROL_PUBLIC_URL`; the proxy must set `X-Forwarded-Proto=https`, maintain a valid `X-Forwarded-For` chain, and preserve `Host`
- Bootstrap verifies local `/api/health`, then public `/api/health` and `/login` with normal TLS verification; a failed public check rolls the upgrade back

### Production Teardown
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/teardown.sh)
```
- Lists all devops containers, volumes, networks, images
- Requires typing `yes` to confirm
- Does NOT touch unrelated containers

## Docker Image Contents

The `ghcr.io/sauraku/devops:main` image includes:
- Go binary at `/usr/local/bin/devops-control`
- Deploy scripts: `deploy/project.sh`
- Runner compose: `deploy/runner/docker-compose.runner.yml`, `deploy/runner/Dockerfile.runner`, `deploy/runner/entrypoint.sh`
- Backup/restore scripts: `scripts/backup-db.sh`, `scripts/restore-db.sh`
- Shell tools: `bash`, `curl`, `gzip`, `python3`, `openssl`

## Key Constraints

- Backend binds `127.0.0.1` by default (prod inside Docker uses `0.0.0.0`)
- Prod `BASE_DIR` must be `/opt/devops-control` (enforced by `-e BASE_DIR`)
- Runner is non-root and has no Docker socket, Docker CLI, sudo, SSH keys, or host project mounts
- Deploy scripts need `bash`, `curl`, `gzip`, `python3`, `openssl`
- `.env.template` must NOT have hardcoded `DATABASE_URL` — compose generates it from `POSTGRES_PASSWORD`
- Data persistence: volume maps `~/.devops-control` → `/opt/devops-control`
- Project data survives devops-control restarts (SQLite + Docker volumes persist)

## Security Notes

- Devops token is single-password auth (no multi-user)
- `ENCRYPTION_KEY` protects separate runner, registry, and project-environment records
- The runner can only call its project-scoped deploy route and read that project's operation status over the dedicated control network with a scoped token
- The browser session cookie is signed and expiring; the master token is never embedded in UI assets
- All child processes receive only `PATH` + `HOME` + explicit env vars (not full `os.Environ()`)
- Deploy locks are reconciled on startup and preserved while their owner process is alive

## Common Tasks

### Check project container health
```bash
curl -s -b /tmp/sc https://{devops-domain}/api/projects/{project-id}/status | python3 -m json.tool
```

### Trigger deploy via API
```bash
curl -s -b /tmp/sc -X POST "https://{devops-domain}/api/projects/{project-id}/deploy" \
  -H "Content-Type: application/json" -H "X-CSRF-Token: $CSRF" \
  -d '{"ref":"main","branch":"main","confirmation":"deploy"}'
```

### Start/restart runner
```bash
curl -s -b /tmp/sc -X POST "https://{devops-domain}/api/projects/{project-id}/runner" \
  -H "Content-Type: application/json" -H "X-CSRF-Token: $CSRF" \
  -d '{"action":"start"}'
```
