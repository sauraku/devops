#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

write_stub() {
  local name="$1" body="$2"
  printf '%s\n' "${body}" > "${tmp_dir}/${name}"
  chmod 700 "${tmp_dir}/${name}"
}

write_stub docker '#!/usr/bin/env bash
if [ "${1:-}" = compose ] && [ "${2:-}" = version ] && [ "${3:-}" = --short ]; then
  printf "%s\n" "${DOCKER_COMPOSE_PLUGIN_VERSION:-2.0.0}"
  exit "${DOCKER_COMPOSE_PLUGIN_STATUS:-0}"
fi
exit 1'
write_stub docker-compose '#!/usr/bin/env bash
if [ "${1:-}" = version ] && [ "${2:-}" = --short ]; then
  printf "%s\n" "${DOCKER_COMPOSE_STANDALONE_VERSION:-2.0.0}"
  exit "${DOCKER_COMPOSE_STANDALONE_STATUS:-0}"
fi
exit 1'

# shellcheck source=../deploy/lib.sh
source "${repo_root}/deploy/lib.sh"

PATH="${tmp_dir}:/usr/bin:/bin"

DOCKER_COMPOSE_PLUGIN_STATUS=0
DOCKER_COMPOSE_STANDALONE_STATUS=1
export DOCKER_COMPOSE_PLUGIN_STATUS DOCKER_COMPOSE_STANDALONE_STATUS
select_compose_command
[ "${#COMPOSE_CMD[@]}" -eq 2 ]
[ "${COMPOSE_CMD[0]}" = docker ]
[ "${COMPOSE_CMD[1]}" = compose ]

DOCKER_COMPOSE_PLUGIN_STATUS=1
DOCKER_COMPOSE_STANDALONE_STATUS=0
export DOCKER_COMPOSE_PLUGIN_STATUS DOCKER_COMPOSE_STANDALONE_STATUS
select_compose_command
[ "${#COMPOSE_CMD[@]}" -eq 1 ]
[ "${COMPOSE_CMD[0]}" = docker-compose ]

DOCKER_COMPOSE_PLUGIN_STATUS=1
DOCKER_COMPOSE_STANDALONE_STATUS=0
DOCKER_COMPOSE_STANDALONE_VERSION=1.29.2
export DOCKER_COMPOSE_PLUGIN_STATUS DOCKER_COMPOSE_STANDALONE_STATUS
export DOCKER_COMPOSE_STANDALONE_VERSION
if select_compose_command 2>/dev/null; then
  echo "compose command detection accepted standalone Compose v1" >&2
  exit 1
fi

DOCKER_COMPOSE_PLUGIN_STATUS=1
DOCKER_COMPOSE_STANDALONE_STATUS=1
DOCKER_COMPOSE_STANDALONE_VERSION=2.0.0
export DOCKER_COMPOSE_PLUGIN_STATUS DOCKER_COMPOSE_STANDALONE_STATUS
export DOCKER_COMPOSE_STANDALONE_VERSION
if select_compose_command 2>/dev/null; then
  echo "compose command detection accepted unavailable implementations" >&2
  exit 1
fi

echo "compose command tests passed"
