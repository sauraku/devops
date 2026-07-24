#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
runner_runtime="$test_root/runtime"
mkdir -p "$runner_runtime"

make_runner() {
  local name="$1" runner_dir
  runner_dir="$test_root/$name"
  mkdir -p "$runner_dir"
  printf '%s\n' test-version > "$runner_dir/.runner_version"
  : > "$runner_dir/.runner"
  printf '%s\n' "$runner_dir"
}

run_entrypoint() {
  local runner_dir="$1"
  shift
  RUNNER_DIR="$runner_dir" RUNNER_RUNTIME_DIR="$runner_runtime" RUNNER_VERSION=test-version \
    bash "$repo_root/deploy/runner/entrypoint.sh" "$@"
}

runner_dir="$(make_runner exit-status)"
cat > "$runner_dir/run.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s' 'runner-output-with-unterminated-final-byte'
exit 37
EOF
chmod +x "$runner_dir/run.sh"
set +e
run_entrypoint "$runner_dir" > "$test_root/unterminated.out" 2>&1
status=$?
set -e
[[ "$status" -eq 37 ]]
expected='runner-output-with-unterminated-final-byte'
actual="$(tail -c "${#expected}" "$test_root/unterminated.out")"
[[ "$actual" = "$expected" ]]

# Repository-controlled stdout can imitate GitHub's deletion message. The
# entrypoint never treats it as authorization, so all registration files remain.
runner_dir="$(make_runner injected-marker)"
: > "$runner_dir/.runner_migrated"
: > "$runner_dir/.credentials"
: > "$runner_dir/.credentials_rsaparams"
cat > "$runner_dir/run.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' 'The runner registration has been deleted from the server'
exit 0
EOF
chmod +x "$runner_dir/run.sh"
set +e
run_entrypoint "$runner_dir" >"$test_root/injected.out" 2>&1
status=$?
set -e
[[ "$status" -eq 0 ]]
for credential in .runner .runner_migrated .credentials .credentials_rsaparams; do
  [[ -e "$runner_dir/$credential" ]]
done
! grep -q '/api/projects/.*/runner-recovery' "$repo_root/deploy/runner/entrypoint.sh"

# Large binary-free output is streamed directly and no filesystem FIFO or
# temporary log is involved.
runner_dir="$(make_runner direct-stream)"
cat > "$runner_dir/run.sh" <<'EOF'
#!/usr/bin/env bash
head -c 4194304 /dev/zero | tr '\0' x
EOF
chmod +x "$runner_dir/run.sh"
run_entrypoint "$runner_dir" >/dev/null
! grep -q 'runner-output-detector' "$repo_root/deploy/runner/entrypoint.sh"

# The runner image must fail closed for unknown architectures and authenticate
# every downloaded release artifact before extraction.
runner_dockerfile="$repo_root/deploy/runner/Dockerfile.runner"
grep -Fq '*) echo "Unsupported runner architecture: $ARCH" >&2; exit 1 ;;' "$runner_dockerfile"
grep -Fq 'curl --fail --show-error --location' "$runner_dockerfile"
grep -Fq "sha256sum -c -" "$runner_dockerfile"
checksum_count="$(grep -Ec 'RUNNER_SHA256="[[:xdigit:]]{64}"' "$runner_dockerfile")"
[[ "$checksum_count" -eq 3 ]]

printf '%s\n' "runner entrypoint tests passed"
