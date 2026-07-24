# DevOps Control TODO

## Environment configuration integrity

- [x] Replace destructive environment-map replacement with explicit patch semantics:
  - Preserve saved values for keys omitted from an update request.
  - Require an explicit `clear` operation to remove a saved value.
  - Record an audit event containing only added, updated, and cleared key names (never values).
  - Add regression coverage for a full Medusa SMTP bulk save followed by a partial update, proving all SMTP values remain available to deployment.
  - Note: the same whole-row-replace anti-pattern causes the Pause/Resume state wipe under P0 below — solve both with one patch-semantics design.

## P0 — Correctness & security (fix before next deploy)

- [x] **Project-state updates can race and wipe fields** (`internal/api/handlers.go`, `internal/services/deploy.go` → `internal/db/projects.go`): pause/resume, deploy completion, and abort now submit only operation-owned fields to the database patch path; concurrent pause state has regression coverage.
- [x] **Deploy lock release needs a zero-row diagnostic** (`internal/db/locks.go`): `ReleaseLock` returns a typed ownership error when no matching project/operation lock is deleted; service callers log or propagate release failures and wrong-owner releases have regression coverage.
- [x] **WebSocket concurrent-write panic + dead broadcast** (`internal/api/websocket.go:154-164, 182`): per-connection `sync.Mutex` guard; log streamer simplified.
- [x] **Medusa admin password in `docker exec` argv** (`deploy/project.sh`): stream credentials over stdin rather than placing them in the host command line.
- [x] **Bootstrap re-run silently wipes or cannot rotate GHCR auth** (`deploy/bootstrap.sh`): an omitted `GITHUB_TOKEN` preserves the persisted controller value, a supplied nonempty token atomically replaces and deduplicates it, and an explicitly empty token clears only that controller value. Supplied PATs authenticate pulls through a disposable mode-0700 Docker config and never modify caller/global Docker auth (`tests/bootstrap_token_test.sh`).
- [x] **Bootstrap update can destroy the working controller before proving its replacement** (`deploy/bootstrap.sh`): stage all env changes; give `TRUSTED_PROXY_CIDRS` and `DEPLOY_CONTROL_PUBLIC_URL` explicit omit/replace/clear semantics; run the controller image's exact proxy validator; resolve images and validate public TLS before stopping; preserve the exact owned old container ID by rename; require local and public health/login checks; atomically commit the env only after readiness; and roll back container plus env on failure. A restored controller also has a bounded health check, while incomplete rollback exits distinctly with identity-specific recovery commands. First-install failures publish neither env nor container (`tests/bootstrap_token_test.sh`).
- [x] **Runner stdout could delete persistent registration state** (`deploy/runner/entrypoint.sh`, `internal/services/project.go`): repository-controlled output is never an authorization signal. A controller-owned monitor verifies exact runner absence through GitHub, then serially removes the exact Compose-owned container and Compose-labelled state volume before registering a clean replacement. API/PAT failures preserve state and retry; no recovery credential or endpoint is exposed to jobs.
- [x] **Backup/restore scripts over-trust the project env file** (one root cause, six exploits):
  - [x] Restore controller-owned paths (`STATE_DIR`, `BACKUP_DIR`, `LOCK_DIR`, `MANIFEST_FILE`) after `load_dotenv`, so an env file cannot redirect locking or output.
  - [x] Validate `BACKUP_ID` charset in `scripts/backup-db.sh:78` (restore-db.sh already does — drift).
  - [x] Verify backup SHA256 against `manifest.jsonl` before `pg_restore` (`scripts/restore-db.sh:137-231` never reads the manifest it consumes from).
  - [x] Reject tar members with `..`/absolute paths before extracting downloaded archives (`scripts/restore-db.sh:333-334` — currently RCE-via-backup-bucket).
  - [x] Hardcode `https://oauth2.googleapis.com/token`; ignore dotenv and credential-file token-URI values.
  - [x] Pass OAuth bearer tokens to curl through a private temporary config rather than argv.

## P1 — Security hardening

- [x] **Plaintext browser credentials in prod:** secure-cookie deployments refuse the login form and submission over non-HTTPS, while terminal and deployment-log WebSockets require effective HTTPS. Forwarded transport is honored only from configured trusted proxies; direct loopback HTTP remains available for local development.
- [x] **Env shadowing in `dockerCommandEnv`** (`internal/docker/client.go`): use a shared reserved-key policy, including Docker/Compose wildcard namespaces, for Docker and deployment process environments.
- [x] **Project registry logins shared one global DOCKER_CONFIG:** explicit project credentials now authenticate in independent mode-0700 per-operation Docker config directories. Config extraction, deploy, and rollback receive only their operation's directory, which is removed on completion; concurrent-project isolation has regression coverage.
- [x] **A project PAT could register its runner but private GHCR deployment still failed:** deployments now reuse a stored long-lived GitHub PAT only when no explicit registry password exists, the repository is a strict HTTPS GitHub repository, and the registry is exactly `ghcr.io`. The rendered Compose policy limits authenticated pulls to that repository's package namespace and exact immutable deployment tag. Explicit credentials win, non-GHCR registries never receive the PAT, all registry-helper output is suppressed so credentials cannot be reflected into logs, and the isolated auth directory is removed after the operation.
- [x] **No rate limit on `LoginHandler`** (`internal/api/auth.go`): bounded per-client-IP exponential backoff with failure audit events; forwarding headers affect the key only through explicitly trusted proxies.
- [x] **`ContainerReadFile` unbounded** (`internal/docker/client.go`, `handlers.go`): stream through a 10MB limit and return a bounded client response.
- [x] **`ComposeRecreate` proceeds when env file missing** (`internal/docker/client.go:512-516`) while the down path hard-fails. Fail closed.
- [x] **WS failed-write cleanup** (`internal/api/websocket.go`): truncation is handled, writes have deadlines, and any deadline/write failure closes and unregisters the client so half-dead connections cannot retain streamer goroutines.
- [x] **Error leaks:** mixed service/infrastructure failures are logged server-side and return stable operation-specific messages; pure request-validation errors remain actionable, and internal container/Compose/path output is never sent to the browser.
- [x] **Token namespace confusion:** project runners retain `X-Deploy-Control-Token`, while master API clients use `Authorization: Bearer` or the distinct `X-DevOps-Control-Token`; neither header authenticates in the other namespace.
- [x] **WS route ignores its own `deployID`** and streams any `?name=` project log (`internal/api/websocket.go:166-183`). Derive log name from deployID; drop the query param.
- [x] **SQLite migrations:** migrations are versioned and transactional in `schema_migrations`, fail on unknown future schemas, add missing columns only after introspection, and have fresh/existing/idempotent regression coverage.
- [x] **`detectComposeBinary` has no timeout at startup** (`internal/docker/client.go:33-39`) — a hung daemon blocks controller boot indefinitely.
- [ ] **Scripts hardening:**
  - [x] `scripts/restore-db.sh:25`: wrong compose-file default/cwd — never `cd`s into `PROJECT_DIR` like backup-db.sh does.
  - [x] `scripts/list-releases.sh:6`: now reads `${BASE_DIR}/Releases/${PROJECT_ID}` instead of flat path.
  - `deploy/teardown.sh`: kills host processes by `ps` substring match without proving process ownership. Runner-network collection is fixed.
  - [x] `deploy/runner/Dockerfile.runner`: Actions runner version and per-architecture SHA256 checksums are pinned; downloads fail closed before extraction.
  - `docker/Dockerfile`: runtime base `docker:24-cli` is a mutable EOL tag; no `HEALTHCHECK`; unused `py3-jwt`/`py3-cryptography` (scripts hand-roll JWT with openssl — either use the libs or drop them).
  - Installer is unpinned curl|bash from `main` (`bootstrap.sh:5`, `teardown.sh:7`) — document a tagged-release/commit-pinned URL.
  - [x] `deploy/bootstrap.sh:304`: publishes `-p "${HOST_PORT}:8787"` on all interfaces — default to `127.0.0.1` bind with an explicit escape hatch.
  - [x] `deploy/bootstrap.sh:177,195-198`: `chmod 600` on env file only on creation path — re-runs can leave appended secrets world-readable.
  - [x] `deploy/teardown.sh:8`: `set -uo pipefail` without `-e` masks mid-script failures.
  - [x] `deploy/teardown.sh`: add non-interactive `TEARDOWN_CONFIRM=yes` path and reject implicit non-TTY confirmation.
  - [x] `deploy/lib.sh:20-24`: extend dotenv blocklist (`TMPDIR`, `*_PROXY`, `DOCKER_*`, `PYTHON*`, `NODE_*`); tolerate readonly-var export failures.
  - [x] `deploy/runner/entrypoint.sh`: guard `rm -rf` against empty/`/` `RUNNER_DIR`; align default labels with compose (`development` only); execute the runner directly so output and exit status are byte-preserved without a detector, FIFO, or temporary log. Recovery decisions belong exclusively to the controller and GitHub API (`tests/runner_entrypoint_test.sh`).
  - [x] `deploy/runner/docker-compose.runner.yml`: remove `host.docker.internal:host-gateway`; runner callbacks use the dedicated control network.
  - [x] GCP private key tempfile unlinked only on happy path (`scripts/backup-db.sh:296-300`, `scripts/restore-db.sh:174-178`) — try/finally or in-process signing.
  - [x] Signal traps use explicit 130/143 exits so cleanup records cancellation as a failure.
  - [x] `deploy/project.sh:498-503`: `IMAGE_TAG` never charset-validated before being written into the dotenv file (newline → env injection).
  - [x] `deploy/project.sh`: audit JSONL actor and message values are encoded once as JSON values; quotes, slashes, and control characters cannot corrupt the record.
- [ ] **Terminal polish** (`internal/api/terminal.go`): rate limiting is per trusted client; SSH setup errors are sanitized; pipe errors are checked; resize controls require an explicit `{"type":"resize"}` frame so pasted JSON remains terminal input; ed25519 and RSA keys are supported. SSH-agent forwarding/support remains open.

## P2 — Design: replace patchy workarounds with proper abstractions

- [ ] **Unify the four operation runners:** `runDeploy` / `runBackup` / `runRestore` / `runRollback` still duplicate process-group, log, exit-code, and lock-release handling. All four now use the same controller-owned environment allowlist, so the remaining work is to extract one `runOperation(script, args, env, timeout, logPath, kind)` helper and make log-open failures explicit.
- [x] **Use patch semantics for project state and environment overrides:** state callers submit only owned fields to the SQL patch, while environment saves use an encrypted transactional read-modify-write with explicit clear keys. Pause/resume, deploy completion, abort, partial SMTP updates, and explicit clears have regression coverage.
- [ ] **Move duplicated script blocks into `deploy/lib.sh`:** `branch_slug` ×3, compose-binary detect ×3, `verify_owned_container` ×2 verbatim, Firebase JWT/upload ×2 verbatim (drift has already caused bugs — see list-releases and BACKUP_ID validation).
- [x] **Delete the hardcoded service fallback** (`internal/services/project.go`): return no inferred services when neither `devops.json` nor Compose parsing provides them.
- [x] **`case "deployments"` → ProjectStatus fake endpoint** (`internal/api/router.go:273-274`): now returns 501 instead of wrong data.
- [ ] **Gate the sibling-checkout fallback in `ensureEnvTemplate` behind a dev flag** (`internal/services/project.go:907-918`) — local-dev workaround in prod code.
- [ ] **Make `ProjectRoot` explicit** (`cmd/devops-control/main.go:190`): currently assumes scripts sit next to the binary (`filepath.Dir(exe)`). Env var + startup fail-fast validation that `deploy/project.sh` exists.
- [x] **Atomic env-file writes** (`internal/services/project.go`): write sorted keys to a same-directory temporary file, sync, then rename.
- [ ] **Schedule removal of legacy shims:** `MigrateLegacyRunnerTokens`, `MigrateLegacyProjectEnvOverrides`, legacy project-dir cleanup (`project.go:392-396`), "legacy strict blank-template gating" (`project.go:1028`). Pick a cutoff version.
- [ ] **Small cleanups:**
  - Deprecated `strings.Title` + hand-conjugated audit verbs (`internal/services/project.go:1256,1261`) — use static messages.
  - [x] Duplicated dead `err` check in `serveStatic` (`internal/api/router.go:142-149`).
  - [x] Dead `read_devops_json` with python string-interpolation injection flaw (`deploy/project.sh:195-214`) — delete it.
  - [x] WS upgrade check is case-sensitive (`internal/api/router.go:310`) — RFC 7230 tokens are case-insensitive.
  - Backup scheduler: double-wrapped goroutine (main `SafeGo` + internal `safeGo`) and `time.Sleep` design — no missed-run catch-up, no config reload (`internal/services/backup.go:741-776`).
  - [x] Hand-rolled `stringsTrim`/`containsRune` reimplement stdlib (`internal/db/projects.go:42-59`).
  - [ ] `docker compose` v1 fallback is half-broken (v1 `config` lacks `--format json`) and v1 is EOL — require the v2 plugin (`deploy/project.sh:176-179` and twins). The Go Docker client still falls back to `docker-compose`.
  - [x] Leftover debug logging on every status poll (`internal/api/handlers.go:112-115`).
  - [x] Post-`up` restart-policy enforcement resolves exact Compose labels and surfaces failures.
  - `teardownPlaceholder` guesses types by key suffix (`internal/services/project.go:453-464`) — works, but document as deliberate or replace with typed template metadata.
  - [x] `walCheckpointer` stops and joins on `DB.Close`; a recovered checkpoint panic is isolated to one tick rather than terminating the scheduler.
  - `ReleaseAllLocks` is a blanket delete with no owner-liveness check (`internal/db/locks.go:68-71`) — restrict to confirmed-dead owners per AGENTS.md.

## Supply chain & release hygiene

- [ ] Pin all base images by digest (runtime `docker:24-cli`, runner base).
- [x] SHA256-verify the GitHub Actions runner tarball at image build.
- [ ] Publish a tagged/checksummed installer; stop documenting curl|bash from mutable `main`.
- [ ] Add `HEALTHCHECK` to the control-plane image (hit `/api/health`).
