#!/usr/bin/env bash
# Tests for `check-zod-version.sh`: every manifest that declares zod names one exact version, and
# the check says what it covered.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-zod-version.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-zod-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — the root and two workspaces on zod 4.3.6 (one of them as a
# devDependency), and a workspace that declares no zod. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/contracts" "$root/packages/pi-envoy" \
    "$root/packages/dispatch"
  ln -s "$check_script" "$root/.github/scripts/check-zod-version.sh"
  cat > "$root/package.json" <<'JSON'
{
  "workspaces": ["packages/contracts", "packages/pi-envoy", "packages/dispatch"],
  "dependencies": {
    "zod": "4.3.6"
  }
}
JSON
  cat > "$root/packages/contracts/package.json" <<'JSON'
{
  "name": "@legion/contracts",
  "dependencies": {
    "zod": "4.3.6"
  }
}
JSON
  cat > "$root/packages/pi-envoy/package.json" <<'JSON'
{
  "name": "@sjawhar/pi-legion-envoy",
  "devDependencies": {
    "zod": "4.3.6"
  }
}
JSON
  printf '{"name":"@legion/dispatch"}\n' > "$root/packages/dispatch/package.json"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-zod-version.sh" 2>&1)
  status=$?
  set -e
}

# set_zod <file> <version>: rewrites the one zod declaration in a fixture manifest.
set_zod() {
  sed -i "s/\"zod\": \"[^\"]*\"/\"zod\": \"$2\"/" "$1"
}

echo "case: every manifest names the same exact zod"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" \
  "$(contains "$out" 'every zod declaration names 4.3.6 (3 across 4 manifests)')"

echo "case: a range in one manifest"
ranges=("^4.3.6" "~4.3.6" "4" "4.x" ">=4.3.6" "latest")
for index in "${!ranges[@]}"; do
  range=${ranges[index]}
  root=$(fixture "range-$index")
  set_zod "$root/packages/pi-envoy/package.json" "$range"
  run_check "$root"
  check "$range fails" "$(is "$status" 1)"
  check "$range is named at its manifest and line" \
    "$(contains "$out" "packages/pi-envoy/package.json:4: devDependencies name zod $range, which is a range")"
done

echo "case: one manifest pinned to another exact zod"
root=$(fixture drift)
set_zod "$root/packages/contracts/package.json" "4.4.0"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the manifest that drifted" \
  "$(contains "$out" 'packages/contracts/package.json:4: names zod 4.4.0, where the manifests disagree')"
check "names every version and where each is declared" \
  "$(contains "$out" '4.3.6 in package.json:4, packages/pi-envoy/package.json:4; 4.4.0 in packages/contracts/package.json:4')"
check "does not blame the manifests that agree" "$(is "$(contains "$out" 'package.json:4: names zod 4.3.6')" false)"

echo "case: a tie names every declaration"
root=$(fixture tie)
set_zod "$root/packages/contracts/package.json" "4.4.0"
set_zod "$root/packages/pi-envoy/package.json" "4.5.0"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the root" "$(contains "$out" '^::error file=package.json::package.json:4: names zod 4.3.6')"
check "names contracts" "$(contains "$out" 'packages/contracts/package.json:4: names zod 4.4.0')"
check "names pi-envoy" "$(contains "$out" 'packages/pi-envoy/package.json:4: names zod 4.5.0')"

echo "case: two sections of one manifest disagree"
root=$(fixture two-sections)
cat > "$root/packages/contracts/package.json" <<'JSON'
{
  "name": "@legion/contracts",
  "dependencies": {
    "zod": "4.3.6"
  },
  "peerDependencies": {
    "zod": "4.4.0"
  }
}
JSON
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the peer declaration" "$(contains "$out" 'packages/contracts/package.json:7: names zod 4.4.0')"

echo "case: a root override counts as a declaration"
root=$(fixture override)
python3 - "$root/package.json" <<'PY'
import json, sys
path = sys.argv[1]
manifest = json.load(open(path))
manifest["overrides"] = {"zod": "4.4.0"}
json.dump(manifest, open(path, "w"), indent=2)
PY
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the override" "$(contains "$out" 'names zod 4.4.0')"

echo "case: a new workspace that declares zod joins the set"
root=$(fixture new-workspace)
mkdir -p "$root/packages/newcomer"
printf '{\n  "name": "newcomer",\n  "dependencies": {\n    "zod": "^4.1.8"\n  }\n}\n' \
  > "$root/packages/newcomer/package.json"
sed -i 's|"packages/dispatch"\]|"packages/dispatch", "packages/newcomer"]|' "$root/package.json"
run_check "$root"
check "a range in it fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'packages/newcomer/package.json:4: dependencies name zod ^4.1.8')"

echo "case: nothing declares zod"
root=$(fixture empty)
for manifest in "$root/package.json" "$root/packages/contracts/package.json" "$root/packages/pi-envoy/package.json"; do
  sed -i '/"zod"/d' "$manifest"
done
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it proves nothing" "$(contains "$out" 'so it proves nothing')"

echo "case: a manifest the check cannot read"
root=$(fixture bad-json)
printf '{"name": "@legion/contracts",\n' > "$root/packages/contracts/package.json"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, without a traceback" \
  "$(contains "$out" 'packages/contracts/package.json is not valid JSON')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

root=$(fixture missing-workspace)
rm "$root/packages/dispatch/package.json"
run_check "$root"
check "a listed workspace with no manifest fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'packages/dispatch/package.json is missing')"

root=$(fixture no-workspaces)
printf '{"dependencies": {"zod": "4.3.6"}}\n' > "$root/package.json"
run_check "$root"
check "a root with no workspace list fails" "$(is "$status" 1)"
check "says what it needs" "$(contains "$out" 'non-empty array of workspace paths')"

summary "check-zod-version.sh"
