#!/usr/bin/env bash
# Stage 4a's gate for the Go coordinator: the Agent Sandbox runtime (packages/daemon-go/internal/
# runtime/sandbox) proven on the production cluster, in namespace `legion`, from the devbox. A Go
# harness (sandbox/live_test.go, build tag e2e) drives the runtime through the Legion daemon's own
# restricted identity and hosts the worker stream on the devbox's private address; the pods it
# launches run the worker image under test and dial it. Operator steps (exec, PVC reads, a Secret's
# hash, the namespace list) use the admin context. Each check prints what it observed, naming the
# identity, then `CHECK <name>: PASS`; the first that fails ends the run non-zero, naming it.
#
# Every pod carries the operator fixture's pod (scripts/e2e/fixtures/operator-route/pod.yml): its
# model route, overlay, ServiceAccount and projected token. Legion holds none of it. The run
# creates its own copy of the ConfigMap the fixture mounts, named for the run's project, before the
# harness runs; the teardown deletes it with the rest of the run's objects.
#
# Everything the run creates carries its own project label, s4a-<run id>, and lib/namespace-rig.sh
# owns it: on any exit the teardown deletes by exact name every Sandbox the harness recorded, then
# everything labelled with that exact project — never by label existence — and namespace-clean
# compares the namespace with the snapshot taken before the run, restricted to objects of this run
# or of no project, so another tree's objects cannot fail it. Nothing outside `legion` is touched.
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
label_prefix=s4a-
project="${label_prefix}$(date -u +%Y%m%d%H%M%S)-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
run_label=$project
fixture=$root/scripts/e2e/fixtures/operator-route
route_configmap=legion-operator-route-$project
record=$work/sandboxes
check=setup
torn_down=
snapshotted=
compared=
ok=

mkdir -p "$evidence"
# tee shares the driver's process group, so a signal to the group (Ctrl-C, a closed pane, timeout's
# TERM) would end it before cleanup writes, and cleanup's first write would die of SIGPIPE: tee
# ignores the signals the driver traps, and outlives the driver's last line.
exec > >(trap '' HUP INT TERM && exec tee -a "$evidence/transcript.log") 2>&1
# fd 7 keeps the transcript for cleanup: a signal runs the EXIT trap under the redirections of the
# command it interrupted, whose output may be /dev/null or an evidence file.
exec 7>&1

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "CHECK $check: PASS"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/namespace-rig.sh
. "$root/scripts/e2e/lib/namespace-rig.sh"

cleanup() {
  local status=$?
  # A second signal must not cut the teardown short, and a closed output must not end it.
  trap '' HUP INT TERM PIPE
  exec >&7 2>&7
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
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root") || fail "lib/built-from.sh could not read the source revision"
while IFS= read -r line; do note "$line"; done <<<"$built"
[ -z "$from" ] || note "STAGE4A_FROM=$from: a development run, never the proof"

begin snapshot
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
note "[operator] $(wc -l <"$evidence/namespace-before.txt") objects in $namespace carry no project label or project $project"

begin operator-route
op create configmap "$route_configmap" --from-file=models.yml="$fixture/models.yml" --from-file=overlay.yml="$fixture/overlay.yml" \
  --dry-run=client -o yaml | kubectl label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create ConfigMap $route_configmap"
note "[operator] ConfigMap $route_configmap: models.yml and overlay.yml from $fixture, label legion.dev/project=$run_label"
pass

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
  LEGION_E2E_OPERATOR_POD="$fixture/pod.yml" \
  LEGION_E2E_OPERATOR_CONFIGMAP="$route_configmap" \
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
