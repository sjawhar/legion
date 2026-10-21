#!/usr/bin/env bash
# Tests for `scripts/legion-setup.sh`: what it installs, what it refuses to install, where it
# writes, and what it reports when the install does not finish. Every one of them runs against a
# stub `bun` first on PATH, which records the call and never installs anything: a real
# `bun install` needs the network CI does not give this job, and one on the container's own disk
# evicts the pod this harness runs in.
#
# Each case builds a miniature issue volume under a temporary directory — the workspace under a
# `/workspaces/` component, a state directory of its own, a `$HOME` and a `$TMPDIR` — and runs the
# script in an environment of its own (`env -i`), because this harness may itself run inside a
# Legion role container, whose LEGION_* variables would otherwise be the ones the script read.
# After every case, that fixture's `$HOME` and `$TMPDIR` must still be empty: they are the
# container's own ephemeral storage, and the script's whole point is to stay off it.
#
# Run from anywhere: bash scripts/legion-setup.test.sh
# CI runs it in the test job of pr-and-main.yaml.
set -euo pipefail
# Every fixture tree is built outside a command substitution, but the assertions read files
# through one: without this, a `$(…)` that failed would be read as an empty value rather than
# ending the case.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
setup_script="$script_dir/legion-setup.sh"
# The derivation case needs a temporary root with no `/workspaces/` component of its own, since
# that component is exactly what the script looks for.
tmp_root=${TMPDIR:-/tmp}
case "$tmp_root" in
*/workspaces/*) tmp_root=/tmp ;;
esac
work=$(mktemp -d "$tmp_root/legion-setup-test.XXXXXX")
# One case leaves its state directory unwritable, which would otherwise outlive the harness.
trap 'chmod -R u+rwX "$work" > /dev/null 2>&1; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/../.github/scripts/test-lib.sh"

# fixture <name>: a miniature issue volume, and the globals every case reads. Each case names its
# own tree: two cases sharing a name would share a workspace, and the second would start from the
# first's node_modules.
fixture() {
  case_root="$work/$1"
  if [ -e "$case_root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    exit 1
  fi
  workspace="$case_root/legion/workspaces/sjawhar/legion"
  install_root="$case_root/legion"
  cache="$install_root/bun/cache"
  state="$install_root/bun/state"
  marker="$state/installed"
  record="$case_root/bun-calls"
  state_dir="$case_root/state"
  report="$state_dir/legion-setup-report.json"
  mkdir -p "$workspace" "$case_root/bin" "$case_root/home" "$case_root/tmp" "$state_dir"
  printf 'bun.lock of the %s case\n' "$1" > "$workspace/bun.lock"
  cat > "$case_root/bin/bun" <<'STUB'
#!/usr/bin/env bash
# The stub bun: no network, no install, no cache. It records the call the script made — the
# arguments, the working directory it ran in, the cache directory it was given and its own pid —
# and then behaves as STUB_MODE says.
set -u
{
  echo "args: $*"
  echo "pwd: $PWD"
  echo "cache: ${BUN_INSTALL_CACHE_DIR-}"
  echo "pid: $$"
} >> "$STUB_RECORD"
case "${STUB_MODE-ok}" in
ok)
  mkdir -p node_modules/.bin
  printf '#!/bin/sh\necho 2.3.5\n' > node_modules/.bin/biome
  chmod 755 node_modules/.bin/biome
  echo "stub bun: installed 1986 packages"
  ;;
fail)
  # The filler first, the awkward characters last: the report's detail is the log's tail, so this
  # is what proves the escaping rather than the truncation.
  head -c 4000 /dev/zero | tr '\0' x
  printf '\nerror: he said "quoted", wrote C:\\path\\to and a\ttab\n'
  printf 'error: h\xc3\xa9llo \xe2\x9c\x93 is not ascii\n'
  exit 1
  ;;
killed) kill -9 $$ ;;
hang) exec sleep 300 ;;
esac
STUB
  chmod 755 "$case_root/bin/bun"
}

# run_setup <working directory> [NAME=value …]: runs the script under /bin/sh with the stub first
# on PATH and nothing of this harness's own environment. Not a subshell, so `out` (its output,
# stdout and stderr together) and `status` survive the call.
run_setup() {
  local cwd=$1
  shift
  set +e
  out=$(cd "$cwd" && env -i \
    PATH="$case_root/bin:$PATH" \
    HOME="$case_root/home" \
    TMPDIR="$case_root/tmp" \
    STUB_RECORD="$record" \
    "$@" \
    sh "$setup_script" 2>&1)
  status=$?
  set -e
}

# exists <path>: true/false for check.
exists() { [ -e "$1" ] && echo true || echo false; }

# calls: how many times the stub bun ran in this case.
calls() {
  local count=0
  if [ -e "$record" ]; then
    count=$(grep -c '^args: ' "$record" || true)
  fi
  echo "$count"
}

# field <name>: one field of this case's report, as python's json module reads it.
field() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$report" "$1"
}

# parses <path>: true/false — the file is valid JSON.
parses() {
  python3 -c 'import json,sys;json.load(open(sys.argv[1]))' "$1" > /dev/null 2>&1 && echo true || echo false
}

# at_most <actual> <bound>: true/false for check.
at_most() { [ "$1" -le "$2" ] && echo true || echo false; }

# check_clean: the container's own ephemeral storage is never written to — a breach evicts the
# whole pod and every role's work in it.
check_clean() {
  local spill
  spill=$(find "$case_root/home" "$case_root/tmp" -mindepth 1 | wc -l)
  check "nothing written to \$HOME or \$TMPDIR" "$(is "$spill" 0)"
}

echo "case: no LEGION_WORKSPACE — a developer's own clone"
fixture no-workspace
run_setup "$workspace"
check "exits 0" "$(is "$status" 0)"
check "says nothing" "$(is "$out" "")"
check "the stub never ran" "$(is "$(calls)" 0)"
check "no install root is created" "$(is "$(exists "$install_root/bun")" false)"
check "no report is written" "$(is "$(exists "$report")" false)"
check_clean

echo "case: no bun.lock in the working directory — the controller, or the other repository"
fixture no-lockfile
rm "$workspace/bun.lock"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir"
check "exits 0" "$(is "$status" 0)"
check "says nothing" "$(is "$out" "")"
check "the stub never ran" "$(is "$(calls)" 0)"
check "no install root is created" "$(is "$(exists "$install_root/bun")" false)"
check "no report is written" "$(is "$(exists "$report")" false)"
check_clean

echo "case: a fresh workspace"
fixture fresh
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the stub ran exactly once" "$(is "$(calls)" 1)"
check "with install --frozen-lockfile" "$(contains "$(cat "$record")" '^args: install --frozen-lockfile$')"
check "with the cache under the derived root" "$(contains "$(cat "$record")" "^cache: $cache\$")"
check "in the workspace" "$(contains "$(cat "$record")" "^pwd: $workspace\$")"
check "the marker holds the lock file's cksum" "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check "the install's output reaches the role container's log" "$(contains "$out" 'stub bun: installed 1986 packages')"
check "no report is written" "$(is "$(exists "$report")" false)"
check_clean

echo "case: the deployment already names bun's download cache"
fixture pod-cache
pod_cache=$case_root/pod/bun/install/cache
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  BUN_INSTALL_CACHE_DIR="$pod_cache" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the install uses the deployment's cache, not one of its own" \
  "$(contains "$(cat "$record")" "^cache: $pod_cache\$")"
check "and makes none beside the state" "$(is "$(exists "$install_root/bun/cache")" false)"
check "while the marker still goes beside the state" "$(is "$(exists "$marker")" true)"
check_clean

echo "case: the marker matches and the packages are there"
fixture already-installed
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
: > "$record"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the stub never ran again" "$(is "$(calls)" 0)"
check "and it says why" "$(contains "$out" 'already installed')"
check_clean

echo "case: the marker matches but node_modules was removed"
fixture node-modules-gone
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
rm -rf "$workspace/node_modules"
: > "$record"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the stub ran again" "$(is "$(calls)" 1)"
check_clean

echo "case: a forward merge moves bun.lock mid-issue"
fixture lock-moved
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
first=$(cat "$marker")
printf 'a forward merge moved this lock\n' >> "$workspace/bun.lock"
: > "$record"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the stub ran again" "$(is "$(calls)" 1)"
check "the marker is the new lock's cksum" "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check "which is not the one before the merge" "$(is "$(is "$(cat "$marker")" "$first")" false)"
check_clean

echo "case: another role's install holds the lock"
fixture lock-held
mkdir -p "$state"
ready="$case_root/holder-ready"
(
  exec 9> "$state/install.lock"
  flock 9
  : > "$ready"
  exec sleep 30
) &
holder=$!
for _ in $(seq 200); do
  if [ -e "$ready" ]; then
    break
  fi
  sleep 0.05
done
check "the holder took the lock" "$(exists "$ready")"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_LOCK_WAIT=1 STUB_MODE=ok
kill "$holder" > /dev/null 2>&1 || true
wait "$holder" > /dev/null 2>&1 || true
check "exits 0 rather than waiting the install out" "$(is "$status" 0)"
check "the stub never ran" "$(is "$(calls)" 0)"
check "a report names the lock" "$(is "$(field reason)" 'lock-timeout')"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: the install fails"
fixture install-failed
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=fail
check "exits 0 anyway" "$(is "$status" 0)"
check "the failure reaches the role container's log" "$(contains "$out" 'bun install exited 1')"
check "the report names the failure" "$(is "$(field reason)" 'install-failed')"
check "and carries the status" "$(is "$(field exitStatus)" 1)"
check "the detail is bounded" "$(at_most "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["detail"]))' "$report")" 1000)"
check "the detail is one line" "$(is "$(field detail | wc -l)" 1)"
check "an unset issue, role and generation are empty strings" "$(is "$(field issue)$(field role)$(field generation)" "")"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: the install is killed (a 1 Gi role, the kernel reclaiming its memory)"
fixture install-killed
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=killed
check "exits 0 anyway" "$(is "$status" 0)"
check "the report names the kill" "$(is "$(field reason)" 'install-killed')"
check "and carries the status" "$(is "$(field exitStatus)" 137)"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: the install hangs"
fixture install-hangs
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_INSTALL_TIMEOUT=1 STUB_MODE=hang
check "exits 0 anyway" "$(is "$status" 0)"
check "the report names the timeout" "$(is "$(field reason)" 'install-timeout')"
check "and carries timeout's status" "$(is "$(field exitStatus)" 124)"
check "no marker is left" "$(is "$(exists "$marker")" false)"
stub_pid=$(sed -n 's/^pid: //p' "$record" | tail -n 1)
check "the stub's process is gone" "$(kill -0 "$stub_pid" > /dev/null 2>&1 && echo false || echo true)"
check_clean

echo "case: the report identifies the run that wrote it, whatever bun printed"
fixture report-identity
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_ISSUE=INFRA-71 LEGION_ROLE=implementer LEGION_GENERATION=3 STUB_MODE=fail
check "exits 0" "$(is "$status" 0)"
check "the report is valid JSON" "$(parses "$report")"
check "it carries this run's issue" "$(is "$(field issue)" 'INFRA-71')"
check "its role" "$(is "$(field role)" 'implementer')"
check "its generation" "$(is "$(field generation)" '3')"
check "the schema version" "$(is "$(field schemaVersion)" '1')"
check "and what kind of report it is" "$(is "$(field kind)" 'install-failed')"
check "at is RFC 3339 UTC" "$(contains "$(field at)" '^[0-9]\{4\}-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z$')"
check "the stub's quotes survive the escaping" "$(contains "$(field detail)" 'he said "quoted"')"
check "and its backslashes" "$(contains "$(field detail)" 'C:\\path\\to')"
echo "  the report this case wrote:"
cat "$report"
check_clean

echo "case: a later install clears the report it failed with"
fixture report-cleared
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=fail
check "the failed run wrote a report" "$(exists "$report")"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the stale report is gone" "$(is "$(exists "$report")" false)"
check "and the marker is written" "$(exists "$marker")"
check_clean

echo "case: LEGION_STATE_DIR unset"
fixture no-state-dir
run_setup "$workspace" LEGION_WORKSPACE="$workspace" STUB_MODE=fail
check "exits 0 with nowhere to report" "$(is "$status" 0)"
check "and writes nothing there" "$(is "$(find "$state_dir" -mindepth 1 | wc -l)" 0)"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: LEGION_STATE_DIR is a directory this user cannot write to"
fixture unwritable-state-dir
chmod 500 "$state_dir"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=fail
check "exits 0 anyway" "$(is "$status" 0)"
if touch "$state_dir/probe" 2> /dev/null; then
  # uid 0 ignores a directory's mode bits. CI and a role container (uid 1000) both run as an
  # ordinary user, where the branch below is the one that runs.
  rm -f "$state_dir/probe"
  check "this user ($(id -u)) can write there regardless, so the report is written" "$(exists "$report")"
else
  check "no report is written" "$(is "$(exists "$report")" false)"
fi
chmod 700 "$state_dir"
check_clean

echo "case: a workspace with no /workspaces/ component"
fixture fallback
plain="$case_root/plain"
mkdir -p "$plain"
cp "$workspace/bun.lock" "$plain/bun.lock"
run_setup "$plain" LEGION_WORKSPACE="$plain" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "exits 0" "$(is "$status" 0)"
check "the cache is under .legion-setup" "$(contains "$(cat "$record")" "^cache: $plain/\.legion-setup/bun/cache\$")"
check "the marker too" "$(exists "$plain/.legion-setup/bun/state/installed")"
check "and nothing is written beside the workspace" "$(is "$(exists "$install_root/bun")" false)"
check_clean

summary "scripts/legion-setup.sh"
