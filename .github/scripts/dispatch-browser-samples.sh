#!/usr/bin/env bash
# The memory and CPU sampler of the Dispatch browser suite's browser steps, and the two readings
# it ends with.
#
# Who runs it, and why it is a script: the "Browser-test dispatch dashboard" step of
# .github/workflows/dispatch-browser.yaml's shard job and the step of that name in its alone
# job. The two jobs repeat their step lists for the reason that file's head gives - a composite
# action carries neither `timeout-minutes` nor `services:` - but that reason is a composite
# action's and not a script's: a step that calls a script keeps its own `timeout-minutes`. So
# this body is written once, both steps call it, and a fix here is a fix for both jobs.
#
# Two modes, the two halves of one reading:
#
#   .github/scripts/dispatch-browser-samples.sh sample <dir> &
#   .github/scripts/dispatch-browser-samples.sh report <dir> "<title>"
#
# `sample` truncates <dir>/memory-samples.txt and <dir>/cpu-samples.txt and then ticks until it
# is killed, so the caller starts it with `&` and keeps its pid, as both browser steps do:
# `sampler=$!` and a `trap` that kills it. It runs in the foreground of its own process on
# purpose - the caller's trap is what stops it, and a script that backgrounded the loop itself
# would hand the caller a pid it did not start.
#
# `report` prints the two `::notice::` lines, titled with the leg the caller names (`shard 1`,
# `webkit-alone`), and changes nothing else: the browser steps run it after the suite and before
# their own `exit`, so a step still ends on the suite's status and not on this reading's.
# <dir> is $RUNNER_TEMP in both jobs, beside the lanes' own run directory and under what the
# steps upload: a file written into the checkout would leave it dirty afterwards.
#
# What the tick records, every 5 s for the length of the browser step:
#
# MemAvailable, from /proc/meminfo. The lanes' Postgres containers run outside the job's cgroup,
# so /proc/meminfo is the one reading that counts them, and the lowest value over the step is
# what the reading prints.
#
# Beside it, where the CPU goes, which is the bound a run of four lanes on four vCPU hits
# (INFRA-317): one line per process of a named group, with the CPU that process has used, read
# from /proc/<pid>/stat. `pidstat` is not on the runner image and `ps -o pcpu` is a process's
# average over its whole life, not what it spent since the last tick, so neither answers this;
# /proc costs one `ps` and one `awk` per tick. A group's total is the sum of its processes'
# ticks apart, and the whole reading of a process this tick first saw, which is what catches a
# renderer that was started after the last tick. Nothing is ever counted twice, so the named
# groups are a floor: what is lost is a process that lived and died inside one tick, and the
# last seconds of one that died between ticks. `everything else` is the machine's own busy time
# (/proc/stat) less the named groups, so a reader sees the whole step without downloading the
# samples: it holds that loss, the runner's own work, any container on the machine (the shard
# job's file store) and the sampler itself. On the alone job it is also the reading that says
# the machine was quiet, which is the claim those rows rest on.
#
# The two sample files are the readings' whole input, which is what its test asserts against
# (dispatch-browser-samples.test.sh): fixture samples in, known numbers and titles out. The
# browser steps upload both files as well, so a reader can take the same numbers again.
#
# Run from anywhere: it reads /proc and writes only inside <dir>.
# CI runs it in the shard and alone jobs of dispatch-browser.yaml, and its tests in ci.yaml's
# test job.
set -euo pipefail
shopt -s inherit_errexit

usage() {
  echo "::error::dispatch-browser-samples.sh takes a mode and its arguments:" \
    ".github/scripts/dispatch-browser-samples.sh sample <dir>, which samples until it is" \
    "killed, or .github/scripts/dispatch-browser-samples.sh report <dir> <title>, which prints" \
    "the two readings. It was given: $*"
  exit 1
}

[ "$#" -ge 2 ] || usage "$@"
mode=$1
dir=$2
memory="$dir/memory-samples.txt"
cpu="$dir/cpu-samples.txt"

case "$mode" in
  sample)
    [ "$#" -eq 2 ] || usage "$@"
    [ -d "$dir" ] || { echo "::error::dispatch-browser-samples.sh sample: $dir is not a directory"; exit 1; }
    # Truncated, not appended to: a step that sampled before (a re-run of the script in one job)
    # would otherwise have its readings taken over two runs of the suite.
    : > "$memory"
    : > "$cpu"
    while :; do
      awk '/^MemAvailable:/ { print $2 }' /proc/meminfo >> "$memory"
      # One tick of the CPU reading: the machine's own busy total first, then one line per
      # process of a named group, `<pid> <ticks> <group>`. The machine line is written by the
      # BEGIN block, so it is always this tick's first line, and the reading below counts a tick
      # by it.
      ps -eo pid=,args= | awk '
        function group(args) {
          if (args ~ /ms-playwright/)     return "browser"
          if (args ~ /(^|\/)bun( |$)/)    return "bun"
          if (args ~ /playwright/)        return "runner"
          if (args ~ /dispatch-e2e( |$)/) return "dispatch"
          if (args ~ /postgres|psql/)     return "postgres"
          return ""
        }
        BEGIN { while ((getline l < "/proc/stat") > 0) if (l ~ /^cpu /) { split(l, c); print "machine", c[2]+c[3]+c[4]+c[7]+c[8]+c[9]; break } }
        { g = group($0); if (g == "") next
          if ((getline l < ("/proc/" $1 "/stat")) <= 0) next
          close("/proc/" $1 "/stat"); sub(/^[^ ]+ \(.*\) /, "", l); split(l, s)
          print $1, s[12] + s[13], g }' >> "$cpu"
      sleep 5
    done
    ;;
  report)
    [ "$#" -eq 3 ] || usage "$@"
    title=$3
    # A workflow command reads `::notice title=<value> <name>::`, where a property's value ends at
    # a `,` and the command's own name at a `:`, and `%` begins an escape. So a title holding any
    # of them is encoded here, once, rather than at each of the two `::notice` lines below: an
    # alone leg's title is its project name (dispatch-browser.yaml:708), and a project named
    # `a,b-alone` would otherwise be titled `a`. No name of today's matrix holds one - the install
    # step refuses an engine name a comma or a space would produce - so this keeps a title true
    # rather than fixing a fault anyone has seen. `%` is replaced first, or it would re-encode the
    # `%` of the two replacements after it.
    title=${title//%/%25}
    title=${title//,/%2C}
    title=${title//:/%3A}
    # A sampler that never started wrote no file. /dev/null gives the readings below zero
    # samples, so each says it took none rather than ending the step that called it: the step's
    # own exit is the suite's status, and a missing reading must not take that over.
    [ -f "$memory" ] || memory=/dev/null
    [ -f "$cpu" ] || cpu=/dev/null
    # The lowest MemAvailable of the step, over the samples that are numbers: a short write at
    # the moment the sampler was killed is not a reading of anything.
    awk -v title="$title" '$1 ~ /^[0-9]+$/ { if (++n == 1 || $1 + 0 < low) low = $1 + 0 } END { if (n) printf "::notice title=%s MemAvailable::the lowest MemAvailable during the browser step was %d kB, over %d samples\n", title, low, n; else print "::warning::the browser step took no MemAvailable sample" }' "$memory"
    # Where the CPU went, in the groups the tick names. A tick is a `machine` line and the
    # process lines after it; the busy time between two ticks is the difference of their machine
    # totals, and a group's is the sum over its processes of the same difference. A process the
    # first tick already saw contributes nothing for the time before it, which is why the first
    # tick sets a baseline and adds nothing; a process a later tick first sees contributes its
    # whole reading, since it was started inside the step. A `<` difference is a pid reused by
    # another process, which contributes nothing rather than a negative.
    awk -v title="$title" '
      $1 == "machine" { if (samples++) busy += $2 - was; was = $2; next }
      ($1, $3) in seen { if ($2 >= seen[$1, $3]) group[$3] += $2 - seen[$1, $3]; seen[$1, $3] = $2; next }
      { if (samples > 1) group[$3] += $2; seen[$1, $3] = $2 }
      END {
        if (!samples) { print "::warning::the browser step took no CPU sample"; exit }
        split("browser runner dispatch postgres bun", names, " ")
        for (i = 1; i <= 5; i++) {
          named += group[names[i]]
          split_up = split_up sprintf("%s %.0f (%.0f%%), ", names[i], group[names[i]] / 100, busy ? 100 * group[names[i]] / busy : 0)
        }
        printf "::notice title=%s CPU::the browser step used %.0f CPU-seconds over %d samples: %severything else %.0f (%.0f%%)\n", title, busy / 100, samples, split_up, (busy - named) / 100, busy ? 100 * (busy - named) / busy : 0
      }' "$cpu"
    ;;
  *)
    usage "$@"
    ;;
esac
