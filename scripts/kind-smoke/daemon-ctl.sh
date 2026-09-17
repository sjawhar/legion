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
# shellcheck disable=SC2034 # The sourced poll() reads this process global.
poll_timeout_line=0
host_daemon_booted() {
  tail -n "+$host_boot_log_first_line" "$state/logs/daemon.log" | grep -Fq 'legion daemon listening on '
}
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
    [ -f "$state/logs/daemon.log" ] || : >"$state/logs/daemon.log"
    host_boot_log_first_line=$(( $(wc -l <"$state/logs/daemon.log") + 1 ))
    start_host_daemon
    poll "${SMOKE_DAEMON_BOOT_WAIT:-600}" "the host daemon to finish booting" host_daemon_booted || {
      printf 'DAEMON-CTL start FAILED: host daemon did not finish booting (see %s)\n' "$state/logs/daemon.log" >&2
      exit 1
    }
    printf 'DAEMON-CTL start OK\n'
    ;;
  *)
    printf 'usage: daemon-ctl.sh <stop|start>\n' >&2
    exit 2
    ;;
esac
