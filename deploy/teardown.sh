#!/usr/bin/env bash
# Complete teardown — removes devops-control, all project containers,
# volumes, runner state, and persistent data. Does NOT touch unrelated
# containers (jellyfin, immich, etc.).
#
# Run via:
#   bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/teardown.sh)
set -uo pipefail

CONTAINER="${CONTAINER:-devops-control}"
INSTALL_MARKER_NAME=".devops-control-installation"
INSTALL_MARKER_PRODUCT="product=devops-control"
INSTALL_MARKER_FORMAT="format=1"
if [ -z "${DATA_DIR+x}" ]; then
  if [ -f "/opt/devops-control/$INSTALL_MARKER_NAME" ]; then
    DATA_DIR="/opt/devops-control"
  else
    DATA_DIR="${HOME:-}/.devops-control"
  fi
fi

trim_directory_suffix() {
  local path="$1" previous
  while :; do
    previous="$path"
    while [ "$path" != "/" ] && [ "${path%/}" != "$path" ]; do
      path="${path%/}"
    done
    case "$path" in
      */.) path="${path%/.}" ;;
    esac
    [ "$path" != "$previous" ] || break
  done
  printf '%s\n' "$path"
}

reject_unsafe_data_dir() {
  local path="$1" home_path="" relative
  if [ -n "${HOME:-}" ] && [ -d "$HOME" ]; then
    home_path="$(cd -P -- "$HOME" 2>/dev/null && pwd -P || true)"
  fi
  if [ -n "$home_path" ] && [ "$path" = "$home_path" ]; then
    echo "Refusing to use a home directory as DATA_DIR: $path" >&2
    return 1
  fi
  case "$path" in
    /|/root|/var/root|/home|/Users)
      echo "Refusing unsafe DATA_DIR: $path" >&2
      return 1
      ;;
    /home/*)
      relative="${path#/home/}"
      if [[ "$relative" != */* ]]; then
        echo "Refusing to use a home directory as DATA_DIR: $path" >&2
        return 1
      fi
      ;;
    /Users/*)
      relative="${path#/Users/}"
      if [[ "$relative" != */* ]]; then
        echo "Refusing to use a home directory as DATA_DIR: $path" >&2
        return 1
      fi
      ;;
  esac
  relative="${path#/}"
  if [[ "$relative" != */* ]]; then
    echo "Refusing top-level DATA_DIR: $path" >&2
    return 1
  fi
}

canonicalize_data_dir() {
  local path="$1" parent leaf canonical_parent
  if [ -d "$path" ]; then
    cd -P -- "$path" 2>/dev/null && pwd -P
    return
  fi
  parent="$(dirname -- "$path")"
  leaf="$(basename -- "$path")"
  canonical_parent="$(cd -P -- "$parent" 2>/dev/null && pwd -P)" || return 1
  printf '%s/%s\n' "${canonical_parent%/}" "$leaf"
}

verify_installation_root() {
  local marker="$DATA_DIR/$INSTALL_MARKER_NAME" expected_dir line_count
  if [ -L "$DATA_DIR" ] || [ ! -d "$DATA_DIR" ]; then
    echo "Refusing recursive deletion: DATA_DIR is no longer the verified directory $DATA_DIR" >&2
    return 1
  fi
  if [ "$(cd -P -- "$DATA_DIR" 2>/dev/null && pwd -P || true)" != "$DATA_DIR" ]; then
    echo "Refusing recursive deletion: DATA_DIR canonical path changed" >&2
    return 1
  fi
  if [ -L "$marker" ] || [ ! -f "$marker" ]; then
    echo "Refusing recursive deletion: missing regular installation marker $marker" >&2
    return 1
  fi
  line_count="$(awk 'END {print NR}' "$marker" 2>/dev/null || true)"
  if [ "$line_count" != "3" ] ||
     [ "$(sed -n '1p' "$marker" 2>/dev/null || true)" != "$INSTALL_MARKER_PRODUCT" ] ||
     [ "$(sed -n '2p' "$marker" 2>/dev/null || true)" != "$INSTALL_MARKER_FORMAT" ] ||
     [ "$(sed -n '3p' "$marker" 2>/dev/null || true)" != "data_dir=$DATA_DIR" ]; then
    echo "Refusing recursive deletion: installation marker does not match $DATA_DIR" >&2
    return 1
  fi
  for expected_dir in State Projects Logs Run; do
    if [ ! -d "$DATA_DIR/$expected_dir" ] || [ -L "$DATA_DIR/$expected_dir" ]; then
      echo "Refusing recursive deletion: invalid installation layout at $DATA_DIR/$expected_dir" >&2
      return 1
    fi
  done
}

DATA_DIR_INPUT="$(trim_directory_suffix "$DATA_DIR")"
case "$DATA_DIR_INPUT" in
  ""|*$'\n'*|*$'\r'*) echo "Invalid DATA_DIR" >&2; exit 2 ;;
esac
if [ -L "$DATA_DIR_INPUT" ]; then
  echo "Refusing symlink DATA_DIR: $DATA_DIR_INPUT" >&2
  exit 2
fi
if [ -e "$DATA_DIR_INPUT" ] && [ ! -d "$DATA_DIR_INPUT" ]; then
  echo "DATA_DIR is not a directory: $DATA_DIR_INPUT" >&2
  exit 2
fi
DATA_DIR="$(canonicalize_data_dir "$DATA_DIR_INPUT")" || {
  echo "Could not canonicalize DATA_DIR: $DATA_DIR_INPUT" >&2
  exit 2
}
reject_unsafe_data_dir "$DATA_DIR" || exit 2
DATA_DIR_PRESENT=false
if [ -d "$DATA_DIR" ]; then
  verify_installation_root || exit 2
  DATA_DIR_PRESENT=true
fi

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║                      ⚠️  DESTRUCTIVE TEARDOWN  ⚠️           ║"
echo "╠══════════════════════════════════════════════════════════════╣"
echo "║ This will PERMANENTLY DELETE all devops resources:          ║"
echo "║                                                            ║"
echo "║   • DevOps control plane container & image                 ║"
echo "║   • ALL project containers managed by devops               ║"
echo "║   • ALL project volumes (databases, state)                 ║"
echo "║   • ALL GitHub runner containers & state volumes           ║"
echo "║   • DevOps Docker networks                                 ║"
echo "║   • Persistent database (projects, config, secrets)        ║"
echo "║   • All deployment logs and audit history                  ║"
echo "║                                                            ║"
echo "║   Unrelated containers (jellyfin, immich, etc.) are SAFE.  ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

GITHUB_ORG="${GITHUB_ORG:-sauraku}"
case "${GITHUB_ORG}" in *[!A-Za-z0-9_.-]*|"") echo "Invalid GITHUB_ORG" >&2; exit 2 ;; esac

# Ownership comes only from exact labels and registered Compose project names.
DEV_CONTAINERS=$(docker ps -a --filter label=com.sauraku.devops.role=control --format '{{.Names}}' 2>/dev/null || true)
DEV_VOLUMES=""
DEV_NETWORKS=""
DEV_IMAGES=$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | while read -r image; do
  if [[ "$image" == "ghcr.io/${GITHUB_ORG}/devops:"* ]] ||
     [[ "$image" == "ghcr.io/${GITHUB_ORG}/devops-runner:"* ]]; then
    printf '%s\n' "$image"
  fi
done)

# Collect exact Compose project names from registered project env files.
PROJECT_NAMES=""
COMPOSE_PROJECTS=""
COMPOSE_CONTAINERS=""
COMPOSE_VOLUMES=""
if [ -d "${DATA_DIR}/Projects" ]; then
  PROJECT_NAMES=$(ls -1 "${DATA_DIR}/Projects/" 2>/dev/null || sudo -n ls -1 "${DATA_DIR}/Projects/" 2>/dev/null || true)
fi
for project in ${PROJECT_NAMES}; do
  case "${project}" in *[!a-z0-9_.-]*|"") continue ;; esac
  for env_file in "${DATA_DIR}/Projects/${project}"/.env.*; do
    [ -f "${env_file}" ] || continue
    compose_project=$(awk -F= '$1 == "COMPOSE_PROJECT_NAME" {print substr($0, index($0, "=") + 1); exit}' "${env_file}")
    case "${compose_project}" in *[!a-z0-9_.-]*|"") continue ;; esac
    COMPOSE_PROJECTS="${COMPOSE_PROJECTS} ${compose_project}"
  done
  COMPOSE_PROJECTS="${COMPOSE_PROJECTS} devops-runner-${project}"
done
COMPOSE_PROJECTS=$(echo "${COMPOSE_PROJECTS}" | tr ' ' '\n' | sort -u | xargs)
for compose_project in ${COMPOSE_PROJECTS}; do
  matched_containers=$(docker ps -a --filter "label=com.docker.compose.project=${compose_project}" --format '{{.Names}}' 2>/dev/null || true)
  matched_volumes=$(docker volume ls --filter "label=com.docker.compose.project=${compose_project}" --format '{{.Name}}' 2>/dev/null || true)
  matched_networks=$(docker network ls --filter "label=com.docker.compose.project=${compose_project}" --format '{{.Name}}' 2>/dev/null || true)
  COMPOSE_CONTAINERS="${COMPOSE_CONTAINERS} ${matched_containers}"
  COMPOSE_VOLUMES="${COMPOSE_VOLUMES} ${matched_volumes}"
  DEV_NETWORKS="${DEV_NETWORKS} ${matched_networks}"
done
COMPOSE_CONTAINERS=$(echo "${COMPOSE_CONTAINERS}" | tr ' ' '\n' | sort -u | xargs)
COMPOSE_VOLUMES=$(echo "${COMPOSE_VOLUMES}" | tr ' ' '\n' | sort -u | xargs)

HAS_ANY=false

show_list() {
  local label="$1"; shift
  if [ -n "$*" ]; then
    HAS_ANY=true
    echo "${label}"
    for item in "$@"; do
      echo "  ${item}"
    done
    echo ""
  fi
}

show_list "── DevOps Containers ──" ${DEV_CONTAINERS}
show_list "── DevOps Volumes ──" ${DEV_VOLUMES}
show_list "── DevOps Networks ──" ${DEV_NETWORKS}
show_list "── DevOps Docker Images ──" ${DEV_IMAGES}

if [ -n "${COMPOSE_CONTAINERS}" ]; then
  HAS_ANY=true
  echo "── Project Containers (managed through compose) ──"
  for c in ${COMPOSE_CONTAINERS}; do
    status=$(docker inspect --format '{{.State.Status}}' "${c}" 2>/dev/null || true)
    echo "  ${c} (${status})"
  done
  echo ""
fi

if [ -n "${COMPOSE_VOLUMES}" ]; then
  HAS_ANY=true
  echo "── Project Volumes ──"
  for v in ${COMPOSE_VOLUMES}; do
    echo "  ${v}"
  done
  echo ""
fi

if [ "$DATA_DIR_PRESENT" = true ]; then
  HAS_ANY=true
  echo "── Persistent Data ──"
  du -sh "${DATA_DIR}" 2>/dev/null | awk '{print "  " $0}'
  echo ""
fi

if [ "${HAS_ANY}" = false ]; then
  echo "Nothing to clean up. Server is already clean."
  exit 0
fi

echo "⚠  WARNING: Only the resources listed above will be removed."
echo "   Unrelated containers (jellyfin, immich, etc.) will NOT be touched."
echo ""
echo -n "Type 'yes' to confirm permanent deletion of listed resources: "
read -r CONFIRM
if [ "${CONFIRM}" != "yes" ]; then
  echo "Aborted."
  exit 0
fi

# Re-check the installation identity after confirmation and before any
# destructive Docker, process, or filesystem operation.
if [ "$DATA_DIR_PRESENT" = true ]; then
  verify_installation_root || exit 2
fi

echo ""
echo "==> Tearing down..."

# Stop only the host process recorded by this installation.
PID_FILE="${DATA_DIR}/Run/devops-control.pid"
if [ -f "${PID_FILE}" ]; then
  DEVOP_PID=$(sed -n '1p' "${PID_FILE}")
  if [[ "${DEVOP_PID}" =~ ^[0-9]+$ ]]; then
    command_line=$(ps -p "${DEVOP_PID}" -o command= 2>/dev/null || true)
    case "${command_line}" in
      *devops-control*)
        echo "  Stopping host devops-control process (PID: ${DEVOP_PID})..."
        kill "${DEVOP_PID}" 2>/dev/null || true
        sleep 1
        ;;
    esac
  fi
fi

# Remove only the resources listed above
for c in ${DEV_CONTAINERS} ${COMPOSE_CONTAINERS}; do
  echo "  Removing container: ${c}"
  docker rm -f "${c}" 2>/dev/null || true
done

for v in ${DEV_VOLUMES} ${COMPOSE_VOLUMES}; do
  echo "  Removing volume: ${v}"
  docker volume rm -f "${v}" 2>/dev/null || true
done

for n in ${DEV_NETWORKS}; do
  echo "  Removing network: ${n}"
  docker network rm "${n}" 2>/dev/null || true
done

for img in ${DEV_IMAGES}; do
  echo "  Removing image: ${img}"
  docker rmi -f "${img}" 2>/dev/null || true
done

# Remove persistent data only after re-verifying the path-bound installation
# identity. Prefer non-interactive sudo before an unprivileged attempt so a
# partial failure cannot delete the marker and invalidate the safety check.
DATA_REMOVAL_FAILED=false
if [ "$DATA_DIR_PRESENT" = true ]; then
  verify_installation_root || {
    echo "  Refusing to remove persistent data because its identity changed." >&2
    DATA_REMOVAL_FAILED=true
  }
fi
if [ "$DATA_DIR_PRESENT" = true ] && [ "$DATA_REMOVAL_FAILED" = false ]; then
  echo "  Removing persistent data: ${DATA_DIR}"
  if [ "$(id -u)" -eq 0 ]; then
    rm -rf -- "$DATA_DIR" 2>/dev/null || DATA_REMOVAL_FAILED=true
  elif sudo -n true >/dev/null 2>&1; then
    verify_installation_root && sudo -n rm -rf -- "$DATA_DIR" 2>/dev/null || DATA_REMOVAL_FAILED=true
  else
    rm -rf -- "$DATA_DIR" 2>/dev/null || DATA_REMOVAL_FAILED=true
  fi
  if [ -e "$DATA_DIR" ] || [ -L "$DATA_DIR" ]; then
    DATA_REMOVAL_FAILED=true
  fi
fi

if [ "$DATA_REMOVAL_FAILED" = true ]; then
  echo "" >&2
  echo "Teardown incomplete: could not safely remove verified installation data at $DATA_DIR." >&2
  exit 1
fi

echo ""
echo "==> Teardown complete. Only devops resources were removed."
