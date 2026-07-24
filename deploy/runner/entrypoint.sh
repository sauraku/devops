#!/bin/bash
set -euo pipefail

# Use the RUNNER_DIR environment variable passed from docker-compose
RUNNER_DIR="${RUNNER_DIR:-/home/runner/actions-runner}"
RUNNER_RUNTIME_DIR="${RUNNER_RUNTIME_DIR:-/run/devops-runner-registration}"

# If RUNNER_DIR is mounted to a host directory, ensure its binaries are updated to match the container's version
if [ "$RUNNER_DIR" != "/home/runner/actions-runner" ]; then
  if [ -z "${RUNNER_DIR}" ] || [ "${RUNNER_DIR}" = "/" ] || [ "${RUNNER_DIR}" = "/home" ] || [ "${RUNNER_DIR}" = "/home/runner" ]; then
    echo "❌ Error: Invalid RUNNER_DIR '${RUNNER_DIR}'" >&2
    exit 1
  fi
  CURRENT_VER=""
  if [ -f "$RUNNER_DIR/.runner_version" ]; then
    CURRENT_VER=$(cat "$RUNNER_DIR/.runner_version")
  fi

  if [ "$CURRENT_VER" != "$RUNNER_VERSION" ]; then
    echo "🔄 Updating runner binaries from version '$CURRENT_VER' to '$RUNNER_VERSION'..."
    mkdir -p "$RUNNER_DIR"
    
    # Remove existing symlinks or directories for bin/externals
    rm -rf "$RUNNER_DIR/bin" "$RUNNER_DIR/externals"
    
    # Copy fresh files from the image
    cp -r /home/runner/actions-runner/. "$RUNNER_DIR/"
    
    # Save the updated version
    echo "$RUNNER_VERSION" > "$RUNNER_DIR/.runner_version"
    echo "✅ Runner binaries updated successfully."
  fi
fi

configure_runner() {
  local labels
  if [ -L "$RUNNER_RUNTIME_DIR" ] || [ ! -d "$RUNNER_RUNTIME_DIR" ] || [ ! -O "$RUNNER_RUNTIME_DIR" ]; then
    echo "❌ Error: Runner runtime directory must be an owned, non-symlink directory: $RUNNER_RUNTIME_DIR" >&2
    return 74
  fi
  if [ -z "${REPO_URL:-}" ]; then
    echo "❌ Error: Runner is not configured, and REPO_URL is missing!" >&2
    return 1
  fi

  REGISTRATION_TOKEN_FILE="${RUNNER_REGISTRATION_TOKEN_FILE:-${RUNNER_RUNTIME_DIR}/token}"
  cleanup_registration_token() {
    rm -f -- "$REGISTRATION_TOKEN_FILE" 2>/dev/null || true
  }
  trap cleanup_registration_token EXIT

  echo "DEVOPS_RUNNER_REGISTRATION_REQUIRED"
  for _ in $(seq 1 60); do
    [ -f "$REGISTRATION_TOKEN_FILE" ] && break
    sleep 1
  done
  if [ ! -f "$REGISTRATION_TOKEN_FILE" ]; then
    echo "❌ Error: Timed out waiting for runner registration credential." >&2
    return 1
  fi

  RUNNER_TOKEN="$(<"$REGISTRATION_TOKEN_FILE")"
  if [ -z "$RUNNER_TOKEN" ] || [[ "$RUNNER_TOKEN" == *$'\n'* ]] || [[ "$RUNNER_TOKEN" == *$'\r'* ]]; then
    unset RUNNER_TOKEN
    echo "❌ Error: Runner registration credential is invalid." >&2
    return 1
  fi
  if [[ "$RUNNER_TOKEN" == ghp_* || "$RUNNER_TOKEN" == github_pat_* || "$RUNNER_TOKEN" =~ ^[A-Fa-f0-9]{40}$ ]]; then
    unset RUNNER_TOKEN
    echo "❌ Error: Refusing a long-lived GitHub credential in the runner container." >&2
    return 1
  fi
  if ! rm -f -- "$REGISTRATION_TOKEN_FILE" || [ -e "$REGISTRATION_TOKEN_FILE" ]; then
    unset RUNNER_TOKEN
    echo "❌ Error: Could not remove runner registration credential before startup." >&2
    return 1
  fi
  trap - EXIT

  echo "⚙️ Configuring GitHub Actions Runner..."
  cd "$RUNNER_DIR"
  labels="${RUNNER_LABELS:-development}"
  echo "⚙️ Registering runner with labels: ${labels}"
  if ! ./config.sh --url "$REPO_URL" --token "$RUNNER_TOKEN" --unattended --replace --name "${RUNNER_NAME:-devops-runner-container}" --labels "${labels}"; then
    echo ""
    echo "❌ ERROR: GitHub Actions Runner registration failed!"
    echo "----------------------------------------------------------------------"
    echo "This is usually caused by an expired or invalid registration token."
    echo "GitHub Actions runner registration tokens expire after 1 hour."
    echo ""
    echo "Please retrieve a new registration token from:"
    echo "👉 $REPO_URL/settings/actions/runners"
    echo "----------------------------------------------------------------------"
    return 1
  fi
  unset RUNNER_TOKEN
}

# Check if runner is already configured. An unconfigured runner asks the
# controller for a short-lived registration token; no GitHub PAT is accepted in
# this container and no credential is inherited through the environment.
if [ ! -f "$RUNNER_DIR/.runner" ] && [ ! -f "$RUNNER_DIR/.runner_migrated" ]; then
  configure_runner
else
  echo "ℹ️ Runner already configured."
fi

echo "🚀 Starting GitHub Actions Runner..."
cd "$RUNNER_DIR"

# Registration credentials must not remain available to repository jobs.
unset RUNNER_TOKEN REGISTRATION_TOKEN_FILE

# Repository-controlled stdout cannot authorize registration cleanup. The
# controller independently monitors deletion hints, verifies exact absence via
# GitHub, and replaces the entire container and state volume when necessary.
exec ./run.sh
