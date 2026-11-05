#!/usr/bin/env bash
# Tests for `race_memory.py`: a command's status passes through, a peak over the limit stops the
# command and fails the step while the run is still going, naming the tests that were running, a
# run within the limit ends with the peak, and a `go test -json` stream's output reaches the log.
#
# Each case runs the real script around a stand-in command — a shell or a Python program that
# prints a -json stream and grows — so no Go, no race binary and no database are needed. The two
# functions the sampling loop reads its numbers with, `summarize` and `choose_peak`, are called
# on their own, over fixture `/proc/<pid>/status` texts written here, because one of the rule's
# two branches cannot be reached on a kernel that reports no `VmHWM` (gVisor) and the other
# cannot be reached on one that does.
#
# Run from anywhere: .github/scripts/race_memory.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
sampler="$script_dir/race_memory.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# run_sampler <limit in kB, or '' for the default> <command...>: runs the script under test and
# leaves its merged output in $out, its status in $status, and the whole seconds it took in
# $elapsed - which is how a case tells a command that was stopped from one that was waited out.
run_sampler() {
  local limit=$1 began=$SECONDS
  shift
  set +e
  if [ -z "$limit" ]; then
    out=$(env -u RACE_MEM_LIMIT_KB "$sampler" "$@" 2>&1)
  else
    out=$(env "RACE_MEM_LIMIT_KB=$limit" "$sampler" "$@" 2>&1)
  fi
  status=$?
  set -e
  elapsed=$((SECONDS - began))
}

# peak <python expression over race_memory>: the peak functions called with no process involved.
peak() {
  env PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c "$1"
}

# holder <test name> <MiB to hold> <seconds to hold it>: a stand-in command for `python3 -c`. It
# names that test as running on a -json stream, waits two seconds so the sampler's first walk
# finds it still small, holds that many MiB of written bytes for that long, and only then says
# what it held - a line a stand-in that was stopped never reaches.
holder() {
  cat <<PY
import json, sys, time
sys.stdout.write(json.dumps({"Action": "run", "Test": "$1"}) + "\n")
sys.stdout.flush()
time.sleep(2)
block = b"x" * ($2 * 1024 * 1024)
time.sleep($3)
sys.stdout.write("held %d bytes\n" % len(block))
PY
}

echo "case: a failing command's status passes through"
run_sampler '' sh -c 'exit 3'
check "exits 3" "$(is "$status" 3)"
check "names the default limit" "$(contains "$out" 'the limit is 8388608 kB (the default')"

echo "case: a peak over the limit fails the run and names the test that was running"
# 64 MiB held past a 40960 kB limit: the first walk finds the stand-in before it has grown, well
# under the limit, and the walk at 5 s finds it over - by when the stream naming the test has
# been read.
run_sampler 40960 python3 -c "$(holder TestTheSamplerNamesMe 64 6)"
check "exits 1" "$(is "$status" 1)"
check "names the limit it was given" \
  "$(contains "$out" 'the limit is 40960 kB (RACE_MEM_LIMIT_KB)')"
check "names the test that was running" \
  "$(contains "$out" 'these tests were running: TestTheSamplerNamesMe')"
check "says the peak passed the limit" "$(contains "$out" 'passed the limit of 40960 kB')"
check "warns that the peak may be between two samples" \
  "$(contains "$out" 'VmHWM can be reached between two samples')"
check "took a sample at 0 s and another at 5 s" \
  "$([ "$(grep -c -- ' s, VmRSS ' <<<"$out")" -ge 2 ] && echo true || echo false)"

echo "case: the command is stopped at that sample rather than waited out"
# The same limit, 256 MiB held past it, and then a minute of sleep: twelve more samples. The
# limit used to be read only after the command had ended, so a case that asserts the status alone
# would have passed then too; this one asserts the clock.
run_sampler 40960 python3 -c "$(holder TestTheSamplerStopsMe 256 60)"
check "exits 1" "$(is "$status" 1)"
check "stopped the command after ${elapsed} s, well inside the stand-in's 62 s" \
  "$([ "$elapsed" -lt 25 ] && echo true || echo false)"
check "names the test it stopped" \
  "$(contains "$out" 'these tests were running: TestTheSamplerStopsMe')"
check "says it stopped the command" "$(contains "$out" 'Stopping the command')"
check "the stand-in never reached the end of its sleep" \
  "$(is "$(contains "$out" '^held [0-9][0-9]* bytes$')" false)"

echo "case: a run within the limit exits 0 and ends with the peak"
run_sampler '' sh -c 'echo the command spoke; sleep 6'
check "exits 0" "$(is "$status" 0)"
check "passes the command's own output through" "$(contains "$out" '^the command spoke$')"
check "writes a sample line as the run goes, not only at the end" \
  "$([ "$(grep -c -- ' s, VmRSS ' <<<"$out")" -ge 2 ] && echo true || echo false)"
check "its last line gives the peak and the limit" \
  "$(contains "${out##*$'\n'}" '^race memory: the peak was [0-9][0-9]* kB (Vm.*), the limit 8388608 kB$')"

echo "case: the peak is the largest VmHWM when the kernel gives one"
hwm=$(peak '
import race_memory
walk = race_memory.summarize([
    "Name:\tgo\nPPid:\t1\nVmRSS:\t    1000 kB\nVmHWM:\t    4200 kB\n",
    "Name:\tdocs.test\nPPid:\t2\nVmRSS:\t    3000 kB\nVmHWM:\t    3900 kB\n",
], 0.0, "none")
print("%d %s" % race_memory.choose_peak(walk.hwm_kb, walk.rss_kb))
')
check "takes the largest VmHWM, not the largest VmRSS" "$(is "$hwm" "4200 VmHWM")"

echo "case: the peak falls back to the highest sampled VmRSS where there is no VmHWM"
rss=$(peak '
import race_memory
walk = race_memory.summarize([
    "Name:\tgo\nPPid:\t1\nVmRSS:\t    1000 kB\n",
    "Name:\tdocs.test\nPPid:\t2\nVmRSS:\t    3000 kB\n",
], 0.0, "none")
print("%d %s" % race_memory.choose_peak(walk.hwm_kb, walk.rss_kb))
')
check "takes the highest sampled VmRSS" "$(is "$rss" "3000 VmRSS")"

echo "case: a -json stream's output reaches the log"
stream='
import json, sys
for event in [
    {"Action": "run", "Test": "TestA"},
    {"Action": "output", "Test": "TestA", "Output": "--- FAIL: TestA (0.00s)\n"},
    {"Action": "fail", "Test": "TestA"},
    {"Action": "output", "Output": "FAIL\tlegion/internal/dispatch/docs\t0.10s\n"},
]:
    sys.stdout.write(json.dumps(event) + "\n")
'
run_sampler '' python3 -c "$stream"
check "exits 0" "$(is "$status" 0)"
check "the --- FAIL line shows" "$(contains "$out" '^--- FAIL: TestA (0.00s)$')"
check "the package's own line shows" "$(contains "$out" '^FAIL.*legion/internal/dispatch/docs')"
check "the -json envelope does not" "$(is "$(contains "$out" '"Action"')" false)"

summary "race_memory.py"
