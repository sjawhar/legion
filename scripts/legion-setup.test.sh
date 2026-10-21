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
# arguments, the working directory it ran in, the cache directory it was given, the log file it
# was handed as its output and its own pid — and then behaves as STUB_MODE says.
set -u
# The log by fd 2 rather than fd 1: inside a command substitution fd 1 is the substitution's own
# pipe, while fd 2 is still the file the script redirected this install onto. /proc is Linux's,
# as are the flock and the timeout the script under test is built on.
log_path=$(readlink /proc/self/fd/2 || true)
{
  echo "args: $*"
  echo "pwd: $PWD"
  echo "cache: ${BUN_INSTALL_CACHE_DIR-}"
  echo "log: $log_path"
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
cold-race)
  # The cold-entry race Legion's own plugin install met (plugins.go): the first attempt dies
  # filling the cache, the second, against the cache the other install just filled, succeeds. The
  # counter is a file, since each attempt is a new process. The message is the one plugins.go
  # measured unless a case names another, so a case can hold the retry to the part of the
  # sentence the script matches rather than to the whole of it.
  attempts="$STUB_RECORD.attempts"
  printf 'x' >> "$attempts"
  if [ "$(wc -c < "$attempts")" -eq 1 ]; then
    echo "${STUB_COLD_RACE_MESSAGE:-error: ENOENT: failed copying files from cache to destination}"
    exit 1
  fi
  mkdir -p node_modules/.bin
  printf '#!/bin/sh\necho 2.3.5\n' > node_modules/.bin/biome
  chmod 755 node_modules/.bin/biome
  echo "stub bun: installed 1986 packages"
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
# --backend=copyfile, because that cache and this node_modules are on one filesystem, where bun's
# default backend hardlinks the installed files out of the cache every role of the issue shares
# (plugins.go:155-163, and the install's own comment).
check "with install --backend=copyfile --frozen-lockfile" \
  "$(contains "$(cat "$record")" '^args: install --backend=copyfile --frozen-lockfile$')"
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

echo "case: another install of this issue holds the cache for longer than the whole budget"
fixture lock-held
mkdir -p "$cache"
ready="$case_root/holder-ready"
(
  exec 9< "$cache"
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
check "the holder took the cache's lock" "$(exists "$ready")"
# The budget, not a lock wait of its own: the run gives up when its whole wall clock is spent,
# which is what keeps the role's registration deadline out of a holder's hands. The holder lives
# far longer than the budget, so the elapsed time is what proves the bound rather than the reason
# alone — a run that waited the holder out would report the same lock-timeout 30 s later.
began=$(date +%s)
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=2 STUB_MODE=ok
elapsed=$(($(date +%s) - began))
kill "$holder" > /dev/null 2>&1 || true
wait "$holder" > /dev/null 2>&1 || true
echo "  the run took ${elapsed}s of its 2s budget, against a holder that holds the lock for 30s"
check "exits 0 rather than waiting the install out" "$(is "$status" 0)"
check "within the budget, not the holder's lifetime" "$(at_most "$elapsed" 8)"
check "the stub never ran" "$(is "$(calls)" 0)"
check "a report names the lock" "$(is "$(field reason)" 'lock-timeout')"
check "and the budget it waited out" "$(contains "$(field detail)" "whole 2s budget")"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: a run that never got the lock leaves the marker another role wrote"
# The holder writes the marker as its last act inside the lock, so a `-w` that expires in that
# window must not delete a marker that is true: this run looked at no node_modules and learned
# nothing about what is installed. Deleting it would cost every later role of the issue a full
# reinstall for a fact this run never observed.
fixture lock-held-marker
mkdir -p "$state" "$cache"
ready="$case_root/holder-ready"
(
  exec 9< "$cache"
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
check "the holder took the cache's lock" "$(exists "$ready")"
printf 'a marker another role wrote\n' > "$marker"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=2 STUB_MODE=ok
kill "$holder" > /dev/null 2>&1 || true
wait "$holder" > /dev/null 2>&1 || true
check "exits 0" "$(is "$status" 0)"
check "reports the lock" "$(is "$(field reason)" 'lock-timeout')"
check "and leaves the other role's marker exactly as it was" \
  "$(is "$(cat "$marker")" 'a marker another role wrote')"
check_clean

echo "case: another install filled the cache under this one (the cold-entry race)"
fixture cold-race
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  STUB_MODE=cold-race
check "exits 0" "$(is "$status" 0)"
check "the install is retried once" "$(is "$(calls)" 2)"
check "and the retry is announced" "$(contains "$out" 'retrying once')"
check "the marker is written after the retry" "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check "no report is left" "$(is "$(exists "$report")" false)"
check_clean

echo "case: the retry of the cold-entry race takes its wait from what is left of the budget"
fixture cold-race-in-budget
began=$(date +%s)
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=30 STUB_MODE=cold-race
elapsed=$(($(date +%s) - began))
check "exits 0" "$(is "$status" 0)"
check "both attempts happened" "$(is "$(calls)" 2)"
check "inside the budget they shared" "$(at_most "$elapsed" 30)"
check "the marker is written after the retry" "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check_clean

echo "case: the cold-entry race with too little budget left for a second attempt"
fixture cold-race-out-of-budget
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=5 STUB_MODE=cold-race
check "exits 0 anyway" "$(is "$status" 0)"
check "the install is not retried into the registration deadline" "$(is "$(calls)" 1)"
check "and the retry is not announced" "$(is "$(contains "$out" 'retrying once')" false)"
check "the report names the failure the attempt died with" "$(is "$(field reason)" 'install-failed')"
check "and carries its status" "$(is "$(field exitStatus)" 1)"
check "the detail is what bun said" "$(contains "$(field detail)" 'ENOENT: failed copying files')"
check "no marker is left" "$(is "$(exists "$marker")" false)"
check_clean

echo "case: the install fails"
fixture install-failed
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=fail
check "exits 0 anyway" "$(is "$status" 0)"
check "the install is not retried, since this is not the cache race" "$(is "$(calls)" 1)"
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
  LEGION_SETUP_BUDGET=1 STUB_MODE=hang
check "exits 0 anyway" "$(is "$status" 0)"
check "the report names the timeout" "$(is "$(field reason)" 'install-timeout')"
check "and carries timeout's status" "$(is "$(field exitStatus)" 124)"
check "no marker is left" "$(is "$(exists "$marker")" false)"
stub_pid=$(sed -n 's/^pid: //p' "$record" | tail -n 1)
check "the stub's process is gone" "$(kill -0 "$stub_pid" > /dev/null 2>&1 && echo false || echo true)"
check_clean

echo "case: the hold bounds how long one attempt keeps the cache's lock"
# The lock is not ours alone: the daemon's own plugin install takes the same inode in the same
# container and fails the launch when its wait runs out, so an install that cannot finish inside
# LEGION_SETUP_LOCK_HOLD yields the lock rather than the budget's whole 270 s. The budget here is
# large, so only the hold can end this run — and the lock must be free the moment it does.
fixture lock-hold
began=$(date +%s)
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=120 LEGION_SETUP_LOCK_HOLD=1 STUB_MODE=hang
elapsed=$(($(date +%s) - began))
echo "  the run took ${elapsed}s against a 120s budget and a 1s hold"
check "exits 0" "$(is "$status" 0)"
check "the hold ended it, not the budget" "$(at_most "$elapsed" 15)"
check "the report names the timeout" "$(is "$(field reason)" 'install-timeout')"
check "and carries timeout's status" "$(is "$(field exitStatus)" 124)"
check "it vouches for nothing: no marker" "$(is "$(exists "$marker")" false)"
# The release is the point of the bound: another process must be able to take the lock at once.
flock -w 2 "$cache" true && took=true || took=false
check "and the lock is free the moment the run ends" "$(is "$took" true)"
# What the next role's locked re-check must judge: a tree a cut-off install left behind, with no
# marker, is reinstalled rather than taken for a finished one — the marker is the only thing
# standing between a half-written node_modules and a role that skips the install.
mkdir -p "$workspace/node_modules/.bin"
printf '#!/bin/sh\necho 2.3.5\n' > "$workspace/node_modules/.bin/biome"
chmod 755 "$workspace/node_modules/.bin/biome"
: > "$record"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
check "the next role installs over what the cut-off run left" "$(is "$(calls)" 1)"
check "and only then is a marker written" \
  "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
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

echo "case: the cold-entry race worded another way"
fixture cold-race-reworded
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  STUB_COLD_RACE_MESSAGE='error: failed copying files from cache into node_modules (ENOENT)' \
  STUB_MODE=cold-race
check "exits 0" "$(is "$status" 0)"
check "the install is retried on the part of the message both wordings share" "$(is "$(calls)" 2)"
check "and the retry is announced" "$(contains "$out" 'retrying once')"
check "the marker is written after the retry" "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check "no report is left" "$(is "$(exists "$report")" false)"
check_clean

echo "case: a waiter arrives behind an install that finished while it queued"
fixture waiter-behind-install
mkdir -p "$cache" "$state"
ready="$case_root/holder-ready"
# The handshake is the run's own log: the script makes it only after its outer check has found
# nothing installed, so a holder that waits for that file and only then writes the tree is
# writing behind that check — which is the race two roles of one issue starting together run.
# Without the handshake this case would be a sleep against a check microseconds wide, and the
# fast path would sometimes answer where the re-check under the lock is what is being tested.
(
  exec 9< "$cache"
  flock 9
  : > "$ready"
  for _ in $(seq 400); do
    if [ -n "$(find "$state" -name 'install.*' -print -quit 2> /dev/null)" ]; then
      break
    fi
    sleep 0.05
  done
  mkdir -p "$workspace/node_modules/.bin"
  printf '#!/bin/sh\necho 2.3.5\n' > "$workspace/node_modules/.bin/biome"
  chmod 755 "$workspace/node_modules/.bin/biome"
  cksum < "$workspace/bun.lock" > "$marker"
) &
holder=$!
for _ in $(seq 200); do
  if [ -e "$ready" ]; then
    break
  fi
  sleep 0.05
done
check "the holder took the cache's lock" "$(exists "$ready")"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" \
  LEGION_SETUP_BUDGET=60 STUB_MODE=ok
wait "$holder" > /dev/null 2>&1 || true
check "exits 0" "$(is "$status" 0)"
check "it queued for the lock rather than taking the fast path" \
  "$(contains "$out" 'installing this bun.lock')"
check "and the re-check under the lock found the install finished" \
  "$(contains "$out" 'another role installed this bun.lock while this run waited; nothing to do')"
check "and it claims no install of its own, which it did not make" \
  "$(is "$(contains "$out" 'installed this bun.lock.s packages into')" false)"
check "so no second install ran over the tree the other role's agent is working in" \
  "$(is "$(calls)" 0)"
check "the marker that role wrote is left exactly as it was" \
  "$(is "$(cat "$marker")" "$(cksum < "$workspace/bun.lock")")"
check "and no report tells this agent the packages are missing" "$(is "$(exists "$report")" false)"
check_clean

echo "case: each run's log is its own, and goes when the run goes"
fixture per-run-log
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=fail
first_log=$(sed -n 's/^log: //p' "$record" | tail -n 1)
check "the install's output went to a log of this run's own" \
  "$(contains "$first_log" "^$state/install\.")"
check "whose tail is what the report quotes" "$(contains "$(field detail)" 'he said "quoted"')"
check "and which is removed when the run ends" "$(is "$(exists "$first_log")" false)"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=killed
second_log=$(sed -n 's/^log: //p' "$record" | tail -n 1)
check "the next run writes to a file of its own, never the one before it" \
  "$(is "$(is "$second_log" "$first_log")" false)"
check "its report is its own run's" "$(is "$(field reason)" 'install-killed')"
check "so its detail cannot be the earlier run's" "$(is "$(contains "$(field detail)" 'quoted')" false)"
check "and no log is left behind on the issue volume" \
  "$(is "$(find "$state" -name 'install.*' | wc -l)" 0)"
check_clean

echo "case: no install root this role can write under"
fixture no-install-root
# The derived root is the workspace's `/workspaces/` parent and the fallback is a directory in
# the workspace itself; a volume this role cannot write to makes both refuse, which is every
# check unavailable for every role of the issue — the failure the report mechanism exists for.
chmod 500 "$install_root" "$workspace"
run_setup "$workspace" LEGION_WORKSPACE="$workspace" LEGION_STATE_DIR="$state_dir" STUB_MODE=ok
if mkdir "$install_root/probe" 2> /dev/null; then
  # uid 0 ignores a directory's mode bits. CI and a role container (uid 1000) both run as an
  # ordinary user, where the branch below is the one that runs.
  rmdir "$install_root/probe"
  check "this user ($(id -u)) can write there regardless, so the install runs" "$(is "$(calls)" 1)"
else
  check "exits 0" "$(is "$status" 0)"
  check "the role container's log says why" "$(contains "$out" 'no writable install root')"
  check "the stub never ran" "$(is "$(calls)" 0)"
  check "the report names the missing install root" "$(is "$(field reason)" 'no-install-root')"
  check "and carries no process's status, because none ran" "$(is "$(field exitStatus)" 0)"
  check "its detail names the roots it tried" "$(contains "$(field detail)" '\.legion-setup')"
  echo "  the report this case wrote:"
  cat "$report"
fi
chmod 700 "$install_root" "$workspace"
check_clean

summary "scripts/legion-setup.sh"
