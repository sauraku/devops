#!/usr/bin/env bash
# Production start script for devops-control.
# Does NOT rebuild (unlike local.sh). Expects a pre-built binary.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$DIR/devops-control"
ENV_FILE="$DIR/.env.prod"

if [ ! -f "$BINARY" ]; then
  echo "!! Binary not found at $BINARY. Run 'go build -o $BINARY ./cmd/devops-control/' first."
  exit 1
fi

# Load production env file if present
if [ -f "$ENV_FILE" ]; then
  set -a
  source "$ENV_FILE"
  set +a
fi

# Required — fail early if missing
: "${DEPLOY_CONTROL_TOKEN:?DEPLOY_CONTROL_TOKEN is required}"
: "${JWT_SECRET:?JWT_SECRET is required}"
: "${COOKIE_SECRET:?COOKIE_SECRET is required}"

export DEPLOY_CONTROL_TOKEN JWT_SECRET COOKIE_SECRET
export BASE_DIR="${BASE_DIR:-$DIR/data}"

mkdir -p "$BASE_DIR"

echo "==> Starting DevOps Control (production)"
echo "    Binary: $BINARY"
echo "    Data:   $BASE_DIR"
echo "    PID:    $$"
echo ""

exec "$BINARY"
