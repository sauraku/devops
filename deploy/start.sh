#!/usr/bin/env bash
# Production start script for devops-control.
# Does NOT rebuild (unlike local.sh). Expects a pre-built binary.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="$DIR/devops-control"
ENV_FILE="$DIR/.env.prod"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

if [ ! -f "$BINARY" ]; then
  echo "!! Binary not found at $BINARY. Run 'go build -o $BINARY ./cmd/devops-control/' first."
  exit 1
fi

# Load production env file if present
if [ -f "$ENV_FILE" ]; then
  load_dotenv "$ENV_FILE"
fi

# Required — fail early if missing
: "${DEPLOY_CONTROL_TOKEN:?DEPLOY_CONTROL_TOKEN is required}"
: "${COOKIE_SECRET:?COOKIE_SECRET is required}"
: "${ENCRYPTION_KEY:?ENCRYPTION_KEY is required}"

export DEPLOY_CONTROL_TOKEN COOKIE_SECRET ENCRYPTION_KEY
export BASE_DIR="${BASE_DIR:-$DIR/data}"
export RUNNER_NETWORK="${RUNNER_NETWORK:-devops-control-runners}"
export RUNNER_CONTROL_URL="${RUNNER_CONTROL_URL:-http://host.docker.internal:${DEPLOY_CONTROL_PORT:-8787}}"

mkdir -p "$BASE_DIR/Run"
chmod 0750 "$BASE_DIR/Run"
ensure_runner_network "$RUNNER_NETWORK"

echo "==> Starting DevOps Control (production)"
echo "    Binary: $BINARY"
echo "    Data:   $BASE_DIR"
echo "    PID:    $$"
echo ""

exec "$BINARY"
