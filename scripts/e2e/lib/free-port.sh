#!/usr/bin/env bash
# Prints one TCP port that nothing listens on, picked below the kernel's ephemeral range and never
# one of the ports given as arguments (the ones this run already picked and has not bound yet).
#
#   port=$(bash scripts/e2e/lib/free-port.sh [<taken-port>...])
#
# Below that range because the kernel autobinds every outbound socket from it. Between this pick
# and the caller's bind, any connection the run opens (go build's module fetches, a Postgres dial,
# a curl) can take a port inside it, and the bind then fails with EADDRINUSE. The kernel never
# autobinds a port outside the range, and `ss` rules out one a process listens on.
set -euo pipefail

read -r ephemeral_low _ </proc/sys/net/ipv4/ip_local_port_range
low=20000
[ "$ephemeral_low" -gt $((low + 1000)) ] || {
  echo "free-port: the ephemeral range starts at $ephemeral_low, leaving no room above $low" >&2
  exit 1
}
for _ in $(seq 1 50); do
  port=$((low + RANDOM % (ephemeral_low - low)))
  case " $* " in *" $port "*) continue ;; esac
  # Its own statement, so set -e stops the run when ss is missing or fails; `ss` exits 0 with no
  # output when nothing listens. A pipe to grep -q would instead let an early grep exit SIGPIPE ss.
  busy=$(ss -ltnH "sport = :$port")
  [ -n "$busy" ] || {
    echo "$port"
    exit 0
  }
done
echo "free-port: no free port found below $ephemeral_low" >&2
exit 1
