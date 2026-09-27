#!/usr/bin/env bash
# A stage driver's check that it is not starting beside what an earlier run of the same stage left
# running. A driver that dies with no chance to clean up (SIGKILL, a crashed host) leaves its daemon,
# its listener, its agents' processes and its containers behind, and a later run would otherwise
# start beside them without a word. Everything a run starts carries the pid of the driver that
# started it: each container is named <prefix>-<role>-<pid>, and every process of the run holds its
# work directory, /tmp/<prefix>.<pid>.<random>, in its argv, its working directory or its
# environment (an agent only as its working directory, a bridge only through the state directories
# it is handed). A run of the same stage that is still alive owns its own, and two lanes may run a
# stage at once, so only what a dead driver started is a leftover.

# leftover_started PID prints the process's start time, in clock ticks since boot (proc(5), stat
# field 22), and fails for a pid with no process or a zombie.
leftover_started() {
  local stat
  stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
  # The command name, field 2, is parenthesised and may hold spaces; the fields after it are not.
  read -r -a stat <<<"${stat##*) }"
  [ "${stat[0]}" != Z ] || return 1
  printf '%s\n' "${stat[19]}"
}

# leftover_owner OWNER SINCE: OWNER is a live process that started no later than SINCE (clock ticks
# since boot), so it is the driver that started what SINCE dates, not a later process given its pid.
leftover_owner() {
  local start
  start=$(leftover_started "$1") || return 1
  [ "$start" -le "$2" ]
}

# refuse_leftovers PREFIX fails, naming each one and the command that removes it, when a container
# or a process of an earlier PREFIX run outlived its driver. The caller's `fail` reports it.
refuse_leftovers() {
  local prefix=$1 name owner created p text since boot ticks
  local -a leftover_list=()
  boot=$(awk '$1 == "btime" { print $2 }' /proc/stat)
  ticks=$(getconf CLK_TCK)
  while read -r name; do
    owner=${name##*-}
    case $owner in '' | *[!0-9]*) continue ;; esac
    created=$(docker inspect -f '{{.Created}}' "$name" 2>/dev/null) || continue
    since=$((($(date -d "$created" +%s) - boot) * ticks))
    leftover_owner "$owner" "$since" || leftover_list+=("container $name (docker rm -f -v $name)")
  done < <(docker ps -a --filter "name=^$prefix-" --format '{{.Names}}')
  for p in /proc/[0-9]*; do
    # A pid is its namespace's own: a process in another pid namespace (an agent box) names a
    # driver pid that means nothing here, and one of another user's reads as nothing.
    [ "$p/ns/pid" -ef /proc/self/ns/pid ] || continue
    # A process that ends between the listing and the reads has nothing left to read; it is no
    # leftover. Each read is guarded, so the caller's ERR trap never sees one fail.
    text="$(tr '\0' ' ' 2>/dev/null <"$p/cmdline" || true) $(readlink "$p/cwd" 2>/dev/null || true)/ $(tr '\0' ' ' 2>/dev/null <"$p/environ" || true)"
    [[ $text =~ /tmp/$prefix\.([0-9]+)\. ]] || continue
    owner=${BASH_REMATCH[1]}
    since=$(leftover_started "${p#/proc/}") || continue
    leftover_owner "$owner" "$since" && continue
    text=$(tr '\0' ' ' 2>/dev/null <"$p/cmdline" || true)
    text=${text% }
    leftover_list+=("process ${p#/proc/}, ${text:0:100} (kill ${p#/proc/})")
  done
  [ "${#leftover_list[@]}" -eq 0 ] && return 0
  fail "an earlier $prefix run whose driver is gone left these running: $(printf '%s; ' "${leftover_list[@]}")remove them and run again"
}
