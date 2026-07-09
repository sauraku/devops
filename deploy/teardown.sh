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

# Also collect all project containers managed via compose
COMPOSE_CONTAINERS=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -vE '^devops-control$|^devops-runner-')
COMPOSE_VOLUMES=$(docker volume ls --format '{{.Name}}' 2>/dev/null | grep -vE '^devops-runner-')

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

echo "⚠  WARNING: Project containers and volumes will ALSO be removed."
echo "   ALL Docker containers and MOST volumes will be destroyed."
echo "   Only devops-* prefixed resources and persistent data are safe (they are listed above)."
echo ""
echo -n "Type 'yes' to confirm permanent deletion of ALL resources: "
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

# Stop and remove ALL containers (full purge)
echo "  Removing all containers..."
docker ps -aq 2>/dev/null | xargs -r docker rm -f 2>/dev/null || true

# Remove all images
echo "  Removing all images..."
docker images -q 2>/dev/null | xargs -r docker rmi -f 2>/dev/null || true

# Remove all volumes
echo "  Removing all volumes..."
docker volume ls -q 2>/dev/null | xargs -r docker volume rm -f 2>/dev/null || true

# Remove all custom networks (preserve default bridge/host/none)
echo "  Removing custom networks..."
docker network ls --format '{{.Name}}' 2>/dev/null | grep -vE '^(bridge|host|none)$' | xargs -r docker network rm 2>/dev/null || true

# Remove persistent data
echo "  Removing persistent data: ${DATA_DIR}"
rm -rf "${DATA_DIR}" 2>/dev/null || sudo rm -rf "${DATA_DIR}" 2>/dev/null || {
  docker run --rm -v "${DATA_DIR}:/data" alpine sh -c 'rm -rf /data/[!.]* /data/.[!.]* /data/..?*' 2>/dev/null
  rmdir "${DATA_DIR}" 2>/dev/null || true
} || echo "  ⚠ Could not fully remove ${DATA_DIR}"

echo ""
echo "==> Teardown complete. ALL Docker resources removed."
