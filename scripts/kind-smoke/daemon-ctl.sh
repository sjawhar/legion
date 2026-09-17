#!/usr/bin/env bash
# scripts/kind-smoke/daemon-ctl.sh stop|start — operate on the recorded host-mode daemon only.
set -euo pipefail
# shellcheck source=scripts/kind-smoke/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"
# shellcheck source=scripts/kind-smoke/up.sh
source "${BASH_SOURCE[0]%/*}/up.sh"

verb="${1:-}"
[ "$#" = 1 ] || { printf 'usage: daemon-ctl.sh <stop|start>\n' >&2; exit 2; }
smoke_init
if [ "$(record_read daemon-mode)" != host ]; then
  printf 'DAEMON-CTL %s FAILED: this is not a host-daemon smoke instance\n' "$verb" >&2
  exit 1
fi

case "$verb" in
  stop)
    terminate_pid_file daemon "${SMOKE_DAEMON_STOP_WAIT:-70}"
    printf 'DAEMON-CTL stop OK\n'
    ;;
  start)
    start_host_daemon
    printf 'DAEMON-CTL start OK\n'
    ;;
  *)
    printf 'usage: daemon-ctl.sh <stop|start>\n' >&2
    exit 2
    ;;
esac
