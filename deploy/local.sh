#!/usr/bin/env bash
# Local development lifecycle manager. Start rebuilds the binary, UI, and runner;
# status and stop never build or rewrite configuration.
# For production, use ./start.sh (requires pre-built binary + .env.prod).
set -euo pipefail
umask 077

DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$DIR"
# shellcheck source=lib.sh
source "$DIR/deploy/lib.sh"

# ── Gather what we need ──────────────────────────────────────────────
ENV_FILE="$DIR/.env.local"
if [ -f "$ENV_FILE" ]; then
  load_dotenv "$ENV_FILE"
fi

COMMAND="${1:-start}"
if [ "$#" -gt 1 ]; then
	echo "Usage: $0 [start|stop|status]" >&2
	exit 2
fi

if [ -z "${BASE_DIR:-}" ]; then
	DEFAULT_DIR="$DIR/data"
	if [ "$COMMAND" = start ] && [ -t 0 ]; then
		read -rp "Project base directory [${DEFAULT_DIR}]: " BASE_DIR
	fi
	BASE_DIR="${BASE_DIR:-$DEFAULT_DIR}"
	export BASE_DIR
fi

BINARY="$DIR/devops-control"
CONTROL_PORT="${DEPLOY_CONTROL_PORT:-8787}"
if ! [[ "$CONTROL_PORT" =~ ^[0-9]{1,5}$ ]] || (( 10#$CONTROL_PORT < 1 || 10#$CONTROL_PORT > 65535 )); then
	echo "DEPLOY_CONTROL_PORT must be an integer between 1 and 65535." >&2
	exit 2
fi
LIFECYCLE_BASE_DIR="${BASE_DIR:-$DIR/data}"
LIFECYCLE_PID_FILE="$LIFECYCLE_BASE_DIR/Run/devops-control.pid"
LIFECYCLE_LOCK_DIR="$LIFECYCLE_BASE_DIR/Run/local-lifecycle.lock"
LIFECYCLE_LOCK_HELD=false
STARTING_PID=""
STARTING_PID_FILE_TMP=""

process_is_owned() {
	local pid="$1"
	local command
	command="$(ps -p "$pid" -o command= 2>/dev/null || true)"
	[[ "$command" == "$BINARY" || "$command" == "$BINARY "* ]]
}

process_owns_port() {
	local pid="$1"
	lsof -a -p "$pid" -iTCP:"$CONTROL_PORT" -sTCP:LISTEN -t 2>/dev/null | grep -qx "$pid"
}

port_is_in_use() {
	if command -v lsof >/dev/null 2>&1; then
		lsof -tiTCP:"$CONTROL_PORT" -sTCP:LISTEN >/dev/null 2>&1
		return
	fi
	( : > "/dev/tcp/127.0.0.1/$CONTROL_PORT" ) >/dev/null 2>&1
}

health_is_ready() {
	curl --silent --fail --connect-timeout 1 --max-time 2 --output /dev/null \
		"http://127.0.0.1:${CONTROL_PORT}/api/health"
}

remove_pid_file_if_matches() {
	local expected_pid="$1"
	local recorded_pid
	[ -f "$LIFECYCLE_PID_FILE" ] || return 0
	recorded_pid="$(sed -n '1p' "$LIFECYCLE_PID_FILE")"
	if [ "$recorded_pid" = "$expected_pid" ]; then
		rm -f "$LIFECYCLE_PID_FILE"
	fi
}

wait_for_exit() {
	local pid="$1"
	local _
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.5
	done
	! kill -0 "$pid" 2>/dev/null
}

stop_recorded_process() {
	local context="$1"
	local pid

	if [ ! -f "$LIFECYCLE_PID_FILE" ]; then
		return 2
	fi
	pid="$(sed -n '1p' "$LIFECYCLE_PID_FILE")"
	if ! [[ "$pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pid" 2>/dev/null; then
		rm -f "$LIFECYCLE_PID_FILE"
		return 2
	fi
	if ! process_is_owned "$pid"; then
		echo "PID $pid is not the DevOps Control binary; refusing to stop it." >&2
		return 1
	fi

	echo "$context (pid=$pid)..."
	if ! kill "$pid" 2>/dev/null && kill -0 "$pid" 2>/dev/null; then
		echo "Could not signal DevOps Control (pid=$pid)." >&2
		return 1
	fi
	if ! wait_for_exit "$pid"; then
		echo "DevOps Control did not stop within 5 seconds (pid=$pid)." >&2
		return 1
	fi
	remove_pid_file_if_matches "$pid"
}

release_lifecycle_lock() {
	if [ "$LIFECYCLE_LOCK_HELD" = true ] && [ -d "$LIFECYCLE_LOCK_DIR" ]; then
		if [ "$(sed -n '1p' "$LIFECYCLE_LOCK_DIR/pid" 2>/dev/null || true)" = "$$" ]; then
			rm -f "$LIFECYCLE_LOCK_DIR/pid"
			rmdir "$LIFECYCLE_LOCK_DIR" 2>/dev/null || true
		fi
	fi
	LIFECYCLE_LOCK_HELD=false
}

acquire_lifecycle_lock() {
	local lock_pid
	mkdir -p "$LIFECYCLE_BASE_DIR/Run"
	chmod 0750 "$LIFECYCLE_BASE_DIR/Run"
	if ! mkdir "$LIFECYCLE_LOCK_DIR" 2>/dev/null; then
		if [ -L "$LIFECYCLE_LOCK_DIR" ] || [ ! -d "$LIFECYCLE_LOCK_DIR" ]; then
			echo "Refusing unsafe lifecycle lock path: $LIFECYCLE_LOCK_DIR" >&2
			return 1
		fi
		lock_pid="$(sed -n '1p' "$LIFECYCLE_LOCK_DIR/pid" 2>/dev/null || true)"
		if [[ "$lock_pid" =~ ^[0-9]+$ ]] && kill -0 "$lock_pid" 2>/dev/null; then
			echo "Another local lifecycle operation is running (pid=$lock_pid)." >&2
			return 1
		fi
		rm -f "$LIFECYCLE_LOCK_DIR/pid"
		if ! rmdir "$LIFECYCLE_LOCK_DIR" 2>/dev/null || ! mkdir "$LIFECYCLE_LOCK_DIR" 2>/dev/null; then
			echo "Could not recover stale lifecycle lock: $LIFECYCLE_LOCK_DIR" >&2
			return 1
		fi
	fi
	printf '%s\n' "$$" > "$LIFECYCLE_LOCK_DIR/pid"
	LIFECYCLE_LOCK_HELD=true
}

cleanup_on_exit() {
	local exit_code="$?"
	trap - EXIT
	if [ -n "$STARTING_PID" ]; then
		if kill -0 "$STARTING_PID" 2>/dev/null && process_is_owned "$STARTING_PID"; then
			kill "$STARTING_PID" 2>/dev/null || true
			wait_for_exit "$STARTING_PID" || true
		fi
		remove_pid_file_if_matches "$STARTING_PID"
	fi
	if [ -n "$STARTING_PID_FILE_TMP" ]; then
		rm -f "$STARTING_PID_FILE_TMP"
	fi
	release_lifecycle_lock
	exit "$exit_code"
}

trap cleanup_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

case "$COMMAND" in
	status)
		if [ ! -f "$LIFECYCLE_PID_FILE" ]; then
			if port_is_in_use; then
				echo "DevOps Control is not owned by this manager, but port $CONTROL_PORT is in use." >&2
				exit 1
			fi
			echo "DevOps Control is stopped."
			exit 0
		fi
		pid="$(sed -n '1p' "$LIFECYCLE_PID_FILE")"
		if ! [[ "$pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pid" 2>/dev/null; then
			echo "DevOps Control is stopped (stale PID file: $LIFECYCLE_PID_FILE)." >&2
			exit 1
		fi
		if ! process_is_owned "$pid"; then
			echo "PID $pid is not the DevOps Control binary; refusing to claim it." >&2
			exit 1
		fi
		if command -v lsof >/dev/null 2>&1 && ! process_owns_port "$pid"; then
			echo "DevOps Control process $pid is running but does not own port $CONTROL_PORT." >&2
			exit 1
		fi
		if health_is_ready; then
			echo "DevOps Control is running and healthy (pid=$pid, port=$CONTROL_PORT)."
			exit 0
		fi
		echo "DevOps Control is running but unhealthy (pid=$pid, port=$CONTROL_PORT)." >&2
		exit 1
		;;
	stop)
		acquire_lifecycle_lock
		stop_result=0
		stop_recorded_process "==> Stopping DevOps Control" || stop_result="$?"
		if [ "$stop_result" -eq 0 ]; then
			echo "DevOps Control stopped."
			exit 0
		fi
		if [ "$stop_result" -eq 2 ]; then
			echo "DevOps Control is already stopped."
			exit 0
		fi
		exit "$stop_result"
		;;
	start)
		acquire_lifecycle_lock
		if [ -f "$LIFECYCLE_PID_FILE" ]; then
			pid="$(sed -n '1p' "$LIFECYCLE_PID_FILE")"
			if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
				if ! process_is_owned "$pid"; then
					echo "PID $pid is not the DevOps Control binary; refusing to overwrite its PID file." >&2
					exit 1
				fi
			elif [[ "$pid" =~ ^[0-9]+$ ]]; then
				rm -f "$LIFECYCLE_PID_FILE"
			else
				echo "Invalid PID file: $LIFECYCLE_PID_FILE" >&2
				exit 1
			fi
		fi
		if port_is_in_use; then
			if [ ! -f "$LIFECYCLE_PID_FILE" ]; then
				echo "Port $CONTROL_PORT is already in use by an unowned process." >&2
				exit 1
			fi
			pid="$(sed -n '1p' "$LIFECYCLE_PID_FILE")"
			if command -v lsof >/dev/null 2>&1 && ! process_owns_port "$pid"; then
				echo "Port $CONTROL_PORT is already in use by a process other than managed PID $pid." >&2
				exit 1
			fi
		fi
		;;
	*)
		echo "Usage: $0 [start|stop|status]" >&2
		exit 2
		;;
esac

is_weak_secret() {
	local value="${1:-}"
	[ "${#value}" -lt 32 ] || [[ "$value" =~ change[-_]me|placeholder ]]
}

if is_weak_secret "${DEPLOY_CONTROL_TOKEN:-}"; then
	if [ -t 0 ]; then
		read -rsp "Enter a deploy control token [generate]: " DEPLOY_CONTROL_TOKEN
		echo
	fi
	if is_weak_secret "${DEPLOY_CONTROL_TOKEN:-}"; then
		DEPLOY_CONTROL_TOKEN="$(openssl rand -hex 32)"
		echo "==> Generated a deploy control token; it will be stored in .env.local (mode 0600)."
	fi
  export DEPLOY_CONTROL_TOKEN
fi

if is_weak_secret "${COOKIE_SECRET:-}"; then
  COOKIE_SECRET="$(openssl rand -hex 32)"
  export COOKIE_SECRET
fi

if is_weak_secret "${ENCRYPTION_KEY:-}"; then
  ENCRYPTION_KEY="$(openssl rand -hex 32)"
  export ENCRYPTION_KEY
fi

RUNNER_NETWORK="${RUNNER_NETWORK:-devops-control-runners}"
RUNNER_CONTROL_URL="${RUNNER_CONTROL_URL:-http://host.docker.internal:${CONTROL_PORT}}"
RUNNER_IMAGE="devops-control-runner:local"
DEPLOY_CONTROL_HOST="${DEPLOY_CONTROL_HOST:-127.0.0.1}"
DEPLOY_CONTROL_PORT="$CONTROL_PORT"
export DEPLOY_CONTROL_HOST DEPLOY_CONTROL_PORT RUNNER_IMAGE RUNNER_NETWORK RUNNER_CONTROL_URL

# ── Persist secrets ────────────────────────────────────────────────────
cat > "$ENV_FILE" <<EOF
# Auto-generated by local.sh — do not commit
COOKIE_SECRET=$COOKIE_SECRET
ENCRYPTION_KEY=$ENCRYPTION_KEY
DEPLOY_CONTROL_TOKEN=$DEPLOY_CONTROL_TOKEN
DEPLOY_CONTROL_HOST=$DEPLOY_CONTROL_HOST
DEPLOY_CONTROL_PORT=$DEPLOY_CONTROL_PORT
ENV_NAME=${ENV_NAME:-dev}
BASE_DIR=$BASE_DIR
RUNNER_IMAGE=devops-control-runner:local
RUNNER_NETWORK=$RUNNER_NETWORK
RUNNER_CONTROL_URL=$RUNNER_CONTROL_URL
EOF
if [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "GITHUB_TOKEN=$GITHUB_TOKEN" >> "$ENV_FILE"
fi
chmod 600 "$ENV_FILE"

# ── Build (always rebuild UI + Go) ───────────────────────────────────
echo "==> Building UI ..."
(cd "$DIR/ui" && npm ci --silent && npm run build)
echo "==> Building isolated runner image ..."
docker build -t devops-control-runner:local -f "$DIR/deploy/runner/Dockerfile.runner" "$DIR/deploy/runner"
ensure_runner_network "$RUNNER_NETWORK"
echo "==> Building Go binary ..."
go build -o "$BINARY" ./cmd/devops-control/
echo "==> Build done."

# ── Start ────────────────────────────────────────────────────────────
echo ""

# Stop only the prior process recorded by this script. Keeping it alive through
# the build avoids downtime when compilation fails.
stop_result=0
stop_recorded_process "==> Stopping previous DevOps Control process" || stop_result="$?"
if [ "$stop_result" -eq 1 ]; then
	exit 1
fi
if port_is_in_use; then
  echo "!! Port $CONTROL_PORT is already in use; refusing to stop an unowned process." >&2
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$CONTROL_PORT" -sTCP:LISTEN >&2 || true
  fi
  exit 1
fi

echo "==> Starting DevOps Control in background..."
mkdir -p "$BASE_DIR/Logs"
CONTROL_LOG="$BASE_DIR/Logs/devops-control.log"
nohup "$BINARY" >> "$CONTROL_LOG" 2>&1 </dev/null &
BGPID=$!
STARTING_PID="$BGPID"
STARTING_PID_FILE_TMP="$LIFECYCLE_PID_FILE.$$"
printf '%s\n' "$BGPID" > "$STARTING_PID_FILE_TMP"
mv "$STARTING_PID_FILE_TMP" "$LIFECYCLE_PID_FILE"
STARTING_PID_FILE_TMP=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if health_is_ready; then
		break
	fi
	if ! kill -0 "$BGPID" 2>/dev/null; then
		echo "!! DevOps Control exited during startup. Recent log output:" >&2
		tail -n 40 "$CONTROL_LOG" >&2 || true
		exit 1
	fi
	sleep 0.5
done
if ! health_is_ready; then
	echo "!! DevOps Control did not become healthy. Recent log output:" >&2
	tail -n 40 "$CONTROL_LOG" >&2 || true
	exit 1
fi
STARTING_PID=""
echo "    URL:   http://localhost:${CONTROL_PORT}"
echo "    PID:   $BGPID"
echo "    Data:  $BASE_DIR"
echo "    Log:   $CONTROL_LOG"
echo ""
echo "    Stop with: ./deploy/local.sh stop"
