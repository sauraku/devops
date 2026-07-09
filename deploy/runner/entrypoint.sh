#!/bin/bash
set -e

# Use the RUNNER_DIR environment variable passed from docker-compose
RUNNER_DIR="${RUNNER_DIR:-/home/runner/actions-runner}"

# If RUNNER_DIR is mounted to a host directory, ensure its binaries are updated to match the container's version
if [ "$RUNNER_DIR" != "/home/runner/actions-runner" ]; then
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

# Check if runner is already configured
if [ ! -f "$RUNNER_DIR/.runner" ] && [ ! -f "$RUNNER_DIR/.runner_migrated" ]; then
  if [ -z "$RUNNER_TOKEN" ] || [ -z "$REPO_URL" ]; then
    echo "❌ Error: Runner is not configured, and RUNNER_TOKEN or REPO_URL is missing!"
    exit 1
  fi

  # Detect if RUNNER_TOKEN is a GitHub Personal Access Token (PAT)
  if [[ "$RUNNER_TOKEN" == ghp_* || "$RUNNER_TOKEN" == github_pat_* ]]; then
    echo "🔑 Detected GitHub Personal Access Token (PAT). Fetching a fresh runner registration token..."
    
    CLEAN_URL="${REPO_URL%.git}"
    CLEAN_URL="${CLEAN_URL%/}"
    OWNER_REPO=$(echo "$CLEAN_URL" | awk -F'[:/]' '{print $(NF-1)"/"$NF}')
    
    echo "📦 Repository identified: $OWNER_REPO"
    
    API_RESPONSE=$(curl -s -X POST \
      -H "Accept: application/vnd.github+json" \
      -H "Authorization: Bearer $RUNNER_TOKEN" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      "https://api.github.com/repos/${OWNER_REPO}/actions/runners/registration-token" || true)
      
    REG_TOKEN=$(echo "$API_RESPONSE" | grep -o '"token": *"[^"]*"' | head -n1 | grep -o '"[^"]*"$' | tr -d '"' || true)
    
    if [ -n "$REG_TOKEN" ]; then
      echo "✅ Successfully fetched registration token."
      RUNNER_TOKEN="$REG_TOKEN"
    else
      echo "❌ Failed to fetch registration token using PAT." >&2
      exit 1
    fi
  fi
  
  echo "⚙️ Configuring GitHub Actions Runner..."
  cd "$RUNNER_DIR"
  labels="${RUNNER_LABELS:-development,production}"
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
    exit 1
  fi
else
  echo "ℹ️ Runner already configured."
fi

echo "🚀 Starting GitHub Actions Runner..."
cd "$RUNNER_DIR"

RUN_LOG=$(mktemp)
trap 'rm -f "$RUN_LOG"' EXIT

if [ -d "/logs" ]; then
  ./run.sh 2>&1 | tee "$RUN_LOG" | tee -a /logs/runner.log || true
else
  ./run.sh 2>&1 | tee "$RUN_LOG" || true
fi

# Check if the runner failed because registration was deleted from server
if grep -q "The runner registration has been deleted from the server" "$RUN_LOG" 2>/dev/null; then
  echo "⚠️ Detected that the runner registration was deleted from the GitHub server."
  echo "🧹 Removing invalid local configuration files..."
  rm -f "$RUNNER_DIR/.runner" "$RUNNER_DIR/.runner_migrated" "$RUNNER_DIR/.credentials" "$RUNNER_DIR/.credentials_rsaparams"
  exit 1
fi


