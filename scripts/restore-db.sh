#!/usr/bin/env bash
set -euo pipefail
umask 077

if [ "$#" -ne 1 ]; then
  echo "Usage: $0 <backup-id>" >&2
  exit 2
fi

BACKUP_ID="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=../deploy/lib.sh
source "${SERVER_DIR}/deploy/lib.sh"
TRUSTED_PROJECT_DIR="${PROJECT_DIR:-$PWD}"
TRUSTED_STATE_DIR="${PROJECT_STATE_DIR:-${BASE_DIR:-${SERVER_DIR}}/State/${PROJECT_ID:-default}}"
TRUSTED_BACKUP_DIR="${BACKUP_DIR_PATH:-${BASE_DIR:-${SERVER_DIR}}/Backups/${PROJECT_ID:-default}}"
TRUSTED_LOG_DIR="${PROJECT_LOG_DIR:-${BASE_DIR:-${SERVER_DIR}}/Logs/${PROJECT_ID:-default}}"
TRUSTED_LOCK_DIR="${TRUSTED_STATE_DIR}/deploy-lock"
TRUSTED_MANIFEST_FILE="${BACKUP_MANIFEST_FILE:-${TRUSTED_BACKUP_DIR}/manifest.jsonl}"
TRUSTED_AUDIT_LOG="${TRUSTED_LOG_DIR}/deploy-control-audit.log"
PROJECT_DIR="${TRUSTED_PROJECT_DIR}"
STATE_DIR="${TRUSTED_STATE_DIR}"
BACKUP_DIR="${TRUSTED_BACKUP_DIR}"
LOG_DIR="${TRUSTED_LOG_DIR}"
LOCK_DIR="${TRUSTED_LOCK_DIR}"
MANIFEST_FILE="${TRUSTED_MANIFEST_FILE}"
AUDIT_LOG="${TRUSTED_AUDIT_LOG}"

mkdir -p "${STATE_DIR}" "${BACKUP_DIR}" "${LOG_DIR}"

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
COMPOSE_CMD=(docker compose)

TARGET_BRANCH="${TARGET_BRANCH:-${GITHUB_REF_NAME:-rc}}"
case "${TARGET_BRANCH}" in
  refs/heads/*) TARGET_BRANCH="${TARGET_BRANCH#refs/heads/}" ;;
esac

TRUSTED_PROJECT_ID="${PROJECT_ID:-}"
TRUSTED_TARGET_BRANCH="${TARGET_BRANCH}"
TRUSTED_COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-}"
TRUSTED_COMPOSE_FILE="${COMPOSE_FILE}"
if [ -z "${TRUSTED_PROJECT_ID}" ]; then
  echo "Restore failed: PROJECT_ID must be supplied by the control plane." >&2
  exit 1
fi
case "${TRUSTED_PROJECT_ID}" in
  *[!a-z0-9_.-]*|"") echo "Restore failed: invalid PROJECT_ID." >&2; exit 1 ;;
esac

branch_slug="$(printf '%s' "${TRUSTED_TARGET_BRANCH}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_.-]+/-/g; s/^[.-]+//; s/[.-]+$//')"
if [ -z "${branch_slug}" ]; then
  branch_slug="rc"
elif ! printf '%s' "${branch_slug}" | grep -Eq '^[a-z0-9]'; then
  branch_slug="branch-${branch_slug}"
fi
EXPECTED_COMPOSE_PROJECT_NAME="${TRUSTED_PROJECT_ID}-${branch_slug}"
if [ -n "${TRUSTED_COMPOSE_PROJECT_NAME}" ] && [ "${TRUSTED_COMPOSE_PROJECT_NAME}" != "${EXPECTED_COMPOSE_PROJECT_NAME}" ]; then
  echo "Restore failed: Compose project does not match the registered project and branch." >&2
  exit 1
fi

if [ -n "${ENV_FILE:-}" ]; then
  DEPLOY_ENV_FILE="${ENV_FILE}"
elif [ -f "${SERVER_DIR}/.env.${TARGET_BRANCH}" ]; then
  DEPLOY_ENV_FILE="${SERVER_DIR}/.env.${TARGET_BRANCH}"
else
  DEPLOY_ENV_FILE="${SERVER_DIR}/.env"
fi

if [ ! -f "${DEPLOY_ENV_FILE}" ]; then
  echo "Restore failed: environment file '${DEPLOY_ENV_FILE}' does not exist." >&2
  exit 1
fi

load_dotenv "${DEPLOY_ENV_FILE}"

# The project dotenv is application configuration, not control-plane input.
# Restore controller-owned paths after parsing it so a project cannot redirect
# restore input, logs, manifests, or the cross-operation lock.
PROJECT_DIR="${TRUSTED_PROJECT_DIR}"
STATE_DIR="${TRUSTED_STATE_DIR}"
BACKUP_DIR="${TRUSTED_BACKUP_DIR}"
LOG_DIR="${TRUSTED_LOG_DIR}"
LOCK_DIR="${TRUSTED_LOCK_DIR}"
MANIFEST_FILE="${TRUSTED_MANIFEST_FILE}"
AUDIT_LOG="${TRUSTED_AUDIT_LOG}"
PROJECT_ID="${TRUSTED_PROJECT_ID}"
TARGET_BRANCH="${TRUSTED_TARGET_BRANCH}"
ENV_NAME="${branch_slug}"
COMPOSE_PROJECT_NAME="${EXPECTED_COMPOSE_PROJECT_NAME}"

cd "${PROJECT_DIR}"
COMPOSE_FILE="${TRUSTED_COMPOSE_FILE}"
POSTGRES_DB="${POSTGRES_DB:-${PROJECT_ID:-project}-db}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
# Container names from the project dotenv are untrusted. Service targets are
# resolved from exact Compose ownership labels immediately before use.
unset POSTGRES_CONTAINER BACKEND_CONTAINER STOREFRONT_CONTAINER
BACKUP_FILE="${BACKUP_DIR}/${BACKUP_ID}.dump.gz"
case "${BACKUP_ID}" in
  *[!a-zA-Z0-9_.-]*|"") echo "Restore failed: invalid backup id" >&2; exit 1 ;;
esac

verify_owned_container() {
  local container_id="${1:?container id is required}"
  local service="${2:?service is required}"
  local require_running="${3:-true}"
  local actual expected
  actual="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Running}}' "${container_id}" 2>/dev/null)" || {
    echo "Restore failed: unable to inspect Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2
    return 1
  }
  expected="${COMPOSE_PROJECT_NAME}|${service}"
  case "${actual}" in
    "${expected}|true") return 0 ;;
    "${expected}|false")
      if [ "${require_running}" = "false" ]; then
        return 0
      fi
      echo "Restore failed: Compose service ${COMPOSE_PROJECT_NAME}/${service} is not running." >&2
      ;;
    *) echo "Restore failed: container ownership changed for Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2 ;;
  esac
  return 1
}

owned_compose_container() {
  local service="${1:?service is required}"
  local require_running="${2:-true}"
  local output container_id
  local -a container_ids=()
  output="$(docker ps -aq \
    --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
    --filter "label=com.docker.compose.service=${service}")" || {
    echo "Restore failed: unable to resolve Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2
    return 1
  }
  while IFS= read -r container_id; do
    [ -n "${container_id}" ] && container_ids+=("${container_id}")
  done <<< "${output}"
  if [ "${#container_ids[@]}" -ne 1 ]; then
    echo "Restore failed: expected exactly one container for Compose service ${COMPOSE_PROJECT_NAME}/${service}; found ${#container_ids[@]}." >&2
    return 1
  fi
  container_id="${container_ids[0]}"
  verify_owned_container "${container_id}" "${service}" "${require_running}" || return 1
  printf '%s\n' "${container_id}"
}

# ── Download from Firebase Storage if local file is missing ──
firebase_curl() {
  local access_token="$1" config rc
  shift
  case "${access_token}" in
    *[!A-Za-z0-9._~-]*|"") return 1 ;;
  esac
  config="$(mktemp)" || return 1
  chmod 600 "${config}"
  printf 'header = "Authorization: Bearer %s"\n' "${access_token}" > "${config}"
  if curl --config "${config}" "$@"; then
    rc=0
  else
    rc=$?
  fi
  rm -f "${config}"
  return "${rc}"
}

if [ ! -f "${BACKUP_FILE}" ]; then
  FIREBASE_BUCKET="${FIREBASE_STORAGE_BUCKET:-}"
  FIREBASE_CREDS="${GOOGLE_APPLICATION_CREDENTIALS:-}"
  FIREBASE_CLIENT="${FIREBASE_CLIENT_EMAIL:-}"
  FIREBASE_PK_B64="${FIREBASE_PRIVATE_KEY_B64:-}"
  FIREBASE_TOKEN_URI="https://oauth2.googleapis.com/token"

  if [ -n "${FIREBASE_BUCKET}" ] && { [ -n "${FIREBASE_CREDS}" ] && [ -f "${FIREBASE_CREDS}" ] || [ -n "${FIREBASE_PK_B64}" ]; }; then
    echo "0. Backup file not found locally. Downloading from Firebase Storage..."
    
    ACCESS_TOKEN=$(python3 -c "
import json, time, base64, urllib.request, urllib.parse, subprocess, os, tempfile

creds_path, client_email, pk_b64, token_uri = __import__('sys').argv[1:5]

if creds_path and os.path.isfile(creds_path):
    with open(creds_path) as f:
        key = json.load(f)
    client_email = key['client_email']
    pem_bytes = key['private_key'].encode()
elif pk_b64:
    pem_bytes = base64.b64decode(pk_b64)
else:
    raise RuntimeError('no Firebase credentials available')

now = int(time.time())
header_b64 = base64.urlsafe_b64encode(json.dumps({'alg':'RS256','typ':'JWT'}, separators=(',',':')).encode()).rstrip(b'=').decode()
payload_b64 = base64.urlsafe_b64encode(json.dumps({
    'iss': client_email,
    'scope': 'https://www.googleapis.com/auth/devstorage.read_write',
    'aud': token_uri,
    'exp': now + 3600,
    'iat': now,
}, separators=(',',':')).encode()).rstrip(b'=').decode()
sign_input = (header_b64 + '.' + payload_b64).encode()

key_path = None
try:
    with tempfile.NamedTemporaryFile(delete=False) as tf:
        tf.write(pem_bytes)
        key_path = tf.name
    result = subprocess.run(['openssl', 'dgst', '-sha256', '-sign', key_path], input=sign_input, capture_output=True)
    result.check_returncode()
finally:
    if key_path and os.path.exists(key_path):
        os.unlink(key_path)
signature = base64.urlsafe_b64encode(result.stdout).rstrip(b'=').decode()

jwt_assertion = f'{sign_input.decode()}.{signature}'
data = urllib.parse.urlencode({'grant_type': 'urn:ietf:params:oauth:grant-type:jwt-bearer', 'assertion': jwt_assertion}).encode()
req = urllib.request.Request(token_uri, data=data)
with urllib.request.urlopen(req) as resp:
    token_data = json.loads(resp.read())
print(token_data['access_token'])
" "${FIREBASE_CREDS}" "${FIREBASE_CLIENT}" "${FIREBASE_PK_B64}" "${FIREBASE_TOKEN_URI}" 2>/dev/null || echo "")
    
    if [ -n "${ACCESS_TOKEN:-}" ]; then
      REMOTE_MANIFEST_PATH="backups/${COMPOSE_PROJECT_NAME}/manifest.jsonl"
      ENCODED_MANIFEST_PATH=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${REMOTE_MANIFEST_PATH}" 2>/dev/null)
      MANIFEST_TMP_FILE="${MANIFEST_FILE}.download.$$"
      MANIFEST_HTTP_CODE=$(firebase_curl "${ACCESS_TOKEN}" -s -o "${MANIFEST_TMP_FILE}" -w "%{http_code}" \
        "https://storage.googleapis.com/storage/v1/b/${FIREBASE_BUCKET}/o/${ENCODED_MANIFEST_PATH}?alt=media" 2>&1)
      if [ "${MANIFEST_HTTP_CODE}" != "200" ] || [ ! -s "${MANIFEST_TMP_FILE}" ]; then
        rm -f "${MANIFEST_TMP_FILE}"
        echo "ERROR: Failed to download the backup manifest from Firebase (HTTP ${MANIFEST_HTTP_CODE})." >&2
        exit 1
      fi
      mv -f "${MANIFEST_TMP_FILE}" "${MANIFEST_FILE}"

      # Download DB dump
      REMOTE_DB_PATH="backups/${COMPOSE_PROJECT_NAME}/${BACKUP_ID}.dump.gz"
      ENCODED_PATH=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${REMOTE_DB_PATH}" 2>/dev/null)
      HTTP_CODE=$(firebase_curl "${ACCESS_TOKEN}" -s -o "${BACKUP_FILE}" -w "%{http_code}" \
        "https://storage.googleapis.com/storage/v1/b/${FIREBASE_BUCKET}/o/${ENCODED_PATH}?alt=media" 2>&1)
      
      if [ "${HTTP_CODE}" = "200" ]; then
        echo "Downloaded DB backup: ${BACKUP_ID}.dump.gz"
        
        # Download file backup if it exists
        FILE_BACKUP_ID="files-uploads-${BACKUP_ID}"
        REMOTE_FILE_PATH="backups/${COMPOSE_PROJECT_NAME}/${FILE_BACKUP_ID}.tar.gz"
        LOCAL_FILE_BACKUP="${BACKUP_DIR}/${FILE_BACKUP_ID}.tar.gz"
        ENCODED_FILE_PATH=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${REMOTE_FILE_PATH}" 2>/dev/null)
        HTTP_CODE_FB=$(firebase_curl "${ACCESS_TOKEN}" -s -o "${LOCAL_FILE_BACKUP}" -w "%{http_code}" \
          "https://storage.googleapis.com/storage/v1/b/${FIREBASE_BUCKET}/o/${ENCODED_FILE_PATH}?alt=media" 2>&1)
        
        if [ "${HTTP_CODE_FB}" = "200" ]; then
          echo "Downloaded file backup: ${FILE_BACKUP_ID}.tar.gz"
        fi
      else
        echo "ERROR: Failed to download backup from Firebase (HTTP ${HTTP_CODE})" >&2
        exit 1
      fi
    else
      echo "ERROR: Could not obtain Firebase access token" >&2
      exit 1
    fi
  else
    echo "ERROR: Local backup not found at ${BACKUP_FILE}" >&2
    echo "Firebase Storage credentials not configured (set FIREBASE_CLIENT_EMAIL + FIREBASE_PRIVATE_KEY_B64 in project env)." >&2
    exit 1
  fi
fi

if [ ! -f "${BACKUP_FILE}" ]; then
  echo "Restore failed: backup file '${BACKUP_FILE}' does not exist." >&2
  exit 1
fi

if [ ! -f "${MANIFEST_FILE}" ]; then
  echo "Restore failed: backup manifest '${MANIFEST_FILE}' does not exist." >&2
  exit 1
fi

echo "Verifying backup SHA256 against manifest..."
python3 - "${MANIFEST_FILE}" "${BACKUP_ID}" "${BACKUP_FILE}" <<'PY'
import json, sys, hashlib, os
import re

manifest_path, backup_id, backup_file = sys.argv[1:4]

expected_sha = None
restore_eligible = False
with open(manifest_path, "r", encoding="utf-8") as handle:
    for line in handle:
        if not line.strip():
            continue
        try:
            entry = json.loads(line)
            if entry.get("backup_id") == backup_id:
                expected_sha = entry.get("sha256")
                restore_eligible = entry.get("restore_eligible") is True
        except json.JSONDecodeError:
            continue

if not isinstance(expected_sha, str) or not re.fullmatch(r"[0-9a-f]{64}", expected_sha):
    print(f"ERROR: backup_id '{backup_id}' has no valid SHA256 in the manifest.", file=sys.stderr)
    sys.exit(1)
if not restore_eligible:
    print(f"ERROR: backup_id '{backup_id}' is not restore-eligible.", file=sys.stderr)
    sys.exit(1)

print(f"Expected SHA256: {expected_sha}")
hasher = hashlib.sha256()
with open(backup_file, "rb") as f:
    for chunk in iter(lambda: f.read(65536), b""):
        hasher.update(chunk)
actual_sha = hasher.hexdigest()
print(f"Actual SHA256:   {actual_sha}")

if actual_sha != expected_sha:
    print("ERROR: Backup SHA256 mismatch! Backup file is corrupted or has been tampered with.", file=sys.stderr)
    sys.exit(1)
else:
    print("Backup SHA256 verified successfully.")
PY

# Fail closed before acquiring the restore lock. Application containers may be
# absent and will be created by Compose later; PostgreSQL must already exist.
POSTGRES_CONTAINER="$(owned_compose_container postgres true)"

# Acquire lock
if mkdir "${LOCK_DIR}" 2>/dev/null; then
  {
    echo "operation=restore"
    echo "backup_id=${BACKUP_ID}"
    echo "pid=$$"
    echo "started_at=$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  } > "${LOCK_DIR}/info"
else
  echo "Restore refused: another deploy, rollback, backup, or restore is active." >&2
  if [ -f "${LOCK_DIR}/info" ]; then
    sed -n '1,20p' "${LOCK_DIR}/info" >&2 || true
  fi
  exit 1
fi

cleanup() {
  rm -rf "${LOCK_DIR}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "1. Creating pre-restore safety backup..."
PRERESTORE_ID="prerestore-${BACKUP_ID}-$(date +%s)"
if ! BACKUP_ID="${PRERESTORE_ID}" \
     BACKUP_KIND=prerestore \
     BACKUP_REASON="pre-restore safety backup before restoring ${BACKUP_ID}" \
     BACKUP_SKIP_LOCK=true \
     TARGET_BRANCH="${TARGET_BRANCH}" \
     ENV_FILE="${DEPLOY_ENV_FILE}" \
     "${SCRIPT_DIR}/backup-db.sh"; then
  echo "Restore failed: pre-restore safety backup could not be completed. Aborting restore." >&2
  exit 1
fi

echo "2. Stopping write-capable application containers..."
"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" stop backend storefront

echo "3. Restoring database custom dump..."
POSTGRES_CONTAINER="$(owned_compose_container postgres true)"
set +e
gzip -dc "${BACKUP_FILE}" | docker exec -i "${POSTGRES_CONTAINER}" \
  pg_restore -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" \
  --clean --if-exists --exit-on-error --no-owner --no-privileges
restore_status=("${PIPESTATUS[@]}")
set -e

if [ "${restore_status[0]}" -ne 0 ] || [ "${restore_status[1]}" -ne 0 ]; then
  echo "Restore failed (gzip=${restore_status[0]}, pg_restore=${restore_status[1]}). Recovering the pre-restore backup..." >&2
  PRERESTORE_FILE="${BACKUP_DIR}/${PRERESTORE_ID}.dump.gz"
  POSTGRES_CONTAINER="$(owned_compose_container postgres true)"
  if gzip -dc "${PRERESTORE_FILE}" | docker exec -i "${POSTGRES_CONTAINER}" \
      pg_restore -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" \
      --clean --if-exists --exit-on-error --no-owner --no-privileges; then
    echo "Pre-restore database state recovered; restarting application containers." >&2
    "${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" up -d backend storefront
  else
    echo "CRITICAL: automatic recovery failed; backend and storefront remain stopped." >&2
  fi
  exit 1
fi

echo "4. Starting backend with current configuration..."
"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" up -d backend

echo "5. Running post-restore command inside container..."
if [ -n "${RESTORE_COMMAND_JSON:-}" ]; then
  BACKEND_CONTAINER="$(owned_compose_container backend true)"
  python3 - "${BACKEND_CONTAINER}" "${COMPOSE_PROJECT_NAME}" "${RESTORE_COMMAND_JSON}" <<'PY'
import json
import subprocess
import sys

container, compose_project, raw = sys.argv[1:4]
command = json.loads(raw)
if not isinstance(command, list) or not command or not all(isinstance(arg, str) and arg for arg in command):
    raise SystemExit("invalid restore command")
info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
labels = info.get("Config", {}).get("Labels", {}) or {}
if (labels.get("com.docker.compose.project") != compose_project or
        labels.get("com.docker.compose.service") != "backend" or
        not info.get("State", {}).get("Running")):
    raise SystemExit("backend container ownership changed before restore command")
subprocess.run(["docker", "exec", container, *command], check=True)
PY
else
  echo "No RESTORE_COMMAND configured — skipping."
fi

# ── Restore files from backup archive ──
FILE_BACKUP_ID="files-uploads-${BACKUP_ID}"
LOCAL_FILE_BACKUP="${BACKUP_DIR}/${FILE_BACKUP_ID}.tar.gz"
if [ -f "${LOCAL_FILE_BACKUP}" ] && [ -s "${LOCAL_FILE_BACKUP}" ]; then
  echo "5b. Restoring uploaded files..."
  BACKEND_CONTAINER="$(owned_compose_container backend true)"
  docker cp "${LOCAL_FILE_BACKUP}" "${BACKEND_CONTAINER}:/tmp/file-restore.tar.gz"
  BACKEND_CONTAINER="$(owned_compose_container backend true)"
  docker exec "${BACKEND_CONTAINER}" sh -c \
    'set -eu
     archive=/tmp/file-restore.tar.gz
     listing=$(mktemp)
     verbose_listing=$(mktemp)
     stage=$(mktemp -d)
     trap "rm -f \"$listing\" \"$verbose_listing\" \"$archive\"; rm -rf \"$stage\"" EXIT
     tar tzf "$archive" > "$listing"
     if grep -E "(^/|^\.\./|/\.\./)" "$listing" >/dev/null; then
       echo "ERROR: Path traversal detected in backup archive!" >&2
       exit 1
     fi
     tar tvzf "$archive" > "$verbose_listing"
     if grep -Ev "^[d-]" "$verbose_listing" >/dev/null; then
       echo "ERROR: Backup archive contains links or special files!" >&2
       exit 1
     fi
     tar xzf "$archive" -C "$stage"
     target=/app/apps/backend/uploads
     rm -rf "$target"
     mkdir -p "$target"
     cp -a "$stage"/. "$target"/'
  echo "Files restored to /app/apps/backend/uploads/"
fi

echo "6. Starting storefront with current configuration..."
"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" up -d storefront

echo "7. Verifying health check..."
HEALTH_PORT="9000"
attempt=1
success=0
while [ "${attempt}" -le 12 ]; do
  echo "Checking health, attempt ${attempt}/12..."
  if BACKEND_CONTAINER="$(owned_compose_container backend true)" && docker exec "${BACKEND_CONTAINER}" node -e '
const http = require("http");
const req = http.get({host:"127.0.0.1", port:Number(process.argv[1]), path:"/health", timeout:2000}, res => {
  res.resume();
  res.on("end", () => process.exit(res.statusCode === 200 ? 0 : 1));
});
req.on("timeout", () => req.destroy());
req.on("error", () => process.exit(1));
' "${HEALTH_PORT}"; then
    success=1
    break
  fi
  sleep 5
  attempt=$((attempt+1))
done

if [ "${success}" -ne 1 ]; then
  echo "Restore warning: Backend health check failed or timed out." >&2
  exit 1
else
  echo "Restore completed successfully."
fi

# Audit log entry
printf '{"timestamp":"%s","action":"restore","status":"success","backup_id":"%s","actor":"%s"}\n' \
  "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" "${BACKUP_ID}" "${DEPLOY_ACTOR:-unknown}" >> "${AUDIT_LOG}"
