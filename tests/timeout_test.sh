#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

# shellcheck source=../deploy/lib.sh
source "${repo_root}/deploy/lib.sh"

process_is_active() {
  local state
  state="$(ps -o stat= -p "$1" 2>/dev/null | tr -d '[:space:]' || true)"
  [ -n "${state}" ] && [[ "${state}" != Z* ]]
}

output="$(run_with_timeout 2 sh -c 'printf success')"
[ "${output}" = "success" ]

if [ -r /proc/self/stat ]; then
  mkdir "${tmp_dir}/no-procps"
  cat >"${tmp_dir}/no-procps/ps" <<'EOF'
#!/usr/bin/env sh
echo "ps fallback was used on Linux" >&2
exit 99
EOF
  chmod 700 "${tmp_dir}/no-procps/ps"
  set +e
  PATH="${tmp_dir}/no-procps:${PATH}" run_with_timeout 0.1 sh -c 'sleep 30'
  status=$?
  set -e
  [ "${status}" -eq 124 ]
fi

set +e
run_with_timeout 2 sh -c 'exit 7'
status=$?
set -e
[ "${status}" -eq 7 ]

pid_file="${tmp_dir}/child.pid"
set +e
run_with_timeout 0.2 sh -c 'sleep 30 & echo "$!" > "$1"; wait' sh "${pid_file}"
status=$?
set -e
[ "${status}" -eq 124 ]

child_pid="$(cat "${pid_file}")"
if process_is_active "${child_pid}"; then
  echo "timed-out child process is still running" >&2
  exit 1
fi

set +e
run_with_timeout invalid true >/dev/null 2>&1
status=$?
set -e
[ "${status}" -eq 2 ]

for invalid_timeout in nan inf -inf; do
  set +e
  run_with_timeout "${invalid_timeout}" true >/dev/null 2>&1
  status=$?
  set -e
  [ "${status}" -eq 2 ]
done

stubborn_pid_file="${tmp_dir}/stubborn.pid"
set +e
run_with_timeout 0.2 sh -c '
  trap "exit 0" TERM
  sh -c "trap \"\" TERM; while :; do sleep 1; done" &
  echo "$!" > "$1"
  wait
' sh "${stubborn_pid_file}"
status=$?
set -e
[ "${status}" -eq 124 ]

stubborn_pid="$(cat "${stubborn_pid_file}")"
if process_is_active "${stubborn_pid}"; then
  echo "TERM-ignoring descendant survived timeout escalation" >&2
  exit 1
fi

grep -Fq 'run_with_timeout 900 "${COMPOSE_CMD[@]}"' "${repo_root}/deploy/project.sh"
if grep -Fq "start_new_session=True" "${repo_root}/deploy/lib.sh"; then
  echo "timeout helper detaches commands from controller process group" >&2
  exit 1
fi

echo "timeout tests passed"
