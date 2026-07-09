#!/usr/bin/env bash
# Complete teardown — removes devops-control, all project containers,
# volumes, runner state, and persistent data. Does NOT touch unrelated
# containers (jellyfin, immich, etc.).
#
# Run via:
#   bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/teardown.sh)
set -uo pipefail

CONTAINER="${CONTAINER:-devops-control}"
DATA_DIR="${DATA_DIR:-${HOME}/.devops-control}"

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

# Gather what will be deleted — use devops-* patterns, not project-specific names
DEV_CONTAINERS=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E '^devops-control$|^devops-runner-|^devops-deploy-control-')
DEV_VOLUMES=$(docker volume ls --format '{{.Name}}' 2>/dev/null | grep -E '^devops-runner-')
DEV_NETWORKS=$(docker network ls --format '{{.Name}}' 2>/dev/null | grep -E '^devops-orchestrator-')
DEV_IMAGES=$(docker images --filter "reference=ghcr.io/${GITHUB_ORG}/devops" --format '{{.Repository}}:{{.Tag}}' 2>/dev/null)

# Derive compose project names from registered projects in the data directory
PROJECT_NAMES=""
if [ -d "${DATA_DIR}/Projects" ]; then
  PROJECT_NAMES=$(ls -1 "${DATA_DIR}/Projects/" 2>/dev/null || sudo -n ls -1 "${DATA_DIR}/Projects/" 2>/dev/null || true)
fi

# Collect all project containers managed via compose — match known project prefixes only
COMPOSE_CONTAINERS=""
COMPOSE_VOLUMES=""
for project in ${PROJECT_NAMES}; do
  # Each project may have env files: .env, .env.dev, .env.main, etc.
  # Derive compose project names from each env file
  for envf in "${DATA_DIR}/Projects/${project}/.env" "${DATA_DIR}/Projects/${project}/.env."*; do
    [ -f "${envf}" ] || continue
    cpn=""
    # shellcheck disable=SC1090
    cpn=$(grep -s '^COMPOSE_PROJECT_NAME=' "${envf}" 2>/dev/null | tail -1 | cut -d= -f2)
    [ -z "${cpn}" ] && cpn="${project}"
    # Match containers and volumes with this prefix
    matched_containers=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^${cpn}-" || true)
    matched_volumes=$(docker volume ls --format '{{.Name}}' 2>/dev/null | grep -E "^${cpn}_" || true)
    COMPOSE_CONTAINERS="${COMPOSE_CONTAINERS} ${matched_containers}"
    COMPOSE_VOLUMES="${COMPOSE_VOLUMES} ${matched_volumes}"
  done
done
# Deduplicate
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

if [ -d "${DATA_DIR}" ]; then
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

echo ""
echo "==> Tearing down..."

# Kill any host-level devops-control process (runs outside Docker via ./local.sh or ./start.sh)
DEVOP_PID=$(pgrep -f "devops-control" 2>/dev/null || true)
if [ -n "${DEVOP_PID}" ]; then
  echo "  Stopping host devops-control process (PID: ${DEVOP_PID})..."
  kill "${DEVOP_PID}" 2>/dev/null || true
  sleep 1
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

# Remove persistent data
if [ -d "${DATA_DIR}" ]; then
  echo "  Removing persistent data: ${DATA_DIR}"
  rm -rf "${DATA_DIR}" 2>/dev/null || sudo -n rm -rf "${DATA_DIR}" 2>/dev/null || {
    docker run --rm -v "${DATA_DIR}:/data" alpine sh -c 'rm -rf /data/[!.]* /data/.[!.]* /data/..?*' 2>/dev/null
    rmdir "${DATA_DIR}" 2>/dev/null || true
  } || echo "  ⚠ Could not remove ${DATA_DIR} (try: sudo rm -rf ${DATA_DIR})"
fi

echo ""
echo "==> Teardown complete. Only devops resources were removed."
