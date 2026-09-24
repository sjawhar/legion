#!/usr/bin/env bash
# Prints one TCP port that nothing listens on, picked below the kernel's ephemeral range and never
# one of the ports given as arguments (the ones this run already picked and has not bound yet).
#
#   port=$(bash scripts/e2e/lib/free-port.sh [<taken-port>...])
#
# Below that range because the kernel autobinds every outbound socket from it. Between this pick
# and the caller's bind, any connection the run opens (go build's module fetches, a Postgres dial,
# a curl) can take a port inside it, and the bind then fails with EADDRINUSE. The daemon-go job's
# Stage 1 proof did exactly that: `listen on 127.0.0.1:35612: bind: address already in use`. The
# kernel never autobinds a port outside the range, and `ss` rules out one a process listens on.
set -euo pipefail

command -v ss >/dev/null || {
  # Not decoration: without ss the busy check below would pass every port.
  echo "free-port: ss (iproute2) is required" >&2
  exit 1
}
read -r ephemeral_low _ </proc/sys/net/ipv4/ip_local_port_range
low=20000
[ "$ephemeral_low" -gt $((low + 1000)) ] || {
  echo "free-port: the ephemeral range starts at $ephemeral_low, leaving no room above $low" >&2
  exit 1
}
for _ in $(seq 1 50); do
  port=$((low + RANDOM % (ephemeral_low - low)))
  case " $* " in *" $port "*) continue ;; esac
  # Captured rather than piped to grep -q: under pipefail an early grep exit can SIGPIPE ss and
  # read a busy port as free.
  [ -n "$(ss -ltnH "sport = :$port")" ] || {
    echo "$port"
    exit 0
  }
done
echo "free-port: no free port found below $ephemeral_low" >&2
exit 1
