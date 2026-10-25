#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/e2e-local.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly output_file="${temporary_dir}/output"
readonly first_output_file="${temporary_dir}/first-output"

greeter_pid=""
first_pid=""

cleanup() {
  for pid in "$greeter_pid" "$first_pid"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -f "$output_file" "$first_output_file" "${temporary_dir}/greeter.port"
  rmdir "$temporary_dir"
}
trap cleanup EXIT

bash "$driver" --help >"$output_file"
[[ "$(<"$output_file")" == *"Usage: e2e-local.sh"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: --help describes the local driver and starts no server\n'

if bash "$driver" --not-an-option >"$output_file" 2>&1; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"unknown option: --not-an-option"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: rejects unsupported arguments and starts no server\n'

# Something else already answers NATS's greeting on the port the driver is told to use: a server
# left by an earlier run, or a second run of this script at the same time. The driver's own server
# cannot bind that port and exits at once. The driver must stop there and say why, rather than run
# the whole acceptance against a server it did not start and report PASS for it.
readonly greeter_port_file="${temporary_dir}/greeter.port"
GREETER_PORT_FILE="$greeter_port_file" bun -e '
  import { writeFileSync } from "node:fs";
  const server = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: { open(socket) { socket.write("INFO {\"server_id\":\"not-the-drivers\"}\r\n"); }, data() {} },
  });
  writeFileSync(process.env.GREETER_PORT_FILE, String(server.port));
  setInterval(() => {}, 1 << 30);
' &
greeter_pid=$!
for _ in $(seq 1 100); do
  [[ -s "$greeter_port_file" ]] && break
  sleep 0.1
done
[[ -s "$greeter_port_file" ]] || {
  printf 'the stand-in NATS greeter never reported its port\n' >&2
  exit 1
}
greeter_port="$(<"$greeter_port_file")"
if E2E_NATS_PORT="$greeter_port" bash "$driver" >"$output_file" 2>&1; then
  cat "$output_file" >&2
  printf 'FAIL: the driver passed against a NATS server it did not start, on port %s\n' "$greeter_port" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"address already in use"* ]] || {
  cat "$output_file" >&2
  printf 'FAIL: the driver did not report that its own NATS server could not bind port %s\n' "$greeter_port" >&2
  exit 1
}
printf 'PASS: refuses to run against a NATS server it did not start\n'

# A second run in the same checkout shares the first's out/e2e: its evidence, its logs and its
# JetStream store. The second run is given ports of its own, so nothing but the claim on out/e2e can
# refuse it, and it must be refused before it removes anything; the first must then finish as if
# the second had never started.
readonly evidence_dir="${project_root}/packages/envoy/out/e2e"
# The poll below reads this file, so it exists before the run that fills it is started.
: >"$first_output_file"
bash "$driver" >"$first_output_file" 2>&1 &
first_pid=$!
# The driver prints its build line only once it holds out/e2e and has cleared it. Its output file is
# this test's own, so nothing a finished run left behind can be mistaken for it.
deadline=$((SECONDS + 60))
until grep -q '^building listener and NATS server$' "$first_output_file"; do
  if ! kill -0 "$first_pid" 2>/dev/null; then
    cat "$first_output_file" >&2
    printf 'FAIL: the first run ended before it held out/e2e\n' >&2
    exit 1
  fi
  if ((SECONDS >= deadline)); then
    cat "$first_output_file" >&2
    printf 'FAIL: the first run did not hold out/e2e within 60 seconds\n' >&2
    exit 1
  fi
  sleep 0.1
done
if E2E_NATS_PORT=14322 E2E_PORT=19120 E2E_SESSION_PORT=19121 bash "$driver" >"$output_file" 2>&1; then
  cat "$output_file" >&2
  printf 'FAIL: a second run in the same checkout passed while the first was still using out/e2e\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"another run of e2e-local.sh is using ${evidence_dir}; this run removed nothing."* ]] || {
  cat "$output_file" >&2
  printf 'FAIL: the second run did not say that another run holds %s\n' "$evidence_dir" >&2
  exit 1
}
first_rc=0
wait "$first_pid" || first_rc=$?
first_pid=""
if ((first_rc != 0)) || [[ "$(<"$first_output_file")" != *"PASS: "* ]]; then
  cat "$first_output_file" >&2
  printf 'FAIL: the first run exited %s after a second run in the same checkout was refused\n' "$first_rc" >&2
  exit 1
fi
[[ -s "${evidence_dir}/envelopes.jsonl" && -e "${evidence_dir}/listener.log" ]] || {
  printf 'FAIL: the first run passed, but its evidence in %s is gone\n' "$evidence_dir" >&2
  exit 1
}
printf 'PASS: refuses a second run in the same checkout, and the first finishes with its evidence\n'

# A run stopped part-way never reports success: a caller that sends TERM (a CI cancel, a timeout)
# sees 143, the run's own servers are gone, and out/e2e is free for the next run.
: >"$first_output_file"
bash "$driver" >"$first_output_file" 2>&1 &
first_pid=$!
deadline=$((SECONDS + 60))
until grep -q '^waiting for listener health$' "$first_output_file"; do
  if ! kill -0 "$first_pid" 2>/dev/null || ((SECONDS >= deadline)); then
    cat "$first_output_file" >&2
    printf 'FAIL: the run to be stopped never reached its listener-health wait\n' >&2
    exit 1
  fi
  sleep 0.1
done
kill -TERM "$first_pid"
stopped_rc=0
wait "$first_pid" || stopped_rc=$?
first_pid=""
if ((stopped_rc != 143)); then
  cat "$first_output_file" >&2
  printf 'FAIL: a run stopped with TERM exited %s, want 143\n' "$stopped_rc" >&2
  exit 1
fi
flock -n "${evidence_dir}/run.lock" true || {
  printf 'FAIL: a run stopped with TERM left %s locked\n' "$evidence_dir" >&2
  exit 1
}
printf 'PASS: a run stopped with TERM exits 143 and frees out/e2e\n'
