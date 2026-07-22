# DevOps Control TODO

## Environment configuration integrity

- [ ] Replace destructive environment-map replacement with explicit patch semantics:
  - Preserve saved values for keys omitted from an update request.
  - Require an explicit `clear` operation to remove a saved value.
  - Record an audit event containing only added, updated, and cleared key names (never values).
  - Add regression coverage for a full Medusa SMTP bulk save followed by a partial update, proving all SMTP values remain available to deployment.
  - Note: the same whole-row-replace anti-pattern causes the Pause/Resume state wipe under P0 below — solve both with one patch-semantics design.

## P0 — Correctness & security (fix before next deploy)

- [ ] **Project-state updates can race and wipe fields** (`internal/api/handlers.go:244-248, 273-276`, `internal/services/deploy.go` → `internal/db/projects.go:238-254`): column-level patching protects pause/resume, but deploy completion and abort still read the full state then write stale values. Make all callers submit only the fields they own.
- [ ] **Deploy lock release needs a zero-row diagnostic** (`internal/db/locks.go:63`): `ReleaseLock` deletes by `project_id AND operation_id`, but a zero-row result is silently ignored. Warn or return a typed ownership-mismatch result.
- [x] **WebSocket concurrent-write panic + dead broadcast** (`internal/api/websocket.go:154-164, 182`): per-connection `sync.Mutex` guard; log streamer simplified.
- [x] **Medusa admin password in `docker exec` argv** (`deploy/project.sh`): stream credentials over stdin rather than placing them in the host command line.
- [x] **Bootstrap re-run silently wipes GHCR auth** (`deploy/bootstrap.sh`): rely on the persisted env-file value when the bootstrap invocation omits `GITHUB_TOKEN`.
- [x] **Backup/restore scripts over-trust the project env file** (one root cause, six exploits):
  - [x] Restore controller-owned paths (`STATE_DIR`, `BACKUP_DIR`, `LOCK_DIR`, `MANIFEST_FILE`) after `load_dotenv`, so an env file cannot redirect locking or output.
  - [x] Validate `BACKUP_ID` charset in `scripts/backup-db.sh:78` (restore-db.sh already does — drift).
  - [x] Verify backup SHA256 against `manifest.jsonl` before `pg_restore` (`scripts/restore-db.sh:137-231` never reads the manifest it consumes from).
  - [x] Reject tar members with `..`/absolute paths before extracting downloaded archives (`scripts/restore-db.sh:333-334` — currently RCE-via-backup-bucket).
  - [x] Hardcode `https://oauth2.googleapis.com/token`; ignore dotenv and credential-file token-URI values.
  - [x] Pass OAuth bearer tokens to curl through a private temporary config rather than argv.

## P1 — Security hardening

- [ ] **Plaintext transport in prod:** master cookie + SSH password traverse `http://`/`ws://` (`ui/src/components/Terminal.tsx:52`, `auth.go` `CookieSecure:false`). Document TLS-terminator requirement; refuse terminal unless TLS.
- [x] **Env shadowing in `dockerCommandEnv`** (`internal/docker/client.go`): use a shared reserved-key policy for Docker and deployment process environments.
- [ ] **`docker login` writes to shared global DOCKER_CONFIG** (`internal/docker/client.go:688`): two projects on one registry → last-login-wins. Fix: per-project `DOCKER_CONFIG` dirs.
- [x] **No rate limit on `LoginHandler`** (`internal/api/auth.go`): bounded per-IP exponential backoff with failure audit events.
- [x] **`ContainerReadFile` unbounded** (`internal/docker/client.go`, `handlers.go`): stream through a 10MB limit and return a bounded client response.
- [x] **`ComposeRecreate` proceeds when env file missing** (`internal/docker/client.go:512-516`) while the down path hard-fails. Fail closed.
- [ ] **WS log stream panics on truncation** (`internal/api/websocket.go:111` negative `make` when file shrinks) and has **no write deadlines** (half-dead clients leak goroutines). A deadline is now set, but failed writes do not close and remove the connection, so half-dead clients can still leak goroutines.
- [ ] **Error leaks:** many handlers return `err.Error()` wrapping absolute paths and full compose output (`internal/api/handlers.go:96-483`, `internal/docker/client.go:269`). Route internal failures through `internalError`; sanitize client-facing messages.
- [ ] **Token namespace confusion:** master token and project-scoped token share the `X-Deploy-Control-Token` header (`internal/api/auth.go:31-49`). Separate header names.
- [x] **WS route ignores its own `deployID`** and streams any `?name=` project log (`internal/api/websocket.go:166-183`). Derive log name from deployID; drop the query param.
- [ ] **SQLite:** `SetMaxOpenConns(4)` invites writer contention (db.go:24 — use 1 for modernc sqlite); `ALTER TABLE` migrations swallow all errors and there is no `schema_version` table (db.go:169). Add versioned migrations.
- [x] **`detectComposeBinary` has no timeout at startup** (`internal/docker/client.go:33-39`) — a hung daemon blocks controller boot indefinitely.
- [ ] **Scripts hardening:**
  - [x] `scripts/restore-db.sh:25`: wrong compose-file default/cwd — never `cd`s into `PROJECT_DIR` like backup-db.sh does.
  - [x] `scripts/list-releases.sh:6`: now reads `${BASE_DIR}/Releases/${PROJECT_ID}` instead of flat path.
  - `deploy/teardown.sh:272`: kills host processes by `ps` substring match; also never collects the runner network (label `com.sauraku.devops.role=runner-network`).
  - `deploy/runner/Dockerfile.runner:33-35`: Actions runner tarball fetched with no SHA256 verification — pin version + checksum.
  - `docker/Dockerfile`: runtime base `docker:24-cli` is a mutable EOL tag; no `HEALTHCHECK`; unused `py3-jwt`/`py3-cryptography` (scripts hand-roll JWT with openssl — either use the libs or drop them).
  - Installer is unpinned curl|bash from `main` (`bootstrap.sh:5`, `teardown.sh:7`) — document a tagged-release/commit-pinned URL.
  - [x] `deploy/bootstrap.sh:304`: publishes `-p "${HOST_PORT}:8787"` on all interfaces — default to `127.0.0.1` bind with an explicit escape hatch.
  - [x] `deploy/bootstrap.sh:177,195-198`: `chmod 600` on env file only on creation path — re-runs can leave appended secrets world-readable.
  - [x] `deploy/teardown.sh:8`: `set -uo pipefail` without `-e` masks mid-script failures.
  - [x] `deploy/teardown.sh`: add non-interactive `TEARDOWN_CONFIRM=yes` path and reject implicit non-TTY confirmation.
  - [x] `deploy/lib.sh:20-24`: extend dotenv blocklist (`TMPDIR`, `*_PROXY`, `DOCKER_*`, `PYTHON*`, `NODE_*`); tolerate readonly-var export failures.
  - [x] `deploy/runner/entrypoint.sh:19`: guard `rm -rf` against empty/`/` `RUNNER_DIR`; align default labels with compose (`development` only); bound the tee'd run log on the 128MB tmpfs.
  - [x] `deploy/runner/docker-compose.runner.yml`: remove `host.docker.internal:host-gateway`; runner callbacks use the dedicated control network.
  - [x] GCP private key tempfile unlinked only on happy path (`scripts/backup-db.sh:296-300`, `scripts/restore-db.sh:174-178`) — try/finally or in-process signing.
  - [x] Signal traps use explicit 130/143 exits so cleanup records cancellation as a failure.
  - [x] `deploy/project.sh:498-503`: `IMAGE_TAG` never charset-validated before being written into the dotenv file (newline → env injection).
  - [ ] `deploy/project.sh:85`: audit JSONL `actor` not JSON-escaped (message is; actor was missed). Escaping was added, but the format still wraps the JSON value in quotes, yielding invalid JSON for actor values.
- [ ] **Terminal polish** (`internal/api/terminal.go`): global rate-limit timestamp blocks all users for 5s (55-74 — [x] rate limit by IP address); SSH setup errors leak internal paths to the browser (119,135 — [x] client errors sanitized); `StdinPipe`/`StdoutPipe` errors ignored → nil deref (157-158 — [x] nil deref check added); `{`-prefix sniffing swallows pasted JSON as resize frames (232-241 — use a distinct frame type); only `id_rsa` attempted (106-112 — [x] add ed25519; agent support remains open).

## P2 — Design: replace patchy workarounds with proper abstractions

- [ ] **Unify the four operation runners:** `runDeploy` / `runBackup` / `runRestore` / `runRollback` are ~90 near-identical lines each (env build, process group, log file, exit-code mapping, lock release). Extract one `runOperation(script, args, env, timeout, logPath, kind)` helper with the shared reserved-env filter — backup/restore/rollback currently lack deploy's filtering and silently discard output when log creation fails (`internal/services/backup.go:214-218`).
- [ ] **Unify project-state and env-override storage around patch semantics** (see top section + P0 state wipe): one merge-then-write path used by pause/resume, deploy completion, abort, and env saves.
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
  - `walCheckpointer` has no stop tied to `DB.Close` and never restarts after a recovered panic (`internal/db/db.go:38-50`).
  - `ReleaseAllLocks` is a blanket delete with no owner-liveness check (`internal/db/locks.go:68-71`) — restrict to confirmed-dead owners per AGENTS.md.

## Supply chain & release hygiene

- [ ] Pin all base images by digest (runtime `docker:24-cli`, runner base).
- [ ] SHA256-verify the GitHub Actions runner tarball at image build.
- [ ] Publish a tagged/checksummed installer; stop documenting curl|bash from mutable `main`.
- [ ] Add `HEALTHCHECK` to the control-plane image (hit `/api/health`).
