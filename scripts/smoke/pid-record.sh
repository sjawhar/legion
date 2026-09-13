# shellcheck shell=bash
# Sourced by up.sh, checkpoints.sh, and down.sh -- `source "$(dirname "${BASH_SOURCE[0]}")/pid-record.sh"`
# -- never executed.
#
# A rig process is recorded as a pair of files, `<name>.pid` and `<name>.start`, the second holding
# the Linux `/proc/<pid>/stat` start time (field 22) captured when the process was started. A record
# names a live rig process only while a process with that pid exists AND started at that exact tick:
# a pid the kernel has since reissued to an unrelated process is not this rig's. up.sh writes the
# pair, checkpoints.sh asks whether the recorded process is still the one running, and down.sh
# kills only a process whose start time still matches its record.
process_start_time() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $22}' "/proc/${pid}/stat"
}

pid_is_live() {
  local pid_file="$1"
  local start_file="${pid_file%.pid}.start"
  local pid
  local expected_start
  local actual_start

  [[ -r "$pid_file" && -r "$start_file" ]] || return 1
  pid="$(<"$pid_file")"
  expected_start="$(<"$start_file")"
  kill -0 "$pid" 2>/dev/null || return 1
  actual_start="$(process_start_time "$pid")" || return 1
  [[ "$actual_start" == "$expected_start" ]]
}
