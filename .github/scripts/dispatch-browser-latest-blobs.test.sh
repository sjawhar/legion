#!/usr/bin/env bash
# Tests for `dispatch-browser-latest-blobs.sh`: the merge job of dispatch-browser.yaml must judge
# a re-run on the attempt that ran, so each case builds a download directory of real directories
# and files - the shape `actions/download-artifact` leaves without `merge-multiple` - and reads
# what the script wrote.
#
# The file contents stand in for the Playwright blob zips: this script picks files, it does not
# read them, so a line of text per file is what tells one attempt's copy from another's. That a
# real merge of two attempts' blobs turns on which copy won is proved against Playwright itself,
# in the pull request that added this script.
#
# NO CASE REACHES THE NETWORK OR PLAYWRIGHT: each one is directories, files and the script.
#
# Run from anywhere: .github/scripts/dispatch-browser-latest-blobs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
select_script="$script_dir/dispatch-browser-latest-blobs.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: an empty download directory and an empty output directory for one case. Echoes
# the case's root, which holds `down` and `out`. A repeated name is refused here rather than
# surfacing as one case reading another's files.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "two cases named $1" >&2
    return 1
  fi
  mkdir -p "$root/down" "$root/out"
  echo "$root"
}

# leg <root> <leg> <attempt> <mark>: the artifact one leg's upload leaves at one attempt - a
# directory named for the artifact, holding the leg's blob report and the row count beside it.
# <mark> is written into both files, so the output says which attempt's copy arrived.
leg() {
  local dir="$1/down/dispatch-browser-blob-$2-$3"
  mkdir -p "$dir/blob-report" "$dir/rows"
  printf '%s\n' "$4" > "$dir/blob-report/report-$2.zip"
  printf '%s\n' "$4" > "$dir/rows/$2.txt"
}

# run_select <root>: runs the script under test over that case's directories, setting `out` to
# its combined output and `status` to its exit code. Not a subshell, so both survive the call.
run_select() {
  set +e
  out=$("$select_script" "$1/down" "$1/out" 2>&1)
  status=$?
  set -e
}

# held <root> <path>: the one line of a file the script wrote, or `absent`.
held() {
  if [ -f "$1/out/$2" ]; then cat "$1/out/$2"; else echo absent; fi
}

echo "case: the newest attempt of a leg wins"
root=$(fixture newest-wins)
leg "$root" shard-1 1 first-attempt
leg "$root" shard-1 2 second-attempt
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the output holds attempt 2's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" second-attempt)"
check "and attempt 2's rows" "$(is "$(held "$root" rows/shard-1.txt)" second-attempt)"
check "and nothing of attempt 1" "$(is "$(grep -rl first-attempt "$root/out" | wc -l)" 0)"
check "says which attempt it took" "$(contains "$out" '^shard-1: attempt 2, passing over attempt(s) 1$')"

echo "case: a leg the re-run did not run keeps its earlier attempt"
# `gh run rerun --failed` re-runs the failed legs alone, so a leg that was green has no artifact
# at the new attempt. Dropping it would merge a report short of that leg's rows.
root=$(fixture rerun-failed-only)
leg "$root" shard-1 1 shard-1-first
leg "$root" shard-1 2 shard-1-second
leg "$root" shard-2 1 shard-2-first
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the re-run leg is at attempt 2" "$(is "$(held "$root" blob-report/report-shard-1.zip)" shard-1-second)"
check "the leg not re-run keeps attempt 1's blob" "$(is "$(held "$root" blob-report/report-shard-2.zip)" shard-2-first)"
check "and attempt 1's rows" "$(is "$(held "$root" rows/shard-2.txt)" shard-2-first)"
check "which it says it took" "$(contains "$out" '^shard-2: attempt 1$')"

echo "case: attempt 10 beats attempt 9"
# The attempts are compared as numbers. Read as text, attempt 10 would sort below attempt 9 and
# the tenth run of a lane would be judged on the ninth's rows.
root=$(fixture attempt-ten)
leg "$root" shard-1 9 ninth
leg "$root" shard-1 10 tenth
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the output holds attempt 10's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" tenth)"
check "says it passed over attempt 9" "$(contains "$out" '^shard-1: attempt 10, passing over attempt(s) 9$')"

echo "case: every leg shape reads"
# A shard leg's name ends with its number and an alone leg's with its project, so the digits at
# the end of the artifact name are the attempt in both.
root=$(fixture leg-shapes)
leg "$root" shard-3 1 shard-3-blob
leg "$root" alone-webkit-alone 1 webkit-blob
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the shard leg is selected" "$(is "$(held "$root" blob-report/report-shard-3.zip)" shard-3-blob)"
check "the alone leg is selected" "$(is "$(held "$root" blob-report/report-alone-webkit-alone.zip)" webkit-blob)"
check "and named with its attempt" "$(contains "$out" '^alone-webkit-alone: attempt 1$')"

echo "case: a directory whose name does not read fails"
# A name ending in anything but digits carries no attempt, so which of two copies of a leg is
# the newer cannot be read from it. The whole step fails rather than merge a set it cannot
# order: a blob dropped quietly is a merged report short of a leg's rows, which reads as a
# shorter suite and not as an error.
root=$(fixture name-unreadable)
leg "$root" shard-1 1 shard-1-first
mkdir -p "$root/down/dispatch-browser-blob-shard-1-retry/blob-report"
run_select "$root"
check "fails" "$(is "$status" 1)"
check "names the directory" "$(contains "$out" 'down/dispatch-browser-blob-shard-1-retry is not a leg')"
check "says what the name must be" "$(contains "$out" 'dispatch-browser-blob-<leg>-<attempt>')"
check "and what to do about it" "$(contains "$out" 'narrow the download step.s pattern')"
check "and merges nothing" "$(is "$(grep -rl shard-1-first "$root/out" | wc -l)" 0)"

root=$(fixture name-no-leg)
mkdir -p "$root/down/dispatch-browser-blob-2/blob-report"
run_select "$root"
check "a name with an attempt and no leg fails too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-blob-2 is not a leg')"

root=$(fixture name-not-a-directory)
printf 'stray\n' > "$root/down/dispatch-browser-blob-shard-1-1"
run_select "$root"
check "a file where a directory was expected fails too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-blob-shard-1-1 is not a leg')"

echo "case: an empty download directory writes empty output and does not fail"
# The merge step after this one fails when there is nothing to merge, and the merge job's first
# reading names the legs that owed a blob, which is what says why.
root=$(fixture download-empty)
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "says so in one line" "$(contains "$out" 'No leg.s blob artifact was downloaded into')"
check "writes the blob directory" "$(is "$(find "$root/out/blob-report" -mindepth 1 | wc -l)" 0)"
check "writes the rows directory" "$(is "$(find "$root/out/rows" -mindepth 1 | wc -l)" 0)"

root=$(fixture download-absent)
rmdir "$root/down"
run_select "$root"
check "an absent download directory is the same" "$(is "$status" 0)"
check "and the output directories are written" "$(is "$(find "$root/out" -type d | wc -l)" 3)"

echo "case: the rows file travels with its blob"
# A leg's rows are the count its log printed, which reading one holds against the merged report.
# Taking one attempt's blob beside another's rows would report a mismatch that never happened.
root=$(fixture rows-travel)
leg "$root" shard-1 1 first-attempt
leg "$root" shard-1 2 second-attempt
leg "$root" shard-2 2 shard-2-second
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "each selected leg's blob and rows are the one attempt's" \
  "$(is "$(held "$root" blob-report/report-shard-1.zip)" "$(held "$root" rows/shard-1.txt)")"
check "for every leg" \
  "$(is "$(held "$root" blob-report/report-shard-2.zip)" "$(held "$root" rows/shard-2.txt)")"
check "and the output holds one file per leg per directory" \
  "$(is "$(find "$root/out" -type f | wc -l)" 4)"

echo "case: a leg name holding a space is one leg"
# This script never sees the merge job's capture: a leg reaches it as the tail of an artifact
# directory name, which dispatch-browser.yaml builds from the project alone
# (`dispatch-browser-blob-alone-<project>-<attempt>`). So a project named with a space arrives
# here as one leg holding a space. Read with word splitting, as this loop was, that name becomes
# two legs, neither with a blob or a row count of its own.
root=$(fixture leg-with-space)
leg "$root" "alone-chromium hidpi" 1 spaced
leg "$root" shard-1 1 plain
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "selects two legs, not three" "$(contains "$out" 'Selected 2 leg(s)')"
check "and names the spaced leg whole" "$(contains "$out" 'alone-chromium hidpi: attempt 1')"
check "its blob lands under its own name" \
  "$(is "$(held "$root" 'blob-report/report-alone-chromium hidpi.zip')" spaced)"
check "and its rows travel with it" \
  "$(is "$(held "$root" 'rows/alone-chromium hidpi.txt')" spaced)"

echo "case: a call that names no directories"
set +e
out=$("$select_script" 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "says what it takes" "$(contains "$out" '<download-dir> <out-dir>')"

summary "dispatch-browser-latest-blobs.sh"
