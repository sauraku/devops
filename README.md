# DevOps Control Plane

A self-hosted Go and React control plane for Docker Compose projects. The Go binary embeds the UI, stores state in SQLite, serializes operations per project, and owns deploy, backup, restore, runner, and container lifecycle actions.

## Local development

```bash
./deploy/local.sh start
```

The script installs the locked UI dependencies, builds the UI and Go binary, builds the isolated runner image, and starts the portal at `http://127.0.0.1:8787`. Secrets are generated once in the gitignored `.env.local` with mode `0600`. Lifecycle mutations are serialized, the existing portal stays available until replacement builds succeed, and only the exact PID recorded for this checkout may be stopped. An unknown process on the configured port is never evicted.

`./deploy/local.sh status` performs a read-only health check. `./deploy/local.sh stop` stops only the process whose PID and command match this checkout. Neither command rebuilds artifacts or rewrites local configuration.

Run the full static and unit-test suite with:

```bash
make test
```

## Production bootstrap

```bash
GITHUB_TOKEN=<github-pat> \
  bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
```

Bootstrap pulls both `ghcr.io/sauraku/devops:main` and `ghcr.io/sauraku/devops-runner:main`, resolves the pulled artifacts to registry digests, and pins the running controller and future runners to those exact digests. Set `IMAGE` and `RUNNER_IMAGE` to explicit digest references when a release process selects them upstream. The portal is published on `127.0.0.1:8787`; expose it through a TLS reverse proxy instead of publishing it directly to the network. Secrets live in `/opt/devops-control/.env.prod` or the fallback `~/.devops-control/.env.prod`.

## Project contract

Each project publishes a config image at:

```text
ghcr.io/<owner>/<repo>-deploy-config:<image-tag>
```

The image must contain these files under `/app`:

- `docker-compose.yml`
- `.env.template`
- `devops.json`

The config image tag must match the requested application image tag. Runner callbacks supply the immutable `sha-<commit>` tag and are provenance-checked. A manual portal deploy intentionally redeploys the latest built image for the project's configured branch, using the matching mutable branch tag for both application and config images. The deploy is rejected if the config image cannot be pulled or any required file cannot be extracted. Existing cached configuration and application images are never used as a fallback after a pull failure.

The rendered Compose model is checked before it reaches the host Docker daemon. Services may not use privileged mode, host namespaces, added capabilities, devices, non-loopback published ports, or bind mounts outside the project workspace and project log directory.

Environment overrides must be declared in `.env.template`. They are validated as single-line dotenv values and encrypted in SQLite. Existing generated secrets are preserved across deploys. A project may use `devops.json` `environment.operator_required`, `environment.generated_secrets`, `environment.controller_managed`, and `environment.non_secret` to distinguish required operator input, generated credentials, derived values, and operational flags; without this contract, the portal retains its strict legacy blank-template checks. Dotenv files are parsed as data and are never sourced as shell code.

Example `devops.json` restore configuration:

```json
{
  "version": "1",
  "services": {
    "postgres": {
      "backup": { "database": "myapp", "user": "postgres" }
    },
    "backend": {
      "health": { "port": 9000, "path": "/health" },
      "restore": { "command": ["npx", "prisma", "migrate", "deploy"] }
    }
  },
  "logs": {
    "directory": ".",
    "container_internal": { "backend": "/logs/backend.log" }
  }
}
```

Restore commands are JSON argument arrays, not shell command strings. Restore creates and verifies a pre-restore backup, stops writers, uses `pg_restore --clean --if-exists --exit-on-error`, and automatically attempts recovery if the requested restore fails.

## GitHub runner deployment path

The per-project runner is intentionally unprivileged. It has no Docker socket, Docker CLI, sudo, SSH keys, project checkout mount, or control-plane data mount. It reaches the controller on the dedicated `devops-control-runners` network (or through the Docker host gateway for native local development) and receives a project-scoped token that authorizes only `POST /api/projects/<same-project>/deploy` and read-only `GET /api/projects/<same-project>/deployments/<operation-id>/status`. Scoped requests must carry complete GitHub provenance matching the project's registered repository and branch, including a full commit SHA and the exact immutable `sha-<commit>` image tag.

The deploy response is asynchronous and returns a stable `operation.id` plus `status_url`. Callers must poll that status until `operation.terminal` is true and treat only `operation.successful: true` with `status: success` as success. The phases are `pending`, `manual_approval`, `running`, and `terminal`. When auto-apply is disabled, the operation remains in `pending_approval` until an authenticated portal session approves that exact operation; approval is a CSRF-protected mutation and is never authorized by the runner token.

```yaml
deploy:
  runs-on: [self-hosted, project-myapp, production]
  steps:
    - name: Request deployment
      env:
        GH_REF: ${{ github.ref }}
        GH_SHA: ${{ github.sha }}
        GH_BRANCH: ${{ github.ref_name }}
        GH_RUN_ID: ${{ github.run_id }}
        GH_RUN_NUMBER: ${{ github.run_number }}
        GH_ACTOR: ${{ github.actor }}
        GH_REPOSITORY: ${{ github.repository }}
        GH_WORKFLOW: ${{ github.workflow }}
      run: |
        payload=$(jq -n \
          --arg ref "$GH_REF" \
          --arg sha "$GH_SHA" \
          --arg branch "$GH_BRANCH" \
          --arg run_id "$GH_RUN_ID" \
          --arg run_number "$GH_RUN_NUMBER" \
          --arg actor "$GH_ACTOR" \
          --arg repository "$GH_REPOSITORY" \
          --arg workflow "$GH_WORKFLOW" \
          '{ref:$ref,sha:$sha,branch:$branch,image_tag:("sha-"+$sha),confirmation:"deploy",
            github_run_id:$run_id,github_run_number:$run_number,github_actor:$actor,
            github_repository:$repository,github_workflow:$workflow}')
        response=$(curl --fail-with-body --silent --show-error \
          -H "Content-Type: application/json" \
          -H "X-Deploy-Control-Token: ${DEPLOY_CONTROL_TOKEN:?missing scoped token}" \
          --data "$payload" \
          "${DEPLOY_CONTROL_URL:?missing control URL}/api/projects/myapp/deploy")
        operation_id=$(jq -er '.operation.id' <<<"$response")
        # Poll the project-scoped status route with a bounded deadline and fail
        # unless this exact operation reaches terminal success.
```

## Security model

- Browser login exchanges the master token for a signed, expiring, HttpOnly, SameSite=Strict session cookie.
- Browser mutations require a per-session CSRF token; the master token is never embedded in HTML or JavaScript.
- Master bearer authentication remains available for trusted administration automation.
- Runner tokens, registry passwords, and project environment overrides use separate encrypted records.
- App and runner container actions resolve exact Docker Compose ownership labels; names and prefixes are not treated as ownership.
- Operation locks are per project and survive control-plane restarts while the owner process is alive.
- The browser SSH terminal is disabled by default. Enabling it also requires a valid `SSH_KNOWN_HOSTS` file; unknown host keys are rejected.

## Main paths

```text
cmd/devops-control/       Go entrypoint and TCP/Unix listeners
internal/api/             HTTP, session auth, CSRF, WebSocket handlers
internal/services/        Project, deploy, backup, restore orchestration
internal/docker/          Docker Compose ownership and policy boundary
internal/db/              SQLite schema and encrypted storage
deploy/project.sh         Fail-closed project deployment
deploy/runner/            Isolated GitHub runner image and Compose model
scripts/                  Backup, restore, rollback helpers
ui/                       React frontend
```
