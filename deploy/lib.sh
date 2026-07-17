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
      HOME|PATH|PWD|OLDPWD|SHLVL|SHELL|BASH_ENV|ENV|CDPATH|GLOBIGNORE|IFS|LD_*|DYLD_*)
        continue
        ;;
    esac
    if [[ "$value" == *$'\n'* ]]; then
      echo "Invalid multiline environment value for $key in $file" >&2
      return 1
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
