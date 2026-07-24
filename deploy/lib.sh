#!/usr/bin/env bash

# Parse dotenv files as data. Never execute their contents as shell code.
load_dotenv() {
  local file="$1" line key value
  [ -f "$file" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    case "$line" in
      ''|'#'*) continue ;;
      export\ *) line="${line#export }" ;;
    esac
    key="${line%%=*}"
    [ "$key" != "$line" ] || continue
    value="${line#*=}"
    if [[ ! "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
      echo "Invalid environment key in $file: $key" >&2
      return 1
    fi
    case "$key" in
      HOME|PATH|PWD|OLDPWD|SHLVL|SHELL|BASH_ENV|ENV|CDPATH|GLOBIGNORE|IFS|LD_*|DYLD_*|TMPDIR|*_PROXY|DOCKER_*|COMPOSE_*|BUILDKIT_*|BUILDX_*|PYTHON*|NODE_*)
        continue
        ;;
    esac
    if [[ "$value" == *$'\n'* ]]; then
      echo "Invalid multiline environment value for $key in $file" >&2
      return 1
    fi
    # A caller may intentionally protect controller-owned state with readonly.
    # Inspect attributes without expanding or logging the variable's value.
    local declaration
    if declaration="$(declare -p "$key" 2>/dev/null)" && [[ "$declaration" =~ ^declare\ -[^[:space:]]*r ]]; then
      continue
    fi
    export "$key=$value"
  done < "$file"
}

dotenv_value() {
  python3 - "$1" "$2" <<'PY'
import re
import sys

path, wanted = sys.argv[1:3]
key_re = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
with open(path, encoding="utf-8") as handle:
    for raw in handle:
        line = raw.rstrip("\r\n")
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key_re.fullmatch(key) and key == wanted:
            if "\x00" in value or "\n" in value or "\r" in value:
                raise SystemExit("invalid multiline dotenv value")
            print(value, end="")
            break
PY
}

select_compose_command() {
  local version
  if version="$(docker compose version --short 2>/dev/null)" &&
     [[ "${version}" =~ ^v?([2-9]|[1-9][0-9]+)\. ]]; then
    COMPOSE_CMD=(docker compose)
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1 &&
     version="$(docker-compose version --short 2>/dev/null)" &&
     [[ "${version}" =~ ^v?([2-9]|[1-9][0-9]+)\. ]]; then
    COMPOSE_CMD=(docker-compose)
    return 0
  fi
  echo "Docker Compose v2 is required (docker compose or docker-compose)." >&2
  return 1
}

run_with_timeout() {
  if [ "$#" -lt 2 ]; then
    echo "Usage: run_with_timeout <seconds> <command> [args...]" >&2
    return 2
  fi

  python3 -c '
import os
import math
import signal
import subprocess
import sys
import time

try:
    timeout = float(sys.argv[1])
except ValueError:
    print("timeout must be a positive number", file=sys.stderr)
    raise SystemExit(2)
if not math.isfinite(timeout) or timeout <= 0 or not sys.argv[2:]:
    print("timeout must be positive and a command is required", file=sys.stderr)
    raise SystemExit(2)

try:
    # Keep the command in the deployment process group. The controller owns
    # that group and can therefore abort every descendant even if this wrapper
    # is killed before it can forward a signal.
    process = subprocess.Popen(sys.argv[2:])
except OSError as error:
    print(f"failed to start {sys.argv[2]}: {error}", file=sys.stderr)
    raise SystemExit(127 if error.errno == 2 else 126)

forwarded_signal = None
forwarded_at = None
known_descendants = set()

def proc_process_table():
    children = {}
    states = {}
    try:
        entries = os.scandir("/proc")
    except OSError:
        return None, None
    with entries:
        for entry in entries:
            if not entry.name.isdigit():
                continue
            try:
                with open(
                    os.path.join("/proc", entry.name, "stat"),
                    encoding="utf-8",
                ) as stat_file:
                    stat = stat_file.read()
                # /proc/<pid>/stat wraps comm in parentheses and comm may
                # contain spaces. Fields after the final ")" begin with state
                # and parent PID.
                closing_parenthesis = stat.rfind(")")
                fields = stat[closing_parenthesis + 1:].split()
                if closing_parenthesis < 0 or len(fields) < 2:
                    continue
                pid = int(entry.name)
                state = fields[0]
                parent_pid = int(fields[1])
            except (OSError, ValueError):
                continue
            states[pid] = state
            children.setdefault(parent_pid, []).append(pid)
    return children, states

def ps_process_table():
    try:
        listing = subprocess.check_output(
            ["ps", "-eo", "pid=,ppid="],
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (OSError, subprocess.SubprocessError):
        return {}, {}
    children = {}
    states = {}
    for row in listing.splitlines():
        fields = row.split()
        if len(fields) != 2:
            continue
        try:
            pid, parent_pid = map(int, fields)
        except ValueError:
            continue
        children.setdefault(parent_pid, []).append(pid)
        states[pid] = "?"
    return children, states

def process_table():
    children, states = proc_process_table()
    if children is not None:
        return children, states
    return ps_process_table()

def descendants(root_pid):
    children, _states = process_table()
    found = []
    pending = list(children.get(root_pid, ()))
    while pending:
        pid = pending.pop()
        found.append(pid)
        pending.extend(children.get(pid, ()))
    return found

def process_is_active(pid):
    _children, states = proc_process_table()
    if states is not None:
        state = states.get(pid, "")
        return bool(state) and state != "Z"
    try:
        state = subprocess.check_output(
            ["ps", "-o", "stat=", "-p", str(pid)],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return False
    return bool(state) and not state.startswith("Z")

def signal_tree(signum):
    current_descendants = descendants(process.pid)
    known_descendants.update(current_descendants)
    # Signal leaves before their parents to prevent a departing leader from
    # orphaning a child before the child is recorded.
    for pid in reversed(current_descendants):
        try:
            os.kill(pid, signum)
        except ProcessLookupError:
            pass
    try:
        os.kill(process.pid, signum)
    except ProcessLookupError:
        pass

def forward_signal(signum, _frame):
    global forwarded_signal, forwarded_at
    if forwarded_signal is None:
        forwarded_signal = signum
        forwarded_at = time.monotonic()
        signal_tree(signum)

for handled_signal in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(handled_signal, forward_signal)

deadline = time.monotonic() + timeout
timed_out = False
termination_started_at = None

while True:
    return_code = process.poll()
    if return_code is not None and not (timed_out or forwarded_signal is not None):
        if timed_out:
            raise SystemExit(124)
        if forwarded_signal is not None:
            raise SystemExit(128 + forwarded_signal)
        raise SystemExit(return_code if return_code >= 0 else 128 - return_code)

    now = time.monotonic()
    if forwarded_signal is not None:
        termination_started_at = forwarded_at
    elif not timed_out and now >= deadline:
        timed_out = True
        termination_started_at = now
        signal_tree(signal.SIGTERM)

    if termination_started_at is not None:
        # Capture children created during shutdown while the leader still
        # exists, and do not report completion while any captured descendant
        # remains active.
        known_descendants.update(descendants(process.pid))
        descendants_active = any(process_is_active(pid) for pid in known_descendants)
        if return_code is not None and not descendants_active:
            if timed_out:
                raise SystemExit(124)
            raise SystemExit(128 + forwarded_signal)

    if termination_started_at is not None and now - termination_started_at >= 5:
        for pid in known_descendants:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        try:
            os.kill(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        if timed_out:
            raise SystemExit(124)
        raise SystemExit(128 + forwarded_signal)

    time.sleep(0.05)
' "$@"
}

ensure_runner_network() {
  local network="${1:?runner network name is required}" label
  if docker network inspect "$network" >/dev/null 2>&1; then
    label="$(docker network inspect -f '{{index .Labels "com.sauraku.devops.role"}}' "$network" 2>/dev/null || true)"
    if [ "$label" != "runner-network" ]; then
      echo "Refusing to use unowned Docker network named $network." >&2
      return 1
    fi
    return 0
  fi
  docker network create --label com.sauraku.devops.role=runner-network "$network" >/dev/null
}
