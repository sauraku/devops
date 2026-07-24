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
GITHUB_TOKEN_WAS_SET="${GITHUB_TOKEN+x}"
TRUSTED_PROXY_CIDRS_WAS_SET="${TRUSTED_PROXY_CIDRS+x}"
DEPLOY_CONTROL_PUBLIC_URL_WAS_SET="${DEPLOY_CONTROL_PUBLIC_URL+x}"
HOST_PORT="${HOST_PORT:-8787}"
HOST_BIND="${HOST_BIND:-127.0.0.1}"
GITHUB_USER="${GITHUB_USER:-sauraku}"
RUNNER_NETWORK="${RUNNER_NETWORK:-devops-control-runners}"
RUNNER_CONTROL_URL="${RUNNER_CONTROL_URL:-http://${CONTAINER}:8787}"
INSTALL_MARKER_NAME=".devops-control-installation"
INSTALL_MARKER_PRODUCT="product=devops-control"
INSTALL_MARKER_FORMAT="format=1"
BOOTSTRAP_HEALTH_ATTEMPTS="${BOOTSTRAP_HEALTH_ATTEMPTS:-30}"

case "$BOOTSTRAP_HEALTH_ATTEMPTS" in
  ''|*[!0-9]*) echo "BOOTSTRAP_HEALTH_ATTEMPTS must be a positive integer." >&2; exit 2 ;;
esac
if [ "$BOOTSTRAP_HEALTH_ATTEMPTS" -lt 1 ] || [ "$BOOTSTRAP_HEALTH_ATTEMPTS" -gt 120 ]; then
  echo "BOOTSTRAP_HEALTH_ATTEMPTS must be between 1 and 120." >&2
  exit 2
fi

case "${HOST_BIND}" in
  127.0.0.1|0.0.0.0)
    ;;
  *)
    echo "HOST_BIND must be 127.0.0.1 (default) or 0.0.0.0." >&2
    exit 2
    ;;
esac

if [[ "${GITHUB_TOKEN:-}" == *$'\n'* ]] || [[ "${GITHUB_TOKEN:-}" == *$'\r'* ]]; then
  echo "GITHUB_TOKEN must be a single-line value." >&2
  exit 2
fi
if [[ "${TRUSTED_PROXY_CIDRS:-}" == *$'\n'* ]] || [[ "${TRUSTED_PROXY_CIDRS:-}" == *$'\r'* ]]; then
  echo "TRUSTED_PROXY_CIDRS must be a single-line value." >&2
  exit 2
fi
if [[ "${DEPLOY_CONTROL_PUBLIC_URL:-}" == *$'\n'* ]] || [[ "${DEPLOY_CONTROL_PUBLIC_URL:-}" == *$'\r'* ]]; then
  echo "DEPLOY_CONTROL_PUBLIC_URL must be a single-line value." >&2
  exit 2
fi
SUPPLIED_GITHUB_TOKEN="${GITHUB_TOKEN:-}"
unset GITHUB_TOKEN

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

unset_dotenv_value() {
  local file="$1" key="$2" tmp line
  tmp="$(mktemp "${file}.tmp.XXXXXX")"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "$key="*) ;;
      *) printf '%s\n' "$line" >> "$tmp" ;;
    esac
  done < "$file"
  chmod 0600 "$tmp"
  mv -f -- "$tmp" "$file"
}

dotenv_value() {
  local file="$1" key="$2" line value=""
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "$key="*) value="${line#*=}" ;;
    esac
  done < "$file"
  printf '%s' "$value"
}

ensure_dotenv_value() {
  local file="$1" key="$2" value="$3"
  if ! grep -q "^${key}=" "$file" 2>/dev/null; then
    set_dotenv_value "$file" "$key" "$value"
  fi
}

validate_trusted_proxy_cidrs() {
  local raw="$1"
  [ -n "$raw" ] || return 0
  docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    -e "TRUSTED_PROXY_CIDRS=$raw" \
    "$IMAGE" validate-trusted-proxy-cidrs
}

validate_public_url() {
  local value="$1" authority
  case "$value" in
    https://?*) ;;
    *) return 1 ;;
  esac
  authority="${value#https://}"
  authority="${authority%/}"
  case "$authority" in
    ''|*/*|*\?*|*\#*|*@*|*' '*|*$'\t'*) return 1 ;;
  esac
}

wait_for_http() {
  local url="$1" protocol="$2" attempt response_code
  for ((attempt = 1; attempt <= BOOTSTRAP_HEALTH_ATTEMPTS; attempt++)); do
    response_code="$(curl --silent --show-error --max-time 5 --proto "=${protocol}" \
      --output /dev/null --write-out '%{http_code}' "$url" 2>/dev/null || true)"
    if [ "$response_code" = 200 ]; then
      return 0
    fi
    if [ "$attempt" -lt "$BOOTSTRAP_HEALTH_ATTEMPTS" ]; then
      sleep 1
    fi
  done
  return 1
}

preflight_https_transport() {
  local origin="${1%/}"
  curl --silent --show-error --max-time 5 --proto '=https' \
    --output /dev/null "${origin}/api/health" >/dev/null
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
env_parent="$(dirname "$ENV_FILE")"
mkdir -p -- "$env_parent"
if [ -L "$ENV_FILE" ] || { [ -e "$ENV_FILE" ] && [ ! -f "$ENV_FILE" ]; }; then
  echo "Refusing unsafe env file: $ENV_FILE" >&2
  exit 1
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

durable_sync_path() {
  local path="$1"
  if ! sync -f "$path" >/dev/null 2>&1; then
    sync
  fi
}

journal_value() {
  local file="$1" key="$2"
  sed -n "s/^${key}=//p" "$file" | tail -n 1
}

docker_container_id() {
  docker inspect -f '{{.Id}}' "$1" 2>/dev/null || true
}

docker_container_role() {
  docker inspect -f '{{index .Config.Labels "com.sauraku.devops.role"}}' "$1" 2>/dev/null || true
}

docker_container_transaction() {
  docker inspect -f '{{index .Config.Labels "com.sauraku.devops.bootstrap-transaction"}}' "$1" 2>/dev/null || true
}

docker_container_running() {
  [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" = true ]
}

current_boot_id() {
  if [ -r /proc/sys/kernel/random/boot_id ]; then
    tr -d '\r\n' < /proc/sys/kernel/random/boot_id
  else
    printf '%s' unavailable
  fi
}

process_start_token() {
  local pid="$1"
  if [ -r "/proc/$pid/stat" ]; then
    awk '{ print $22 }' "/proc/$pid/stat" 2>/dev/null || true
  else
    printf '%s' unavailable
  fi
}

release_install_lock() {
  local owner token
  [ "${BOOTSTRAP_LOCK_HELD:-0}" -eq 1 ] || return 0
  owner="$BOOTSTRAP_LOCK_DIR/owner"
  token="$(journal_value "$owner" token 2>/dev/null || true)"
  if [ "$token" != "$BOOTSTRAP_LOCK_TOKEN" ]; then
    echo "Warning: bootstrap lock ownership changed; refusing to remove $BOOTSTRAP_LOCK_DIR." >&2
    BOOTSTRAP_LOCK_HELD=0
    return 1
  fi
  rm -f -- "$owner"
  if ! rmdir -- "$BOOTSTRAP_LOCK_DIR"; then
    echo "Warning: could not release bootstrap lock $BOOTSTRAP_LOCK_DIR." >&2
    BOOTSTRAP_LOCK_HELD=0
    return 1
  fi
  durable_sync_path "$(dirname "$BOOTSTRAP_LOCK_DIR")"
  BOOTSTRAP_LOCK_HELD=0
}

acquire_install_lock() {
  local owner="$BOOTSTRAP_LOCK_DIR/owner" attempt owner_pid owner_boot owner_start
  local observed_start quarantine
  for ((attempt = 1; attempt <= 20; attempt++)); do
    if mkdir -- "$BOOTSTRAP_LOCK_DIR" 2>/dev/null; then
      if ! printf 'format=1\npid=%s\ntoken=%s\nboot_id=%s\nstart_token=%s\n' \
        "$$" "$BOOTSTRAP_LOCK_TOKEN" "$(current_boot_id)" "$(process_start_token "$$")" > "$owner" ||
         ! chmod 0600 "$owner"; then
        rm -f -- "$owner"
        rmdir -- "$BOOTSTRAP_LOCK_DIR" 2>/dev/null || true
        echo "Could not initialize bootstrap lock $BOOTSTRAP_LOCK_DIR." >&2
        return 1
      fi
      durable_sync_path "$owner"
      durable_sync_path "$BOOTSTRAP_LOCK_DIR"
      BOOTSTRAP_LOCK_HELD=1
      return 0
    fi

    if [ -L "$BOOTSTRAP_LOCK_DIR" ] || [ ! -d "$BOOTSTRAP_LOCK_DIR" ]; then
      echo "Refusing unsafe bootstrap lock: $BOOTSTRAP_LOCK_DIR" >&2
      return 1
    fi
    if [ -L "$owner" ]; then
      echo "Refusing unsafe bootstrap lock owner: $owner" >&2
      return 1
    fi
    if [ ! -f "$owner" ]; then
      if [ "$attempt" -lt 20 ]; then
        sleep 0.05
        continue
      fi
      owner_pid=""
      owner_boot=""
      owner_start=""
    else
      owner_pid="$(journal_value "$owner" pid)"
      owner_boot="$(journal_value "$owner" boot_id)"
      owner_start="$(journal_value "$owner" start_token)"
    fi
    case "$owner_pid" in
      ''|*[!0-9]*) ;;
      *)
        if kill -0 "$owner_pid" 2>/dev/null; then
          if [ "$owner_boot" = unavailable ] || [ "$owner_boot" = "$(current_boot_id)" ]; then
            observed_start="$(process_start_token "$owner_pid")"
            if [ "$owner_start" = unavailable ] || [ "$observed_start" = "$owner_start" ]; then
              echo "Another bootstrap is already running for $DATA_DIR (pid $owner_pid)." >&2
              return 75
            fi
          fi
        fi
        ;;
    esac

    quarantine="${BOOTSTRAP_LOCK_DIR}.stale.$$.$attempt"
    if mv -- "$BOOTSTRAP_LOCK_DIR" "$quarantine" 2>/dev/null; then
      rm -f -- "$quarantine/owner"
      if ! rmdir -- "$quarantine"; then
        echo "Refusing bootstrap lock containing unexpected files: $quarantine" >&2
        return 1
      fi
      durable_sync_path "$(dirname "$BOOTSTRAP_LOCK_DIR")"
    fi
  done
  echo "Could not acquire bootstrap lock $BOOTSTRAP_LOCK_DIR." >&2
  return 75
}

write_transaction_journal() {
  local phase="$1" tmp
  tmp="$(mktemp "$DATA_DIR/Run/.bootstrap-transaction.tmp.XXXXXX")"
  if ! chmod 0600 "$tmp" ||
     ! printf '%s\n' \
       "format=1" \
       "transaction_id=$TRANSACTION_ID" \
       "phase=$phase" \
       "container=$CONTAINER" \
       "old_container_present=$HAD_OLD_CONTAINER" \
       "old_container_id=$OLD_CONTAINER_ID" \
       "old_container_name=$CONTAINER" \
       "old_container_rollback_name=$OLD_CONTAINER_NAME" \
       "old_container_was_running=$OLD_CONTAINER_WAS_RUNNING" \
       "candidate_container_id=$CANDIDATE_CONTAINER_ID" \
       "env_file=$ENV_FILE" \
       "staged_env=$STAGED_ENV" \
       "rollback_env=$ROLLBACK_ENV" \
       "env_had_live=$ENV_HAD_LIVE" \
       "network_created=$NETWORK_CREATED" \
       "runner_network=$RUNNER_NETWORK" > "$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  durable_sync_path "$tmp"
  if ! mv -f -- "$tmp" "$TRANSACTION_JOURNAL"; then
    rm -f -- "$tmp"
    return 1
  fi
  durable_sync_path "$TRANSACTION_JOURNAL"
  durable_sync_path "$(dirname "$TRANSACTION_JOURNAL")"
  TRANSACTION_PHASE="$phase"
}

clear_transaction_journal() {
  if [ -e "$TRANSACTION_JOURNAL" ]; then
    rm -f -- "$TRANSACTION_JOURNAL"
    durable_sync_path "$(dirname "$TRANSACTION_JOURNAL")"
  fi
}

restore_previous_environment() {
  local env_file="$1" rollback_env="$2" env_had_live="$3" restore_tmp
  if [ "$env_had_live" -eq 1 ]; then
    if [ ! -f "$rollback_env" ] || [ -L "$rollback_env" ]; then
      echo "ROLLBACK ERROR: previous environment snapshot is unavailable: $rollback_env" >&2
      return 1
    fi
    restore_tmp="$(mktemp "${env_file}.recover.XXXXXX")"
    if ! cp -- "$rollback_env" "$restore_tmp" ||
       ! chmod 0600 "$restore_tmp"; then
      rm -f -- "$restore_tmp"
      return 1
    fi
    durable_sync_path "$restore_tmp"
    if ! mv -f -- "$restore_tmp" "$env_file"; then
      rm -f -- "$restore_tmp"
      return 1
    fi
    durable_sync_path "$env_file"
    durable_sync_path "$(dirname "$env_file")"
  else
    rm -f -- "$env_file"
    durable_sync_path "$(dirname "$env_file")"
  fi
}

validate_transaction_path() {
  local path="$1" prefix="$2"
  [ -z "$path" ] || case "$path" in
    "$prefix".*) return 0 ;;
    *) return 1 ;;
  esac
}

recover_unfinished_transaction() {
  local journal="$TRANSACTION_JOURNAL" format phase transaction_id journal_container
  local old_present old_id old_name rollback_name old_was_running candidate_id
  local journal_env staged_env rollback_env env_had_live network_created journal_network
  local canonical_id canonical_role canonical_transaction old_role failed=0

  [ -e "$journal" ] || return 0
  if [ -L "$journal" ] || [ ! -f "$journal" ]; then
    echo "Refusing unsafe bootstrap transaction journal: $journal" >&2
    return 1
  fi

  format="$(journal_value "$journal" format)"
  phase="$(journal_value "$journal" phase)"
  transaction_id="$(journal_value "$journal" transaction_id)"
  journal_container="$(journal_value "$journal" container)"
  old_present="$(journal_value "$journal" old_container_present)"
  old_id="$(journal_value "$journal" old_container_id)"
  old_name="$(journal_value "$journal" old_container_name)"
  rollback_name="$(journal_value "$journal" old_container_rollback_name)"
  old_was_running="$(journal_value "$journal" old_container_was_running)"
  candidate_id="$(journal_value "$journal" candidate_container_id)"
  journal_env="$(journal_value "$journal" env_file)"
  staged_env="$(journal_value "$journal" staged_env)"
  rollback_env="$(journal_value "$journal" rollback_env)"
  env_had_live="$(journal_value "$journal" env_had_live)"
  network_created="$(journal_value "$journal" network_created)"
  journal_network="$(journal_value "$journal" runner_network)"

  if [ "$format" != 1 ] || [ -z "$transaction_id" ] ||
     [ "$journal_container" != "$CONTAINER" ] || [ "$old_name" != "$CONTAINER" ] ||
     [ "$journal_env" != "$ENV_FILE" ] || [ "$journal_network" != "$RUNNER_NETWORK" ]; then
    echo "Bootstrap transaction journal does not match this installation invocation; refusing recovery." >&2
    return 1
  fi
  case "$phase" in
    prepared|old_stopping|old_stopped|old_renaming|old_renamed|candidate_starting|candidate_started|candidate_healthy|env_committing|committed) ;;
    *) echo "Invalid bootstrap transaction phase: $phase" >&2; return 1 ;;
  esac
  case "$old_present:$old_was_running:$env_had_live:$network_created" in
    [01]:[01]:[01]:[01]) ;;
    *) echo "Invalid bootstrap transaction flags." >&2; return 1 ;;
  esac
  case "$rollback_name" in
    "$CONTAINER"-bootstrap-rollback-*) ;;
    *) echo "Invalid rollback container name in bootstrap journal." >&2; return 1 ;;
  esac
  if ! validate_transaction_path "$staged_env" "$ENV_FILE.candidate" ||
     ! validate_transaction_path "$rollback_env" "$ENV_FILE.rollback"; then
    echo "Invalid environment snapshot path in bootstrap journal." >&2
    return 1
  fi
  if [ "$old_present" -eq 1 ] && [ -z "$old_id" ]; then
    echo "Bootstrap journal is missing the previous controller identity." >&2
    return 1
  fi

  echo "==> Recovering interrupted bootstrap transaction $transaction_id ($phase)"
  if [ "$phase" = committed ]; then
    canonical_id="$(docker_container_id "$CONTAINER")"
    canonical_role="$(docker_container_role "$CONTAINER")"
    canonical_transaction="$(docker_container_transaction "$CONTAINER")"
    if [ -z "$candidate_id" ] || [ "$canonical_id" != "$candidate_id" ] ||
       [ "$canonical_role" != control ] || [ "$canonical_transaction" != "$transaction_id" ]; then
      echo "RECOVERY ERROR: committed candidate identity does not match the journal." >&2
      return 1
    fi
    if ! docker_container_running "$candidate_id"; then
      if ! docker start "$candidate_id" >/dev/null 2>&1; then
        echo "RECOVERY ERROR: committed candidate $candidate_id could not be started." >&2
        return 1
      fi
    fi
    if ! wait_for_http "http://127.0.0.1:${HOST_PORT}/api/health" http; then
      echo "RECOVERY ERROR: committed candidate $candidate_id is not healthy." >&2
      return 1
    fi
    if [ "$old_present" -eq 1 ] && docker inspect "$old_id" >/dev/null 2>&1; then
      old_role="$(docker_container_role "$old_id")"
      if [ "$old_role" != control ]; then
        echo "RECOVERY ERROR: previous controller $old_id is not owned by devops-control." >&2
        return 1
      fi
      if docker_container_running "$old_id"; then
        docker stop "$old_id" >/dev/null 2>&1 || failed=1
      fi
      if [ "$failed" -eq 0 ]; then
        docker rm "$old_id" >/dev/null 2>&1 || failed=1
      fi
    fi
    if [ "$failed" -ne 0 ]; then
      echo "RECOVERY ERROR: could not finalize removal of previous controller $old_id." >&2
      return 1
    fi
  else
    if [ "$phase" = env_committing ]; then
      if ! restore_previous_environment "$journal_env" "$rollback_env" "$env_had_live"; then
        echo "RECOVERY ERROR: could not restore the previous environment." >&2
        failed=1
      fi
    fi

    canonical_id="$(docker_container_id "$CONTAINER")"
    if [ -n "$canonical_id" ] && { [ "$old_present" -eq 0 ] || [ "$canonical_id" != "$old_id" ]; }; then
      canonical_role="$(docker_container_role "$CONTAINER")"
      canonical_transaction="$(docker_container_transaction "$CONTAINER")"
      if [ "$canonical_role" != control ] ||
         { [ -n "$candidate_id" ] && [ "$canonical_id" != "$candidate_id" ]; } ||
         { [ -z "$candidate_id" ] && [ "$canonical_transaction" != "$transaction_id" ]; }; then
        echo "RECOVERY ERROR: canonical container is not the interrupted candidate; refusing removal." >&2
        failed=1
      else
        if docker_container_running "$canonical_id"; then
          docker stop "$canonical_id" >/dev/null 2>&1 || failed=1
        fi
        if [ "$failed" -eq 0 ]; then
          docker rm "$canonical_id" >/dev/null 2>&1 || failed=1
        fi
      fi
    fi

    if [ "$old_present" -eq 1 ]; then
      if ! docker inspect "$old_id" >/dev/null 2>&1; then
        echo "RECOVERY ERROR: captured previous controller $old_id no longer exists." >&2
        failed=1
      elif [ "$(docker_container_role "$old_id")" != control ]; then
        echo "RECOVERY ERROR: captured previous controller $old_id is not owned by devops-control." >&2
        failed=1
      elif [ "$(docker_container_id "$CONTAINER")" != "$old_id" ]; then
        if docker inspect "$CONTAINER" >/dev/null 2>&1; then
          echo "RECOVERY ERROR: canonical container name remains occupied." >&2
          failed=1
        elif ! docker rename "$old_id" "$CONTAINER" >/dev/null 2>&1; then
          echo "RECOVERY ERROR: could not restore previous controller $old_id to $CONTAINER." >&2
          failed=1
        fi
      fi
      if [ "$failed" -eq 0 ] && [ "$old_was_running" -eq 1 ]; then
        if ! docker_container_running "$old_id" &&
           ! docker start "$old_id" >/dev/null 2>&1; then
          echo "RECOVERY ERROR: could not restart previous controller $old_id." >&2
          failed=1
        elif ! wait_for_http "http://127.0.0.1:${HOST_PORT}/api/health" http; then
          echo "RECOVERY ERROR: restored previous controller $old_id is not healthy." >&2
          failed=1
        fi
      fi
    fi

    if [ "$network_created" -eq 1 ] &&
       docker network inspect "$journal_network" >/dev/null 2>&1; then
      if [ "$(docker network inspect -f '{{index .Labels "com.sauraku.devops.role"}}' "$journal_network" 2>/dev/null || true)" != runner-network ] ||
         ! docker network rm "$journal_network" >/dev/null 2>&1; then
        echo "RECOVERY ERROR: could not remove transaction-created runner network $journal_network." >&2
        failed=1
      fi
    fi
    if [ "$failed" -ne 0 ]; then
      return 1
    fi
  fi

  [ -z "$staged_env" ] || rm -f -- "$staged_env"
  [ -z "$rollback_env" ] || rm -f -- "$rollback_env"
  clear_transaction_journal
  echo "==> Interrupted bootstrap transaction reconciled"
}

TEMP_DOCKER_CONFIG=""
STAGED_ENV=""
ROLLBACK_ENV=""
ENV_HAD_LIVE=0
ENV_COMMITTED=0
HAD_OLD_CONTAINER=0
OLD_CONTAINER_WAS_RUNNING=0
OLD_CONTAINER_RENAMED=0
OLD_CONTAINER_ID=""
OLD_CONTAINER_NAME="${CONTAINER}-bootstrap-rollback-$$"
CANDIDATE_CONTAINER_ID=""
TRANSACTION_ID=""
TRANSACTION_PHASE=""
TRANSACTION_JOURNAL="$DATA_DIR/Run/bootstrap-transaction"
BOOTSTRAP_LOCK_DIR="$DATA_DIR/Run/bootstrap.lock"
BOOTSTRAP_LOCK_TOKEN="$$-$(date +%s)-${RANDOM:-0}"
BOOTSTRAP_LOCK_HELD=0
PRESERVE_TRANSACTION_JOURNAL=0
ROLLOUT_STARTED=0
NETWORK_CREATED=0
TRANSACTION_SUCCEEDED=0

cleanup_bootstrap_files() {
  if [ "$PRESERVE_TRANSACTION_JOURNAL" -ne 1 ]; then
    [ -z "$STAGED_ENV" ] || rm -f -- "$STAGED_ENV"
    [ -z "$ROLLBACK_ENV" ] || rm -f -- "$ROLLBACK_ENV"
  fi
  if [ -n "$TEMP_DOCKER_CONFIG" ] && [ -d "$TEMP_DOCKER_CONFIG" ]; then
    rm -rf -- "$TEMP_DOCKER_CONFIG"
  fi
}

rollback_on_exit() {
  local status=$? canonical_owner="" canonical_id="" rollback_id=""
  local rollback_container_present=0 rollback_failed=0 restored_old=0 previous_current_name=""
  trap - EXIT HUP INT TERM
  set +e

  if [ "$TRANSACTION_SUCCEEDED" -ne 1 ]; then
    if [ "$ENV_COMMITTED" -eq 1 ]; then
      if [ "$ENV_HAD_LIVE" -eq 1 ] && [ -n "$ROLLBACK_ENV" ] && [ -f "$ROLLBACK_ENV" ]; then
        if ! restore_previous_environment "$ENV_FILE" "$ROLLBACK_ENV" "$ENV_HAD_LIVE"; then
          echo "ROLLBACK ERROR: could not restore the previous environment file $ENV_FILE." >&2
          rollback_failed=1
        fi
      else
        if ! rm -f -- "$ENV_FILE"; then
          echo "ROLLBACK ERROR: could not remove the uncommitted environment file $ENV_FILE." >&2
          rollback_failed=1
        fi
      fi
    fi

    if [ "$ROLLOUT_STARTED" -eq 1 ]; then
      if docker inspect "$OLD_CONTAINER_NAME" >/dev/null 2>&1; then
        rollback_container_present=1
        rollback_id="$(docker inspect -f '{{.Id}}' "$OLD_CONTAINER_NAME" 2>/dev/null)"
        if [ -z "$OLD_CONTAINER_ID" ] || [ "$rollback_id" != "$OLD_CONTAINER_ID" ]; then
          echo "ROLLBACK ERROR: $OLD_CONTAINER_NAME is not the captured previous controller; refusing to start it." >&2
          rollback_failed=1
          rollback_container_present=0
        else
          previous_current_name="$OLD_CONTAINER_NAME"
        fi
      fi

      if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        canonical_id="$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
        if [ -n "$OLD_CONTAINER_ID" ] && [ "$canonical_id" = "$OLD_CONTAINER_ID" ]; then
          restored_old=1
          previous_current_name="$CONTAINER"
        elif [ "$rollback_container_present" -eq 1 ] || [ "$HAD_OLD_CONTAINER" -eq 0 ]; then
          canonical_owner="$(docker inspect -f '{{index .Config.Labels "com.sauraku.devops.role"}}' "$CONTAINER" 2>/dev/null)"
          if [ "$canonical_owner" != control ]; then
            echo "ROLLBACK ERROR: refusing to remove unowned canonical container $CONTAINER." >&2
            rollback_failed=1
          else
            if ! docker stop "$canonical_id" >/dev/null 2>&1; then
              echo "ROLLBACK ERROR: could not stop unhealthy candidate $CONTAINER ($canonical_id)." >&2
              rollback_failed=1
            fi
            if ! docker rm "$canonical_id" >/dev/null 2>&1; then
              echo "ROLLBACK ERROR: could not remove unhealthy candidate $CONTAINER ($canonical_id)." >&2
              rollback_failed=1
            fi
          fi
        fi
      fi

      if [ "$rollback_container_present" -eq 1 ]; then
        if docker inspect "$CONTAINER" >/dev/null 2>&1; then
          canonical_id="$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
          if [ "$canonical_id" = "$OLD_CONTAINER_ID" ]; then
            restored_old=1
            previous_current_name="$CONTAINER"
          else
            echo "ROLLBACK ERROR: canonical name $CONTAINER is still occupied; previous controller remains $OLD_CONTAINER_NAME." >&2
            rollback_failed=1
          fi
        elif docker rename "$OLD_CONTAINER_ID" "$CONTAINER" >/dev/null 2>&1; then
          canonical_id="$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
          if [ "$canonical_id" = "$OLD_CONTAINER_ID" ]; then
            restored_old=1
            previous_current_name="$CONTAINER"
            rollback_container_present=0
          else
            echo "ROLLBACK ERROR: renamed container identity did not match captured previous controller." >&2
            rollback_failed=1
          fi
        else
          echo "ROLLBACK ERROR: could not restore $OLD_CONTAINER_NAME to canonical name $CONTAINER." >&2
          rollback_failed=1
        fi
      elif [ "$HAD_OLD_CONTAINER" -eq 1 ] && docker inspect "$CONTAINER" >/dev/null 2>&1; then
        canonical_id="$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null)"
        if [ "$canonical_id" = "$OLD_CONTAINER_ID" ]; then
          restored_old=1
          previous_current_name="$CONTAINER"
        else
          echo "ROLLBACK ERROR: canonical container is not the captured previous controller." >&2
          rollback_failed=1
        fi
      fi

      if [ "$restored_old" -eq 1 ] && [ "$OLD_CONTAINER_WAS_RUNNING" -eq 1 ]; then
        if ! docker start "$OLD_CONTAINER_ID" >/dev/null 2>&1; then
          echo "ROLLBACK ERROR: previous controller $CONTAINER ($OLD_CONTAINER_ID) could not be started." >&2
          rollback_failed=1
        elif ! wait_for_http "http://127.0.0.1:${HOST_PORT}/api/health" http; then
          echo "ROLLBACK ERROR: restored previous controller $CONTAINER ($OLD_CONTAINER_ID) failed its health check." >&2
          rollback_failed=1
        fi
      fi
    fi

    if [ "$NETWORK_CREATED" -eq 1 ]; then
      if ! docker network rm "$RUNNER_NETWORK" >/dev/null 2>&1; then
        echo "ROLLBACK ERROR: could not remove newly created runner network $RUNNER_NETWORK." >&2
        rollback_failed=1
      fi
    fi
  fi

  if [ "$rollback_failed" -eq 1 ]; then
    echo "ROLLBACK FAILED: automatic recovery was incomplete." >&2
    if [ "$previous_current_name" = "$OLD_CONTAINER_NAME" ]; then
      echo "Known previous controller: $OLD_CONTAINER_NAME ($OLD_CONTAINER_ID), left stopped." >&2
      echo "Recovery: docker stop $CONTAINER && docker rm $CONTAINER && docker rename $OLD_CONTAINER_ID $CONTAINER && docker start $OLD_CONTAINER_ID" >&2
    elif [ "$previous_current_name" = "$CONTAINER" ]; then
      echo "Previous controller: $CONTAINER ($OLD_CONTAINER_ID)." >&2
      echo "Recovery: docker start $OLD_CONTAINER_ID && curl --fail http://127.0.0.1:${HOST_PORT}/api/health" >&2
    elif [ -n "$OLD_CONTAINER_ID" ]; then
      echo "Expected previous controller ID: $OLD_CONTAINER_ID. Verify its current name before starting it." >&2
    fi
    status=70
    PRESERVE_TRANSACTION_JOURNAL=1
  elif [ "$TRANSACTION_SUCCEEDED" -ne 1 ] &&
       [ "$PRESERVE_TRANSACTION_JOURNAL" -ne 1 ]; then
    clear_transaction_journal
  fi

  cleanup_bootstrap_files
  release_install_lock || true
  exit "$status"
}

trap rollback_on_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if acquire_install_lock; then
  :
else
  lock_status=$?
  exit "$lock_status"
fi
if ! recover_unfinished_transaction; then
  echo "RECOVERY FAILED: the unfinished bootstrap transaction was retained for a safe retry." >&2
  PRESERVE_TRANSACTION_JOURNAL=1
  exit 70
fi

# A supplied PAT wins. Otherwise an existing persisted controller PAT may
# authenticate this rollout even when the requested env mutation is omission
# or explicit clearing. Either credential is confined to a disposable Docker
# config and removed from the shell before any further external command.
BOOTSTRAP_GITHUB_TOKEN="$SUPPLIED_GITHUB_TOKEN"
if [ -z "$BOOTSTRAP_GITHUB_TOKEN" ] && [ -f "$ENV_FILE" ]; then
  BOOTSTRAP_GITHUB_TOKEN="$(dotenv_value "$ENV_FILE" GITHUB_TOKEN)"
fi
case "$BOOTSTRAP_GITHUB_TOKEN" in
  *$'\n'*|*$'\r'*) echo "Persisted GITHUB_TOKEN must be a single-line value." >&2; exit 2 ;;
esac
if [ -n "$BOOTSTRAP_GITHUB_TOKEN" ]; then
  TEMP_DOCKER_CONFIG="$(mktemp -d "${TMPDIR:-/tmp}/devops-bootstrap-docker.XXXXXX")"
  chmod 0700 "$TEMP_DOCKER_CONFIG"
  export DOCKER_CONFIG="$TEMP_DOCKER_CONFIG"
  echo "==> Logging in to ghcr.io with an isolated bootstrap credential..."
  printf '%s' "$BOOTSTRAP_GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
fi
unset BOOTSTRAP_GITHUB_TOKEN

echo "==> Pulling image: $IMAGE"
docker pull "$IMAGE"
docker pull "$RUNNER_IMAGE"

# Pin the entire rollout, including validation, to the pulled content.
IMAGE="$(resolve_immutable_image "$IMAGE")"
RUNNER_IMAGE="$(resolve_immutable_image "$RUNNER_IMAGE")"

STAGED_ENV="$(mktemp "${ENV_FILE}.candidate.XXXXXX")"
chmod 0600 "$STAGED_ENV"
if [ -f "$ENV_FILE" ]; then
  ENV_HAD_LIVE=1
  ROLLBACK_ENV="$(mktemp "${ENV_FILE}.rollback.XXXXXX")"
  cp -- "$ENV_FILE" "$STAGED_ENV"
  cp -- "$ENV_FILE" "$ROLLBACK_ENV"
  chmod 0600 "$STAGED_ENV" "$ROLLBACK_ENV"
else
  echo "==> Preparing a new environment with random secrets"
  cat > "$STAGED_ENV" <<EOF
# Auto-generated by deploy/bootstrap.sh
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
  chmod 0600 "$STAGED_ENV"
fi

ensure_dotenv_value "$STAGED_ENV" BASE_DIR "$DATA_DIR"
ensure_dotenv_value "$STAGED_ENV" RUNNER_NETWORK "$RUNNER_NETWORK"
ensure_dotenv_value "$STAGED_ENV" RUNNER_CONTROL_URL "$RUNNER_CONTROL_URL"
ensure_dotenv_value "$STAGED_ENV" GITHUB_USER "$GITHUB_USER"
set_dotenv_value "$STAGED_ENV" RUNNER_IMAGE "$RUNNER_IMAGE"

if [ -n "$GITHUB_TOKEN_WAS_SET" ]; then
  if [ -n "$SUPPLIED_GITHUB_TOKEN" ]; then
    echo "==> Staging supplied controller GITHUB_TOKEN"
    set_dotenv_value "$STAGED_ENV" GITHUB_TOKEN "$SUPPLIED_GITHUB_TOKEN"
  else
    echo "==> Clearing the persisted controller GITHUB_TOKEN (host Docker auth is unchanged)"
    unset_dotenv_value "$STAGED_ENV" GITHUB_TOKEN
  fi
fi
unset SUPPLIED_GITHUB_TOKEN
if [ -n "$TRUSTED_PROXY_CIDRS_WAS_SET" ]; then
  if [ -n "${TRUSTED_PROXY_CIDRS:-}" ]; then
    echo "==> Staging supplied trusted-proxy configuration"
    set_dotenv_value "$STAGED_ENV" TRUSTED_PROXY_CIDRS "$TRUSTED_PROXY_CIDRS"
  else
    echo "==> Clearing persisted trusted-proxy configuration"
    unset_dotenv_value "$STAGED_ENV" TRUSTED_PROXY_CIDRS
  fi
fi
if [ -n "$DEPLOY_CONTROL_PUBLIC_URL_WAS_SET" ]; then
  if [ -n "${DEPLOY_CONTROL_PUBLIC_URL:-}" ]; then
    echo "==> Staging supplied public control-plane URL"
    set_dotenv_value "$STAGED_ENV" DEPLOY_CONTROL_PUBLIC_URL "$DEPLOY_CONTROL_PUBLIC_URL"
  else
    echo "==> Clearing persisted public control-plane URL"
    unset_dotenv_value "$STAGED_ENV" DEPLOY_CONTROL_PUBLIC_URL
  fi
fi

candidate_proxy_cidrs="$(dotenv_value "$STAGED_ENV" TRUSTED_PROXY_CIDRS)"
candidate_public_url="$(dotenv_value "$STAGED_ENV" DEPLOY_CONTROL_PUBLIC_URL)"
candidate_cookie_secure="$(dotenv_value "$STAGED_ENV" COOKIE_SECURE)"

if ! validate_trusted_proxy_cidrs "$candidate_proxy_cidrs"; then
  echo "TRUSTED_PROXY_CIDRS contains an invalid IP address or CIDR." >&2
  exit 2
fi

if [ -n "$candidate_public_url" ] && ! validate_public_url "$candidate_public_url"; then
  echo "DEPLOY_CONTROL_PUBLIC_URL must be an HTTPS origin without credentials, path, query, or fragment." >&2
  exit 2
fi
cookie_secure_enabled=0
candidate_cookie_secure="$(printf '%s' "$candidate_cookie_secure" | tr '[:upper:]' '[:lower:]')"
case "$candidate_cookie_secure" in
  1|true) cookie_secure_enabled=1 ;;
esac
if [ "$cookie_secure_enabled" -eq 1 ]; then
  if [ -z "$candidate_proxy_cidrs" ] || [ -z "$candidate_public_url" ]; then
    echo "COOKIE_SECURE=true requires TRUSTED_PROXY_CIDRS and DEPLOY_CONTROL_PUBLIC_URL=https://..." >&2
    exit 2
  fi
fi
if [ -n "$candidate_public_url" ] && ! preflight_https_transport "$candidate_public_url"; then
  echo "Could not establish verified TLS transport to DEPLOY_CONTROL_PUBLIC_URL." >&2
  exit 1
fi

if docker network inspect "$RUNNER_NETWORK" >/dev/null 2>&1; then
  network_owner="$(docker network inspect -f '{{index .Labels "com.sauraku.devops.role"}}' "$RUNNER_NETWORK" 2>/dev/null || true)"
  if [ "$network_owner" != runner-network ]; then
    echo "Refusing to use unowned Docker network named $RUNNER_NETWORK." >&2
    exit 1
  fi
else
  docker network create --label com.sauraku.devops.role=runner-network "$RUNNER_NETWORK" >/dev/null
  NETWORK_CREATED=1
fi

# Complete host and ownership preflight before the running controller is
# touched. A rollback name collision is never overwritten.
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
DOCKER_GID="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
if [ -z "$DOCKER_GID" ]; then
  DOCKER_GID="$(getent group docker 2>/dev/null | cut -d: -f3 || true)"
fi
if [ -z "$DOCKER_GID" ]; then
  echo "Could not determine the Docker socket group ID." >&2
  exit 1
fi
if docker inspect "$OLD_CONTAINER_NAME" >/dev/null 2>&1; then
  echo "Refusing to overwrite rollback container $OLD_CONTAINER_NAME." >&2
  exit 1
fi
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  HAD_OLD_CONTAINER=1
  owner_label="$(docker inspect -f '{{index .Config.Labels "com.sauraku.devops.role"}}' "$CONTAINER" 2>/dev/null || true)"
  if [ "$owner_label" != control ]; then
    echo "Refusing to replace unowned container named $CONTAINER." >&2
    exit 1
  fi
  OLD_CONTAINER_ID="$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null || true)"
  if [ -z "$OLD_CONTAINER_ID" ]; then
    echo "Could not capture the existing controller identity." >&2
    exit 1
  fi
  if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || true)" = true ]; then
    OLD_CONTAINER_WAS_RUNNING=1
  fi
fi

TRANSACTION_ID="$(date +%s)-$$-${RANDOM:-0}"
if ! write_transaction_journal prepared; then
  echo "Could not create the durable bootstrap transaction journal." >&2
  exit 1
fi

ROLLOUT_STARTED=1
if [ "$HAD_OLD_CONTAINER" -eq 1 ]; then
  echo "==> Preserving existing controller for rollback"
  if [ "$OLD_CONTAINER_WAS_RUNNING" -eq 1 ]; then
    write_transaction_journal old_stopping
    docker stop "$CONTAINER" >/dev/null
    write_transaction_journal old_stopped
  fi
  write_transaction_journal old_renaming
  docker rename "$CONTAINER" "$OLD_CONTAINER_NAME"
  OLD_CONTAINER_RENAMED=1
  write_transaction_journal old_renamed
fi

echo "==> Starting candidate devops-control"
write_transaction_journal candidate_starting
docker run -d \
  --name "$CONTAINER" \
  --restart unless-stopped \
  --pull never \
  --label com.sauraku.devops.role=control \
  --label "com.sauraku.devops.bootstrap-transaction=$TRANSACTION_ID" \
  --user "${HOST_UID}:${HOST_GID}" \
  --group-add "$DOCKER_GID" \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=128m,mode=1777 \
  --network "$RUNNER_NETWORK" \
  -e "BASE_DIR=$DATA_DIR" \
  -e "GITHUB_USER=$GITHUB_USER" \
  -e DOCKER_CONFIG=/tmp/docker-config \
  -e "RUNNER_NETWORK=$RUNNER_NETWORK" \
  -e "RUNNER_CONTROL_URL=$RUNNER_CONTROL_URL" \
  --env-file "$STAGED_ENV" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DATA_DIR":"$DATA_DIR" \
  -p "${HOST_BIND}:${HOST_PORT}:8787" \
  "$IMAGE" >/dev/null

CANDIDATE_CONTAINER_ID="$(docker_container_id "$CONTAINER")"
if [ -z "$CANDIDATE_CONTAINER_ID" ] ||
   [ "$(docker_container_transaction "$CONTAINER")" != "$TRANSACTION_ID" ]; then
  echo "Could not capture the candidate controller identity." >&2
  exit 1
fi
write_transaction_journal candidate_started

if ! wait_for_http "http://127.0.0.1:${HOST_PORT}/api/health" http; then
  echo "Candidate controller did not become healthy; restoring the previous controller." >&2
  exit 1
fi

if [ -n "$candidate_public_url" ]; then
  public_origin="${candidate_public_url%/}"
  if ! wait_for_http "${public_origin}/api/health" https; then
    echo "Public HTTPS health check failed; restoring the previous controller." >&2
    exit 1
  fi
  if ! wait_for_http "${public_origin}/login" https; then
    echo "Public HTTPS login check failed; restoring the previous controller." >&2
    exit 1
  fi
fi

write_transaction_journal candidate_healthy

# Commit the environment only after the candidate is reachable through every
# configured path. The preserved controller is removed only after that rename.
ENV_COMMITTED=1
write_transaction_journal env_committing
if ! mv -f -- "$STAGED_ENV" "$ENV_FILE"; then
  echo "Could not commit the candidate environment; restoring the previous controller." >&2
  exit 1
fi
durable_sync_path "$ENV_FILE"
durable_sync_path "$(dirname "$ENV_FILE")"
write_transaction_journal committed
TRANSACTION_SUCCEEDED=1
STAGED_ENV=""

finalization_pending=0
if [ "$OLD_CONTAINER_RENAMED" -eq 1 ]; then
  final_rollback_id="$(docker inspect -f '{{.Id}}' "$OLD_CONTAINER_NAME" 2>/dev/null || true)"
  if [ "$final_rollback_id" != "$OLD_CONTAINER_ID" ]; then
    echo "Warning: rollback container identity changed; refusing to remove $OLD_CONTAINER_NAME" >&2
    finalization_pending=1
  elif ! docker rm "$OLD_CONTAINER_ID" >/dev/null; then
    echo "Warning: the stopped rollback container could not be removed: $OLD_CONTAINER_NAME" >&2
    finalization_pending=1
  fi
fi
if [ "$finalization_pending" -eq 0 ]; then
  clear_transaction_journal
  [ -z "$ROLLBACK_ENV" ] || rm -f -- "$ROLLBACK_ENV"
  ROLLBACK_ENV=""
else
  echo "Warning: durable bootstrap journal retained so the next run can finish cleanup." >&2
fi

echo "==> Done. Container: $CONTAINER"
echo "    Logs: docker logs -f $CONTAINER"
echo "    Token: saved in $ENV_FILE"
echo "    Control plane bind: ${HOST_BIND}:${HOST_PORT}"
echo "    Browser URL: $candidate_public_url"
echo "    To update later, run the same command again."
