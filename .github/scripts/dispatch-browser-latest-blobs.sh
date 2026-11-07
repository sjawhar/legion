#!/usr/bin/env bash
# The newest attempt of each leg's blob report, and no other, for the merge job of
# .github/workflows/dispatch-browser.yaml.
#
# A run's artifact list holds every attempt's artifacts, and the merge job used to download them
# with `merge-multiple: true`: a leg re-run by `gh run rerun --failed` then had two artifacts
# matching the download pattern (`dispatch-browser-blob-shard-1-1` and
# `dispatch-browser-blob-shard-1-2`), whose inner paths are the same file names
# (`blob-report/report-shard-1.zip`, `rows/shard-1.txt`), so the two attempts overwrote one
# another in an order nothing fixes. Driven with real Playwright 1.64.1 blobs, one copy order
# gave a merged report of {"expected":2,"unexpected":0,"flaky":0} and the other
# {"expected":0,"unexpected":1,"flaky":0}: a green re-run could be published red, or a red one
# green.
#
# So the merge job downloads without `merge-multiple`, which leaves each artifact in a directory
# named for it, and this script picks the directories to merge. The semantics are the ones
# .github/scripts/ci-ok-report.sh already gives the same problem for the jobs' ci-ok reports
# (`group_by(.job) | map(max_by(.attempt | tonumber))`): the highest attempt of a leg wins, and a
# leg the re-run did not run keeps the artifact of the attempt that ran it, since a failed-only
# re-run uploads nothing for the legs that were green.
#
# It takes the directory `actions/download-artifact` filled and the directory to write:
#   .github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir>
# Each entry of <download-dir> must be a directory named `dispatch-browser-blob-<leg>-<attempt>`,
# where <attempt> is the run of digits at the end and <leg> is everything between the prefix and
# it (`shard-1`, `alone-webkit-alone`). A name that does not read is a failure, not a file passed
# over: a blob silently dropped is a merged report short of a leg's rows, which reads as a
# shorter suite rather than as an error.
#
# It writes <out-dir>/blob-report and <out-dir>/rows, holding each leg's selected attempt's files
# and no other's, and prints one line per leg saying which attempt it took and which it passed
# over - the merge job's log is where a reader looks when a re-run's number surprises them.
#
# An empty or absent <download-dir> is not a failure here: it writes the two empty directories
# and says so. The merge step after it already fails when there is nothing to merge, and the
# merge job's reading one names the legs that owed a blob.
#
# Run from anywhere: .github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir>
# CI runs it in dispatch-browser.yaml's merge job, at the step "Take the newest attempt of each
# leg's blob report", and its tests in ci.yaml's test job, at "Test the browser lane's blob
# attempt selection".
set -euo pipefail
shopt -s inherit_errexit

if [ "$#" -ne 2 ]; then
  echo "::error::dispatch-browser-latest-blobs.sh takes the download directory and the" \
    "directory to write, and was given $# argument(s):" \
    ".github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir>"
  exit 1
fi
download_dir=$1
out_dir=$2
prefix=dispatch-browser-blob-

mkdir -p "$out_dir/blob-report" "$out_dir/rows"

if [ ! -d "$download_dir" ] || [ -z "$(ls -A "$download_dir" 2>/dev/null)" ]; then
  echo "No leg's blob artifact was downloaded into $download_dir, so $out_dir/blob-report and" \
    "$out_dir/rows are empty."
  exit 0
fi

# The highest attempt per leg, and the attempts it beat. Read in name order, so two runs over one
# download directory report the legs in the same order.
declare -A taken=()        # leg -> the attempt selected, as a number
declare -A taken_dir=()    # leg -> the directory that attempt is in
declare -A passed_over=()  # leg -> the other attempts, in the order they were read
unreadable=()
while IFS= read -r -d '' path; do
  name=${path##*/}
  # <attempt> is the trailing run of digits: `${name##*[!0-9]}` drops everything up to the last
  # character that is not one. A greedy `(.+)-([0-9]+)$` would read
  # dispatch-browser-blob-shard-1-10 as leg `shard-1-1` at attempt 0.
  attempt=${name##*[!0-9]}
  rest=${name%"$attempt"}
  leg=
  case "$rest" in "$prefix"?*-) leg=${rest#"$prefix"}; leg=${leg%-} ;; esac
  if [ ! -d "$path" ] || [ -z "$attempt" ] || [ -z "$leg" ]; then
    unreadable+=("$name")
    continue
  fi
  # Base 10 explicitly: an attempt written 08 is eight, and `$((08))` is a refusal.
  number=$((10#$attempt))
  if [ -z "${taken[$leg]:-}" ]; then
    taken[$leg]=$number
    taken_dir[$leg]=$path
  elif [ "$number" -gt "${taken[$leg]}" ]; then
    passed_over[$leg]="${passed_over[$leg]:-}${passed_over[$leg]:+, }${taken[$leg]}"
    taken[$leg]=$number
    taken_dir[$leg]=$path
  else
    passed_over[$leg]="${passed_over[$leg]:-}${passed_over[$leg]:+, }$number"
  fi
done < <(find "$download_dir" -mindepth 1 -maxdepth 1 -print0 | LC_ALL=C sort -z)

if [ "${#unreadable[@]}" -gt 0 ]; then
  for name in "${unreadable[@]}"; do
    echo "::error::$download_dir/$name is not a leg's blob report: this step reads a directory" \
      "named ${prefix}<leg>-<attempt>, where <attempt> is the run of digits at the end" \
      "(${prefix}shard-1-2, ${prefix}alone-webkit-alone-1). Name the upload in" \
      ".github/workflows/dispatch-browser.yaml that way, or narrow the download step's pattern" \
      "so it does not match this artifact."
  done
  exit 1
fi

# Each leg's selected directory, copied whole: its blob report and the row count that travels
# with it come from the one attempt, so a leg contributes both or neither.
#
# One leg per line, not `for leg in $(…)`: a leg name is an artifact name's tail and a project
# may be named with a space, which word splitting would read as two legs. A here-string rather
# than a pipe only to match the merge job's own loop, where it is load-bearing because that loop
# accumulates its totals; this loop accumulates nothing, so a pipe would have served as well.
while IFS= read -r leg; do
  [ -n "$leg" ] || continue
  src=${taken_dir[$leg]}
  for part in blob-report rows; do
    if [ -d "$src/$part" ]; then
      cp -R "$src/$part/." "$out_dir/$part/"
    fi
  done
  if [ -n "${passed_over[$leg]:-}" ]; then
    echo "$leg: attempt ${taken[$leg]}, passing over attempt(s) ${passed_over[$leg]}"
  else
    echo "$leg: attempt ${taken[$leg]}"
  fi
done <<< "$(printf '%s\n' "${!taken[@]}" | LC_ALL=C sort)"

echo "Selected ${#taken[@]} leg(s) into $out_dir."
