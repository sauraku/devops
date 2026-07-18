#!/usr/bin/env bash
# Bootstrap devops-control on a production server.
#
# Usage:
#   bash <(curl -fsSL https://raw.githubusercontent.com/sauraku/devops/main/deploy/bootstrap.sh)
#
# Before running, make sure Docker is authenticated to ghcr.io with a PAT:
#   echo <token> | docker login ghcr.io -u sauraku --password-stdin
set -euo pipefail
umask 077

IMAGE="${IMAGE:-ghcr.io/sauraku/devops:main}"
RUNNER_IMAGE="${RUNNER_IMAGE:-ghcr.io/sauraku/devops-runner:main}"
CONTAINER="${CONTAINER:-devops-control}"
DATA_DIR_WAS_SET="${DATA_DIR+x}"
DATA_DIR="${DATA_DIR:-/opt/devops-control}"
ENV_FILE_WAS_SET="${ENV_FILE+x}"
ENV_FILE="${ENV_FILE:-/opt/devops-control/.env.prod}"
HOST_PORT="${HOST_PORT:-8787}"
GITHUB_USER="${GITHUB_USER:-sauraku}"
RUNNER_NETWORK="${RUNNER_NETWORK:-devops-control-runners}"
RUNNER_CONTROL_URL="${RUNNER_CONTROL_URL:-http://${CONTAINER}:8787}"
INSTALL_MARKER_NAME=".devops-control-installation"
INSTALL_MARKER_PRODUCT="product=devops-control"
INSTALL_MARKER_FORMAT="format=1"

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

write_installation_marker() {
  local marker="$DATA_DIR/$INSTALL_MARKER_NAME" marker_tmp=""
  if [ -L "$marker" ] || { [ -e "$marker" ] && [ ! -f "$marker" ]; }; then
    echo "Refusing unsafe installation marker: $marker" >&2
    return 1
  fi
  if [ -f "$marker" ]; then
    if [ "$(sed -n '1p' "$marker" 2>/dev/null || true)" != "$INSTALL_MARKER_PRODUCT" ] ||
       [ "$(sed -n '2p' "$marker" 2>/dev/null || true)" != "$INSTALL_MARKER_FORMAT" ]; then
      echo "Refusing to replace an unrecognized installation marker: $marker" >&2
      return 1
    fi
  fi
  marker_tmp="$(mktemp "$DATA_DIR/.installation-marker.tmp.XXXXXX")"
  if ! chmod 0600 "$marker_tmp" ||
     ! printf '%s\n%s\ndata_dir=%s\n' \
       "$INSTALL_MARKER_PRODUCT" "$INSTALL_MARKER_FORMAT" "$DATA_DIR" > "$marker_tmp" ||
     ! mv -f -- "$marker_tmp" "$marker"; then
    rm -f -- "$marker_tmp"
    return 1
  fi
}

# Fall back to user home if /opt is not writable
if [ ! -w "$(dirname "$DATA_DIR")" ] && [ ! -w "$DATA_DIR" ]; then
  if [ -n "$DATA_DIR_WAS_SET" ]; then
    echo "Configured DATA_DIR is not writable: $DATA_DIR" >&2
    exit 1
  fi
  if [ -z "${HOME:-}" ]; then
    echo "HOME is required when /opt is not writable." >&2
    exit 1
  fi
  DATA_DIR="${HOME}/.devops-control"
  echo "==> /opt not writable, using $DATA_DIR"
fi

# Create and identify the data root before any external changes. Teardown only
# recursively removes roots carrying this path-bound marker and layout.
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
mkdir -p -- "$DATA_DIR_INPUT"
if [ -L "$DATA_DIR_INPUT" ] || [ ! -d "$DATA_DIR_INPUT" ]; then
  echo "Refusing unsafe DATA_DIR: $DATA_DIR_INPUT" >&2
  exit 2
fi
DATA_DIR="$(cd -P -- "$DATA_DIR_INPUT" && pwd -P)"
reject_unsafe_data_dir "$DATA_DIR"
if [ -z "$ENV_FILE_WAS_SET" ]; then
  ENV_FILE="$DATA_DIR/.env.prod"
fi
for expected_dir in State Projects Logs Run; do
  if [ -L "$DATA_DIR/$expected_dir" ]; then
    echo "Refusing symlink in installation layout: $DATA_DIR/$expected_dir" >&2
    exit 2
  fi
  mkdir -p -- "$DATA_DIR/$expected_dir"
  if [ ! -d "$DATA_DIR/$expected_dir" ] || [ -L "$DATA_DIR/$expected_dir" ]; then
    echo "Invalid installation layout: $DATA_DIR/$expected_dir" >&2
    exit 2
  fi
  chmod 0750 "$DATA_DIR/$expected_dir"
done
write_installation_marker

# Login to GHCR using GITHUB_TOKEN env var if set.
# If not set, assumes Docker is already authenticated (e.g. manual docker login).
# Avoids `gh auth token` which may lack read:packages scope.
if [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "==> Logging in to ghcr.io with GITHUB_TOKEN..."
  printf '%s' "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
fi

# Check for env file
if [ ! -f "$ENV_FILE" ]; then
  echo "==> No env file found. Generating $ENV_FILE with random secrets..."
  mkdir -p "$(dirname "$ENV_FILE")"
  cat > "$ENV_FILE" <<EOF
# Auto-generated by deploy-devops.sh
DEPLOY_CONTROL_TOKEN=$(openssl rand -hex 32)
COOKIE_SECRET=$(openssl rand -hex 32)
ENCRYPTION_KEY=$(openssl rand -hex 32)
DEPLOY_CONTROL_HOST=0.0.0.0
COOKIE_SECURE=true
BASE_DIR=$DATA_DIR
RUNNER_IMAGE=$RUNNER_IMAGE
RUNNER_NETWORK=$RUNNER_NETWORK
RUNNER_CONTROL_URL=$RUNNER_CONTROL_URL
EOF
  chmod 600 "$ENV_FILE"
  echo "    Secrets written to $ENV_FILE"
elif ! grep -q "^BASE_DIR=" "$ENV_FILE"; then
  echo "==> Adding BASE_DIR to existing env file"
  echo "BASE_DIR=$DATA_DIR" >> "$ENV_FILE"
fi
if ! grep -q "^RUNNER_IMAGE=" "$ENV_FILE"; then
  echo "==> Adding RUNNER_IMAGE to existing env file"
  echo "RUNNER_IMAGE=$RUNNER_IMAGE" >> "$ENV_FILE"
fi
if ! grep -q "^RUNNER_NETWORK=" "$ENV_FILE"; then
  echo "RUNNER_NETWORK=$RUNNER_NETWORK" >> "$ENV_FILE"
fi
if ! grep -q "^RUNNER_CONTROL_URL=" "$ENV_FILE"; then
  echo "RUNNER_CONTROL_URL=$RUNNER_CONTROL_URL" >> "$ENV_FILE"
fi

# Persist GITHUB_TOKEN into env file if provided at bootstrap time
if [ -n "${GITHUB_TOKEN:-}" ] && ! grep -q "^GITHUB_TOKEN=" "$ENV_FILE" 2>/dev/null; then
  echo "==> Persisting GITHUB_TOKEN to env file"
  echo "GITHUB_TOKEN=$GITHUB_TOKEN" >> "$ENV_FILE"
fi

echo "==> Pulling image: $IMAGE"
docker pull "$IMAGE"
docker pull "$RUNNER_IMAGE"

resolve_immutable_image() {
  local requested="$1" resolved
  case "$requested" in
    *@sha256:*) printf '%s\n' "$requested"; return 0 ;;
  esac
  resolved="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$requested" | awk 'NR == 1 { print; exit }')"
  case "$resolved" in
    *@sha256:*) printf '%s\n' "$resolved" ;;
    *)
      echo "Could not resolve an immutable digest for pulled image $requested." >&2
      return 1
      ;;
  esac
}

set_dotenv_value() {
  local file="$1" key="$2" value="$3" tmp found=0 line
  case "$value" in
    *$'\n'*|*$'\r'*) echo "Invalid multiline value for $key" >&2; return 1 ;;
  esac
  tmp="$(mktemp "${file}.tmp.XXXXXX")"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "$key="*)
        if [ "$found" -eq 0 ]; then
          printf '%s=%s\n' "$key" "$value" >> "$tmp"
          found=1
        fi
        ;;
      *) printf '%s\n' "$line" >> "$tmp" ;;
    esac
  done < "$file"
  if [ "$found" -eq 0 ]; then
    printf '%s=%s\n' "$key" "$value" >> "$tmp"
  fi
  chmod 0600 "$tmp"
  mv -f -- "$tmp" "$file"
}

# A mutable tag is acceptable as an operator-selected update channel, but the
# running controller and every runner are pinned to the exact pulled content.
IMAGE="$(resolve_immutable_image "$IMAGE")"
RUNNER_IMAGE="$(resolve_immutable_image "$RUNNER_IMAGE")"
set_dotenv_value "$ENV_FILE" RUNNER_IMAGE "$RUNNER_IMAGE"

if docker network inspect "$RUNNER_NETWORK" >/dev/null 2>&1; then
  network_owner="$(docker network inspect -f '{{index .Labels "com.sauraku.devops.role"}}' "$RUNNER_NETWORK" 2>/dev/null || true)"
  if [ "$network_owner" != "runner-network" ]; then
    echo "Refusing to use unowned Docker network named $RUNNER_NETWORK." >&2
    exit 1
  fi
else
  docker network create --label com.sauraku.devops.role=runner-network "$RUNNER_NETWORK" >/dev/null
fi

echo "==> Replacing existing owned container (if any)"
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  owner_label="$(docker inspect -f '{{index .Config.Labels "com.sauraku.devops.role"}}' "$CONTAINER" 2>/dev/null || true)"
  if [ "$owner_label" != "control" ]; then
    echo "Refusing to replace unowned container named $CONTAINER." >&2
    exit 1
  fi
  docker stop "$CONTAINER"
  docker rm "$CONTAINER"
fi

echo "==> Starting devops-control"
docker run -d \
  --name "$CONTAINER" \
  --restart unless-stopped \
  --pull never \
  --label com.sauraku.devops.role=control \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=128m \
  --network "$RUNNER_NETWORK" \
  -e "BASE_DIR=$DATA_DIR" \
  -e GITHUB_TOKEN="${GITHUB_TOKEN:-}" \
  -e "RUNNER_NETWORK=$RUNNER_NETWORK" \
  -e "RUNNER_CONTROL_URL=$RUNNER_CONTROL_URL" \
  --env-file "$ENV_FILE" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DATA_DIR":"$DATA_DIR" \
  -p "${HOST_PORT}:8787" \
  "$IMAGE"

echo "==> Done. Container: $CONTAINER"
echo "    Logs: docker logs -f $CONTAINER"
echo "    Token: saved in $ENV_FILE"
echo "    Login locally at http://127.0.0.1:${HOST_PORT} or through your TLS reverse proxy"
echo "    To update later, run the same command again."
