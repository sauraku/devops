# DevOps Control Plane

Go + React self-hosted DevOps dashboard. Single binary with embedded UI. Deploy Docker Compose projects, manage GitHub Actions runners, backup/restore databases.

## Quick Start (Local)

```bash
./local.sh
# Token: apple
# BASE_DIR: ./data (accept default)
# Opens http://localhost:8787
```

## Production Deploy

```bash
GITHUB_TOKEN=<pat> bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
```

Opens `http://<server-ip>:8787`. Token is saved at `~/.devops-control/.env.prod`.

## Production Teardown

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/teardown.sh)
```

## Adding a Project — Handshake Contract

To make your project deployable via devops-control, you need:

### Required

1. **Config image** — a Docker image published to GHCR at:
   ```
   ghcr.io/<owner>/<repo>-deploy-config:<branch>
   ```
   The image must contain these files at `/app/`:
   - `docker-compose.yml` — Compose file defining your services
   - `.env.template` — Environment variable template (see rules below)

2. **Application images** — your service images published to GHCR:
   ```
   ghcr.io/<owner>/<repo>-<service>:<branch>
   ```
   Referenced in your `docker-compose.yml` as `${IMAGE_TAG:-latest}`.

3. **`.env.template` rules:**
   - Use `change_me` placeholders for secrets (e.g. `POSTGRES_PASSWORD=change_me`)
   - Do NOT hardcode `DATABASE_URL` — the deploy script generates it from `POSTGRES_PASSWORD`
   - The template is parsed by devops-control UI for env var configuration
   - User overrides persist across redeploys

### Optional

4. **`devops.json`** — placed in the project root (local) or config image. Describes services, ports, backup config:
   ```json
   {
     "version": "1",
     "project_name": "My Project",
     "compose_file": "docker-compose.prod.yml",
     "env_template": ".env.template",
     "services": {
       "postgres": {
         "backup": {
           "database": "myapp",
           "user": "postgres"
         }
       },
       "backend": {
         "health": { "port": 9000, "path": "/health" },
         "restore": {
           "command": "npx prisma migrate deploy"
         }
       },
       "frontend": {
         "health": { "port": 3000, "path": "/" }
       }
     },
     "ports": {
       "main": { "postgres": 5434, "backend": 9001, "frontend": 3001 },
       "default": { "postgres": 5435, "backend": 9002, "frontend": 3002 }
     },
     "env_defaults": {
       "main": {
         "NEXT_PUBLIC_API_URL": "https://api.example.com",
         "COOKIE_SECURE": "true"
       }
     },
     "backup": {
       "file_paths": ["uploads"],
       "retention_days": 30,
       "schedule": "daily"
     }
   }
   ```
   See full schema at `internal/models/config.go`.

5. **`scripts/backup-db.sh`** — database backup script (called by devops on schedule)
   - Receives env vars: `COMPOSE_PROJECT_NAME`, `POSTGRES_DB`, `BACKUP_DIR`
   - Should dump DB to `$BACKUP_DIR`

6. **`scripts/restore-db.sh`** — database restore script (called from UI)
   - Receives env vars: `COMPOSE_PROJECT_NAME`, `POSTGRES_DB`, `RESTORE_FILE`
   - Should restore DB from `$RESTORE_FILE`

7. **GitHub Actions self-hosted runner workflow** — for CI/CD auto-deploy:
   ```yaml
   - name: Trigger devops deploy
     run: |
       curl -s -X POST "http://{server}:8787/api/projects/{project}/deploy" \
         -H "Content-Type: application/json" \
         -H "Authorization: Bearer ${{ secrets.DEPLOY_CONTROL_TOKEN }}" \
         -d '{"ref":"${{ github.ref_name }}","branch":"${{ github.ref_name }}","confirmation":"deploy"}'
   ```
   Requires `DEPLOY_CONTROL_TOKEN` secret set in GitHub repo.

## How Devops Finds Your Template

1. **Config image** — `ghcr.io/<owner>/<repo>-deploy-config:<branch>` is pulled and `docker cp` extracts `.env.template` + `docker-compose.yml`
2. **Local checkout** — falls back to `~/Documents/<project-id>/.env.template` (dev only)
3. Config image is the recommended approach for production

## Architecture

```
Go binary → embeds React UI
├── HTTP API (project CRUD, deploy, backup, runner management)
├── WebSocket (live log streaming)
├── Docker SDK (compose, container management)
└── SQLite (projects, deployments, backups, state)
```

- **Backend**: Go, SQLite, Docker SDK
- **Frontend**: React 19, TypeScript, Tailwind v4, TanStack Query, Vite
- **Deploy**: Docker multi-stage build → GHCR → deploy script pulls and runs
- **Auth**: Single token → cookie + CSRF

## Project Structure

```
├── cmd/devops-control/main.go   # Entry point
├── internal/
│   ├── api/                     # HTTP handlers, router, auth, WebSocket
│   │   └── ui/dist/             # Built React app (embedded via go:embed)
│   ├── db/                      # SQLite schema, queries, crypto
│   ├── docker/                  # Docker client
│   ├── models/                  # Data structures
│   └── services/                # Business logic (project, deploy, backup, audit)
├── ui/                          # React frontend source
├── scripts/                     # Backup/restore helper scripts
├── docker/Dockerfile            # Multi-stage build
├── deploy/runner/               # Runner image + compose + entrypoint
├── deploy/bootstrap.sh          # Production bootstrap
├── deploy/project.sh            # Project deploy (called by Go)
├── deploy/teardown.sh           # Production cleanup
├── deploy/local.sh              # Local dev
├── AGENTS.md                    # Agent reference
├── docs/DESIGN.md               # Design tokens
└── README.md                    # This file
```
