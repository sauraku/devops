#!/usr/bin/env bash
set -euo pipefail
umask 077

if [ "$#" -ne 1 ]; then
  echo "Usage: $0 <commit-sha>" >&2
  exit 2
fi

TARGET_COMMIT="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_ID="${PROJECT_ID:-}"

case "${TARGET_COMMIT}" in
  *[!0-9a-fA-F]*|"")
    echo "Rollback refused: commit must be a hexadecimal SHA." >&2
    exit 2
    ;;
esac

release_json="$(python3 - "${RELEASE_DIR:-${BASE_DIR}/Releases/${PROJECT_ID}}" "${TARGET_COMMIT}" <<'PY'
import json
import os
import sys

release_dir, target = sys.argv[1], sys.argv[2].lower()
matches = []
if os.path.isdir(release_dir):
    for name in os.listdir(release_dir):
        if not name.endswith(".json"):
            continue
        path = os.path.join(release_dir, name)
        try:
            with open(path, "r", encoding="utf-8") as handle:
                data = json.load(handle)
        except (OSError, json.JSONDecodeError):
            continue
        sha = str(data.get("commit_sha", "")).lower()
        if data.get("status") == "success" and sha.startswith(target):
            matches.append(data)
if len(matches) != 1:
    print("")
    sys.exit(1)
print(json.dumps(matches[0], sort_keys=True))
PY
)" || {
  echo "Rollback refused: expected exactly one successful release matching '${TARGET_COMMIT}'." >&2
  exit 1
}

resolved_sha="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["commit_sha"])' "${release_json}")"
resolved_branch="$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d.get("branch") or d.get("deploy_ref","main"))' "${release_json}")"
resolved_image_tag="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("image_tag",""))' "${release_json}")"
rollback_project_id="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("project_id",""))' "${release_json}")"

if [ -z "${PROJECT_ID}" ] || [ "${rollback_project_id}" != "${PROJECT_ID}" ]; then
  echo "Rollback refused: release does not belong to project '${PROJECT_ID}'." >&2
  exit 1
fi

AUDIT_LOG="${BASE_DIR:-${SERVER_DIR}}/Logs/${rollback_project_id}/deploy-control-audit.log"
mkdir -p "$(dirname "${AUDIT_LOG}")"
python3 - "${AUDIT_LOG}" "${resolved_sha}" "${resolved_branch}" "${rollback_project_id}" "${DEPLOY_ACTOR:-unknown}" <<'PY'
import datetime
import json
import sys

path, commit, ref, project, actor = sys.argv[1:6]
entry = {
    "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "action": "rollback_requested",
    "commit_sha": commit,
    "ref": ref,
    "project_id": project,
    "actor": actor,
}
with open(path, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(entry, sort_keys=True) + "\n")
PY

DEPLOY_PROCESS_PID="$$" DEPLOY_SHA="${resolved_sha}" DEPLOY_REF="${resolved_branch}" \
  IMAGE_TAG="${resolved_image_tag:-sha-${resolved_sha}}" \
  "${SERVER_DIR}/deploy/project.sh" "${rollback_project_id}" "${resolved_branch}" "${resolved_image_tag:-sha-${resolved_sha}}"
