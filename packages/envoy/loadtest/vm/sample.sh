#!/bin/sh
# packages/envoy/loadtest/vm/sample.sh
#
# The sampler that runs on a load-test VM: every -i seconds it prints one reading per watched
# container, from that container's cgroup v2 files, and one reading for the machine itself, from
# /proc. A reading is "key value" lines ended by a line "end". `loadgen vm-sample` (vmsample.go)
# holds one long-lived ssh per VM that runs this script and turns every reading into a line of
# the cgroup.jsonl that `loadgen cgroup-merge` already merges. cluster.sh's SAMPLE_SCRIPT
# (cluster/cluster.sh:1046-1056) is the model: the deployment runs its containers on plain
# virtual machines, so only the transport changes, from kubectl exec to ssh.
#
# usage: sample.sh [-i SECONDS] [-n ROUNDS] [-m MACHINE ROLE] [-c CGROUP ROOT] [-p PROC]
#                  ROLE[=CGROUP DIR] ...
#
# A role with no "=" reads <cgroup root>/<role>. -n 0, the default, never stops: the collector
# holds the ssh open for the whole run and ends it itself. -c and -p point the reads at a
# prepared tree, which is how sample.test.sh reads this script without a cgroup or a /proc of its
# own. A file this VM cannot read is an "error" line of that reading, never a dropped reading,
# as cluster.sh's SAMPLE_SCRIPT reports one.
set -u

interval=5
rounds=0
machine_role=vm
cgroup_root=/sys/fs/cgroup
proc=/proc

die() {
  echo "sample.sh: $1" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  flag=$1
  case $flag in
    -i | -n | -m | -c | -p)
      [ $# -ge 2 ] || die "$flag needs a value"
      case $flag in
        -i) interval=$2 ;;
        -n) rounds=$2 ;;
        -m) machine_role=$2 ;;
        -c) cgroup_root=$2 ;;
        -p) proc=$2 ;;
      esac
      shift 2
      ;;
    --)
      shift
      break
      ;;
    -*) die "no flag $flag" ;;
    *) break ;;
  esac
done
[ $# -gt 0 ] || die "name at least one ROLE[=CGROUP DIR]"
case $rounds in
  '' | *[!0-9]*) die "-n takes a whole number of rounds, not '$rounds'" ;;
esac

# CLK_TCK is how many ticks of /proc/stat make a second; 100 on every Linux the deployment runs,
# and the fallback when getconf is missing.
clk_tck=$(getconf CLK_TCK 2>/dev/null || echo 100)
case $clk_tck in
  '' | *[!0-9]*) clk_tck=100 ;;
esac

# spec_dir ROLE[=CGROUP DIR]: the cgroup v2 directory a role's counters live in.
spec_dir() {
  case $1 in
    *=*) echo "${1#*=}" ;;
    *) echo "$cgroup_root/$1" ;;
  esac
}

# spec_role ROLE[=CGROUP DIR]: the role name the reading carries.
spec_role() {
  case $1 in
    *=*) echo "${1%%=*}" ;;
    *) echo "$1" ;;
  esac
}

# emit_container ROLE DIR: one reading of the container whose cgroup v2 files are in DIR. The
# keys are the ones cluster.sh's SAMPLE_SCRIPT prints, so the collector parses both the same way.
emit_container() {
  echo "role $1"
  echo "container ${2##*/}"
  for f in cpu.stat cpu.max memory.current memory.stat; do
    if [ -r "$2/$f" ]; then
      case $f in
        cpu.stat) grep -E '^(usage_usec|throttled_usec) ' "$2/cpu.stat" ;;
        cpu.max) echo "cpu_max $(cat "$2/cpu.max")" ;;
        memory.current) echo "memory_current $(cat "$2/memory.current")" ;;
        memory.stat) grep '^inactive_file ' "$2/memory.stat" ;;
      esac
    else
      echo "error $2/$f is not readable"
    fi
  done
  echo end
}

# machine_period ROLE[=CGROUP DIR]...: the period, in microseconds, that the watched containers'
# cpu.max use. The machine's own cpu_max is its processor count as a quota over that same period,
# so cpuMaxCores (cgroup.go:435-449) reads the VM's limit_cores as its processors and cores_mean
# and limit_cores mean for the VM what they mean for a container. 100000 is cgroup v2's default
# period, which a container gets unless its runtime sets another.
machine_period() {
  for spec in "$@"; do
    dir=$(spec_dir "$spec")
    [ -r "$dir/cpu.max" ] || continue
    p=$(awk 'NR == 1 { print $2 }' "$dir/cpu.max")
    case $p in
      '' | *[!0-9]*) ;;
      *)
        echo "$p"
        return
        ;;
    esac
  done
  echo 100000
}

# emit_machine PERIOD: one reading of the VM itself, so the generator VM's own CPU is a series
# like any other.
#   usage_usec: /proc/stat's busy ticks (user, nice, system, irq, softirq and steal; guest and
#     guest_nice are already counted inside user and nice) in microseconds, which is what a
#     container's usage_usec counts.
#   throttled_usec: 0, since no CPU quota throttles the machine itself.
#   cpu_max: the processor count as a quota over PERIOD (machine_period).
#   memory_current and inactive_file: MemTotal less MemFree, and Inactive(file), from
#     /proc/meminfo, so the working set cgroup.go takes (memory.current less inactive_file) is
#     the machine's used memory outside reclaimable page cache, as it is for a container.
emit_machine() {
  echo "role $machine_role"
  echo "container vm"
  if [ -r "$proc/stat" ]; then
    usage=$(awk -v hz="$clk_tck" '$1 == "cpu" { printf "%.0f\n", ($2 + $3 + $4 + $7 + $8 + $9) * 1000000 / hz; exit }' "$proc/stat")
    if [ -n "$usage" ]; then
      echo "usage_usec $usage"
      echo "throttled_usec 0"
    else
      echo "error $proc/stat has no cpu line"
    fi
  else
    echo "error $proc/stat is not readable"
  fi
  if [ -r "$proc/cpuinfo" ]; then
    processors=$(grep -c '^processor' "$proc/cpuinfo")
    if [ "$processors" -gt 0 ]; then
      echo "cpu_max $((processors * $1)) $1"
    else
      echo "error $proc/cpuinfo names no processor"
    fi
  else
    echo "error $proc/cpuinfo is not readable"
  fi
  if [ -r "$proc/meminfo" ]; then
    awk -v f="$proc/meminfo" '
      /^MemTotal:/ { total = $2 }
      /^MemFree:/ { free = $2 }
      /^Inactive\(file\):/ { inactive = $2 }
      END {
        if (total > 0) {
          printf "memory_current %d\ninactive_file %d\n", (total - free) * 1024, inactive * 1024
        } else {
          print "error " f " has no MemTotal"
        }
      }' "$proc/meminfo"
  else
    echo "error $proc/meminfo is not readable"
  fi
  echo end
}

round=0
while :; do
  period=$(machine_period "$@")
  for spec in "$@"; do
    emit_container "$(spec_role "$spec")" "$(spec_dir "$spec")"
  done
  emit_machine "$period"
  round=$((round + 1))
  if [ "$rounds" -gt 0 ] && [ "$round" -ge "$rounds" ]; then
    break
  fi
  sleep "$interval"
done
