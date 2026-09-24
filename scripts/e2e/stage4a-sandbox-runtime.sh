#!/usr/bin/env bash
# Stage 4a's gate for the Go coordinator: the Agent Sandbox runtime (packages/daemon-go/internal/
# runtime/sandbox) proven on the production cluster, in namespace `legion`, from the devbox. A Go
# harness (sandbox/live_test.go, build tag e2e) drives the runtime through the Legion daemon's own
# restricted identity and hosts the worker stream on the devbox's private address; the pods it
# launches run the worker image under test and dial it. Operator steps (exec, PVC reads, a Secret's
# hash, the namespace list) use the admin context. Each check prints what it observed, naming the
# identity, then `CHECK <name>: PASS`; the first that fails ends the run non-zero, naming it.
#
# Everything the run creates carries its own project label, s4a-<run id>. On any exit the teardown
# deletes by exact name every Sandbox the harness recorded, then everything labelled with that exact
# project — never by label existence — and namespace-clean compares the namespace with the snapshot
# taken before the run, restricted to objects of this run or of no project, so another tree's
# objects cannot fail it. Nothing outside `legion` is touched.
#
# Inputs: LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
# ~/.kube/legion-daemon-production) name the restricted identity; LEGION_E2E_OPERATOR_CONTEXT
# (default production) the admin one; LEGION_E2E_IMAGE (required) the worker image by digest;
# STAGE4A_FROM a development entry point, which is never the proof; STAGE4A_EVIDENCE_DIR where the
# transcript and the runtime's log go (default a fresh /tmp directory, kept and printed).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
namespace=legion
port=13371
repo=sjawhar/legion-smoke
app_id=3202636
app_key=LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64
operator=${LEGION_E2E_OPERATOR_CONTEXT:-production}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-$HOME/.kube/legion-daemon-production}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
image=${LEGION_E2E_IMAGE:-}
from=${STAGE4A_FROM:-}
evidence=${STAGE4A_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e4a-evidence.XXXXXXXX)}
work=$(mktemp -d /tmp/legion-e2e4a.XXXXXXXX)
project="s4a-$(date -u +%Y%m%d%H%M%S)-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
record=$work/sandboxes
check=setup
torn_down=
snapshotted=
compared=
ok=

mkdir -p "$evidence"
exec > >(tee -a "$evidence/transcript.log") 2>&1

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
op() { kubectl --context "$operator" -n "$namespace" "$@"; }

# snapshot FILE: the namespace's Sandboxes, Secrets, PVCs, and pods that carry this run's project
# label or no project label at all, as the operator sees them.
snapshot() {
  {
    op get sandboxes,secrets,pvc,pods -l '!legion.dev/project' -o name
    op get sandboxes,secrets,pvc,pods -l "legion.dev/project=$project" -o name
  } | sort >"$1"
}

# teardown: this run's objects, and nothing else, gone. Never fails; runs once.
teardown() {
  [ -z "$torn_down" ] || return 0
  torn_down=1
  echo "== teardown"
  case "$project" in
    s4a-?*) ;;
    *)
      echo "   refused: the run project '$project' lacks the reserved prefix s4a-"
      return 0
      ;;
  esac
  local name left i
  if [ -s "$record" ]; then
    while read -r name; do
      op delete sandbox "$name" --ignore-not-found --wait=false || true
    done < <(sort -u "$record")
  fi
  op delete sandboxes -l "legion.dev/project=$project" --ignore-not-found --wait=false || true
  for i in $(seq 1 150); do
    if ! left=$(op get sandboxes,secrets,pvc,pods -l "legion.dev/project=$project" -o name 2>"$work/teardown.err"); then
      echo "   [operator] context $operator cannot list project $project's objects; they may remain: $(cat "$work/teardown.err")"
      return 0
    fi
    [ -n "$left" ] || break
    [ "$i" -ne 90 ] || op delete secrets,pvc -l "legion.dev/project=$project" --ignore-not-found --wait=false || true
    sleep 2
  done
  echo "   [operator] left of project $project: ${left:-nothing}"
  return 0
}

namespace_clean() {
  compared=1
  begin namespace-clean
  snapshot "$evidence/namespace-after.txt" || fail "the operator could not list namespace $namespace"
  if ! diff -u "$evidence/namespace-before.txt" "$evidence/namespace-after.txt"; then
    fail "namespace $namespace differs from its snapshot (restricted to project $project and unlabelled objects)"
  fi
  note "[operator] $(wc -l <"$evidence/namespace-after.txt") objects before and after, identical (unlabelled or project $project; sandboxes, secrets, pvc, pods)"
  echo "CHECK namespace-clean: PASS"
}

cleanup() {
  local status=$?
  set +e
  teardown
  if [ -z "$compared" ] && [ -n "$snapshotted" ]; then (namespace_clean) || status=1; fi
  rm -rf "$work"
  [ -n "$ok" ] || echo "stage 4a e2e: FAIL (check $check)"
  echo "evidence: $evidence (transcript.log, runtime.log, the namespace snapshots)"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

begin prerequisites
for tool in go kubectl aws curl ss secrets diff; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -n "$runtime_context" ] || fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the runtime must run as the Legion daemon's restricted identity, never the operator's"
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
case "$image" in *@sha256:*) ;; *) fail "LEGION_E2E_IMAGE must be the worker image pinned by digest (…@sha256:…), not '$image'" ;; esac
case "$from" in
  "" | installed | boot-refusal-negative | image-probe | root-ready | gvisor | adopt-working-copy | worker-colocated | suspend | \
    no-affinity | resume | same-agent-negative | kill-pod | respawn-before-register | concurrent-provision | re-adopt | \
    orphan-sweep | release-tree) ;;
  *) fail "STAGE4A_FROM=$from is not an entry point (a check after identity, other than stale-incarnation)" ;;
esac
imds=$(curl -sf -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60') ||
  fail "instance metadata is unreachable; the harness binds the devbox's private address, read from it"
host=$(curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4) ||
  fail "instance metadata has no local-ipv4"
unset imds
if [ -n "$(ss -Hltn "sport = :$port")" ]; then
  fail "port $port is taken on the devbox: $(ss -Hltnp "sport = :$port")"
fi
op get namespace "$namespace" -o name >/dev/null || fail "the operator context $operator cannot read namespace $namespace"
note "run project $project (every object's legion.dev/project label)"
note "image $image"
note "worker stream tcp://$host:$port (the devbox's private address)"
note "runtime identity: context $runtime_context in $runtime_kubeconfig; operator: context $operator"
if command -v jj >/dev/null && jj -R "$root" root >/dev/null 2>&1; then
  note "source: $(jj -R "$root" log -r @ --no-graph -T 'commit_id ++ if(empty, " (working copy: no changes)", " (working copy has changes)")') on $(jj -R "$root" log -r @- --no-graph -T 'commit_id')"
else
  note "source: $(git -C "$root" rev-parse HEAD)"
fi
[ -z "$from" ] || note "STAGE4A_FROM=$from: a development run, never the proof"

begin snapshot
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
note "[operator] $(wc -l <"$evidence/namespace-before.txt") objects in $namespace carry no project label or project $project"

begin build
go -C "$root/packages/daemon-go" test -c -tags e2e -o "$work/stage4a.test" ./internal/runtime/sandbox
note "built the e2e harness from the checkout"

harness_ok=
if env \
  LEGION_E2E_RUNTIME_KUBECONFIG="$runtime_kubeconfig" \
  LEGION_E2E_RUNTIME_CONTEXT="$runtime_context" \
  LEGION_E2E_OPERATOR_CONTEXT="$operator" \
  LEGION_E2E_NAMESPACE="$namespace" \
  LEGION_E2E_PROJECT="$project" \
  LEGION_E2E_IMAGE="$image" \
  LEGION_E2E_REPO="$repo" \
  LEGION_E2E_STREAM_HOST="$host" \
  LEGION_E2E_STREAM_PORT="$port" \
  LEGION_E2E_IMPLEMENT_APP_ID="$app_id" \
  LEGION_E2E_IMPLEMENT_APP_KEY="$app_key" \
  LEGION_E2E_RECORD="$record" \
  LEGION_E2E_WORK="$evidence" \
  LEGION_E2E_FROM="$from" \
  "$work/stage4a.test" -test.run '^TestStage4aSandboxRuntimeLive$' -test.v -test.timeout 150m; then
  harness_ok=1
fi
check=harness
[ -n "$harness_ok" ] || fail "a harness check failed (above)"

teardown
namespace_clean
if [ -n "$from" ]; then
  echo "stage 4a e2e: every check from $from passed — a development run, never the proof"
else
  echo "stage 4a e2e: PASS"
fi
ok=1
