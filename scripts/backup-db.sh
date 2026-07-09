#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_DIR="$(cd "${SERVER_DIR}/.." && pwd)"
STATE_DIR="${SERVER_DIR}/State"
BACKUP_DIR="${BACKUP_DIR_PATH:-${SERVER_DIR}/Backups}"
LOG_DIR="${SERVER_DIR}/Logs"
LOCK_DIR="${STATE_DIR}/deploy-lock"
MANIFEST_FILE="${BACKUP_MANIFEST_FILE:-${BACKUP_DIR}/manifest.jsonl}"

mkdir -p "${STATE_DIR}" "${BACKUP_DIR}" "${LOG_DIR}"

COMPOSE_CMD=(docker compose)
if ! docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
fi

TARGET_BRANCH="${TARGET_BRANCH:-${GITHUB_REF_NAME:-main}}"
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
  echo "Backup failed: environment file '${DEPLOY_ENV_FILE}' does not exist." >&2
  exit 1
fi

set -a
# shellcheck source=/dev/null
source "${DEPLOY_ENV_FILE}"
set +a

ENV_NAME="${ENV_NAME:-${TARGET_BRANCH:-rc}}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-${PROJECT_ID:-project}-${ENV_NAME}}"
POSTGRES_DB="${POSTGRES_DB:-${PROJECT_ID:-project}-db}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-${COMPOSE_PROJECT_NAME:-project}-postgres}"
BACKUP_KIND="${BACKUP_KIND:-manual}"
BACKUP_REASON="${BACKUP_REASON:-${BACKUP_KIND}}"
BACKUP_SKIP_LOCK="${BACKUP_SKIP_LOCK:-false}"
BACKUP_ID="${BACKUP_ID:-$(date -u +"%Y%m%dT%H%M%SZ")-${ENV_NAME}-${BACKUP_KIND}}"
BACKUP_FILE="${BACKUP_DIR}/${BACKUP_ID}.dump.gz"
TMP_FILE="${BACKUP_FILE}.tmp"
VERIFY_LOG="${BACKUP_DIR}/${BACKUP_ID}.pg_restore.list"

acquired_lock=0
cleanup() {
  local rc=$?
  rm -f "${TMP_FILE}"
  if [ "${acquired_lock}" -eq 1 ]; then
    rm -rf "${LOCK_DIR}"
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

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

cd "${REPO_DIR}"
git config --global --add safe.directory "*" >/dev/null 2>&1 || true
COMMIT_SHA="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
STARTED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

running="$(docker inspect -f '{{.State.Running}}' "${POSTGRES_CONTAINER}" 2>/dev/null || true)"
if [ "${running}" != "true" ]; then
  cat >&2 <<EOF
Backup failed: PostgreSQL container '${POSTGRES_CONTAINER}' is not running.
For production predeploy backup this is fail-closed by design. If this is an intentional
first production deploy with no existing database, set ALLOW_INITIAL_DEPLOY_WITHOUT_DB_BACKUP=true
on the deploy invocation after confirming there is no data to preserve.
EOF
  exit 1
fi

echo "Creating ${BACKUP_KIND} backup '${BACKUP_ID}' for ${ENV_NAME}/${POSTGRES_DB}..."
set +e
docker exec "${POSTGRES_CONTAINER}" sh -c 'pg_dump --format=custom --no-owner --no-privileges -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB}"' | gzip -c > "${TMP_FILE}"
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

echo "Backup complete: ${BACKUP_ID}"
