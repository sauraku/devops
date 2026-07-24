#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../deploy/lib.sh
source "${repo_root}/deploy/lib.sh"

test_dir="$(mktemp -d)"
trap 'rm -rf -- "${test_dir}"' EXIT

readonly READONLY_FIXTURE="controller-owned"
dotenv_file="${test_dir}/readonly.env"
printf '%s\n' \
  'READONLY_FIXTURE=project-controlled' \
  'DOCKER_HOST=tcp://project.example:2375' \
  'COMPOSE_FILE=/project/compose.yml' \
  'COMPOSE_PROFILES=debug' \
  'BUILDKIT_HOST=tcp://project.example:1234' \
  'BUILDX_CONFIG=/project/buildx' \
  'SAFE_AFTER_READONLY=loaded' > "${dotenv_file}"

export DOCKER_HOST="tcp://controller.example:2376"
export COMPOSE_FILE="/controller/compose.yml"
export COMPOSE_PROFILES="controller"
export BUILDKIT_HOST="tcp://controller.example:1234"
export BUILDX_CONFIG="/controller/buildx"
load_dotenv "${dotenv_file}"
[[ "${READONLY_FIXTURE}" == "controller-owned" ]]
[[ "${SAFE_AFTER_READONLY}" == "loaded" ]]
[[ "${DOCKER_HOST}" == "tcp://controller.example:2376" ]]
[[ "${COMPOSE_FILE}" == "/controller/compose.yml" ]]
[[ "${COMPOSE_PROFILES}" == "controller" ]]
[[ "${BUILDKIT_HOST}" == "tcp://controller.example:1234" ]]
[[ "${BUILDX_CONFIG}" == "/controller/buildx" ]]

invalid_file="${test_dir}/invalid.env"
printf '%s\n' 'INVALID-KEY=value' > "${invalid_file}"
if load_dotenv "${invalid_file}" 2>/dev/null; then
  echo "load_dotenv accepted an invalid environment key" >&2
  exit 1
fi

printf '%s\n' "lib dotenv tests passed"
