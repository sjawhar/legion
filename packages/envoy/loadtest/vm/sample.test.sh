#!/usr/bin/env bash
# packages/envoy/loadtest/vm/sample.test.sh
#
# Proves sample.sh's readings without a VM, a cgroup tree or a /proc of its own: it fakes
# `getconf` and `sleep` on PATH, as cluster.test.sh fakes the binaries cluster.sh calls. Each
# fake records its argv in one calls file and answers from the prepared tree: getconf gives
# CLK_TCK 100, so the machine's usage_usec is the same number on any host, and sleep returns at
# once, so a two-round run takes no seconds. Everything else sample.sh reads is a file this test
# writes under its own cgroup root and /proc.
#
# Asserts that: (a) one round prints the exact readings of two containers and of the machine,
# each ended by `end`: the container readings carry cpu.stat's usage_usec and throttled_usec,
# cpu.max, memory.current and memory.stat's inactive_file; (b) a cgroup file the VM cannot read
# is an `error` line of that reading, which still ends with `end`, so the collector writes a
# failed read rather than dropping the reading; (c) the machine's reading is /proc/stat's busy
# ticks in microseconds at CLK_TCK, no throttling, its processor count as a quota over the
# period the containers' cpu.max use, and MemTotal less MemFree with Inactive(file) from
# /proc/meminfo; (d) a role given its own cgroup directory is read there and its reading names
# that directory's container; (e) -n rounds are printed with one -i sleep between them, and not
# after the last; (f) a run that names no role and an unknown flag are each refused with exit 2.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly sample="${here}/sample.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT

readonly fake_bin_dir="${temporary_dir}/bin"
readonly calls_file="${temporary_dir}/calls"
readonly cgroup_root="${temporary_dir}/sys/fs/cgroup"
readonly proc_dir="${temporary_dir}/proc"
readonly out_file="${temporary_dir}/out"
readonly err_file="${temporary_dir}/err"
readonly want_file="${temporary_dir}/want"

mkdir -p "$fake_bin_dir"
cat >"${fake_bin_dir}/getconf" <<EOF
#!/usr/bin/env bash
printf 'getconf %s\n' "\$*" >>"${calls_file}"
if [[ "\${1:-}" == CLK_TCK ]]; then
  echo 100
  exit 0
fi
exit 1
EOF
cat >"${fake_bin_dir}/sleep" <<EOF
#!/usr/bin/env bash
printf 'sleep %s\n' "\$*" >>"${calls_file}"
exit 0
EOF
chmod +x "${fake_bin_dir}/getconf" "${fake_bin_dir}/sleep"
export PATH="${fake_bin_dir}:${PATH}"

# The prepared cgroup v2 tree: dispatch has every file, postgres has no memory.stat, which is a
# file sample.sh cannot read.
mkdir -p "${cgroup_root}/dispatch" "${cgroup_root}/postgres" "${temporary_dir}/scope/dispatch.scope" "$proc_dir"
cat >"${cgroup_root}/dispatch/cpu.stat" <<'EOF'
usage_usec 123456789
user_usec 100000000
system_usec 23456789
nr_periods 1200
nr_throttled 3
throttled_usec 4321
EOF
echo '1600000 100000' >"${cgroup_root}/dispatch/cpu.max"
echo '8000000' >"${cgroup_root}/dispatch/memory.current"
cat >"${cgroup_root}/dispatch/memory.stat" <<'EOF'
anon 5000000
file 3000000
inactive_file 500000
active_file 2500000
EOF
cat >"${cgroup_root}/postgres/cpu.stat" <<'EOF'
usage_usec 7000000
throttled_usec 0
EOF
echo '400000 100000' >"${cgroup_root}/postgres/cpu.max"
echo '2000000' >"${cgroup_root}/postgres/memory.current"
# The same container read through a cgroup directory of its own, as a container runtime that
# puts each scope under system.slice gives it.
cp "${cgroup_root}/dispatch/cpu.stat" "${cgroup_root}/dispatch/cpu.max" "${cgroup_root}/dispatch/memory.current" \
  "${cgroup_root}/dispatch/memory.stat" "${temporary_dir}/scope/dispatch.scope/"

# /proc/stat's busy ticks are user 1000, nice 200, system 300, irq 10, softirq 20 and steal 5,
# which is 1,535 ticks, 15,350,000 microseconds at 100 ticks a second; idle and iowait are not
# busy, and guest and guest_nice are already inside user and nice.
cat >"${proc_dir}/stat" <<'EOF'
cpu  1000 200 300 50000 400 10 20 5 0 0
cpu0 250 50 75 12500 100 2 5 1 0 0
intr 123456
ctxt 7890
EOF
: >"${proc_dir}/cpuinfo"
for i in 0 1 2 3; do
  printf 'processor\t: %s\nmodel name\t: a test CPU\n\n' "$i" >>"${proc_dir}/cpuinfo"
done
cat >"${proc_dir}/meminfo" <<'EOF'
MemTotal:       16384000 kB
MemFree:         4096000 kB
MemAvailable:   10000000 kB
Inactive(file):  2048000 kB
EOF

# run_sample ARGS...: sample.sh with the prepared tree, its output and its errors kept.
run_sample() {
  : >"$calls_file"
  sh "$sample" -c "$cgroup_root" -p "$proc_dir" -m vm-test "$@" >"$out_file" 2>"$err_file"
}

# expect_output WHAT: the run's output is the text on standard input.
expect_output() {
  cat >"$want_file"
  if ! diff -u "$want_file" "$out_file"; then
    printf 'FAIL: %s\n' "$1" >&2
    exit 1
  fi
  printf 'PASS: %s\n' "$1"
}

# (a), (b) and (c): one round over two containers and the machine.
run_sample -n 1 dispatch postgres
expect_output "one round prints the containers' cgroup v2 counters, an unreadable file as that reading's error, and the machine's own reading" <<EOF
role dispatch
container dispatch
usage_usec 123456789
throttled_usec 4321
cpu_max 1600000 100000
memory_current 8000000
inactive_file 500000
end
role postgres
container postgres
usage_usec 7000000
throttled_usec 0
cpu_max 400000 100000
memory_current 2000000
error ${cgroup_root}/postgres/memory.stat is not readable
end
role vm-test
container vm
usage_usec 15350000
throttled_usec 0
cpu_max 400000 100000
memory_current 12582912000
inactive_file 2097152000
end
EOF

if ! grep -qx 'getconf CLK_TCK' "$calls_file"; then
  printf 'FAIL: sample.sh did not read CLK_TCK, so its machine reading does not convert /proc/stat ticks\n' >&2
  exit 1
fi
printf 'PASS: the machine reading converts /proc/stat ticks with CLK_TCK\n'

# (d): a role with a cgroup directory of its own.
run_sample -n 1 "dispatch=${temporary_dir}/scope/dispatch.scope"
expect_output "a role given its own cgroup directory is read there, and its reading names that container" <<EOF
role dispatch
container dispatch.scope
usage_usec 123456789
throttled_usec 4321
cpu_max 1600000 100000
memory_current 8000000
inactive_file 500000
end
role vm-test
container vm
usage_usec 15350000
throttled_usec 0
cpu_max 400000 100000
memory_current 12582912000
inactive_file 2097152000
end
EOF

# (e): -n rounds, one -i sleep between them.
run_sample -n 2 -i 7 dispatch
rounds=$(grep -c '^end$' "$out_file")
sleeps=$(grep -c '^sleep 7$' "$calls_file" || true)
if [[ "$rounds" != 4 || "$sleeps" != 1 ]]; then
  cat "$out_file" "$calls_file" >&2
  printf 'FAIL: two rounds over one container and the machine printed %s readings and slept %s times at the interval; want 4 and 1\n' "$rounds" "$sleeps" >&2
  exit 1
fi
printf 'PASS: -n rounds are printed with one -i sleep between them, and none after the last\n'

# (f): the refusals.
expect_refused() { # WHAT PATTERN ARGS...
  local what=$1 pattern=$2 status=0
  shift 2
  run_sample "$@" || status=$?
  if ((status != 2)) || ! grep -qF -- "$pattern" "$err_file"; then
    cat "$out_file" "$err_file" >&2
    printf 'FAIL: %s was not refused with exit 2 and "%s" (exit %s)\n' "$what" "$pattern" "$status" >&2
    exit 1
  fi
  printf 'PASS: %s is refused\n' "$what"
}
expect_refused "a run that names no role" "name at least one ROLE" -n 1
expect_refused "an unknown flag" "no flag -x" -n 1 -x dispatch
expect_refused "a -n that is not a number" "-n takes a whole number" -n often dispatch

printf 'PASS: packages/envoy/loadtest/vm/sample.test.sh\n'
