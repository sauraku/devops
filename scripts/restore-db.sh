#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "Usage: $0 <backup-id>" >&2
  exit 2
fi

BACKUP_ID="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_DIR="$(cd "${SERVER_DIR}/.." && pwd)"
STATE_DIR="${SERVER_DIR}/State"
BACKUP_DIR="${BACKUP_DIR_PATH:-${SERVER_DIR}/Backups}"
LOG_DIR="${SERVER_DIR}/Logs"
LOCK_DIR="${STATE_DIR}/deploy-lock"
MANIFEST_FILE="${BACKUP_MANIFEST_FILE:-${BACKUP_DIR}/manifest.jsonl}"
AUDIT_LOG="${LOG_DIR}/deploy-control-audit.log"

mkdir -p "${STATE_DIR}" "${BACKUP_DIR}" "${LOG_DIR}"

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
COMPOSE_CMD=(docker compose)
if ! docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
fi

TARGET_BRANCH="${TARGET_BRANCH:-${GITHUB_REF_NAME:-rc}}"
case "${TARGET_BRANCH}" in
  refs/heads/*) TARGET_BRANCH="${TARGET_BRANCH#refs/heads/}" ;;
esac

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

set -a
source "${DEPLOY_ENV_FILE}"
set +a

ENV_NAME="${ENV_NAME:-rc}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-${PROJECT_ID:-project}-${ENV_NAME}}"
POSTGRES_DB="${POSTGRES_DB:-${PROJECT_ID:-project}-db}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-${COMPOSE_PROJECT_NAME:-project}-postgres}"
BACKEND_CONTAINER="${BACKEND_CONTAINER:-${COMPOSE_PROJECT_NAME:-project}-backend}"
STOREFRONT_CONTAINER="${STOREFRONT_CONTAINER:-${COMPOSE_PROJECT_NAME:-project}-storefront}"
BACKUP_FILE="${BACKUP_DIR}/${BACKUP_ID}.dump.gz"
case "${BACKUP_ID}" in
  *[!/a-zA-Z0-9_.-]*|"") echo "Restore failed: invalid backup id" >&2; exit 1 ;;
esac

# ── Download from Firebase Storage if local file is missing ──
if [ ! -f "${BACKUP_FILE}" ]; then
  FIREBASE_BUCKET="${FIREBASE_STORAGE_BUCKET:-}"
  FIREBASE_CREDS="${GOOGLE_APPLICATION_CREDENTIALS:-}"
  FIREBASE_CLIENT="${FIREBASE_CLIENT_EMAIL:-}"
  FIREBASE_PK_B64="${FIREBASE_PRIVATE_KEY_B64:-}"
  FIREBASE_TOKEN_URI="${FIREBASE_TOKEN_URI:-https://oauth2.googleapis.com/token}"

  if [ -n "${FIREBASE_BUCKET}" ] && { [ -n "${FIREBASE_CREDS}" ] && [ -f "${FIREBASE_CREDS}" ] || [ -n "${FIREBASE_PK_B64}" ]; }; then
    echo "0. Backup file not found locally. Downloading from Firebase Storage..."
    
    ACCESS_TOKEN=$(python3 -c "
import json, time, jwt, base64, urllib.request, urllib.parse

creds_path = '${FIREBASE_CREDS}'
client_email = '${FIREBASE_CLIENT}'
pk_b64 = '${FIREBASE_PK_B64}'
token_uri = '${FIREBASE_TOKEN_URI}'

if creds_path and __import__('os').path.isfile(creds_path):
    with open(creds_path) as f:
        key = json.load(f)
    client_email = key['client_email']
    token_uri = key.get('token_uri', token_uri)
    private_key = key['private_key']
elif pk_b64:
    private_key = base64.b64decode(pk_b64).decode()
else:
    raise RuntimeError('no Firebase credentials available')

now = int(time.time())
payload = {
    'iss': client_email,
    'scope': 'https://www.googleapis.com/auth/devstorage.read_write',
    'aud': token_uri,
    'exp': now + 3600,
    'iat': now,
}
jwt_token = jwt.encode(payload, private_key, algorithm='RS256')
data = urllib.parse.urlencode({'grant_type': 'urn:ietf:params:oauth:grant-type:jwt-bearer', 'assertion': jwt_token}).encode()
req = urllib.request.Request(token_uri, data=data)
with urllib.request.urlopen(req) as resp:
    token_data = json.loads(resp.read())
print(token_data['access_token'])
" 2>/dev/null || echo "")
    
    if [ -n "${ACCESS_TOKEN:-}" ]; then
      # Download DB dump
      REMOTE_DB_PATH="backups/${COMPOSE_PROJECT_NAME}/${BACKUP_ID}.dump.gz"
      ENCODED_PATH=$(python3 -c "import urllib.parse; print(urllib.parse.quote('${REMOTE_DB_PATH}', safe=''))" 2>/dev/null)
      HTTP_CODE=$(curl -s -o "${BACKUP_FILE}" -w "%{http_code}" \
        -H "Authorization: Bearer ${ACCESS_TOKEN}" \
        "https://storage.googleapis.com/storage/v1/b/${FIREBASE_BUCKET}/o/${ENCODED_PATH}?alt=media" 2>&1)
      
      if [ "${HTTP_CODE}" = "200" ]; then
        echo "Downloaded DB backup: ${BACKUP_ID}.dump.gz"
        
        # Download file backup if it exists
        FILE_BACKUP_ID="files-uploads-${BACKUP_ID}"
        REMOTE_FILE_PATH="backups/${COMPOSE_PROJECT_NAME}/${FILE_BACKUP_ID}.tar.gz"
        LOCAL_FILE_BACKUP="${BACKUP_DIR}/${FILE_BACKUP_ID}.tar.gz"
        ENCODED_FILE_PATH=$(python3 -c "import urllib.parse; print(urllib.parse.quote('${REMOTE_FILE_PATH}', safe=''))" 2>/dev/null)
        HTTP_CODE_FB=$(curl -s -o "${LOCAL_FILE_BACKUP}" -w "%{http_code}" \
          -H "Authorization: Bearer ${ACCESS_TOKEN}" \
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
trap cleanup EXIT INT TERM

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
set +e
gzip -dc "${BACKUP_FILE}" | docker exec -i "${POSTGRES_CONTAINER}" pg_restore -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" --clean --no-owner --no-privileges
restore_rc=$?
set -e

if [ "${restore_rc}" -ne 0 ]; then
  echo "Restore warning: pg_restore exited with code ${restore_rc}. Some objects may have failed to clean or restore." >&2
fi

echo "4. Starting backend with current configuration..."
"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" up -d backend

echo "5. Running post-restore command inside container..."
RESTORE_COMMAND="${RESTORE_COMMAND:-}"
if [ -n "${RESTORE_COMMAND}" ]; then
  docker exec "${BACKEND_CONTAINER}" ${RESTORE_COMMAND}
else
  echo "No RESTORE_COMMAND configured — skipping."
fi

# ── Restore files from backup archive ──
FILE_BACKUP_ID="files-uploads-${BACKUP_ID}"
LOCAL_FILE_BACKUP="${BACKUP_DIR}/${FILE_BACKUP_ID}.tar.gz"
if [ -f "${LOCAL_FILE_BACKUP}" ] && [ -s "${LOCAL_FILE_BACKUP}" ]; then
  echo "5b. Restoring uploaded files..."
  docker exec "${BACKEND_CONTAINER}" sh -c "mkdir -p /app/apps/backend/uploads && cd /app/apps/backend && tar xzf /tmp/file-restore.tar.gz -C uploads/" 2>/dev/null || true
  # Copy to container first, then extract
  docker cp "${LOCAL_FILE_BACKUP}" "${BACKEND_CONTAINER}:/tmp/file-restore.tar.gz" 2>/dev/null
  docker exec "${BACKEND_CONTAINER}" sh -c "mkdir -p /app/apps/backend/uploads && cd /app/apps/backend && tar xzf /tmp/file-restore.tar.gz -C uploads/ && rm /tmp/file-restore.tar.gz" 2>/dev/null
  echo "Files restored to /app/apps/backend/uploads/"
fi

echo "6. Starting storefront with current configuration..."
"${COMPOSE_CMD[@]}" -p "${COMPOSE_PROJECT_NAME}" -f "${COMPOSE_FILE}" --env-file "${DEPLOY_ENV_FILE}" up -d storefront

echo "7. Verifying health check..."
HEALTH_PORT="${BACKEND_PORT:-9000}"
HEALTH_ENDPOINT="http://localhost:${HEALTH_PORT}/health"
attempt=1
success=0
while [ "${attempt}" -le 12 ]; do
  echo "Checking health, attempt ${attempt}/12..."
  HEALTH_HTTP_STATUS="$(curl -s -o /dev/null -w "%{http_code}" "${HEALTH_ENDPOINT}" || true)"
  if [ "${HEALTH_HTTP_STATUS}" = "200" ]; then
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
