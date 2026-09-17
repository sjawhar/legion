#!/usr/bin/env bash
# Harness for node-loss.sh: state and Kubernetes observations are supplied by PATH fakes.
set -Eeuo pipefail
trap 'printf "FAIL %s:%s: %s\n" "${BASH_SOURCE[0]}" "$LINENO" "$BASH_COMMAND" >&2' ERR

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
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

fake curl <<'EOF'
count_file="$TMP_NODE_LOSS_STATE_COUNT"
count="$(cat "$count_file" 2>/dev/null || echo 0)"
count=$((count + 1))
echo "$count" >"$count_file"
if [ "$count" = 1 ]; then
  cat "$TMP_NODE_LOSS_STATE_INITIAL"
elif [ -n "${FAKE_CHANGED_SESSION:-}" ]; then
  cat "$TMP_NODE_LOSS_STATE_CHANGED"
else
  cat "$TMP_NODE_LOSS_STATE_RECOVERED"
fi
EOF
fake kubectl <<'EOF'
all="$*"
case "$all" in
  *" get pod legion-st1-1-planner-g1 -o json") cat "$TMP_NODE_LOSS_OLD_POD" ;;
  *" get pod legion-st1-1-planner-g2 -o json") cat "$TMP_NODE_LOSS_NEW_POD" ;;
  *" get node legion-node-old -o json") cat "$TMP_NODE_LOSS_OLD_NODE" ;;
  *" get node legion-node-new -o json") cat "$TMP_NODE_LOSS_NEW_NODE" ;;
  *" get pvc legion-st1-1 -o json") echo '{"status":{"phase":"Bound"}}' ;;
  *" cordon legion-node-old") ;;
  *" drain legion-node-old --ignore-daemonsets --delete-emptydir-data --timeout=10m") ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF
export PATH="$fake_bin:$PATH"

cat >"$tmp/state-initial.json" <<'EOF'
{"trees":{"ST1-1":{"locator":{"pvcName":"legion-st1-1"}}},"roles":{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","sessionId":"session-1","locator":{"podName":"legion-st1-1-planner-g1","pvcName":"legion-st1-1","ompSessionFile":"/home/legion/.omp/profiles/legion/agent/sessions/work/2026-09-17_planner.jsonl"}}}}
EOF
cat >"$tmp/state-recovered.json" <<'EOF'
{"trees":{"ST1-1":{"locator":{"pvcName":"legion-st1-1"}}},"roles":{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","sessionId":"session-1","locator":{"podName":"legion-st1-1-planner-g2","pvcName":"legion-st1-1","ompSessionFile":"/home/legion/.omp/profiles/legion/agent/sessions/work/2026-09-17_planner.jsonl"}}}}
EOF
cat >"$tmp/state-changed.json" <<'EOF'
{"trees":{"ST1-1":{"locator":{"pvcName":"legion-st1-1"}}},"roles":{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","sessionId":"session-2","locator":{"podName":"legion-st1-1-planner-g2","pvcName":"legion-st1-1","ompSessionFile":"/home/legion/.omp/profiles/legion/agent/sessions/work/2026-09-17_planner.jsonl"}}}}
EOF
cat >"$tmp/old-pod.json" <<'EOF'
{"metadata":{"name":"legion-st1-1-planner-g1"},"spec":{"nodeName":"legion-node-old"}}
EOF
cat >"$tmp/new-pod.json" <<'EOF'
{"metadata":{"name":"legion-st1-1-planner-g2"},"spec":{"nodeName":"legion-node-new","containers":[{"name":"worker","command":["legion","worker-shim","--resume=/home/legion/.omp/profiles/legion/agent/sessions/work/2026-09-17_planner.jsonl","--append-system-prompt","continue"]}]},"status":{"phase":"Running"}}
EOF
cat >"$tmp/old-node.json" <<'EOF'
{"metadata":{"labels":{"topology.kubernetes.io/zone":"us-west-2a","legion.dev/pool":"legion"}}}
EOF
cat >"$tmp/new-node.json" <<'EOF'
{"metadata":{"labels":{"topology.kubernetes.io/zone":"us-west-2a","legion.dev/pool":"legion"}}}
EOF
export TMP_NODE_LOSS_STATE_COUNT="$tmp/state-count"
export TMP_NODE_LOSS_STATE_INITIAL="$tmp/state-initial.json"
export TMP_NODE_LOSS_STATE_RECOVERED="$tmp/state-recovered.json"
export TMP_NODE_LOSS_STATE_CHANGED="$tmp/state-changed.json"
export TMP_NODE_LOSS_OLD_POD="$tmp/old-pod.json"
export TMP_NODE_LOSS_NEW_POD="$tmp/new-pod.json"
export TMP_NODE_LOSS_OLD_NODE="$tmp/old-node.json"
export TMP_NODE_LOSS_NEW_NODE="$tmp/new-node.json"

run_node_loss() {
  local status=0
  : >"$TMP_NODE_LOSS_STATE_COUNT"
  bash "$here/node-loss.sh" "$@" >"$tmp/out.txt" 2>&1 || status=$?
  return "$status"
}
assert_line() { grep -Fxq -- "$1" "$tmp/out.txt" || { cat "$tmp/out.txt" >&2; exit 1; }; }

status=0
run_node_loss || status=$?
[ "$status" = 2 ]
assert_line 'GATE node-loss/context FAILED: --context is required'
cat "$tmp/out.txt"

run_node_loss --context production --daemon-url http://10.1.10.136:13370 ST1-1
assert_line 'GATE node-loss/claims OK: recorded 1 role on legion-node-old in us-west-2a'
assert_line 'GATE node-loss/cordon OK: legion-node-old'
assert_line 'GATE node-loss/drain OK: legion-node-old'
assert_line 'GATE node-loss/session-planner OK: session unchanged'
assert_line 'GATE node-loss/resume-planner OK: recorded session path'
assert_line 'GATE node-loss/recovery-prompt-planner OK: absent'
grep -Fq 'kubectl --context production cordon legion-node-old' "$FAKE_LOG"
grep -Fq 'kubectl --context production drain legion-node-old --ignore-daemonsets --delete-emptydir-data --timeout=10m' "$FAKE_LOG"

status=0
FAKE_CHANGED_SESSION=1 run_node_loss --context production --daemon-url http://10.1.10.136:13370 ST1-1 || status=$?
[ "$status" = 1 ]
assert_line 'GATE node-loss/session-planner FAILED: session planner changed'

echo 'node-loss.test.sh: OK'
