#!/usr/bin/env bash
set -euo pipefail

# Usage: list-releases.sh <project_id>
# Lists release manifests from ${BASE_DIR}/Releases/${PROJECT_ID}/
BASE_DIR="${BASE_DIR:-/opt/devops-control}"
PROJECT_ID="${1:?project_id is required}"
RELEASE_DIR="${BASE_DIR}/Releases/${PROJECT_ID}"

python3 - "${RELEASE_DIR}" <<'PY'
import json
import os
import sys

release_dir = sys.argv[1]
items = []
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
        if data.get("status") == "success":
            data["_file"] = name
            items.append(data)

items.sort(key=lambda item: item.get("deploy_finished_at") or item.get("deployed_at") or "", reverse=True)
print(json.dumps(items, indent=2, sort_keys=True))
PY
