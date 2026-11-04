#!/usr/bin/env bash
# Tests for `disk_watch.py`: `start` writes the state file and leaves a sampler that outlives it,
# the sampler's least value falls when the watched file system fills, `report` stops the sampler
# and prints the four numbers, and it exits 0 whether the margin is ok or short and when there is
# no state file at all.
#
# `start` runs here with a PATH holding python3 alone, so neither `go` nor `docker` is on it:
# that is the runner this script must work on too (no Go toolchain, no Docker daemon, no
# database), and it proves the two tools are named and skipped rather than failing the step. The
# two margins are driven from state files written here by hand, so the numbers in the line are
# chosen rather than measured; the falling-least case is measured, against 64 MiB of real writes.
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/disk_watch.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
watch="$script_dir/disk_watch.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
pid=""
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# A PATH with python3 and nothing else on it: no go, no docker. The script's own shebang finds
# python3 through it, and the sampler it starts is started by sys.executable, an absolute path.
mkdir "$work/bin"
ln -s "$(command -v python3)" "$work/bin/python3"

# run_watch <arguments...>: runs the script under test with that bare PATH, leaving its merged
# output in $out and its status in $status.
run_watch() {
  set +e
  out=$(env -i PATH="$work/bin" HOME="$work" TMPDIR="$work" "$watch" "$@" 2>&1)
  status=$?
  set -e
}

# field <state file> <name>: one value of the state file, or '' when it has no such name.
field() {
  python3 -c '
import json, sys
print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))
' "$1" "$2"
}

# alive <pid>: true/false for check.
alive() { kill -0 "$1" 2>/dev/null && echo true || echo false; }

watched="$work/watched"
mkdir "$watched"
state="$work/state.json"

echo "case: start writes the state file, with no go and no docker on PATH"
run_watch start --path "$watched" --state "$state"
check "exits 0" "$(is "$status" 0)"
check "names the file system it watches" "$(contains "$out" "^disk: $watched on .*total [0-9][0-9]* kB, free [0-9][0-9]* kB$")"
check "names the missing go rather than failing" "$(contains "$out" '^disk: no go on PATH, so `go env GOCACHE` names no directory$')"
check "names the missing docker rather than failing" "$(contains "$out" '^disk: no docker on PATH, so `docker info')"
check "says what it is watching and from how much" "$(contains "$out" "^disk: watching $watched, free [0-9][0-9]* kB, sampler pid [0-9][0-9]*$")"
check "leaves a state file" "$([ -f "$state" ] && echo true || echo false)"
before=$(field "$state" before_kb)
check "the state carries the before number" "$(contains "$before" '^[0-9][0-9]*$')"
pid=$(field "$state" sampler_pid)
check "the state carries a sampler pid" "$(contains "$pid" '^[0-9][0-9]*$')"
check "the state carries the watched path" "$(is "$(field "$state" path)" "$watched")"

echo "case: the detached sampler outlives the process that started it"
check "the sampler is alive after start returned" "$(alive "$pid")"

echo "case: the least value falls when the watched file system fills"
dd if=/dev/zero of="$watched/filler" bs=1M count=64 status=none
least=$before
for _ in $(seq 20); do
  sleep 0.5
  least=$(field "$state" least_kb)
  [ "$least" -lt "$before" ] && break
done
check "least_kb fell below before_kb after 64 MiB was written" \
  "$([ "$least" -lt "$before" ] && echo true || echo false)"
check "before_kb is unchanged" "$(is "$(field "$state" before_kb)" "$before")"
rm -f "$watched/filler"

echo "case: report stops the sampler"
run_watch report --state "$state"
check "exits 0" "$(is "$status" 0)"
check "prints the four numbers" "$(contains "$out" '^disk: before [0-9][0-9]* kB, least [0-9][0-9]* kB, used [0-9][0-9]* kB, margin \(ok\|short\)$')"
check "the sampler is gone" "$(is "$(alive "$pid")" false)"
pid=""

echo "case: report prints the four numbers and exits 0 for a margin that is ok"
ok_state="$work/ok.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 600, "sampler_pid": null}\n' "$watched" > "$ok_state"
run_watch report --state "$ok_state"
check "exits 0" "$(is "$status" 0)"
check "prints before, least, used and an ok margin" \
  "$(is "$out" 'disk: before 1000 kB, least 600 kB, used 400 kB, margin ok')"

echo "case: report exits 0 for a short margin too"
short_state="$work/short.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 300, "sampler_pid": null}\n' "$watched" > "$short_state"
run_watch report --state "$short_state"
check "exits 0" "$(is "$status" 0)"
check "prints the shortfall as a short margin" \
  "$(is "$out" 'disk: before 1000 kB, least 300 kB, used 700 kB, margin short')"

echo "case: report on a missing state file exits 0 and says so"
run_watch report --state "$work/never-written.json"
check "exits 0" "$(is "$status" 0)"
check "says there was no state file to read" \
  "$(contains "$out" "^disk: no state file to read at $work/never-written.json, nothing measured$")"

summary "disk_watch.py"
