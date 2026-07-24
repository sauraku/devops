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
DEPLOY_CONTROL_PUBLIC_URL=https://<devops-domain> \
  TRUSTED_PROXY_CIDRS=<proxy-ip-or-cidr> \
  GITHUB_TOKEN=<github-pat> \
  bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
```

Bootstrap pulls both `ghcr.io/sauraku/devops:main` and `ghcr.io/sauraku/devops-runner:main`, resolves the pulled artifacts to registry digests, and pins the running controller and future runners to those exact digests. The bootstrap GitHub credential is also the default GHCR pull credential for projects; its Docker auth file exists only in the controller's tmpfs. Set `IMAGE` and `RUNNER_IMAGE` to explicit digest references when a release process selects them upstream. The portal is published on `127.0.0.1:8787`; keep that loopback binding and expose it only through a TLS reverse proxy. A firewall is not a substitute for TLS because login and terminal credentials are sent over the browser connection. Secrets live in `/opt/devops-control/.env.prod` or the fallback `~/.devops-control/.env.prod`.

When `COOKIE_SECURE=true`, bootstrap requires both an exact `TRUSTED_PROXY_CIDRS` value and `DEPLOY_CONTROL_PUBLIC_URL`. The public URL must be an HTTPS origin without credentials, path, query, or fragment. Bootstrap first waits for the controller's loopback `/api/health`, then verifies `/api/health` and `/login` through the public origin with normal TLS certificate verification. An upgrade is committed only after those external checks pass; otherwise bootstrap rolls back to the prior controller. Omitting either variable preserves its existing value, a supplied nonempty value replaces it, and an explicitly supplied empty value clears it. Explicitly clearing `GITHUB_TOKEN` removes only the controller environment entry and does not alter unrelated Docker authentication on the host.

Set `COOKIE_SECURE=true` in production and configure `TRUSTED_PROXY_CIDRS` as a comma-separated list containing only the IP addresses or CIDRs of reverse proxies that connect directly to the controller or appear as trusted hops in `X-Forwarded-For`. Bare IP addresses are accepted and treated as `/32` or `/128`; malformed entries prevent startup. Do not trust an entire LAN or container network unless every address in it is controlled as a proxy. The default is empty and ignores all forwarding headers.

The release image exposes a validation-only mode used by bootstrap before replacing the running controller:

```bash
docker run --rm \
  -e TRUSTED_PROXY_CIDRS="${TRUSTED_PROXY_CIDRS:-}" \
  ghcr.io/sauraku/devops:main validate-trusted-proxy-cidrs
```

It runs the controller's exact parser without initializing credentials, SQLite, or Docker access and exits `0` for a valid value or `2` for invalid input.

The final trusted proxy must:

- set `X-Forwarded-Proto` to exactly `https` rather than preserving a client-supplied value;
- either overwrite `X-Forwarded-For` with the client address or append its verified peer address to a valid proxy chain; and
- pass the original `Host` header unchanged.

Use the immediate peer address observed by the controller, not the proxy's host-side listen address. A host Nginx connection to a container commonly appears as a Docker bridge or gateway address rather than `127.0.0.1`. Make one request through the proxy, inspect the controller request log's remote address, verify that it belongs to the intended proxy, and configure that exact IP as `/32` or `/128` where possible. Then configure Nginx to overwrite client-supplied forwarding values:

```nginx
proxy_set_header Host $http_host;
proxy_set_header X-Forwarded-For $remote_addr;
proxy_set_header X-Forwarded-Proto https;
```

Forwarding headers received from an untrusted immediate peer are ignored. Malformed or missing trusted forwarding headers fail closed to the direct peer and plaintext scheme. With secure cookies enabled, plaintext browser requests never render the token form. Both the deployment-log and SSH-terminal websocket endpoints require effective HTTPS and enforce same origin, except for direct loopback development requests whose peer and host are both loopback.

### Upgrading an existing installation

The first upgrade to a secure-proxy-aware release must explicitly supply the public HTTPS origin and the exact immediate proxy peer observed by the controller. Generate one request through the production proxy, then read the peer address from the controller request log:

```bash
docker logs devops-control
```

Each request line includes the direct `IP:port` peer. Verify that IP belongs to the intended reverse proxy, express a single IPv4 address as `/32` or IPv6 address as `/128`, and run:

```bash
DEPLOY_CONTROL_PUBLIC_URL=https://<devops-domain> \
  TRUSTED_PROXY_CIDRS=<observed-proxy-ip>/32 \
  GITHUB_TOKEN=<github-pat> \
  bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
```

Use `/128` for an IPv6 peer. Do not set `COOKIE_SECURE=false` to bypass this production requirement. Failures before the old controller is stopped leave it untouched. After rollout starts, bootstrap preserves the exact old container ID, restores it on candidate or public-route failure, and performs a bounded loopback health check. If automatic rollback itself fails, bootstrap exits with status `70` and prints identity-specific recovery commands; do not start a similarly named container without verifying its ID.

After a successful upgrade, verify both routes without disabling TLS validation:

```bash
curl --fail http://127.0.0.1:8787/api/health
curl --fail https://<devops-domain>/api/health
curl --fail https://<devops-domain>/login
```

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
