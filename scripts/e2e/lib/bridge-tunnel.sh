# shellcheck shell=bash disable=SC2154,SC2034 # the variables below are the caller's (listed below)
# A reverse tunnel from the devbox to a scratch pod+Service of the run's own, for a cluster whose
# pods cannot reach the devbox at all (no tailnet bridge, no routable private address — GKE's
# pattern, unlike AWS's directly-reachable private address). Shared by two callers:
# stage4a-sandbox-runtime.sh's LEGION_E2E_POD_REACHABILITY=bridge mode (two ports: the worker
# stream and the session database) and claude-backend-sandbox.sh (six ports: the daemon API, the
# worker stream, Dispatch, the Envoy listener, NATS and the session database). Pods dial the Service; the Service's one
# pod runs a chisel reverse-tunnel server (mirror.gcr.io/jpillora/chisel, pulled through the mirror
# and pinned by digest per AGENTS.md); the devbox reaches that pod's control port only through
# `kubectl port-forward` (never exposed by the Service, never by the cluster network at all), and a
# chisel client built from the pinned source on the devbox asks the server, over that
# port-forward, to open one reverse listener per port the caller names, each proxying back through
# the tunnel to 127.0.0.1 on the devbox, where the caller's own services already bind.
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
# Sourced after lib/namespace-rig.sh (both callers source both), whose `op` this file reuses for
# every operator-context read and write. The caller sets (beside begin/note/fail/pass and
# namespace-rig.sh's own variables, op included):
#   root         the checkout root, for the pinned chisel client's `go install`
#   operator     the admin kubectl context; bridge_stop and every read here use it alone
#   work         the run's scratch directory (the chisel client binary, logs, never printed)
#   bridge_name  the bridge pod and Service's shared name, deterministic from the project alone
#                (the caller sets it once, beside its other object names, so the cluster-DNS host
#                pods would dial is known before bridge_start ever runs)
#   bridge_priority_class  (optional) a PriorityClass the bridge pod runs at; unset, it has none
#                and a higher-priority pod on a full node can preempt it (which the watchdog then
#                reports by the pod's events)
# defines:
#   kill_harness stops whatever the caller's run drives; the watchdog calls it before
#                kill -TERM "$$" on both of its fatal paths. The watchdog runs under the caller's
#                errexit, so an undefined kill_harness would end the watchdog before it signals.
# and calls:
#   bridge_start <port>...
#     stands the bridge up for one or more ports, each a Service port named p<port> and a reverse
#     listener to 127.0.0.1:<port> on the devbox, or fails loudly; on success sets
#     bridge_started. The cluster-DNS name pods dial is deterministic from bridge_name alone — the
#     caller already computes it the same way, before calling bridge_start — so this file need not
#     hand it back.
#   bridge_stop
#     idempotent and safe to call whether or not bridge_start ever ran: stops the watchdog, the
#     chisel client and the port-forward, then deletes the bridge's auth Secret, pod and Service by
#     exact name.

bridge_chisel_image=mirror.gcr.io/jpillora/chisel@sha256:51d034146bb06e03a493646e63e61d42fd3b5da914c7180c92ba603865768633
bridge_chisel_version=v1.11.8
bridge_control_port=8080
bridge_started=
bridge_node=
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
# bridge_events prints, one line each, the last events the operator context reads for the bridge
# pod, or, when the pod's are gone, for the node it last ran on (Preempted, Evicted, Killing,
# NodeNotReady and the like), so a run the bridge ended says what removed the pod. It never fails.
bridge_events() {
  local events node
  events=$(op get events --field-selector "involvedObject.name=$bridge_name" \
    -o jsonpath='{range .items[*]}{.lastTimestamp}{" "}{.reason}{": "}{.message}{"\n"}{end}' 2>/dev/null | tail -5) || events=
  if [ -z "$events" ] && [ -n "$bridge_node" ]; then
    events=$(op get events --field-selector "involvedObject.kind=Node,involvedObject.name=$bridge_node" \
      -o jsonpath='{range .items[*]}{.lastTimestamp}{" "}{.reason}{": "}{.message}{"\n"}{end}' 2>/dev/null | tail -5 |
      sed "s|^|node $bridge_node: |") || events=
  fi
  printf '%s\n' "${events:-no event of pod $bridge_name${bridge_node:+ or of node $bridge_node} is left to read}"
}
# bridge_down REASON is the watchdog's one fatal path: it names the reason and what removed the
# bridge pod (bridge_events) on the transcript, stops what the run drives (the caller's
# kill_harness), and sends the run's own shell TERM, so its EXIT trap tears everything down at once
# rather than a later check failing on a tunnel already gone. bridge_failed names the reason for
# the caller's FAIL line. Every command here is guarded: the watchdog runs under the caller's
# errexit, and a failing one would end it before it signals.
bridge_down() {
  [ -f "$work/bridge-stopping" ] && return 0
  {
    printf '%s; the bridge pod %s: %s\n' "$1" "$bridge_name" "$(bridge_events | tr '\n' '|')"
  } >"$work/bridge-failed" 2>/dev/null || true
  echo "   BRIDGE TUNNEL DOWN: $1; ending the run" >&2
  bridge_events 2>/dev/null | sed "s|^|   [operator] bridge pod $bridge_name event: |" >&2 || true
  kill_harness || true
  kill -TERM "$$" 2>/dev/null || true
}
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
      kill "$bridge_portforward_pid" 2>/dev/null || true
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
  [ "$#" -gt 0 ] || fail "bridge_start needs at least one port to forward"
  local port container_ports='' service_ports='' forwards=() listed=''
  for port in "$@"; do
    container_ports+=$'\n'"        - {containerPort: $port}"
    service_ports+=$'\n'"    - {name: p$port, port: $port, targetPort: $port}"
    forwards+=("R:0.0.0.0:$port:127.0.0.1:$port")
    listed+="${listed:+, }:$port"
  done

  # Set before any create attempt, not after the pod/Service succeed: confirmed live, 2026-10-17,
  # a failure partway through (the Secret created, the Pod/Service not, or the reverse) left
  # bridge_stop skipping cleanup entirely, since it only acts when this is already set. Every
  # delete bridge_stop issues is by exact name with --ignore-not-found, so running it against
  # objects that were never actually created is already a no-op.
  bridge_started=1
  local auth_user=e2e auth_token
  auth_token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')

  local chisel_bin="$work/bin/chisel"
  mkdir -p "$work/bin"
  GOBIN="$work/bin" go install "github.com/jpillora/chisel@${bridge_chisel_version}" ||
    fail "building the chisel client ($bridge_chisel_version, the same release the bridge pod's server runs) failed"

  # The auth token never appears in the pod's own spec: a Pod and a Secret are readable under
  # different RBAC verbs, and a Secret's own data is already expected to need `get secrets`, so
  # putting the token directly in the pod's `args` (as this once did, confirmed live, 2026-10-17,
  # readable with nothing but `get pods` RBAC, and duplicated again into kubectl apply's own
  # last-applied-configuration annotation on the same object) weakened that boundary for no
  # reason: chisel's server and client both already read `AUTH` from the environment when `--auth`
  # is omitted (confirmed in chisel's own --help), so a real Secret plus `valueFrom.secretKeyRef`
  # is the whole fix, no chisel change needed. Labelled and deleted by bridge_stop like every other
  # object this run creates; `create`, never `apply`, since a Secret's own manifest literally
  # carries the value apply would otherwise consider safe to also duplicate.
  op create -f - >/dev/null <<YAML || fail "the operator could not create the bridge auth Secret $bridge_name-auth"
apiVersion: v1
kind: Secret
metadata:
  name: $bridge_name-auth
  labels: {legion.dev/project: "$run_label"}
stringData: {AUTH: "$auth_user:$auth_token"}
YAML

  # Annotated against Cluster Autoscaler/node consolidation, the same pair every Legion issue pod
  # carries (manifest.go's safeToEvictAnnotation and doNotDisruptAnnotation): confirmed live,
  # 2026-10-17, a bridge pod without them ran fine for ten minutes, alone on its own
  # Node-Auto-Provisioned node, then vanished mid-run (its chisel client's own log: connected at
  # 11:21:07, disconnected with the underlying TCP connection refused — not a graceful shutdown —
  # at 11:31:26), consistent with GKE reclaiming an underutilized node out from under it. The
  # watchdog's bounded port-forward restart (below) is the right answer to a dropped connection,
  # never to the pod itself being gone — every restart attempt correctly failed NotFound once it
  # was. The server binds 127.0.0.1, not chisel's own default of 0.0.0.0 (confirmed live,
  # 2026-10-17: 0.0.0.0 let any pod on the cluster network reach :8080 directly, bypassing the
  # Service entirely, so a leaked AUTH value would have opened a forward tunnel from any pod, not
  # just through kubectl port-forward) — `kubectl port-forward` still reaches it, since it tunnels
  # into the pod's own network namespace and dials 127.0.0.1 there itself, never through the pod's
  # cluster IP or the Service.
  op create -f - >/dev/null <<YAML || fail "the operator could not create the bridge pod/Service $bridge_name"
apiVersion: v1
kind: Pod
metadata:
  name: $bridge_name
  labels: {legion.dev/project: "$run_label", legion.dev/bridge: "$bridge_name"}
  annotations: {cluster-autoscaler.kubernetes.io/safe-to-evict: "false", karpenter.sh/do-not-disrupt: "true"}
spec:
  restartPolicy: Never${bridge_priority_class:+
  priorityClassName: $bridge_priority_class}
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  tolerations: [{key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule}]
  containers:
    - name: chisel
      image: $bridge_chisel_image
      args: ["server", "--host", "127.0.0.1", "--port", "$bridge_control_port", "--reverse"]
      env:
        - name: AUTH
          valueFrom: {secretKeyRef: {name: "$bridge_name-auth", key: AUTH}}
      ports:
        - {containerPort: $bridge_control_port}$container_ports
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
  ports:$service_ports
YAML
  # 240s, not 120s: confirmed live, 2026-10-17, this cluster's own autopilot-default-resources-
  # mutator admission webhook adjusts the chisel container's cpu on create, and bringing up the
  # fresh NAP node this lone pod needs took long enough that 120s timed out the wait while the pod
  # itself reached Ready moments later (its own age already past 2 minutes by the time the
  # timeout's own diagnostic read it).
  op wait "pod/$bridge_name" --for=condition=Ready --timeout=240s >/dev/null ||
    fail "the bridge pod $bridge_name did not become Ready within 240s: $(op get pod "$bridge_name" -o wide 2>&1)"
  # The node, so a pod later gone without events of its own can still be traced (bridge_events).
  bridge_node=$(op get pod "$bridge_name" -o jsonpath='{.spec.nodeName}' 2>/dev/null) || bridge_node=
  note "[operator] bridge pod/Service $bridge_name: chisel server on :$bridge_control_port (control, never exposed by the Service), reverse-forwarding $listed, label legion.dev/project=$run_label"

  bridge_local_control_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$@") ||
    fail "no free devbox port for the bridge's kubectl port-forward"
  bridge_portforward_start ||
    fail "the bridge's kubectl port-forward never came up: $(cat "$work/bridge-portforward-$bridge_portforward_attempt.log")"
  note "[operator] kubectl port-forward pod/$bridge_name $bridge_local_control_port:$bridge_control_port: forwarding"

  # A bounded retry count and a short max interval, not chisel's default of retrying forever: the
  # client gives up and exits if the port-forward it dials stays down past its own budget, which
  # the watchdog below trusts as "this bridge is really gone" rather than restarting indefinitely.
  # AUTH through the environment, never `--auth` on the command line (any local user can read
  # another process's argv; only its owner or root can read its environment) — chisel reads it
  # from there natively when `--auth` is omitted, the same env var name the server's Secret uses.
  AUTH="$auth_user:$auth_token" "$chisel_bin" client --max-retry-count 30 --max-retry-interval 2s "http://127.0.0.1:$bridge_local_control_port" \
    "${forwards[@]}" \
    >"$work/bridge-chisel-client.log" 2>&1 &
  bridge_client_pid=$!

  local ready='' attempt log
  for attempt in $(seq 1 30); do
    kill -0 "$bridge_client_pid" 2>/dev/null ||
      fail "the chisel client exited before the tunnel came up: $(cat "$work/bridge-chisel-client.log")"
    log=$(op logs "$bridge_name" 2>&1) || true
    ready=1
    for port in "$@"; do
      grep -q "tun: proxy#R:$port=>" <<<"$log" || {
        ready=
        break
      }
    done
    [ -z "$ready" ] || break
    sleep 1
  done
  [ -n "$ready" ] ||
    fail "the bridge's reverse listeners for $listed never came up within 30s; bridge pod log: $log; chisel client log: $(cat "$work/bridge-chisel-client.log")"
  note "[harness] chisel client (pid $bridge_client_pid, max-retry-count 30, max-retry-interval 2s): reverse listeners for $listed confirmed in the bridge pod's log"

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
    # Kills both pf_pid (the one this loop is tracking) and bridge_portforward_pid (the global
    # bridge_portforward_start itself updates the instant it forks a new attempt, before this loop
    # has assigned pf_pid from it): a TERM arriving mid-restart, after a new port-forward process
    # exists but before `pf_pid=$bridge_portforward_pid` runs below, would otherwise orphan it.
    trap 'kill "$pf_pid" "$bridge_portforward_pid" 2>/dev/null || true; exit 0' TERM
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
          bridge_down "kubectl port-forward to $bridge_name could not be restarted within 60s"
          exit 1
        fi
      fi
      if ! kill -0 "$bridge_client_pid" 2>/dev/null; then
        [ -f "$work/bridge-stopping" ] && exit 0
        # The port-forward itself is still up here (the outer branch above only reaches this
        # check once kill -0 "$pf_pid" confirmed it); this path's own exit, unlike a received
        # TERM, never runs the trap above, so without this it would be the one way the watchdog
        # could orphan a live port-forward process. `|| true`: one of the two may already be gone,
        # and a failing kill under the caller's errexit would end the watchdog here, before it
        # stops the run (seen live, 2026-10-26: the run went on to fail minutes later elsewhere).
        kill "$pf_pid" "$bridge_portforward_pid" 2>/dev/null || true
        bridge_down "the chisel client exited (its own retry budget, max-retry-count 30, is exhausted): $(tail -3 "$work/bridge-chisel-client.log" 2>/dev/null | tr '\n' ' ')"
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
# cleanup, bridge mode or not. Confirms the pod and Service actually gone itself, polling, rather
# than trusting kubectl's own --wait (confirmed live, 2026-10-17, to sometimes return before the
# pod was actually gone, though a lone chisel pod deleted by hand on this cluster took under 6s).
# Requires the literal word NotFound in each object's own read before counting it gone; a read that
# prints neither the object's name nor NotFound (a transient error) keeps the loop going rather than
# being treated as confirmed gone.

# Every kill, wait and NotFound-expecting read in this function is guarded against the one thing
# this script's own `set -e` cannot tell apart from a real failure: a command whose last-evaluated
# part succeeds at exactly what it was asked to do returns nonzero anyway — `[ -z "$X" ] || kill
# "$X"` when $X is already dead (`kill` itself fails, "No such process"), and `wait PID`/
# `var=$(kubectl get ...)` once the process or object being waited for has actually gone, the
# normal, successful outcome. Unguarded, `set -e` ends the whole script right there, with no signal
# ever having reached it (confirmed live, 2026-10-17, by `strace -f -e trace=kill,tkill,tgkill,
# execve`: the script chose `exit 143` on its own, immediately after `wait`ing for a port-forward
# process it had itself just killed).
bridge_stop() {
  [ -n "$bridge_started" ] || return 0
  bridge_started=
  : >"$work/bridge-stopping"
  { [ -z "$bridge_watchdog_pid" ] || kill "$bridge_watchdog_pid" 2>/dev/null; } || true
  wait "$bridge_watchdog_pid" 2>/dev/null || true
  { [ -z "$bridge_client_pid" ] || kill "$bridge_client_pid" 2>/dev/null; } || true
  wait "$bridge_client_pid" 2>/dev/null || true
  # bridge_portforward_pid here is this shell's own copy, set once by bridge_start's first call
  # and never updated again in this scope: every later restart runs inside the watchdog's own
  # subshell above, which gets its own copy of the variable at fork and updates only that copy.
  # Once a watchdog exists, its own TERM trap (fired by the kill above, or already run on its own
  # exit path before this point — both now kill whichever port-forward is actually current) is
  # the only correct owner of that pid; killing this shell's stale one too would either hit a
  # process already gone (harmless) or, worse, one the OS has since reused for something else
  # entirely. Only with no watchdog at all (bridge_start failed before starting one) is this
  # shell's own bridge_portforward_pid both the only port-forward that ever ran and still this
  # shell's to kill.
  if [ -z "$bridge_watchdog_pid" ]; then
    { [ -z "$bridge_portforward_pid" ] || kill "$bridge_portforward_pid" 2>/dev/null; } || true
    wait "$bridge_portforward_pid" 2>/dev/null || true
  fi
  [ -n "$bridge_name" ] || return 0
  op delete secret "$bridge_name-auth" --ignore-not-found >/dev/null 2>&1 || true
  op delete pod "$bridge_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  op delete service "$bridge_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  local i pod_read svc_read
  # 300 iterations at 2s (10 minutes): this pod is alone on a Node-Auto-Provisioned node, which
  # NAP may reclaim along with it, and a GET-by-name confirmation (not a LIST, which this run no
  # longer depends on bridge_stop for — see teardown_sequence's own ordering fix) is the reliable
  # way to know it is actually gone: a lone chisel pod deleted by hand elsewhere on this cluster
  # took under 6s, well inside this budget. The `|| true` on each read is the same bug class as
  # the wait calls above, found the same way (strace, 2026-10-17) once those alone did not fix it:
  # `kubectl get` on a NotFound object — the very state this loop is waiting for — exits nonzero,
  # and a bare `var=$(...)` assignment with no guard trips `set -e` the instant the pod actually
  # disappears, ending the script right as the wait would have succeeded.
  for i in $(seq 1 300); do
    pod_read=$(op get pod "$bridge_name" -o name 2>&1) || true
    svc_read=$(op get service "$bridge_name" -o name 2>&1) || true
    case "$pod_read" in *NotFound*)
      case "$svc_read" in *NotFound*) return 0 ;; esac
    esac
    sleep 2
  done
  echo "   [operator] could not confirm the bridge pod/Service $bridge_name gone after 600s: pod: $pod_read; service: $svc_read; it may remain"
}
