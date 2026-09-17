#!/usr/bin/env bash
# Harness for scripts/kind-smoke/down.sh: teardown acts only on the records under the instance's
# state directory, verifies ownership before every destructive action, and never deletes by name
# pattern. Every external binary is a PATH fake that logs its argv.
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/kind-smoke/test-lib.sh
source "$here/test-lib.sh"
tmp="$(mktemp -d)"
spawned=()
cleanup() {
  local p
  for p in "${spawned[@]}"; do kill -- "-$p" 2>/dev/null || kill "$p" 2>/dev/null || true; done
  rm -rf -- "$tmp"
}
trap cleanup EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"
fake() {
  {
    # shellcheck disable=SC2016  # The generated fake expands these variables when it runs.
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}
export PATH="$fake_bin:$PATH"
export FAKE_LABEL_FILE="$tmp/label"
echo t1 >"$FAKE_LABEL_FILE"
export FAKE_CLUSTERS="$tmp/clusters"
: >"$FAKE_CLUSTERS"
export FAKE_CONTAINERS="$tmp/containers"
mkdir -p "$FAKE_CONTAINERS"
export FAKE_TMUX="$tmp/tmux"
mkdir -p "$FAKE_TMUX"
fake docker <<'EOF'
case "$*" in
  "inspect -f {{index .Config.Labels \"legion-smoke.instance\"}} "*) [ -f "$FAKE_CONTAINERS/${@: -1}" ] && cat "$FAKE_LABEL_FILE" || { echo "Error: No such object" >&2; exit 1; } ;;
  "rm -f "*) rm -f "$FAKE_CONTAINERS/${@: -1}" ;;
  *) echo "unexpected docker request: $*" >&2; exit 1 ;;
esac
EOF
fake kind <<'EOF'
case "$*" in
  "get clusters") cat "$FAKE_CLUSTERS" ;;
  delete\ cluster*) : >"$FAKE_CLUSTERS" ;;
  *) echo "unexpected kind request: $*" >&2; exit 1 ;;
esac
EOF
fake tmux <<'EOF'
case "$*" in
  *"has-session -t controller"*) [ -f "$FAKE_TMUX/$2-controller" ] ;;
  *has-session*) [ -f "$FAKE_TMUX/$2" ] ;;
  *kill-server*) rm -f "$FAKE_TMUX/$2" "$FAKE_TMUX/$2-controller" ;;
  *) echo "unexpected tmux request: $*" >&2; exit 1 ;;
esac
EOF

run_down() { # run_down STATE_DIR ENV… → output in $tmp/out.txt, exit code returned
  local dir="$1"
  shift
  local status=0
  env SMOKE_DIR="$dir" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 "$@" bash "$here/down.sh" >"$tmp/out.txt" 2>&1 || status=$?
  return $status
}
start_ticks() { awk '{print $22}' "/proc/$1/stat"; }
alive() { kill -0 "$@" 2>/dev/null; } # a pid or -pgid still answers signal 0
plant_process() { # plant_process STATE NAME → a live sleep with a correct record
  sleep 300 &
  local pid=$!
  spawned+=("$pid")
  echo "$pid" >"$1/pids/$2.pid"
  start_ticks "$pid" >"$1/pids/$2.start"
}
plant_group() { # plant_group STATE NAME → a setsid'd group with a correct record
  setsid bash -c 'sleep 300 & wait' &
  local pid=$!
  sleep 0.2
  spawned+=("$pid")
  echo "$pid" >"$1/pids/$2.pid"
  start_ticks "$pid" >"$1/pids/$2.start"
}
plant_records() { # plant_records STATE — the records up.sh writes, with a controller pane
  local s="$1"
  mkdir -p "$s/records" "$s/pids" "$s/logs" "$s/overlay/secrets" "$s/controller" "$s/dispatch-home/.local/share/dispatch"
  mkdir -p "$s/secrets" && chmod 0700 "$s/secrets"
  echo t1 >"$s/records/instance"
  echo legion-smoke-t1 >"$s/records/cluster"
  echo "$s/kubeconfig" >"$s/records/kubeconfig"
  printf 'apiVersion: v1\nkind: Config\n' >"$s/kubeconfig"
  echo legion-smoke-t1-nats >"$s/records/nats-container"
  echo legion-smoke-t1-postgres >"$s/records/postgres-container"
  echo smoke-t1 >"$s/records/project"
  touch "$FAKE_CONTAINERS/legion-smoke-t1-nats" "$FAKE_CONTAINERS/legion-smoke-t1-postgres"
  echo 'tmux legion-smoke-t1 controller' >"$s/records/controller"
  local f
  for f in secrets/dispatch-token secrets/envoy-token secrets/operator-token secrets/postgres-password secrets/postgres.env \
    secrets/dispatch-token-auth-header secrets/envoy-token-auth-header overlay/secrets/providers.env overlay/secrets/operator.env \
    overlay/secrets/github-app-implement.pem overlay/secrets/github-app-review.pem controller/operator-token controller/envoy-token \
    controller/dispatch-token dispatch-home/.local/share/dispatch/signing-key; do
    echo secret >"$s/$f"
  done
}

# 1. an empty state directory: the refusal line, exit 0, nothing called, and no state tree created beside it
mkdir -p "$tmp/empty"
run_down "$tmp/empty"
[ "$(cat "$tmp/out.txt")" = "$tmp/empty has no instance record: this directory never started a kind smoke; stopping nothing, deleting nothing" ]
[ ! -s "$FAKE_LOG" ]
[ ! -e "$tmp/empty/records" ]
[ ! -e "$tmp/empty/secrets" ]
# a state directory that does not exist (a mistyped SMOKE_INSTANCE) is left absent
run_down "$tmp/never-started"
grep -Fq "$tmp/never-started has no instance record" "$tmp/out.txt"
[ ! -e "$tmp/never-started" ]
echo "down.test.sh: refusal OK"

# 2. full records: everything recorded is stopped, in order, by ownership
s="$tmp/state"
plant_records "$s"
echo legion-smoke-t1 >"$FAKE_CLUSTERS"
touch "$FAKE_TMUX/legion-smoke-t1-controller"
plant_process "$s" listener
plant_process "$s" dispatch
plant_group "$s" port-forward
plant_group "$s" legion-177-keeper
# a stale bridge record: a live pid whose recorded start ticks are wrong — must not be signalled
sleep 300 &
stale_pid=$!
spawned+=("$stale_pid")
echo "$stale_pid" >"$s/pids/envoy-bridge.pid"
echo 1 >"$s/pids/envoy-bridge.start"
listener_pid="$(cat "$s/pids/listener.pid")"
dispatch_pid="$(cat "$s/pids/dispatch.pid")"
pf_pgid="$(cat "$s/pids/port-forward.pid")"
keeper_pgid="$(cat "$s/pids/legion-177-keeper.pid")"
run_down "$s" || { cat "$tmp/out.txt" >&2; exit 1; }
cat "$tmp/out.txt"
expected_order=(
  'tmux -L legion-smoke-t1 has-session -t controller'
  'tmux -L legion-smoke-t1 kill-server'
  "kind delete cluster --name legion-smoke-t1 --kubeconfig $s/kubeconfig"
  'docker rm -f legion-smoke-t1-nats'
  'docker rm -f legion-smoke-t1-postgres'
)
prev=0
for want in "${expected_order[@]}"; do
  line="$(grep -nFx -- "$want" "$FAKE_LOG" | head -n1 | cut -d: -f1)"
  [ -n "$line" ] || { echo "missing call: $want" >&2; cat "$FAKE_LOG" >&2; exit 1; }
  [ "$line" -gt "$prev" ] || { echo "out of order: $want" >&2; cat "$FAKE_LOG" >&2; exit 1; }
  prev="$line"
done
grep -Fq 'docker inspect -f {{index .Config.Labels "legion-smoke.instance"}} legion-smoke-t1-nats' "$FAKE_LOG"
refute alive "$listener_pid"
refute alive "$dispatch_pid"
refute alive -- "-$pf_pgid"
refute alive -- "-$keeper_pgid"
alive "$stale_pid"                                                     # the stale record's pid was not signalled
grep -Fq 'GONE envoy-bridge' "$tmp/out.txt"
[ ! -f "$s/pids/listener.pid" ]
[ ! -f "$s/pids/envoy-bridge.pid" ]
[ ! -f "$s/pids/port-forward.start" ]
for f in secrets/dispatch-token secrets/envoy-token secrets/operator-token secrets/postgres-password secrets/postgres.env \
  secrets/dispatch-token-auth-header overlay/secrets/providers.env overlay/secrets/operator.env overlay/secrets/github-app-implement.pem \
  overlay/secrets/github-app-review.pem controller/operator-token controller/envoy-token controller/dispatch-token \
  dispatch-home/.local/share/dispatch/signing-key kubeconfig; do
  [ ! -e "$s/$f" ] || { echo "not shredded: $f" >&2; exit 1; }
done
[ -f "$s/records/instance" ]
[ -f "$s/records/cluster" ]              # records and logs stay for inspection
[ ! -s "$FAKE_CLUSTERS" ]
[ ! -f "$FAKE_TMUX/legion-smoke-t1" ]
tail -n1 "$tmp/out.txt" | grep -Fxq 'KIND SMOKE DOWN'
# a second down finds everything gone and says so
: >"$FAKE_LOG"
run_down "$s" || { cat "$tmp/out.txt" >&2; exit 1; }
grep -Fq 'cluster legion-smoke-t1 is already gone' "$tmp/out.txt"
grep -Fq 'container legion-smoke-t1-nats is already gone' "$tmp/out.txt" || { cat "$tmp/out.txt" >&2; exit 1; }
refute grep -Eq '^(docker rm|kind delete|tmux kill-server)' "$FAKE_LOG"
tail -n1 "$tmp/out.txt" | grep -Fxq 'KIND SMOKE DOWN'
echo "down.test.sh: teardown by record OK"

# 2b. a record naming another cluster, or a kubeconfig outside the state directory, is refused: no kind delete, no shred
s3="$tmp/state3"
plant_records "$s3"
echo 'none: the checkout has no legion controller start' >"$s3/records/controller"
echo legion-smoke-someone-else >"$s3/records/cluster"
echo legion-smoke-someone-else >"$FAKE_CLUSTERS"
: >"$FAKE_LOG"
status=0
run_down "$s3" || status=$?
[ "$status" = 1 ]
grep -Fq "refusing to delete cluster 'legion-smoke-someone-else': the record does not name this instance's cluster legion-smoke-t1" "$tmp/out.txt"
refute grep -Fq 'kind delete' "$FAKE_LOG"
[ -f "$s3/kubeconfig" ]                                                  # nothing of the cluster step ran
s4="$tmp/state4"
plant_records "$s4"
echo 'none: the checkout has no legion controller start' >"$s4/records/controller"
outside="$tmp/outside-kubeconfig"
printf 'apiVersion: v1\n' >"$outside"
echo "$outside" >"$s4/records/kubeconfig"
echo legion-smoke-t1 >"$FAKE_CLUSTERS"
: >"$FAKE_LOG"
status=0
run_down "$s4" || status=$?
[ "$status" = 1 ]
grep -Fq "refusing to use kubeconfig '$outside': the record names a file outside $s4" "$tmp/out.txt"
refute grep -Fq 'kind delete' "$FAKE_LOG"
[ -f "$outside" ]                                                        # the foreign file is untouched
echo "down.test.sh: cluster record cross-check OK"

# 3. a container that carries another instance's label is refused, and the run exits 1 after finishing
s2="$tmp/state2"
plant_records "$s2"
echo 'none: the checkout has no legion controller start' >"$s2/records/controller"
echo other >"$FAKE_LABEL_FILE"
echo legion-smoke-t1 >"$FAKE_CLUSTERS"
: >"$FAKE_LOG"
if run_down "$s2"; then echo "label mismatch should exit 1" >&2; exit 1; fi
grep -Fq "refusing to remove container legion-smoke-t1-nats: label legion-smoke.instance is 'other', not 't1'" "$tmp/out.txt"
refute grep -Fq 'docker rm' "$FAKE_LOG"
grep -Fq 'kind delete cluster --name legion-smoke-t1' "$FAKE_LOG"        # the rest still ran
refute grep -Eq '^tmux ' "$FAKE_LOG"                                          # controller: none → no tmux call
tail -n1 "$tmp/out.txt" | grep -Fxq 'KIND SMOKE DOWN'
echo "down.test.sh: ownership refusal OK"

# A host controller socket name is owned only when it is this instance's legion-<project> server.
s6="$tmp/foreign-controller-state"
plant_records "$s6"
echo host >"$s6/records/daemon-mode"
echo someone-else >"$s6/records/controller-tmux-server"
touch "$FAKE_TMUX/someone-else"
echo legion-smoke-t1 >"$FAKE_CLUSTERS"
: >"$FAKE_LOG"
status=0
run_down "$s6" || status=$?
[ "$status" = 1 ]
[ -f "$FAKE_TMUX/someone-else" ] || { echo 'foreign tmux server was removed' >&2; exit 1; }
refute grep -Fq 'tmux -L someone-else kill-server' "$FAKE_LOG"
echo "down.test.sh: host controller ownership OK"
# 4. host mode removes only the recorded daemon state after a graceful daemon stop.
s5="$tmp/host-state"
echo t1 >"$FAKE_LABEL_FILE"
: >"$FAKE_LOG"
plant_records "$s5"
echo host >"$s5/records/daemon-mode"
echo legion-smoke-t1 >"$s5/records/controller-tmux-server"
echo "$s5/host-daemon/state" >"$s5/records/host-daemon-state-dir"
echo legion-smoke-t1 >"$s5/records/omp-profile"
profile_dir="$tmp/home/.omp/profiles/legion-smoke-t1"
mkdir -p "$profile_dir/plugins"
printf 'profile plugin\n' >"$profile_dir/plugins/package.json"
mkdir -p "$s5/host-daemon/state" "$s5/host-daemon/secrets" "$s5/host-daemon/plugin-skew"
for f in kubeconfig exec-token.sh exec-calls.log instructions.md legion.yaml \
  secrets/github-app-implement.pem secrets/github-app-review.pem state/state.json plugin-skew/package.json; do
  printf 'host daemon data\n' >"$s5/host-daemon/$f"
done
chmod 0700 "$s5/host-daemon/exec-token.sh"
chmod 0600 "$s5/host-daemon/kubeconfig" "$s5/host-daemon/exec-calls.log" \
  "$s5/host-daemon/secrets/github-app-implement.pem" "$s5/host-daemon/secrets/github-app-review.pem"
touch "$FAKE_TMUX/legion-smoke-t1"
echo legion-smoke-t1 >"$FAKE_CLUSTERS"
plant_process "$s5" daemon
daemon_pid="$(cat "$s5/pids/daemon.pid")"
run_down "$s5" HOME="$tmp/home" SMOKE_DAEMON_STOP_WAIT=1 || { cat "$tmp/out.txt" >&2; exit 1; }
refute alive "$daemon_pid"
refute test -e "$FAKE_TMUX/legion-smoke-t1"
refute grep -Fxq legion-smoke-t1 "$FAKE_CLUSTERS"
for f in host-daemon/kubeconfig host-daemon/exec-token.sh host-daemon/exec-calls.log host-daemon/instructions.md \
  host-daemon/legion.yaml host-daemon/secrets/github-app-implement.pem host-daemon/secrets/github-app-review.pem \
  host-daemon/plugin-skew host-daemon/state; do
  [ ! -e "$s5/$f" ] || { echo "host mode retained $f" >&2; exit 1; }
done
[ ! -e "$profile_dir" ] || { echo "host mode retained instance OMP profile" >&2; exit 1; }
[ -f "$s5/records/instance" ]
[ -d "$s5/logs" ]
: >"$FAKE_LOG"
run_down "$s5" HOME="$tmp/home" || { cat "$tmp/out.txt" >&2; exit 1; }
grep -Fq 'cluster legion-smoke-t1 is already gone' "$tmp/out.txt"
grep -Fq 'controller tmux server legion-smoke-t1 is already gone' "$tmp/out.txt"
refute grep -Eq '^(kind delete|tmux kill-server)' "$FAKE_LOG"
echo "down.test.sh: host teardown OK"
echo "down.test.sh: OK"
