#!/usr/bin/env bash
# scripts/kind-smoke/down.sh — tear down exactly what up.sh recorded for this instance. See README.md.
# Every destructive action is keyed on a record under the instance's state directory and verified
# for ownership first (the recorded tmux socket, the recorded pid + start ticks, the docker label
# legion-smoke.instance=<instance>, the recorded cluster name); nothing is ever deleted by name
# pattern. Records and logs stay for inspection; pid files, the kubeconfig, and every secret go.
set -euo pipefail
source "${BASH_SOURCE[0]%/*}/lib.sh"

failures=0
problem() { printf 'error: %s\n' "$*" >&2; failures=$((failures + 1)); }

stop_controller() {
  local c server window
  c="$(record_read controller)"
  [[ "$c" == tmux\ * ]] || return 0
  set -- $c
  server="$2"
  window="$3"
  if tmux -L "$server" has-session -t "$window" 2>/dev/null; then
    # the instance's tmux server holds nothing but the controller pane
    tmux -L "$server" kill-server && note "STOPPED controller (tmux server $server)" || problem "tmux -L $server kill-server failed"
  else
    note "controller pane is already gone (tmux -L $server)"
  fi
}

delete_cluster() {
  local name kubeconfig
  name="$(record_read cluster)"
  [ -n "$name" ] || return 0
  kubeconfig="$(record_read kubeconfig)"
  [ -n "$kubeconfig" ] || kubeconfig="$state/kubeconfig"
  if kind get clusters 2>/dev/null | grep -Fxq -- "$name"; then
    kind delete cluster --name "$name" --kubeconfig "$kubeconfig" && note "DELETED cluster $name" || problem "kind delete cluster $name failed"
  else
    note "cluster $name is already gone"
  fi
  [ -f "$kubeconfig" ] && shred -u -- "$kubeconfig" && note "shredded $kubeconfig"
  return 0
}

remove_container() { # remove_container RECORD
  local name label
  name="$(record_read "$1")"
  [ -n "$name" ] || return 0
  if ! label="$(docker inspect -f '{{index .Config.Labels "legion-smoke.instance"}}' "$name" 2>/dev/null)"; then
    note "container $name is already gone"
    return 0
  fi
  if [ "$label" != "$instance" ]; then
    problem "refusing to remove container $name: label legion-smoke.instance is '$label', not '$instance'"
    return 0
  fi
  docker rm -f "$name" >/dev/null && note "REMOVED container $name" || problem "docker rm -f $name failed"
}

# The fixed list of secret-bearing files, by literal relative name under $state; each is shredded
# when present. Nothing else is globbed.
shred_secrets() {
  local f
  for f in secrets/dispatch-token secrets/envoy-token secrets/operator-token secrets/postgres-password secrets/postgres.env \
    secrets/dispatch-token-auth-header secrets/envoy-token-auth-header \
    overlay/secrets/providers.env overlay/secrets/operator.env \
    overlay/secrets/github-app-implement.pem overlay/secrets/github-app-review.pem \
    controller/operator-token controller/envoy-token controller/dispatch-token \
    dispatch-home/.local/share/dispatch/signing-key; do
    if [ -f "$state/$f" ]; then
      shred -u -- "$state/$f" && note "shredded $f" || problem "shred -u $state/$f failed"
    fi
  done
}

main() {
  smoke_init
  if [ ! -s "$records/instance" ]; then
    note "$state has no instance record: this directory never started a kind smoke; stopping nothing, deleting nothing"
    exit 0
  fi
  [ "$(record_read instance)" = "$instance" ] || fail "$records/instance names instance '$(record_read instance)', not '$instance'; refusing to tear down another instance's directory"
  stop_controller
  terminate_process_group_file port-forward
  terminate_pid_file envoy-bridge
  terminate_pid_file listener
  terminate_pid_file dispatch
  delete_cluster
  remove_container nats-container
  remove_container postgres-container
  shred_secrets
  note "KIND SMOKE DOWN"
  [ "$failures" -eq 0 ] || exit 1
}

main "$@"
