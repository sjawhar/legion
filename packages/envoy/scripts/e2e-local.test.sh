#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/e2e-local.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly output_file="${temporary_dir}/output"

greeter_pid=""

cleanup() {
  if [[ -n "$greeter_pid" ]] && kill -0 "$greeter_pid" 2>/dev/null; then
    kill "$greeter_pid" 2>/dev/null || true
    wait "$greeter_pid" 2>/dev/null || true
  fi
  rm -f "$output_file" "${temporary_dir}/greeter.port"
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
