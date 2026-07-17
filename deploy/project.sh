#!/usr/bin/env bash
set -euo pipefail
umask 077

SERVER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-${SERVER_DIR}}"
# shellcheck source=lib.sh
source "${SERVER_DIR}/lib.sh"

PROJECT_ID="${1:-}"
BRANCH="${2:-}"
IMAGE_TAG="${3:-}"

if [ -z "${PROJECT_ID}" ] || [ -z "${BRANCH}" ] || [ -z "${IMAGE_TAG}" ]; then
  echo "Usage: $(basename "$0") <project_id> <branch> <image_tag>" >&2
  exit 2
fi

case "${PROJECT_ID}" in
  *[!a-z0-9_.-]*|"")
    echo "Error: invalid project id '${PROJECT_ID}'." >&2
    exit 2
    ;;
esac

normalize_ref_name() {
  local ref="$1"
  case "${ref}" in
    refs/heads/*) echo "${ref#refs/heads/}" ;;
    origin/*) echo "${ref#origin/}" ;;
    *) echo "${ref}" ;;
  esac
}

branch_slug() {
  local ref
  ref="$(normalize_ref_name "$1" | tr '[:upper:]' '[:lower:]')"
  ref="$(printf '%s' "${ref}" | sed -E 's/[^a-z0-9_.-]+/-/g; s/^[.-]+//; s/[.-]+$//')"
  if [ -z "${ref}" ]; then
    ref="rc"
  fi
  if ! printf '%s' "${ref}" | grep -Eq '^[a-z0-9]'; then
    ref="branch-${ref}"
  fi
  printf '%s\n' "${ref}"
}

BRANCH="$(normalize_ref_name "${BRANCH}")"
BRANCH_SLUG="$(branch_slug "${BRANCH}")"
PROJECT_DIR="${PROJECT_DIR:-${BASE_DIR}/Projects/${PROJECT_ID}}"
ENV_FILE="${PROJECT_ENV_FILE:-${PROJECT_DIR}/.env.${BRANCH_SLUG}}"
COMPOSE_FILE="${PROJECT_COMPOSE_FILE:-${PROJECT_DIR}/docker-compose.yml}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-${PROJECT_ID}-${BRANCH_SLUG}}"
STATE_DIR="${PROJECT_STATE_DIR:-${BASE_DIR}/State/${PROJECT_ID}}"
RELEASE_DIR="${PROJECT_RELEASE_DIR:-${BASE_DIR}/Releases/${PROJECT_ID}}"
LOG_DIR="${PROJECT_LOG_DIR:-${BASE_DIR}/Logs/${PROJECT_ID}}"
LOCK_DIR="${STATE_DIR}/deploy-lock"
STATE_FILE="${STATE_DIR}/deploy-control.json"
AUDIT_LOG="${LOG_DIR}/deploy-control-audit.log"
DEPLOY_ID="${DEPLOY_ID:-project-${PROJECT_ID}-$(date -u +"%Y%m%dT%H%M%SZ")-$$}"
DEPLOY_STARTED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
LOCK_ACQUIRED=0
temp_container=""
config_tmp_dir=""
compose_config_json=""

mkdir -p "${STATE_DIR}" "${RELEASE_DIR}" "${LOG_DIR}"
PROJECT_LOG_DIR="${LOG_DIR}"
export PROJECT_LOG_DIR

json_escape() {
  python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
}

audit() {
  local action="$1"
  local status="${2:-info}"
  local message="${3:-}"
  printf '{"timestamp":"%s","project_id":"%s","action":"%s","status":"%s","deploy_id":"%s","actor":"%s","message":%s}\n' \
    "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
    "${PROJECT_ID}" \
    "${action}" \
    "${status}" \
    "${DEPLOY_ID}" \
    "${DEPLOY_ACTOR:-${GITHUB_ACTOR:-unknown}}" \
    "$(json_escape "${message}")" >> "${AUDIT_LOG}"
}

write_state() {
  local status="$1"
  local message="${2:-}"
  local now
  now="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  python3 - "$STATE_FILE" "$status" "$message" "$now" <<'PY'
import json
import os
import sys

path, status, message, now = sys.argv[1:5]
try:
    with open(path, "r", encoding="utf-8") as handle:
        state = json.load(handle)
except (OSError, json.JSONDecodeError):
    state = {}

state.update({
    "last_requested_ref": os.environ.get("DEPLOY_REF", ""),
    "last_requested_sha": os.environ.get("DEPLOY_SHA", ""),
    "last_deploy_status": status,
    "last_deploy_message": message,
    "last_run_at": now,
    "active_deploy_id": os.environ.get("DEPLOY_ID", ""),
})
if status == "success":
    state.update({
        "last_deployed_commit": os.environ.get("DEPLOY_SHA", ""),
        "last_deployed_image_tag": os.environ.get("IMAGE_TAG", ""),
        "last_deployed_at": now,
        "active_deploy_id": "",
    })
elif status in {"failed", "blocked"}:
    state["active_deploy_id"] = ""

os.makedirs(os.path.dirname(path), exist_ok=True)
tmp_path = path + ".tmp"
with open(tmp_path, "w", encoding="utf-8") as handle:
    json.dump(state, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.replace(tmp_path, path)
PY
}

acquire_lock() {
  if mkdir "${LOCK_DIR}" 2>/dev/null; then
    LOCK_ACQUIRED=1
    {
      echo "operation=deploy"
      echo "project_id=${PROJECT_ID}"
      echo "deploy_id=${DEPLOY_ID}"
      echo "pid=${DEPLOY_PROCESS_PID:-$$}"
      echo "started_at=${DEPLOY_STARTED_AT}"
      echo "branch=${BRANCH}"
      echo "image_tag=${IMAGE_TAG}"
    } > "${LOCK_DIR}/info"
    return 0
  fi
  echo "Error: another operation is active for project '${PROJECT_ID}'." >&2
  if [ -f "${LOCK_DIR}/info" ]; then
    sed -n '1,20p' "${LOCK_DIR}/info" >&2 || true
  fi
  return 1
}

cleanup() {
  local rc=$?
  if [ -n "${temp_container}" ]; then
    docker rm -f "${temp_container}" >/dev/null 2>&1 || true
  fi
  if [ -n "${config_tmp_dir}" ]; then
    rm -rf "${config_tmp_dir}"
  fi
  if [ -n "${compose_config_json}" ]; then
    rm -f "${compose_config_json}"
  fi
  if [ "${LOCK_ACQUIRED}" -eq 1 ]; then
    rm -rf "${LOCK_DIR}"
  fi
  if [ "${rc}" -ne 0 ]; then
    audit "deploy_finished" "failed" "deploy exited with status ${rc}"
    write_state "failed" "deploy exited with status ${rc}"
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

COMPOSE_CMD=(docker compose)
if ! docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
fi

if ! acquire_lock; then
  audit "deploy_blocked" "blocked" "lock exists"
  write_state "blocked" "operation lock exists"
  exit 1
fi

# ---------------------------------------------------------------------------
# Load devops.json from project directory (the project-source-of-truth contract)
# ---------------------------------------------------------------------------
DEVOPS_JSON="${PROJECT_DIR}/devops.json"
read_devops_json() {
  if [ ! -f "${DEVOPS_JSON}" ]; then
    return
  fi
  python3 -c "
import json, sys
data = json.load(open('${DEVOPS_JSON}'))
dotpath = sys.argv[1]
parts = dotpath.split('.')
val = data
for p in parts:
    if isinstance(val, dict):
        val = val.get(p, {})
    elif isinstance(val, list):
        val = {}
    else:
        val = {}
if val is None:
    val = {}
if isinstance(val, (dict, list)):
    print(json.dumps(val))
else:
    print(str(val))
" "$1" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Pull deploy config image (compose + env template)
# ---------------------------------------------------------------------------
if [ -n "${REPO_URL:-}" ]; then
  CLEAN_URL="${REPO_URL%.git}"
  CLEAN_URL="${CLEAN_URL%/}"
  OWNER_REPO=$(echo "${CLEAN_URL}" | awk -F'[:/]' '{print $(NF-1)"/"$NF}' | tr '[:upper:]' '[:lower:]')
  REGISTRY="${REGISTRY_HOST:-ghcr.io}"
  # Deploy the configuration built from the same immutable commit as the app
  # images. Branch tags are mutable and could otherwise mix two revisions in
  # one release if another workflow finishes between the build and deploy.
  CONFIG_IMAGE="${REGISTRY}/${OWNER_REPO}-deploy-config:${IMAGE_TAG}"

  echo "Pulling deployment configuration image: ${CONFIG_IMAGE}"
  if docker pull "${CONFIG_IMAGE}"; then
    echo "Extracting compose and env template from config image..."
    temp_container=$(docker create "${CONFIG_IMAGE}")
    config_tmp_dir=$(mktemp -d "${PROJECT_DIR}/.config-extract.XXXXXX")
    docker cp "${temp_container}:/app/docker-compose.yml" "${config_tmp_dir}/docker-compose.yml"
    if ! docker cp "${temp_container}:/app/.env.template" "${config_tmp_dir}/.env.template" 2>/dev/null; then
      docker cp "${temp_container}:/app/env.template" "${config_tmp_dir}/.env.template"
    fi
    docker cp "${temp_container}:/app/devops.json" "${config_tmp_dir}/devops.json"
    mv "${config_tmp_dir}/docker-compose.yml" "${COMPOSE_FILE}"
    mv "${config_tmp_dir}/.env.template" "${PROJECT_DIR}/.env.template"
    mv "${config_tmp_dir}/devops.json" "${DEVOPS_JSON}"
    docker cp "${temp_container}:/app/scripts/backup-db.sh" "${PROJECT_DIR}/scripts/backup-db.sh" 2>/dev/null || true
    docker cp "${temp_container}:/app/scripts/restore-db.sh" "${PROJECT_DIR}/scripts/restore-db.sh" 2>/dev/null || true
    docker rm "${temp_container}" >/dev/null 2>&1 || true
    temp_container=""
    rm -rf "${config_tmp_dir}"
    config_tmp_dir=""
  else
    echo "Error: Could not pull deployment configuration image ${CONFIG_IMAGE}." >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Generate env file from template. On first deploy, generate fresh secrets.
# On subsequent deploys, preserve existing secrets from the live env file so
# database passwords stay in sync with the data volume.
# ---------------------------------------------------------------------------
GENERATE=true
if [ "${GENERATE}" = true ] && [ -f "${PROJECT_DIR}/.env.template" ]; then
  echo "Generating environment file ${ENV_FILE} from template..."

  IS_MAIN="false"
  if [ "${BRANCH}" = "main" ]; then IS_MAIN="true"; fi

  python3 - "${PROJECT_DIR}/.env.template" "${ENV_FILE}" "${BRANCH}" "${BRANCH_SLUG}" "${PROJECT_ID}" "${DEVOPS_JSON}" "${IS_MAIN}" <<'PY'
import sys
import os
import secrets
import re
import json

template_path, env_path, branch, branch_slug, project_id, devops_json_path, is_main_str = sys.argv[1:8]
is_main = is_main_str == "true"

env_vars = {}
with open(template_path, "r", encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if "=" in line and not line.startswith("#"):
            k, v = line.split("=", 1)
            env_vars[k.strip()] = v.strip()

# Load existing env file if present — preserve secrets to avoid
# password mismatches with persistent data volumes (Postgres, etc.)
existing_vars = {}
if os.path.exists(env_path):
    with open(env_path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                existing_vars[k.strip()] = v.strip()

# Keys that MUST be preserved across deploys because they're baked into
# persistent state (Postgres data volume, JWT signing, cookie encryption).
# Projects can declare this exact contract in devops.json; legacy projects keep
# the secure compatibility default.
default_sealed_keys = [
    "POSTGRES_PASSWORD",
    "REDIS_PASSWORD",
    "JWT_SECRET",
    "COOKIE_SECRET",
    "AUTH_MFA_ENCRYPTION_KEY",
    "OPENSEARCH_INITIAL_ADMIN_PASSWORD",
    "OPENSEARCH_KEYSTORE_PASSWORD",
]

# Merge existing values into template — existing secrets take priority,
# unless they match placeholder/change-me patterns.
for k, v in existing_vars.items():
    if v and v not in ("change_me", "placeholder", "") and not re.search(r'(change[-_]me|placeholder)', v, re.IGNORECASE):
        env_vars[k] = v

# Load devops.json if present
devops = {}
try:
    with open(devops_json_path, "r", encoding="utf-8") as f:
        devops = json.load(f)
except (OSError, json.JSONDecodeError):
    pass

environment_contract = devops.get("environment", {})
configured_sealed_keys = environment_contract.get("generated_secrets") if isinstance(environment_contract, dict) else None
if configured_sealed_keys is None:
    sealed_keys = default_sealed_keys
elif not isinstance(configured_sealed_keys, list) or any(
    not isinstance(key, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)
    for key in configured_sealed_keys
):
    raise ValueError("environment.generated_secrets must be an array of dotenv key names")
else:
    sealed_keys = configured_sealed_keys

# Generate secure random secrets ONLY for placeholders that have no existing value.
for key in sealed_keys:
    val = env_vars.get(key, "")
    if not val or val in ("change_me", "placeholder", "") or re.search(r'(change[-_]me|placeholder)', val, re.IGNORECASE):
        if key == "OPENSEARCH_INITIAL_ADMIN_PASSWORD":
            env_vars[key] = "Aa1!" + secrets.token_hex(30)
        else:
            env_vars[key] = secrets.token_hex(32)

# Compose project name is always project_id-branch_slug
env_vars["COMPOSE_PROJECT_NAME"] = f"{project_id}-{branch_slug}"
env_vars["ENV_NAME"] = branch_slug

# Port assignment from devops.json or from template defaults
port_set = "main" if is_main else "default"
ports = devops.get("ports", {}).get(port_set, {})
for svc, port in ports.items():
    svc_upper = svc.upper() + "_PORT"
    if svc_upper not in env_vars or not env_vars[svc_upper]:
        env_vars[svc_upper] = str(port)

# Replace only exact values that came from an older tracked template. This
# migrates known defaults without overwriting real operator customizations.
env_defaults = devops.get("env_defaults", {}).get(port_set, {})
legacy_defaults = devops.get("env_legacy_defaults", {}).get(port_set, {})
for k, legacy_values in legacy_defaults.items():
    if not isinstance(legacy_values, list):
        raise ValueError(f"env_legacy_defaults.{port_set}.{k} must be an array")
    existing = env_vars.get(k, "").strip()
    normalized_legacy_values = {str(value).strip() for value in legacy_values}
    if existing in normalized_legacy_values:
        if k not in env_defaults:
            raise ValueError(f"legacy default {k} has no replacement in env_defaults.{port_set}")
        env_vars[k] = str(env_defaults[k])

# Env defaults from devops.json, only if no existing value.
for k, default_val in env_defaults.items():
    existing = env_vars.get(k, "").strip()
    if not existing or existing in ("change_me", "placeholder", "") or re.search(r'(change[-_]me|placeholder)', existing, re.IGNORECASE):
        env_vars[k] = str(default_val)

# Write env file
with open(env_path, "w", encoding="utf-8") as f:
    f.write("# Generated deployment env for project " + project_id + " branch " + branch + "\n")
    # Only declared template keys may be overridden by the controller.
    for k in sorted(env_vars):
        if k in os.environ and os.environ[k]:
            value = os.environ[k]
            if "\n" in value or "\r" in value or "\x00" in value:
                raise ValueError(f"invalid multiline value for {k}")
            env_vars[k] = value
    for k, v in sorted(env_vars.items()):
        f.write(f"{k}={v}\n")
PY
    chmod 600 "${ENV_FILE}" || true
    # Remove explicit DATABASE_URL — compose generates it from POSTGRES_PASSWORD
    sed -i '/^DATABASE_URL=/d' "${ENV_FILE}" 2>/dev/null || true
  else
    echo "Error: env.template not found; cannot generate ${ENV_FILE}" >&2
    write_state "blocked" "missing env.template"
    exit 1
  fi

if [ ! -f "${COMPOSE_FILE}" ]; then
  echo "Error: compose file not found: ${COMPOSE_FILE}" >&2
  write_state "blocked" "missing compose file ${COMPOSE_FILE}"
  exit 1
fi

# Render and validate the effective Compose model before granting it access to
# the host Docker daemon. The rendered file contains secrets and is ephemeral.
compose_config_json=$(mktemp "${STATE_DIR}/compose-config.XXXXXX.json")
if ! "${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" config --format json > "${compose_config_json}"; then
  echo "Error: Compose config is invalid or this Compose version cannot emit JSON." >&2
  exit 1
fi
python3 - "${compose_config_json}" "${PROJECT_DIR}" "${LOG_DIR}" "${COMPOSE_PROJECT_NAME}" <<'PY'
import json
import os
import sys

config_path, project_dir, log_dir, compose_project = sys.argv[1:5]
with open(config_path, encoding="utf-8") as handle:
    config = json.load(handle)

allowed_roots = [os.path.realpath(project_dir), os.path.realpath(log_dir)]

def under_allowed_root(path):
    resolved = os.path.realpath(path)
    return any(resolved == root or resolved.startswith(root + os.sep) for root in allowed_roots)

errors = []
declared_volumes = config.get("volumes") or {}
for kind, resources in (("volume", declared_volumes), ("network", config.get("networks") or {})):
    for logical_name, resource in resources.items():
        resource = resource or {}
        if resource.get("external"):
            errors.append(f"{kind} {logical_name} is external")
        resource_name = resource.get("name", "")
        if not resource_name.startswith(compose_project + "_"):
            errors.append(
                f"{kind} {logical_name} resolves to {resource_name!r} outside Compose project {compose_project}"
            )

services = config.get("services", {})
for name, service in services.items():
    if service.get("privileged"):
        errors.append(f"{name}: privileged mode is forbidden")
    for namespace in ("network_mode", "pid", "ipc"):
        value = service.get(namespace) or ""
        if value == "host":
            errors.append(f"{name}: host {namespace} is forbidden")
        elif value.startswith("container:"):
            errors.append(f"{name}: {namespace} from another container is forbidden")
    for reference in service.get("volumes_from") or []:
        if reference.startswith("container:"):
            errors.append(f"{name}: volumes_from from another container is forbidden")
        elif reference.split(":", 1)[0] not in services:
            errors.append(f"{name}: volumes_from references an undeclared service: {reference}")
    if service.get("cap_add"):
        errors.append(f"{name}: cap_add is forbidden")
    if service.get("devices"):
        errors.append(f"{name}: device access is forbidden")
    for mount in service.get("volumes") or []:
        if not isinstance(mount, dict):
            continue
        source = mount.get("source", "")
        if mount.get("type") == "bind":
            if not source or not under_allowed_root(source):
                errors.append(f"{name}: bind mount outside project roots is forbidden: {source}")
        elif mount.get("type") == "volume" and source and source not in declared_volumes:
            errors.append(f"{name}: undeclared named volume is forbidden: {source}")
    for port in service.get("ports") or []:
        if isinstance(port, dict):
            host_ip = port.get("host_ip") or "0.0.0.0"
            if host_ip not in ("127.0.0.1", "::1"):
                errors.append(f"{name}: published port must bind to loopback, not {host_ip}")

if errors:
    for error in errors:
        print(f"Compose policy violation: {error}", file=sys.stderr)
    raise SystemExit(1)
PY
rm -f "${compose_config_json}"
compose_config_json=""

audit "deploy_started" "started" "branch=${BRANCH}, image_tag=${IMAGE_TAG}"
write_state "running" "deployment started"

echo "========================================="
echo "  Project deployment"
echo "  Project: ${PROJECT_ID}"
echo "  Branch: ${BRANCH}"
echo "  Image tag: ${IMAGE_TAG}"
echo "  Compose project: ${COMPOSE_PROJECT_NAME}"
echo "  Compose file: ${COMPOSE_FILE}"
echo "  Env file: ${ENV_FILE}"
echo "========================================="

if grep -q "^IMAGE_TAG=" "${ENV_FILE}"; then
  ESCAPED_IMAGE_TAG=$(printf '%s\n' "${IMAGE_TAG}" | sed 's:[\/&]:\\&:g;$!s/$/\\/')
  sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=${ESCAPED_IMAGE_TAG}/" "${ENV_FILE}"
else
  printf '\nIMAGE_TAG=%s\n' "${IMAGE_TAG}" >> "${ENV_FILE}"
fi

if ! timeout 300 "${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" pull; then
  echo "Error: image pull failed or timed out; refusing to deploy stale cached images." >&2
  exit 1
fi

# Resolve port conflicts: stop containers from other branches using the same ports
while IFS='=' read -r key value; do
  case "$key" in
    *_PORT)
      port="$value"
      conflicting=$(docker ps --filter "publish=$port" --format '{{.Names}}' 2>/dev/null || true)
      if [ -n "$conflicting" ]; then
        while read -r name; do
          case "$name" in
            "${PROJECT_ID}-${BRANCH_SLUG}"*|"${COMPOSE_PROJECT_NAME}"*) ;;
            *)
              echo "Error: port $port is owned by unrelated container $name; refusing to modify it." >&2
              exit 1
              ;;
          esac
        done <<< "$conflicting"
      fi
      ;;
  esac
done < <(grep '^[^#]*_PORT=' "$ENV_FILE" 2>/dev/null || true)

"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up -d --force-recreate --remove-orphans

# Create admin user if MEDUSA_ADMIN_EMAIL is set
MEDUSA_ADMIN_EMAIL="$(dotenv_value "${ENV_FILE}" MEDUSA_ADMIN_EMAIL)"
MEDUSA_ADMIN_PASSWORD="$(dotenv_value "${ENV_FILE}" MEDUSA_ADMIN_PASSWORD)"
if [ -n "${MEDUSA_ADMIN_EMAIL:-}" ] && [ -n "${MEDUSA_ADMIN_PASSWORD:-}" ]; then
  BACKEND_CONTAINER="${COMPOSE_PROJECT_NAME}-backend"
  echo "Creating admin user ${MEDUSA_ADMIN_EMAIL}..."
  # Wait for backend to be healthy
  for i in $(seq 1 30); do
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}running{{end}}' "${BACKEND_CONTAINER}" 2>/dev/null || echo "")
    if [ "${health}" = "healthy" ] || [ "${health}" = "running" ]; then
      break
    fi
    sleep 2
  done
  docker exec "${BACKEND_CONTAINER}" npx medusa user \
    -e "${MEDUSA_ADMIN_EMAIL}" -p "${MEDUSA_ADMIN_PASSWORD}" 2>&1 || \
    echo "Warning: admin user creation failed (may already exist)"
fi

DEPLOY_FINISHED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
export PROJECT_ID BRANCH IMAGE_TAG DEPLOY_ID DEPLOY_STARTED_AT DEPLOY_FINISHED_AT COMPOSE_PROJECT_NAME COMPOSE_FILE ENV_FILE GITHUB_RUN_ID GITHUB_RUN_NUMBER GITHUB_ACTOR GITHUB_REPOSITORY GITHUB_WORKFLOW COMMIT_MESSAGE

python3 - "${RELEASE_DIR}/${DEPLOY_ID}.json" <<'PY'
import json
import os
import sys

manifest_path = sys.argv[1]
manifest = {
    "status": "success",
    "project_id": os.environ["PROJECT_ID"],
    "deploy_id": os.environ["DEPLOY_ID"],
    "commit_sha": os.environ.get("DEPLOY_SHA", ""),
    "branch": os.environ["BRANCH"],
    "deploy_ref": os.environ.get("DEPLOY_REF", ""),
    "image_tag": os.environ["IMAGE_TAG"],
    "env_file": os.environ["ENV_FILE"],
    "compose_file": os.environ["COMPOSE_FILE"],
    "compose_project": os.environ["COMPOSE_PROJECT_NAME"],
    "deploy_started_at": os.environ["DEPLOY_STARTED_AT"],
    "deploy_finished_at": os.environ["DEPLOY_FINISHED_AT"],
    "script_version": "2026-06-29-generic",
    "github_run_id": os.environ.get("GITHUB_RUN_ID", ""),
    "github_run_number": os.environ.get("GITHUB_RUN_NUMBER", ""),
    "github_actor": os.environ.get("GITHUB_ACTOR", ""),
    "github_repository": os.environ.get("GITHUB_REPOSITORY", ""),
    "github_workflow": os.environ.get("GITHUB_WORKFLOW", ""),
    "commit_message": os.environ.get("COMMIT_MESSAGE", ""),
}
os.makedirs(os.path.dirname(manifest_path), exist_ok=True)
tmp_path = manifest_path + ".tmp"
with open(tmp_path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.replace(tmp_path, manifest_path)
PY

audit "deploy_finished" "success" "image_tag=${IMAGE_TAG}"
write_state "success" "deployment completed"

echo "Deployment completed successfully for ${PROJECT_ID}:${BRANCH} (${IMAGE_TAG})."
