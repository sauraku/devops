#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/state/containers" "$test_root/tmp" "$test_root/global-docker"
printf '%s\n' untouched > "$test_root/global-docker/sentinel"
docker_log="$test_root/docker.log"
docker_config_log="$test_root/docker-config.log"
curl_log="$test_root/curl.log"

cat > "$test_root/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$DOCKER_STUB_LOG"
printf '%s|%s\n' "$1" "${DOCKER_CONFIG:-}" >> "$DOCKER_CONFIG_LOG"
containers="$DOCKER_STUB_STATE/containers"
mkdir -p "$containers"

write_container_state() {
  local id="${4:-}" transaction="${5:-}"
  if [ -z "$id" ] && [ -f "$containers/$1" ]; then
    id="$(sed -n '3p' "$containers/$1")"
  fi
  if [ -z "$transaction" ] && [ -f "$containers/$1" ]; then
    transaction="$(sed -n '4p' "$containers/$1")"
  fi
  [ -n "$id" ] || id="id-$1-$$-${RANDOM:-0}"
  printf '%s\n%s\n%s\n%s\n' "$2" "$3" "$id" "$transaction" > "$containers/$1"
}

kill_bootstrap_after() {
  [ "${DOCKER_KILL_AFTER:-}" = "$1" ] || return 0
  kill -KILL "$PPID"
  exit 137
}

resolve_container_ref() {
  local ref="$1" candidate
  if [ -f "$containers/$ref" ]; then
    printf '%s\n' "$ref"
    return 0
  fi
  for candidate in "$containers"/*; do
    [ -f "$candidate" ] || continue
    if [ "$(sed -n '3p' "$candidate")" = "$ref" ]; then
      basename "$candidate"
      return 0
    fi
  done
  return 1
}

case "$1" in
  login)
    [ -n "${DOCKER_CONFIG:-}" ]
    [ -d "$DOCKER_CONFIG" ]
    mode="$(stat -c '%a' "$DOCKER_CONFIG" 2>/dev/null || stat -f '%Lp' "$DOCKER_CONFIG")"
    [ "$mode" = 700 ]
    cat >/dev/null
    [ "${DOCKER_FAIL:-}" != login ]
    : > "$DOCKER_CONFIG/authenticated"
    ;;
  pull)
    [ "${DOCKER_FAIL:-}" != pull ]
    if [ "${REQUIRE_DOCKER_LOGIN:-}" = 1 ]; then
      [ -n "${DOCKER_CONFIG:-}" ]
      [ -f "$DOCKER_CONFIG/authenticated" ]
    fi
    ;;
  image)
    [ "${DOCKER_FAIL:-}" != inspect-image ]
    requested="${@: -1}"
    printf '%s@sha256:%064d\n' "${requested%@*}" 0
    ;;
  network)
    action="${2:-}"
    network="${@: -1}"
    case "$action" in
      inspect)
        [ -f "$DOCKER_STUB_STATE/network-$network" ] || exit 1
        if [ "${3:-}" = -f ]; then
          printf '%s\n' runner-network
        fi
        ;;
      create)
        : > "$DOCKER_STUB_STATE/network-$network"
        ;;
      rm)
        rm -f -- "$DOCKER_STUB_STATE/network-$network"
        ;;
      *) exit 1 ;;
    esac
    ;;
  inspect)
    name="$(resolve_container_ref "${@: -1}")" || exit 1
    if [ "${2:-}" = -f ]; then
      case "${3:-}" in
        *bootstrap-transaction*) sed -n '4p' "$containers/$name" ;;
        *Labels*) sed -n '1p' "$containers/$name" ;;
        *State.Running*)
          if [ "$(sed -n '2p' "$containers/$name")" = running ]; then
            printf '%s\n' true
          else
            printf '%s\n' false
          fi
          ;;
        *Id*) sed -n '3p' "$containers/$name" ;;
      esac
    fi
    ;;
  stop)
    name="$(resolve_container_ref "$2")" || exit 1
    if [ "${DOCKER_FAIL:-}" = rollback-stop ] && compgen -G "$containers/${name}-bootstrap-rollback-*" >/dev/null; then
      exit 71
    fi
    write_container_state "$name" "$(sed -n '1p' "$containers/$name")" stopped
    kill_bootstrap_after old-stopped
    ;;
  start)
    name="$(resolve_container_ref "$2")" || exit 1
    [ "${DOCKER_FAIL:-}" != start-old ]
    write_container_state "$name" "$(sed -n '1p' "$containers/$name")" running
    ;;
  rename)
    source_name="$(resolve_container_ref "$2")" || exit 1
    [ ! -e "$containers/$3" ]
    if [ "${DOCKER_FAIL:-}" = rename-back ] && [[ "$source_name" == *-bootstrap-rollback-* ]]; then
      exit 72
    fi
    mv -- "$containers/$source_name" "$containers/$3"
    if [[ "$source_name" == *-bootstrap-rollback-* ]]; then
      : > "$DOCKER_STUB_STATE/restored-old"
    fi
    kill_bootstrap_after old-renamed
    ;;
  rm)
    name="$(resolve_container_ref "${@: -1}")" || exit 1
    if [ "${DOCKER_FAIL:-}" = rollback-rm ] && compgen -G "$containers/${name}-bootstrap-rollback-*" >/dev/null; then
      exit 73
    fi
    [ "$(sed -n '2p' "$containers/$name")" != running ] || exit 1
    rm -f -- "$containers/$name"
    ;;
  run)
    shift
    validation_mode=0
    proxy_value=""
    container_name=""
    candidate_env=""
    candidate_transaction=""
    validation_network=0
    validation_read_only=0
    validation_cap_drop=0
    validation_security_opt=0
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -e)
          shift
          case "${1:-}" in
            TRUSTED_PROXY_CIDRS=*) proxy_value="${1#*=}" ;;
          esac
          ;;
        --name)
          shift
          container_name="${1:-}"
          ;;
        --env-file)
          shift
          candidate_env="${1:-}"
          ;;
        --label)
          shift
          case "${1:-}" in
            com.sauraku.devops.bootstrap-transaction=*)
              candidate_transaction="${1#*=}"
              ;;
          esac
          ;;
        --network)
          shift
          [ "${1:-}" = none ] && validation_network=1
          ;;
        --read-only)
          validation_read_only=1
          ;;
        --cap-drop)
          shift
          [ "${1:-}" = ALL ] && validation_cap_drop=1
          ;;
        --security-opt)
          shift
          [ "${1:-}" = no-new-privileges:true ] && validation_security_opt=1
          ;;
        --entrypoint)
          echo "validation must use the image entrypoint" >&2
          exit 1
          ;;
        validate-trusted-proxy-cidrs)
          validation_mode=1
          ;;
      esac
      shift
    done
    if [ "$validation_mode" -eq 1 ]; then
      [ "$validation_network" -eq 1 ]
      [ "$validation_read_only" -eq 1 ]
      [ "$validation_cap_drop" -eq 1 ]
      [ "$validation_security_opt" -eq 1 ]
      case "$proxy_value" in
        *255.255.255.0*|*::ffff:192.0.2.0/64*|*definitely-not-an-ip-or-cidr*) exit 2 ;;
      esac
      exit 0
    fi
    [ -n "$container_name" ]
    [ "${DOCKER_FAIL:-}" != start ]
    [[ "$candidate_env" == *.candidate.* ]]
    [ -f "$candidate_env" ]
    grep -q '^RUNNER_IMAGE=.*@sha256:' "$candidate_env"
    if [ -n "${LIVE_ENV_EXPECTED_CHECKSUM:-}" ]; then
      [ "$(cksum "$BOOTSTRAP_ENV_FILE")" = "$LIVE_ENV_EXPECTED_CHECKSUM" ]
    fi
    if [ -n "${EXPECTED_STAGED_TOKEN:-}" ]; then
      [ "$(sed -n 's/^GITHUB_TOKEN=//p' "$candidate_env")" = "$EXPECTED_STAGED_TOKEN" ]
    fi
    if [ "${EXPECT_STAGED_TOKEN_CLEARED:-}" = 1 ]; then
      ! grep -q '^GITHUB_TOKEN=' "$candidate_env"
    fi
    rm -f -- "$DOCKER_STUB_STATE/restored-old"
    write_container_state "$container_name" control running "" "$candidate_transaction"
    kill_bootstrap_after candidate-started
    ;;
  *)
    echo "unexpected docker command: $*" >&2
    exit 1
    ;;
esac
EOF

cat > "$test_root/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url="${@: -1}"
protocol=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -k|--insecure) echo "TLS bypass is forbidden" >&2; exit 90 ;;
    --proto) shift; protocol="${1:-}" ;;
  esac
  shift
done
case "$url" in
  https://*) [ "$protocol" = =https ] ;;
  http://127.0.0.1:*) [ "$protocol" = =http ] ;;
  *) exit 91 ;;
esac
printf '%s\n' "$url" >> "$CURL_STUB_LOG"
should_fail=0
case "${HTTP_FAIL:-}" in
  health)
    if [ ! -f "$DOCKER_STUB_STATE/restored-old" ] &&
       [[ "$url" == http://127.0.0.1:*/api/health ]]; then
      should_fail=1
    fi
    ;;
  restored-health)
    if [ -f "$DOCKER_STUB_STATE/restored-old" ] &&
       [[ "$url" == http://127.0.0.1:*/api/health ]]; then
      should_fail=1
    fi
    ;;
  public-health)
    [[ "$url" == https://*/api/health ]] && should_fail=1
    ;;
  public-login)
    [[ "$url" == https://*/login ]] && should_fail=1
    ;;
esac
[ "$should_fail" -eq 0 ] || exit 22
printf '%s' 200
EOF

cat > "$test_root/bin/stat" <<'EOF'
#!/usr/bin/env bash
if [ "${@: -1}" = /var/run/docker.sock ]; then
  printf '%s\n' 999
  exit 0
fi
exec /usr/bin/stat "$@"
EOF

cat > "$test_root/bin/mv" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
source_path="${@: -2:1}"
destination="${@: -1}"
if [ "${MV_FAIL_ENV_COMMIT:-}" = 1 ] && [ "$destination" = "$BOOTSTRAP_ENV_FILE" ] && [[ "$source_path" == *.candidate.* ]]; then
  exit 72
fi
if [ "${MV_KILL_AFTER_ENV_COMMIT:-}" = 1 ] && [ "$destination" = "$BOOTSTRAP_ENV_FILE" ] && [[ "$source_path" == *.candidate.* ]]; then
  /bin/mv "$@"
  kill -KILL "$PPID"
  exit 137
fi
exec /bin/mv "$@"
EOF

cat > "$test_root/bin/sync" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$test_root/bin/docker" "$test_root/bin/curl" "$test_root/bin/stat" "$test_root/bin/mv" "$test_root/bin/sync"

run_bootstrap() {
  local data_dir="$1" container="$2" output_file="$3" token_state="$4" token="${5-}"
  local proxy_state="${6:-omitted}" proxy="${7-}" public_state="${8:-omitted}" public_url="${9-}"
  local docker_fail="${10-}" http_fail="${11-}" mv_fail="${12-}" require_login="${13-}"
  local docker_kill_after="${14-}" mv_kill_after_env_commit="${15-}"
  local env_file="$data_dir/.env.prod"
  local live_checksum="" expected_staged_token="" expect_staged_token_cleared=0
  [ ! -f "$env_file" ] || live_checksum="$(cksum "$env_file")"
  if [ "$token_state" = supplied ]; then
    if [ -n "$token" ]; then expected_staged_token="$token"; else expect_staged_token_cleared=1; fi
  fi
  local -a command=(
    env -u GITHUB_TOKEN -u TRUSTED_PROXY_CIDRS -u DEPLOY_CONTROL_PUBLIC_URL
    "PATH=$test_root/bin:/usr/bin:/bin"
    "TMPDIR=$test_root/tmp"
    "DOCKER_CONFIG=$test_root/global-docker"
    "DOCKER_STUB_LOG=$docker_log"
    "DOCKER_CONFIG_LOG=$docker_config_log"
    "DOCKER_STUB_STATE=$test_root/state"
    "CURL_STUB_LOG=$curl_log"
    "BOOTSTRAP_ENV_FILE=$env_file"
    "BOOTSTRAP_HEALTH_ATTEMPTS=1"
    "DATA_DIR=$data_dir"
    "ENV_FILE=$env_file"
    "CONTAINER=$container"
    "DOCKER_FAIL=$docker_fail"
    "DOCKER_KILL_AFTER=$docker_kill_after"
    "HTTP_FAIL=$http_fail"
    "MV_FAIL_ENV_COMMIT=$mv_fail"
    "MV_KILL_AFTER_ENV_COMMIT=$mv_kill_after_env_commit"
    "REQUIRE_DOCKER_LOGIN=$require_login"
    "LIVE_ENV_EXPECTED_CHECKSUM=$live_checksum"
    "EXPECTED_STAGED_TOKEN=$expected_staged_token"
    "EXPECT_STAGED_TOKEN_CLEARED=$expect_staged_token_cleared"
  )
  if [ "$token_state" = supplied ]; then
    command+=("GITHUB_TOKEN=$token")
  fi
  if [ "$proxy_state" = supplied ]; then
    command+=("TRUSTED_PROXY_CIDRS=$proxy")
  fi
  if [ "$public_state" = supplied ]; then
    command+=("DEPLOY_CONTROL_PUBLIC_URL=$public_url")
  fi
  "${command[@]}" bash "$repo_root/deploy/bootstrap.sh" > "$output_file" 2>&1
}

assert_old_is_running() {
  local name="$1"
  [[ "$(sed -n '1p' "$test_root/state/containers/$name")" = control ]]
  [[ "$(sed -n '2p' "$test_root/state/containers/$name")" = running ]]
  ! compgen -G "$test_root/state/containers/${name}-bootstrap-rollback-*" >/dev/null
}

assert_no_transaction_files() {
  local env_file="$1"
  ! compgen -G "${env_file}.candidate.*" >/dev/null
  ! compgen -G "${env_file}.rollback.*" >/dev/null
  [[ ! -e "$(dirname "$env_file")/Run/bootstrap-transaction" ]]
  [[ ! -d "$(dirname "$env_file")/Run/bootstrap.lock" ]]
}

restore_stub_previous() {
  local name="$1" expected_id="$2" backup=""
  backup="$(find "$test_root/state/containers" -maxdepth 1 -type f -name "${name}-bootstrap-rollback-*" -print -quit)"
  if [ -f "$test_root/state/containers/$name" ] && [ "$(sed -n '3p' "$test_root/state/containers/$name")" != "$expected_id" ]; then
    rm -f -- "$test_root/state/containers/$name"
  fi
  if [ -n "$backup" ]; then
    mv -f -- "$backup" "$test_root/state/containers/$name"
  fi
  printf '%s\n%s\n%s\n' control running "$expected_id" > "$test_root/state/containers/$name"
  rm -f -- "$test_root/state/restored-old"
}

data_dir="$test_root/data"
container=devops-control
env_file="$data_dir/.env.prod"
mkdir -p "$data_dir"
secret_one='github_pat_first-test-secret'
secret_two='github_pat_rotated-test-secret'
proxy_one='127.0.0.1/32,::1/128'
public_url='https://admin.example.test'

# Successful first install commits the candidate only after local and public
# checks and confines the supplied PAT to a disposable Docker config.
run_bootstrap "$data_dir" "$container" "$test_root/first.out" supplied "$secret_one" \
  supplied "$proxy_one" supplied "$public_url"
[[ "$(sed -n 's/^GITHUB_TOKEN=//p' "$env_file")" = "$secret_one" ]]
[[ "$(sed -n 's/^TRUSTED_PROXY_CIDRS=//p' "$env_file")" = "$proxy_one" ]]
[[ "$(sed -n 's/^DEPLOY_CONTROL_PUBLIC_URL=//p' "$env_file")" = "$public_url" ]]
[[ "$(grep -c '^GITHUB_TOKEN=' "$env_file")" -eq 1 ]]
assert_old_is_running "$container"
assert_no_transaction_files "$env_file"
! grep -Fq "$secret_one" "$test_root/first.out"
[[ "$(cat "$test_root/global-docker/sentinel")" = untouched ]]
login_config="$(awk -F'|' '$1 == "login" { print $2; exit }' "$docker_config_log")"
[[ -n "$login_config" && "$login_config" != "$test_root/global-docker" ]]
[[ ! -e "$login_config" ]]
awk -F'|' -v expected="$login_config" '$1 == "pull" || $1 == "image" { if ($2 != expected) exit 1 }' "$docker_config_log"
mode="$(stat -c '%a' "$env_file" 2>/dev/null || stat -f '%Lp' "$env_file")"
[[ "$mode" = 600 ]]

# Omission preserves persisted values and uses the persisted controller PAT in
# a fresh isolated config when private pulls require authentication.
login_count="$(grep -c '^login|' "$docker_config_log")"
run_bootstrap "$data_dir" "$container" "$test_root/omitted.out" omitted '' omitted '' omitted '' '' '' '' 1
[[ "$(sed -n 's/^GITHUB_TOKEN=//p' "$env_file")" = "$secret_one" ]]
[[ "$(sed -n 's/^TRUSTED_PROXY_CIDRS=//p' "$env_file")" = "$proxy_one" ]]
[[ "$(grep -c '^login|' "$docker_config_log")" -eq $((login_count + 1)) ]]
omitted_login_config="$(awk -F'|' '$1 == "login" { value=$2 } END { print value }' "$docker_config_log")"
[[ -n "$omitted_login_config" && "$omitted_login_config" != "$test_root/global-docker" ]]
[[ ! -e "$omitted_login_config" ]]
[[ "$(cat "$test_root/global-docker/sentinel")" = untouched ]]
assert_old_is_running "$container"

# Authentication failure happens before staging or stopping and cannot mutate
# the live env or old controller.
before="$(cksum "$env_file")"
set +e
run_bootstrap "$data_dir" "$container" "$test_root/auth-failure.out" omitted '' \
  omitted '' omitted '' login '' '' 1
status=$?
set -e
[[ "$status" -ne 0 ]]
[[ "$(cksum "$env_file")" = "$before" ]]
assert_old_is_running "$container"
assert_no_transaction_files "$env_file"
! grep -Fq "$secret_one" "$test_root/auth-failure.out"
failed_login_config="$(awk -F'|' '$1 == "login" { value=$2 } END { print value }' "$docker_config_log")"
[[ -n "$failed_login_config" && ! -e "$failed_login_config" ]]

# Exact controller validation rejects syntax accepted by Python's ipaddress,
# without changing the live env or stopping the old controller.
for invalid_proxy in '192.0.2.1/255.255.255.0' '::ffff:192.0.2.0/64'; do
  before="$(cksum "$env_file")"
  set +e
  run_bootstrap "$data_dir" "$container" "$test_root/invalid.out" omitted '' \
    supplied "$invalid_proxy" omitted ''
  status=$?
  set -e
  [[ "$status" -eq 2 ]]
  [[ "$(cksum "$env_file")" = "$before" ]]
  assert_old_is_running "$container"
  assert_no_transaction_files "$env_file"
done

# Pull/preflight failure cannot mutate the live env or interrupt the old app.
before="$(cksum "$env_file")"
set +e
run_bootstrap "$data_dir" "$container" "$test_root/preflight.out" supplied "$secret_two" \
  omitted '' omitted '' pull
status=$?
set -e
[[ "$status" -ne 0 ]]
[[ "$(cksum "$env_file")" = "$before" ]]
assert_old_is_running "$container"
assert_no_transaction_files "$env_file"
! grep -Fq "$secret_two" "$test_root/preflight.out"
latest_login_config="$(awk -F'|' '$1 == "login" { value=$2 } END { print value }' "$docker_config_log")"
[[ ! -e "$latest_login_config" ]]

# Runtime boolean parsing is case-insensitive. Mixed-case true still requires
# both proxy and public HTTPS configuration, before the old container is touched.
printf '%s\n' COOKIE_SECURE=TrUe >> "$env_file"
before="$(cksum "$env_file")"
set +e
run_bootstrap "$data_dir" "$container" "$test_root/mixed-cookie-secure.out" omitted '' \
  supplied '' supplied ''
status=$?
set -e
[[ "$status" -eq 2 ]]
[[ "$(cksum "$env_file")" = "$before" ]]
assert_old_is_running "$container"

# Every post-stop failure restores the canonical old container and leaves the
# live env byte-for-byte unchanged.
for failure in start health public-login; do
  before="$(cksum "$env_file")"
  docker_failure=""
  http_failure=""
  if [ "$failure" = start ]; then docker_failure=start; else http_failure="$failure"; fi
  set +e
  run_bootstrap "$data_dir" "$container" "$test_root/${failure}.out" supplied "$secret_two" \
    omitted '' omitted '' "$docker_failure" "$http_failure"
  status=$?
  set -e
  [[ "$status" -ne 0 ]]
  [[ "$(cksum "$env_file")" = "$before" ]]
  assert_old_is_running "$container"
  assert_no_transaction_files "$env_file"
done

# Rollback infrastructure failures never report the original rollout failure
# as successfully recovered. The exact old ID remains discoverable and manual
# recovery instructions name its safe location.
for rollback_failure in rollback-stop rollback-rm rename-back start-old restored-health; do
  before="$(cksum "$env_file")"
  known_old_id="$(sed -n '3p' "$test_root/state/containers/$container")"
  docker_failure="$rollback_failure"
  http_failure=public-login
  mv_failure=""
  if [ "$rollback_failure" = restored-health ]; then
    docker_failure=""
    http_failure=restored-health
    mv_failure=1
  fi
  set +e
  run_bootstrap "$data_dir" "$container" "$test_root/rollback-${rollback_failure}.out" supplied "$secret_two" \
    omitted '' omitted '' "$docker_failure" "$http_failure" "$mv_failure"
  status=$?
  set -e
  [[ "$status" -eq 70 ]]
  [[ "$(cksum "$env_file")" = "$before" ]]
  grep -q 'ROLLBACK FAILED: automatic recovery was incomplete' "$test_root/rollback-${rollback_failure}.out"
  grep -q "$known_old_id" "$test_root/rollback-${rollback_failure}.out"
  [[ -f "$data_dir/Run/bootstrap-transaction" ]]
  compgen -G "${env_file}.candidate.*" >/dev/null
  compgen -G "${env_file}.rollback.*" >/dev/null
  restore_stub_previous "$container" "$known_old_id"
  set +e
  run_bootstrap "$data_dir" "$container" "$test_root/recover-${rollback_failure}.out" omitted '' \
    omitted '' omitted '' pull
  recovery_status=$?
  set -e
  [[ "$recovery_status" -ne 0 ]]
  grep -q 'Interrupted bootstrap transaction reconciled' "$test_root/recover-${rollback_failure}.out"
  assert_old_is_running "$container"
  assert_no_transaction_files "$env_file"
done

# A failed atomic env commit also rolls the healthy candidate back.
before="$(cksum "$env_file")"
set +e
run_bootstrap "$data_dir" "$container" "$test_root/commit.out" supplied "$secret_two" \
  omitted '' omitted '' '' '' 1
status=$?
set -e
[[ "$status" -ne 0 ]]
[[ "$(cksum "$env_file")" = "$before" ]]
assert_old_is_running "$container"
assert_no_transaction_files "$env_file"

# Explicit empty values clear controller settings only. Disable secure-cookie
# production requirements in this test fixture so clearing proxy/public config
# is a valid candidate.
printf '%s\n' COOKIE_SECURE=false >> "$env_file"
clear_login_count="$(grep -c '^login|' "$docker_config_log")"
run_bootstrap "$data_dir" "$container" "$test_root/cleared.out" supplied '' supplied '' supplied '' '' '' '' 1
! grep -q '^GITHUB_TOKEN=' "$env_file"
! grep -q '^TRUSTED_PROXY_CIDRS=' "$env_file"
! grep -q '^DEPLOY_CONTROL_PUBLIC_URL=' "$env_file"
[[ "$(cat "$test_root/global-docker/sentinel")" = untouched ]]
[[ "$(grep -c '^login|' "$docker_config_log")" -eq $((clear_login_count + 1)) ]]
clear_login_config="$(awk -F'|' '$1 == "login" { value=$2 } END { print value }' "$docker_config_log")"
[[ -n "$clear_login_config" && "$clear_login_config" != "$test_root/global-docker" ]]
[[ ! -e "$clear_login_config" ]]
assert_old_is_running "$container"

# First-install readiness failure removes the candidate and leaves no live env
# or canonical container.
first_fail_data="$test_root/first-fail-data"
first_fail_container=devops-first-fail
mkdir -p "$first_fail_data"
set +e
run_bootstrap "$first_fail_data" "$first_fail_container" "$test_root/first-fail.out" omitted '' \
  supplied "$proxy_one" supplied "$public_url" '' health
status=$?
set -e
[[ "$status" -ne 0 ]]
[[ ! -e "$first_fail_data/.env.prod" ]]
[[ ! -e "$test_root/state/containers/$first_fail_container" ]]
assert_no_transaction_files "$first_fail_data/.env.prod"

# SIGKILL cannot run the EXIT trap. Each durable pre-mutation phase must let
# the next invocation identify the exact old controller, undo any candidate
# and env cutover, clear the stale lock, and only then attempt its image pull.
for crash_phase in old-stopped old-renamed candidate-started env-committed; do
  crash_data="$test_root/crash-$crash_phase"
  crash_container="devops-crash-$crash_phase"
  crash_env="$crash_data/.env.prod"
  mkdir -p "$crash_data"
  cp -- "$env_file" "$crash_env"
  printf '%s\n%s\n%s\n\n' control running "id-$crash_container-old" \
    > "$test_root/state/containers/$crash_container"
  before="$(cksum "$crash_env")"

  set +e
  if [ "$crash_phase" = env-committed ]; then
    run_bootstrap "$crash_data" "$crash_container" "$test_root/crash-$crash_phase.out" \
      supplied "$secret_two" omitted '' omitted '' '' '' '' '' '' 1
  else
    run_bootstrap "$crash_data" "$crash_container" "$test_root/crash-$crash_phase.out" \
      supplied "$secret_two" omitted '' omitted '' '' '' '' '' "$crash_phase"
  fi
  crash_status=$?
  set -e
  [[ "$crash_status" -ne 0 ]]
  [[ -f "$crash_data/Run/bootstrap-transaction" ]]
  [[ -d "$crash_data/Run/bootstrap.lock" ]]
  case "$crash_phase" in
    old-stopped) expected_phase=old_stopping ;;
    old-renamed) expected_phase=old_renaming ;;
    candidate-started) expected_phase=candidate_starting ;;
    env-committed) expected_phase=env_committing ;;
  esac
  [[ "$(sed -n 's/^phase=//p' "$crash_data/Run/bootstrap-transaction")" = "$expected_phase" ]]
  [[ "$(sed -n 's/^old_container_id=//p' "$crash_data/Run/bootstrap-transaction")" = "id-$crash_container-old" ]]
  [[ -n "$(sed -n 's/^old_container_rollback_name=//p' "$crash_data/Run/bootstrap-transaction")" ]]
  [[ -n "$(sed -n 's/^staged_env=//p' "$crash_data/Run/bootstrap-transaction")" ]]

  set +e
  run_bootstrap "$crash_data" "$crash_container" "$test_root/recovered-$crash_phase.out" \
    omitted '' omitted '' omitted '' pull
  recovery_status=$?
  set -e
  [[ "$recovery_status" -ne 0 ]]
  grep -q 'Interrupted bootstrap transaction reconciled' "$test_root/recovered-$crash_phase.out"
  [[ "$(cksum "$crash_env")" = "$before" ]]
  assert_old_is_running "$crash_container"
  assert_no_transaction_files "$crash_env"
done

# A live installation-scoped owner blocks concurrent bootstrap before pull or
# cutover. The contender must not steal or remove that lock.
lock_data="$test_root/lock-contention"
lock_container=devops-lock-contention
lock_env="$lock_data/.env.prod"
mkdir -p "$lock_data/Run/bootstrap.lock"
cp -- "$env_file" "$lock_env"
printf '%s\n%s\n%s\n\n' control running "id-$lock_container-old" \
  > "$test_root/state/containers/$lock_container"
printf 'format=1\npid=%s\ntoken=other-bootstrap\nboot_id=unavailable\nstart_token=unavailable\n' "$$" \
  > "$lock_data/Run/bootstrap.lock/owner"
docker_lines_before="$(wc -l < "$docker_log")"
set +e
run_bootstrap "$lock_data" "$lock_container" "$test_root/lock-contention.out" omitted ''
lock_status=$?
set -e
[[ "$lock_status" -eq 75 ]]
grep -q 'Another bootstrap is already running' "$test_root/lock-contention.out"
[[ -d "$lock_data/Run/bootstrap.lock" ]]
[[ "$(wc -l < "$docker_log")" = "$docker_lines_before" ]]
assert_old_is_running "$lock_container"

printf '%s\n' "bootstrap transaction tests passed"
