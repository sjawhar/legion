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
# A dropped `kubectl port-forward` is not treated as a dead bridge: the watchdog restarts it, on
# the same local port, and the chisel client's own bounded reconnect (--max-retry-count 30,
# --max-retry-interval 2s — finite, not chisel's default of forever) picks the tunnel back up once
# the new port-forward is in place, since the client, its auth and its reverse listeners never
# needed to change. Only two things fail the whole run loudly (SIGTERM to the script's own pid):
# a port-forward that cannot be restarted within 60s of dropping, or the chisel client itself
# exiting (its own retry budget exhausted) — two earlier runs died outright to this exact drop,
# live, 2026-10-17, before this fix.
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
bridge_local_control_port=
bridge_portforward_pid=
bridge_portforward_attempt=0
bridge_client_pid=
bridge_watchdog_pid=

# bridge_portforward_start (re)starts kubectl port-forward to the bridge pod's control port, on
# the one local port bridge_start picked (bridge_local_control_port), so the chisel client's
# already-configured target URL keeps working across a restart — the watchdog below calls this
# again, on the same port, when a dropped port-forward needs recovering, rather than tearing the
# whole run down for a transient connection loss (confirmed live, 2026-10-17: two full-length runs
# died to a dropped port-forward with no sign the cluster or the bridge pod itself was unhealthy).
# Each attempt gets its own log file so a recovered run's evidence still shows the whole history.
# Returns failure (never calls fail itself) if this one attempt never starts forwarding within
# 30s; the caller — bridge_start synchronously, the watchdog's own bounded retry loop otherwise —
# decides what that means.
bridge_portforward_start() {
  bridge_portforward_attempt=$((bridge_portforward_attempt + 1))
  local log="$work/bridge-portforward-$bridge_portforward_attempt.log"
  timeout --foreground 86400 kubectl --context "$operator" -n "$namespace" port-forward "pod/$bridge_name" \
    "$bridge_local_control_port:$bridge_control_port" >"$log" 2>&1 &
  bridge_portforward_pid=$!
  local waited=0
  until grep -q "Forwarding from" "$log" 2>/dev/null; do
    kill -0 "$bridge_portforward_pid" 2>/dev/null || {
      echo "   [harness] kubectl port-forward attempt $bridge_portforward_attempt exited before it started forwarding: $(cat "$log")" >&2
      return 1
    }
    waited=$((waited + 1))
    if [ "$waited" -gt 30 ]; then
      echo "   [harness] kubectl port-forward attempt $bridge_portforward_attempt never reported it was forwarding within 30s: $(cat "$log")" >&2
      kill "$bridge_portforward_pid" 2>/dev/null
      return 1
    fi
    sleep 1
  done
  return 0
}
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

  # Annotated against Cluster Autoscaler/node consolidation, the same pair every Legion issue pod
  # carries (manifest.go's safeToEvictAnnotation and doNotDisruptAnnotation): confirmed live,
  # 2026-10-17, a bridge pod without them ran fine for ten minutes, alone on its own
  # Node-Auto-Provisioned node, then vanished mid-run (its chisel client's own log: connected at
  # 11:21:07, disconnected with the underlying TCP connection refused — not a graceful shutdown —
  # at 11:31:26), consistent with GKE reclaiming an underutilized node out from under it. The
  # watchdog's bounded port-forward restart (below) is the right answer to a dropped connection,
  # never to the pod itself being gone — every restart attempt correctly failed NotFound once it
  # was.
  cat <<YAML | op apply -f - >/dev/null || fail "the operator could not create the bridge pod/Service $bridge_name"
apiVersion: v1
kind: Pod
metadata:
  name: $bridge_name
  labels: {legion.dev/project: "$run_label", legion.dev/bridge: "$bridge_name"}
  annotations: {cluster-autoscaler.kubernetes.io/safe-to-evict: "false", karpenter.sh/do-not-disrupt: "true"}
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
  # 240s, not 120s: confirmed live, 2026-10-17, this cluster's own autopilot-default-resources-
  # mutator admission webhook adjusts the chisel container's cpu on create, and bringing up the
  # fresh NAP node this lone pod needs took long enough that 120s timed out the wait while the pod
  # itself reached Ready moments later (its own age already past 2 minutes by the time the
  # timeout's own diagnostic read it).
  op wait "pod/$bridge_name" --for=condition=Ready --timeout=240s >/dev/null ||
    fail "the bridge pod $bridge_name did not become Ready within 240s: $(op get pod "$bridge_name" -o wide 2>&1)"
  note "[operator] bridge pod/Service $bridge_name: chisel server on :$bridge_control_port (control, never exposed by the Service), reverse-forwarding :$stream_port and :$sessiondb_port, label legion.dev/project=$run_label"

  bridge_local_control_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$stream_port" "$sessiondb_port") ||
    fail "no free devbox port for the bridge's kubectl port-forward"
  bridge_portforward_start ||
    fail "the bridge's kubectl port-forward never came up: $(cat "$work/bridge-portforward-$bridge_portforward_attempt.log")"
  note "[operator] kubectl port-forward pod/$bridge_name $bridge_local_control_port:$bridge_control_port: forwarding"

  # A bounded retry count and a short max interval, not chisel's default of retrying forever: the
  # client gives up and exits if the port-forward it dials stays down past its own budget, which
  # the watchdog below trusts as "this bridge is really gone" rather than restarting indefinitely.
  "$chisel_bin" client --max-retry-count 30 --max-retry-interval 2s --auth "$auth_user:$auth_token" "http://127.0.0.1:$bridge_local_control_port" \
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
  note "[harness] chisel client (pid $bridge_client_pid, max-retry-count 30, max-retry-interval 2s): reverse listeners for :$stream_port and :$sessiondb_port confirmed in the bridge pod's log"

  # The watchdog owns every port-forward restart from here: kubectl port-forward never retries
  # itself, but a dropped one is not necessarily a dead bridge — the chisel client, auth and
  # reverse listeners it already set up survive a port-forward restart on the same local port
  # unchanged, and chisel's own bounded reconnect (above) picks the tunnel back up once the new
  # port-forward is in place. The run only fails if a restart can't be gotten up within 60s of
  # the drop, or the chisel client itself exits (it already gave up on its own budget). Confirmed
  # live, 2026-10-17: killing the port-forward process mid-run let the watchdog restart it and the
  # run continue past the point it would otherwise have died.
  # A flag file, not just the TERM bridge_stop sends, decides whether a detected failure here
  # still fires kill -TERM "$$": confirmed live, 2026-10-17, three full-length runs died with the
  # script's own `check` variable never past "harness" — teardown_sequence's bridge_stop had not
  # even reached its first visible line — consistent with this watchdog noticing the tunnel down
  # (a natural reconnect cycle, or bridge_stop's own first steps) and firing kill -TERM "$$" in the
  # same narrow window bridge_stop sends it TERM, before the trap had a chance to run: a `kill`
  # does not preempt a command already in flight (a `sleep`, a `bridge_portforward_start` retry
  # loop), so the two races. The flag file is checked right before every kill -TERM "$$", not only
  # at loop top, so it still catches a check made moments before bridge_stop writes it.
  (
    trap 'kill "$pf_pid" 2>/dev/null; exit 0' TERM
    pf_pid=$bridge_portforward_pid
    down_since=
    while :; do
      [ -f "$work/bridge-stopping" ] && exit 0
      if kill -0 "$pf_pid" 2>/dev/null; then
        down_since=
      else
        if [ -z "$down_since" ]; then
          down_since=$(date +%s)
          echo "   BRIDGE TUNNEL DOWN: kubectl port-forward to $bridge_name exited; restarting" >&2
        fi
        if bridge_portforward_start; then
          pf_pid=$bridge_portforward_pid
          echo "   [harness] kubectl port-forward to $bridge_name recovered after $(($(date +%s) - down_since))s" >&2
          down_since=
        elif [ $(($(date +%s) - down_since)) -ge 60 ]; then
          [ -f "$work/bridge-stopping" ] && exit 0
          echo "   BRIDGE TUNNEL DOWN: kubectl port-forward to $bridge_name could not be restarted within 60s; terminating the run" >&2
          kill -TERM "$$" 2>/dev/null
          exit 1
        fi
      fi
      if ! kill -0 "$bridge_client_pid" 2>/dev/null; then
        [ -f "$work/bridge-stopping" ] && exit 0
        echo "   BRIDGE TUNNEL DOWN: the chisel client exited (its own retry budget, max-retry-count 30, is exhausted); terminating the run: $(cat "$work/bridge-chisel-client.log")" >&2
        kill -TERM "$$" 2>/dev/null
        exit 1
      fi
      sleep 5
    done
  ) &
  bridge_watchdog_pid=$!
  note "[harness] bridge watchdog (pid $bridge_watchdog_pid): restarts a dropped port-forward (up to 60s) and watches the chisel client; the client exiting (budget exhausted) or a port-forward that cannot be restarted in time fails the run"
}

# bridge_stop tears down everything bridge_start may have started, in reverse order, and never
# fails: a torn-down leg that is already gone is not an error. Safe to call unconditionally from
# cleanup, bridge mode or not. Issues both deletes without kubectl's own --wait (which returned
# before the pod was actually gone at least three times, live, 2026-10-17, though a lone chisel
# pod deleted by hand elsewhere on this cluster took under 6s — the discrepancy is unexplained but
# repeatable) and instead polls for both objects gone itself. The first version of this poll
# captured `op get pod,service $bridge_name -o name` and treated empty output as gone — but a
# transient read error (rate limiting, a network blip) also prints nothing to stdout, so that
# version silently treated "the read failed" the same as "confirmed gone" and returned early,
# leaving the pod for namespace-clean to catch, live, twice, after apparently succeeding. This
# version checks each object on its own and requires the literal word NotFound in its own read's
# output before counting it gone; any other text — the object's own name, or an unrelated error —
# keeps the loop going.
# The real cause of every "every CHECK passed, then silent SIGTERM before teardown_sequence ran"
# failure (confirmed live, 2026-10-17, by `strace -f -e trace=kill,tkill,tgkill,execve`, after
# API consistency, this devbox's tool timeout, a watchdog race and a shared process group were
# each ruled out in turn): `wait PID` for a process this function just killed with SIGTERM returns
# 143 (128 + signal 15) as bash's own encoding of "killed by that signal" — the normal, correct
# outcome of killing it — and under this script's `set -e`, a bare `wait` with no `||` treats that
# 143 as a command failure and ends the whole script right there, skipping everything after it.
# Confirmed by the trace: no signal ever reached the script itself; it chose `exit 143` on its own,
# immediately after `wait`ing for the very port-forward process it had just terminated.
bridge_stop() {
  [ -n "$bridge_started" ] || return 0
  bridge_started=
  : >"$work/bridge-stopping"
  [ -z "$bridge_watchdog_pid" ] || kill "$bridge_watchdog_pid" 2>/dev/null
  wait "$bridge_watchdog_pid" 2>/dev/null || true
  [ -z "$bridge_client_pid" ] || kill "$bridge_client_pid" 2>/dev/null
  wait "$bridge_client_pid" 2>/dev/null || true
  [ -z "$bridge_portforward_pid" ] || kill "$bridge_portforward_pid" 2>/dev/null
  wait "$bridge_portforward_pid" 2>/dev/null || true
  [ -n "$bridge_name" ] || return 0
  op delete pod "$bridge_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  op delete service "$bridge_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  local i pod_read svc_read
  # 300 iterations at 2s (10 minutes): this pod is alone on a Node-Auto-Provisioned node, which
  # NAP may reclaim along with it, and a GET-by-name confirmation (not a LIST, which this run no
  # longer depends on bridge_stop for — see teardown_sequence's own ordering fix) is the reliable
  # way to know it is actually gone: a lone chisel pod deleted by hand elsewhere on this cluster
  # took under 6s, well inside this budget.
  for i in $(seq 1 300); do
    pod_read=$(op get pod "$bridge_name" -o name 2>&1)
    svc_read=$(op get service "$bridge_name" -o name 2>&1)
    case "$pod_read" in *NotFound*)
      case "$svc_read" in *NotFound*) return 0 ;; esac
    esac
    sleep 2
  done
  echo "   [operator] could not confirm the bridge pod/Service $bridge_name gone after 600s: pod: $pod_read; service: $svc_read; it may remain"
}
