# shellcheck shell=bash disable=SC2154,SC2034 # the variables below are the caller's (listed below)
# stage4a-sandbox-runtime.sh's LEGION_E2E_POD_REACHABILITY=bridge mode: a reverse tunnel from the
# devbox to a scratch pod+Service of the run's own, for a cluster whose pods cannot reach the
# devbox at all (no tailnet bridge, no routable private address — GKE's pattern, unlike AWS's
# directly-reachable private address). Pods dial the Service; the Service's one pod runs a chisel
# reverse-tunnel server (mirror.gcr.io/jpillora/chisel, pulled through the mirror and pinned by
# digest per AGENTS.md); the devbox reaches that pod's control port only through `kubectl
# port-forward` (never exposed by the Service, never by the cluster network at all), and a chisel
# client built from the pinned source on the devbox asks the server, over that port-forward, to
# open two reverse listeners — one per forwarded port — that proxy back through the tunnel to
# 127.0.0.1 on the devbox, where the harness's own worker-stream listener and scratch session
# Postgres already bind (stage4a-sandbox-runtime.sh's bind_host).
#
# Every object bridge_start creates carries the run's legion.dev/project label (run_label, the
# same one lib/namespace-rig.sh's teardown owns), and bridge_stop deletes them by their one exact
# name, never by broader selection — consistent with namespace-rig's own rule that a shared
# namespace's teardown only ever acts on the run's own objects.
#
# No retry of its own holds either leg up: the chisel client runs with --max-retry-count 0 (give up
# on the first disconnection rather than chisel's default of retrying forever), and `kubectl
# port-forward` already never retries. Either process dying is a torn bridge, and bridge_watchdog
# fails the whole run loudly (SIGTERM to the script's own pid) the moment it notices, rather than
# let a silent reconnect leave pods dialing into a gap mid-run. A run that cannot keep the bridge up
# for its whole length (up to 150 minutes) is not the proof.
#
# Sourced after lib/namespace-rig.sh (stage4a-sandbox-runtime.sh sources both), whose `op` this
# file reuses for every operator-context read and write. The caller sets (beside begin/note/fail/
# pass and namespace-rig.sh's own variables, op included):
#   root         the checkout root, for the pinned chisel client's `go install`
#   operator     the admin kubectl context; bridge_stop and every read here use it alone
#   work         the run's scratch directory (the chisel client binary, logs, never printed)
#   bridge_name  the bridge pod and Service's shared name, deterministic from the project alone
#                (stage4a-sandbox-runtime.sh sets it once, beside its other object names, so the
#                cluster-DNS host pods would dial is known before bridge_start ever runs)
# and calls:
#   bridge_start <stream-port> <session-db-port>
#     stands the bridge up or fails loudly; on success sets bridge_host (the cluster-DNS name
#     pods dial, at the same two ports) and bridge_started.
#   bridge_stop
#     idempotent and safe to call whether or not bridge_start ever ran: stops the watchdog, the
#     chisel client and the port-forward, then deletes the bridge's pod and Service by exact name.

bridge_chisel_image=mirror.gcr.io/jpillora/chisel@sha256:51d034146bb06e03a493646e63e61d42fd3b5da914c7180c92ba603865768633
bridge_chisel_version=v1.11.8
bridge_control_port=8080
bridge_started=
bridge_host=
bridge_portforward_pid=
bridge_client_pid=
bridge_watchdog_pid=

# bridge_start stands up the pod, the Service, the port-forward, the chisel client and the
# watchdog, in that order, and fails loudly (via the caller's fail) the moment any of them does not
# come up within its own bound. It never retries a failed step itself.
bridge_start() {
  local stream_port=$1 sessiondb_port=$2
  bridge_host="$bridge_name.$namespace.svc.cluster.local"
  local auth_user=e2e auth_token
  auth_token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')

  local chisel_bin="$work/bin/chisel"
  mkdir -p "$work/bin"
  GOBIN="$work/bin" go install "github.com/jpillora/chisel@${bridge_chisel_version}" ||
    fail "building the chisel client ($bridge_chisel_version, the same release the bridge pod's server runs) failed"

  cat <<YAML | op apply -f - >/dev/null || fail "the operator could not create the bridge pod/Service $bridge_name"
apiVersion: v1
kind: Pod
metadata:
  name: $bridge_name
  labels: {legion.dev/project: "$run_label", legion.dev/bridge: "$bridge_name"}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  tolerations: [{key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule}]
  containers:
    - name: chisel
      image: $bridge_chisel_image
      args: ["server", "--port", "$bridge_control_port", "--reverse", "--auth", "$auth_user:$auth_token"]
      ports:
        - {containerPort: $bridge_control_port}
        - {containerPort: $stream_port}
        - {containerPort: $sessiondb_port}
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      resources: {requests: {cpu: "50m", memory: "32Mi"}, limits: {cpu: "250m", memory: "64Mi"}}
---
apiVersion: v1
kind: Service
metadata:
  name: $bridge_name
  labels: {legion.dev/project: "$run_label"}
spec:
  selector: {legion.dev/bridge: "$bridge_name"}
  ports:
    - {name: stream, port: $stream_port, targetPort: $stream_port}
    - {name: sessiondb, port: $sessiondb_port, targetPort: $sessiondb_port}
YAML
  bridge_started=1
  op wait "pod/$bridge_name" --for=condition=Ready --timeout=120s >/dev/null ||
    fail "the bridge pod $bridge_name did not become Ready within 120s: $(op get pod "$bridge_name" -o wide 2>&1)"
  note "[operator] bridge pod/Service $bridge_name: chisel server on :$bridge_control_port (control, never exposed by the Service), reverse-forwarding :$stream_port and :$sessiondb_port, label legion.dev/project=$run_label"

  local local_control_port
  local_control_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$stream_port" "$sessiondb_port") ||
    fail "no free devbox port for the bridge's kubectl port-forward"
  timeout --foreground 86400 kubectl --context "$operator" -n "$namespace" port-forward "pod/$bridge_name" \
    "$local_control_port:$bridge_control_port" >"$work/bridge-portforward.log" 2>&1 &
  bridge_portforward_pid=$!
  local waited=0
  until grep -q "Forwarding from" "$work/bridge-portforward.log" 2>/dev/null; do
    kill -0 "$bridge_portforward_pid" 2>/dev/null ||
      fail "kubectl port-forward to $bridge_name exited before it started forwarding: $(cat "$work/bridge-portforward.log")"
    waited=$((waited + 1))
    [ "$waited" -le 30 ] ||
      fail "kubectl port-forward to $bridge_name never reported it was forwarding within 30s: $(cat "$work/bridge-portforward.log")"
    sleep 1
  done
  note "[operator] kubectl port-forward pod/$bridge_name $local_control_port:$bridge_control_port: forwarding"

  "$chisel_bin" client --max-retry-count 0 --auth "$auth_user:$auth_token" "http://127.0.0.1:$local_control_port" \
    "R:0.0.0.0:$stream_port:127.0.0.1:$stream_port" \
    "R:0.0.0.0:$sessiondb_port:127.0.0.1:$sessiondb_port" \
    >"$work/bridge-chisel-client.log" 2>&1 &
  bridge_client_pid=$!

  local ready='' attempt log
  for attempt in $(seq 1 30); do
    kill -0 "$bridge_client_pid" 2>/dev/null ||
      fail "the chisel client exited before the tunnel came up: $(cat "$work/bridge-chisel-client.log")"
    log=$(op logs "$bridge_name" 2>&1) || true
    if grep -q "tun: proxy#R:$stream_port=>" <<<"$log" && grep -q "tun: proxy#R:$sessiondb_port=>" <<<"$log"; then
      ready=1
      break
    fi
    sleep 1
  done
  [ -n "$ready" ] ||
    fail "the bridge's reverse listeners for :$stream_port and :$sessiondb_port never came up within 30s; bridge pod log: $log; chisel client log: $(cat "$work/bridge-chisel-client.log")"
  note "[harness] chisel client (pid $bridge_client_pid, max-retry-count 0): reverse listeners for :$stream_port and :$sessiondb_port confirmed in the bridge pod's log"

  (
    trap 'exit 0' TERM
    while :; do
      if ! kill -0 "$bridge_portforward_pid" 2>/dev/null; then
        echo "   BRIDGE TUNNEL DOWN: kubectl port-forward to $bridge_name exited" >&2
        kill -TERM "$$" 2>/dev/null
        exit 1
      fi
      if ! kill -0 "$bridge_client_pid" 2>/dev/null; then
        echo "   BRIDGE TUNNEL DOWN: the chisel client exited" >&2
        kill -TERM "$$" 2>/dev/null
        exit 1
      fi
      sleep 5
    done
  ) &
  bridge_watchdog_pid=$!
  note "[harness] bridge watchdog (pid $bridge_watchdog_pid): polling the port-forward and chisel client every 5s; either dying fails the run"
}

# bridge_stop tears down everything bridge_start may have started, in reverse order, and never
# fails: a torn-down leg that is already gone is not an error. Safe to call unconditionally from
# cleanup, bridge mode or not.
bridge_stop() {
  [ -n "$bridge_started" ] || return 0
  bridge_started=
  [ -z "$bridge_watchdog_pid" ] || kill "$bridge_watchdog_pid" 2>/dev/null
  wait "$bridge_watchdog_pid" 2>/dev/null
  [ -z "$bridge_client_pid" ] || kill "$bridge_client_pid" 2>/dev/null
  wait "$bridge_client_pid" 2>/dev/null
  [ -z "$bridge_portforward_pid" ] || kill "$bridge_portforward_pid" 2>/dev/null
  wait "$bridge_portforward_pid" 2>/dev/null
  if [ -n "$bridge_name" ]; then
    op delete pod "$bridge_name" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 ||
      echo "   [operator] could not delete bridge pod $bridge_name; it may remain"
    op delete service "$bridge_name" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 ||
      echo "   [operator] could not delete bridge Service $bridge_name; it may remain"
  fi
}
