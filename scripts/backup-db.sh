#!/usr/bin/env bash
set -euo pipefail
umask 077

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
PROJECT_DIR="${TRUSTED_PROJECT_DIR}"
STATE_DIR="${TRUSTED_STATE_DIR}"
BACKUP_DIR="${TRUSTED_BACKUP_DIR}"
LOG_DIR="${TRUSTED_LOG_DIR}"
LOCK_DIR="${TRUSTED_LOCK_DIR}"
MANIFEST_FILE="${TRUSTED_MANIFEST_FILE}"

mkdir -p "${STATE_DIR}" "${BACKUP_DIR}" "${LOG_DIR}"

COMPOSE_CMD=(docker compose)

TARGET_BRANCH="${TARGET_BRANCH:-${GITHUB_REF_NAME:-main}}"
case "${TARGET_BRANCH}" in
  refs/heads/*) TARGET_BRANCH="${TARGET_BRANCH#refs/heads/}" ;;
esac

TRUSTED_PROJECT_ID="${PROJECT_ID:-}"
TRUSTED_TARGET_BRANCH="${TARGET_BRANCH}"
TRUSTED_COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-}"
if [ -z "${TRUSTED_PROJECT_ID}" ]; then
  echo "Backup failed: PROJECT_ID must be supplied by the control plane." >&2
  exit 1
fi
case "${TRUSTED_PROJECT_ID}" in
  *[!a-z0-9_.-]*|"") echo "Backup failed: invalid PROJECT_ID." >&2; exit 1 ;;
esac

branch_slug="$(printf '%s' "${TRUSTED_TARGET_BRANCH}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_.-]+/-/g; s/^[.-]+//; s/[.-]+$//')"
if [ -z "${branch_slug}" ]; then
  branch_slug="rc"
elif ! printf '%s' "${branch_slug}" | grep -Eq '^[a-z0-9]'; then
  branch_slug="branch-${branch_slug}"
fi
EXPECTED_COMPOSE_PROJECT_NAME="${TRUSTED_PROJECT_ID}-${branch_slug}"
if [ -n "${TRUSTED_COMPOSE_PROJECT_NAME}" ] && [ "${TRUSTED_COMPOSE_PROJECT_NAME}" != "${EXPECTED_COMPOSE_PROJECT_NAME}" ]; then
  echo "Backup failed: Compose project does not match the registered project and branch." >&2
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
  echo "Backup failed: environment file '${DEPLOY_ENV_FILE}' does not exist." >&2
  exit 1
fi

load_dotenv "${DEPLOY_ENV_FILE}"

# The project dotenv is application configuration, not control-plane input.
# Restore controller-owned paths after parsing it so a project cannot redirect
# backup output, logs, manifests, or the cross-operation lock.
PROJECT_DIR="${TRUSTED_PROJECT_DIR}"
STATE_DIR="${TRUSTED_STATE_DIR}"
BACKUP_DIR="${TRUSTED_BACKUP_DIR}"
LOG_DIR="${TRUSTED_LOG_DIR}"
LOCK_DIR="${TRUSTED_LOCK_DIR}"
MANIFEST_FILE="${TRUSTED_MANIFEST_FILE}"
PROJECT_ID="${TRUSTED_PROJECT_ID}"
TARGET_BRANCH="${TRUSTED_TARGET_BRANCH}"
ENV_NAME="${branch_slug}"
COMPOSE_PROJECT_NAME="${EXPECTED_COMPOSE_PROJECT_NAME}"
POSTGRES_DB="${POSTGRES_DB:-${PROJECT_ID:-project}-db}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
# Container names from the project dotenv are untrusted. Service targets are
# resolved from exact Compose ownership labels immediately before use.
unset POSTGRES_CONTAINER
BACKUP_KIND="${BACKUP_KIND:-manual}"
BACKUP_REASON="${BACKUP_REASON:-${BACKUP_KIND}}"
BACKUP_SKIP_LOCK="${BACKUP_SKIP_LOCK:-false}"
BACKUP_ID="${BACKUP_ID:-$(date -u +"%Y%m%dT%H%M%SZ")-${ENV_NAME}-${BACKUP_KIND}}"
case "${BACKUP_ID}" in
  *[!a-zA-Z0-9_.-]*|"")
    echo "Error: invalid backup id '${BACKUP_ID}'." >&2
    exit 2
    ;;
esac
BACKUP_FILE="${BACKUP_DIR}/${BACKUP_ID}.dump.gz"
TMP_FILE="${BACKUP_FILE}.tmp"
VERIFY_LOG="${BACKUP_DIR}/${BACKUP_ID}.pg_restore.list"

verify_owned_container() {
  local container_id="${1:?container id is required}"
  local service="${2:?service is required}"
  local require_running="${3:-true}"
  local actual expected
  actual="$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Running}}' "${container_id}" 2>/dev/null)" || {
    echo "Backup failed: unable to inspect Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2
    return 1
  }
  expected="${COMPOSE_PROJECT_NAME}|${service}"
  case "${actual}" in
    "${expected}|true") return 0 ;;
    "${expected}|false")
      if [ "${require_running}" = "false" ]; then
        return 0
      fi
      echo "Backup failed: Compose service ${COMPOSE_PROJECT_NAME}/${service} is not running." >&2
      ;;
    *) echo "Backup failed: container ownership changed for Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2 ;;
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
    echo "Backup failed: unable to resolve Compose service ${COMPOSE_PROJECT_NAME}/${service}." >&2
    return 1
  }
  while IFS= read -r container_id; do
    [ -n "${container_id}" ] && container_ids+=("${container_id}")
  done <<< "${output}"
  if [ "${#container_ids[@]}" -ne 1 ]; then
    echo "Backup failed: expected exactly one container for Compose service ${COMPOSE_PROJECT_NAME}/${service}; found ${#container_ids[@]}." >&2
    return 1
  fi
  container_id="${container_ids[0]}"
  verify_owned_container "${container_id}" "${service}" "${require_running}" || return 1
  printf '%s\n' "${container_id}"
}

acquired_lock=0
cleanup() {
  local rc=$?
  rm -f "${TMP_FILE}"
  if [ "${acquired_lock}" -eq 1 ]; then
    rm -rf "${LOCK_DIR}"
  fi
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "${BACKUP_SKIP_LOCK}" != "true" ]; then
  if mkdir "${LOCK_DIR}" 2>/dev/null; then
    acquired_lock=1
    {
      echo "operation=backup"
      echo "backup_id=${BACKUP_ID}"
      echo "pid=$$"
      echo "started_at=$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    } > "${LOCK_DIR}/info"
  else
    echo "Backup refused: another deploy, rollback, backup, or restore appears to be active." >&2
    if [ -f "${LOCK_DIR}/info" ]; then
      sed -n '1,20p' "${LOCK_DIR}/info" >&2 || true
    fi
    exit 1
  fi
fi

cd "${PROJECT_DIR}"
COMMIT_SHA="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
STARTED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

if ! POSTGRES_CONTAINER="$(owned_compose_container postgres true)"; then
  cat >&2 <<EOF
Backup failed: PostgreSQL service for Compose project '${COMPOSE_PROJECT_NAME}' is unavailable.
For production predeploy backup this is fail-closed by design. If this is an intentional
first production deploy with no existing database, set ALLOW_INITIAL_DEPLOY_WITHOUT_DB_BACKUP=true
on the deploy invocation after confirming there is no data to preserve.
EOF
  exit 1
fi

echo "Creating ${BACKUP_KIND} backup '${BACKUP_ID}' for ${ENV_NAME}/${POSTGRES_DB}..."
POSTGRES_CONTAINER="$(owned_compose_container postgres true)"
set +e
docker exec "${POSTGRES_CONTAINER}" pg_dump --format=custom --no-owner --no-privileges \
  -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" | gzip -c > "${TMP_FILE}"
pipe_status=("${PIPESTATUS[@]}")
dump_rc=${pipe_status[0]}
gzip_rc=${pipe_status[1]}
set -e

if [ "${dump_rc}" -ne 0 ] || [ "${gzip_rc}" -ne 0 ]; then
  echo "Backup failed: pg_dump exit=${dump_rc}, gzip exit=${gzip_rc}." >&2
  exit 1
fi

if [ ! -s "${TMP_FILE}" ]; then
  echo "Backup failed: output archive is empty." >&2
  exit 1
fi

mv "${TMP_FILE}" "${BACKUP_FILE}"
SIZE_BYTES="$(wc -c < "${BACKUP_FILE}" | tr -d ' ')"
SHA256="$(sha256sum "${BACKUP_FILE}" | awk '{print $1}')"

verification_status="unavailable"
verification_detail="pg_restore not available"
if command -v pg_restore >/dev/null 2>&1; then
  if gzip -dc "${BACKUP_FILE}" | pg_restore --list > "${VERIFY_LOG}" 2>&1; then
    verification_status="passed"
    verification_detail="host pg_restore --list passed"
  else
    verification_status="failed"
    verification_detail="host pg_restore --list failed"
  fi
else
  POSTGRES_CONTAINER="$(owned_compose_container postgres true)"
  if gzip -dc "${BACKUP_FILE}" | docker exec -i "${POSTGRES_CONTAINER}" pg_restore --list > "${VERIFY_LOG}" 2>&1; then
    verification_status="passed"
    verification_detail="container pg_restore --list passed"
  else
    verification_status="failed"
    verification_detail="container pg_restore --list failed"
  fi
fi

if [ "${verification_status}" != "passed" ]; then
  echo "Backup failed verification: ${verification_detail}." >&2
  exit 1
fi

FINISHED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
export BACKUP_ID BACKUP_KIND BACKUP_REASON BACKUP_FILE ENV_NAME COMPOSE_PROJECT_NAME
export COMMIT_SHA STARTED_AT FINISHED_AT POSTGRES_DB POSTGRES_USER
export SIZE_BYTES SHA256 DUMP_RC="${dump_rc}" VERIFICATION_STATUS="${verification_status}"
export VERIFICATION_DETAIL="${verification_detail}"

python3 - "$MANIFEST_FILE" <<'PY'
import json
import os
import sys

manifest_path = sys.argv[1]
server_dir = os.path.abspath(os.path.join(os.path.dirname(manifest_path), ".."))
backup_file = os.environ["BACKUP_FILE"]
entry = {
    "backup_id": os.environ["BACKUP_ID"],
    "kind": os.environ["BACKUP_KIND"],
    "reason": os.environ["BACKUP_REASON"],
    "env_name": os.environ["ENV_NAME"],
    "compose_project": os.environ["COMPOSE_PROJECT_NAME"],
    "commit_sha": os.environ["COMMIT_SHA"],
    "timestamp": os.environ["STARTED_AT"],
    "finished_at": os.environ["FINISHED_AT"],
    "database": os.environ["POSTGRES_DB"],
    "database_user": os.environ["POSTGRES_USER"],
    "file_path": os.path.relpath(backup_file, server_dir),
    "size_bytes": int(os.environ["SIZE_BYTES"]),
    "sha256": os.environ["SHA256"],
    "pg_dump_exit_code": int(os.environ["DUMP_RC"]),
    "verification_status": os.environ["VERIFICATION_STATUS"],
    "verification_detail": os.environ["VERIFICATION_DETAIL"],
    "restore_eligible": os.environ["VERIFICATION_STATUS"] == "passed",
}
os.makedirs(os.path.dirname(manifest_path), exist_ok=True)
with open(manifest_path, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(entry, sort_keys=True) + "\n")
print(json.dumps(entry, sort_keys=True))
PY

# ── Upload to Firebase Storage when configured ──
FIREBASE_BUCKET="${FIREBASE_STORAGE_BUCKET:-}"
FIREBASE_CREDS="${GOOGLE_APPLICATION_CREDENTIALS:-}"
FIREBASE_CLIENT="${FIREBASE_CLIENT_EMAIL:-}"
FIREBASE_PK_B64="${FIREBASE_PRIVATE_KEY_B64:-}"
FIREBASE_TOKEN_URI="https://oauth2.googleapis.com/token"

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

if [ -n "${FIREBASE_BUCKET}" ]; then
  echo "Uploading backup to Firebase Storage..."
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
    REMOTE_PATH="backups/${COMPOSE_PROJECT_NAME}/${BACKUP_ID}.dump.gz"
    ENCODED_PATH=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${REMOTE_PATH}" 2>/dev/null)
    HTTP_CODE=$(firebase_curl "${ACCESS_TOKEN}" -s -o /dev/null -w "%{http_code}" \
      -X POST \
      -H "Content-Type: application/octet-stream" \
      --data-binary "@${BACKUP_FILE}" \
      "https://storage.googleapis.com/upload/storage/v1/b/${FIREBASE_BUCKET}/o?uploadType=media&name=${ENCODED_PATH}" 2>&1)

    if [ "${HTTP_CODE}" = "200" ]; then
      REMOTE_MANIFEST_PATH="backups/${COMPOSE_PROJECT_NAME}/manifest.jsonl"
      ENCODED_MANIFEST_PATH=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${REMOTE_MANIFEST_PATH}" 2>/dev/null)
      MANIFEST_HTTP_CODE=$(firebase_curl "${ACCESS_TOKEN}" -s -o /dev/null -w "%{http_code}" \
        -X POST \
        -H "Content-Type: application/json" \
        --data-binary "@${MANIFEST_FILE}" \
        "https://storage.googleapis.com/upload/storage/v1/b/${FIREBASE_BUCKET}/o?uploadType=media&name=${ENCODED_MANIFEST_PATH}" 2>&1)
      if [ "${MANIFEST_HTTP_CODE}" = "200" ]; then
        echo "Firebase upload complete."
      else
        echo "WARNING: Firebase manifest upload failed (HTTP ${MANIFEST_HTTP_CODE}). Local backup remains verified." >&2
      fi
    else
      echo "WARNING: Firebase upload failed (HTTP ${HTTP_CODE}). Backup is available locally." >&2
    fi
  else
    echo "WARNING: Could not obtain Firebase access token. Backup saved locally only." >&2
  fi
fi

echo "Backup complete: ${BACKUP_ID}"
