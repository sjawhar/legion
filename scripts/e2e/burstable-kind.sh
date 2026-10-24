#!/usr/bin/env bash
# The Burstable issue pod live proof on a throwaway kind cluster: an issue pod given a memory
# request below its limit (`runtime.kubernetes.resources.issue`, here `issue: {memory: 32Gi,
# memory_request: 8Gi}`) is one the API server admits, schedules and records as `Burstable`, its
# one `roles` container carrying that reservation, and a file with no `resources` block keeps
# its issue pod `Guaranteed`. The pods are built by Legion's own code: the daemon's loader reads
# each legion.yaml, its sandboxOptions translates the reservation, and the Agent Sandbox runtime
# launches the issue's root through Agent Sandbox v1.0.3's controller under gVisor
# (internal/daemon/burstable_live_test.go). `legion start --check-config` is run on each file and
# on two broken variants. Each check prints `== <name>`, what it observed and `ok <name> (<s>s)`;
# the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on an amd64 (x86_64) Linux host — the gVisor release and the static test
# binary it builds are amd64 only — as
#   LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/burstable-kind.sh
# with docker, go, kubectl, curl, zstd, a tar that supports --zstd, and kind (LEGION_E2E_KIND names
# the binary when it is not on PATH) installed. The cluster is lib/kind-sandbox.sh's, the run's
# own, and every kubectl call names its kubeconfig and context, so the caller's kubeconfig is never
# read. The cluster is deleted on any exit;
# BURSTABLE_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) keeps the transcript,
# the two legion.yaml files and each pod's manifest as the API server holds it.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${BURSTABLE_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-burstable-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# shellcheck source-path=SCRIPTDIR source=lib/kind-sandbox.sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"

cluster=legion-burstable-$$
work=$(mktemp -d "/tmp/legion-e2e-burstable.$$.XXXXXXXX")
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  docker rm -f "$cluster-driver" >/dev/null 2>&1 || true
  kind_sandbox_teardown "$status" "$evidence"
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "burstable kind e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "burstable kind e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT

begin prerequisites
[ "$(uname -m)" = x86_64 ] || fail "the proof runs on amd64 (x86_64) only, since the gVisor release it installs and the driver it builds are amd64; this host is $(uname -m)"
for tool in docker go kubectl curl zstd tar sha256sum sha512sum "$kind_bin"; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
[[ $(tar --help 2>/dev/null) == *--zstd* ]] || fail "this tar has no --zstd; install GNU tar 1.31 or later"
image=${LEGION_E2E_IMAGE:-}
[[ $image == *@sha256:* ]] || fail "LEGION_E2E_IMAGE must name the worker image by digest (<image>@sha256:<64 hex>)"
note "worker image $image"
pass

begin cluster
kind_sandbox_create "$cluster" "$work"
pass

begin agent-sandbox
kind_sandbox_install_agent_sandbox "$work"
pass

begin gvisor
kind_sandbox_install_gvisor "$work"
pass

begin image
kind_sandbox_load_image "$image"
pass

begin launch
# The driver runs in a container on the kind network, beside the node, and binds the worker
# stream on that container's own address: the pods reach the kind network's containers, while a
# host firewall can drop what they send the host at the network's gateway. So the driver is a
# static test binary, and it reads the cluster through kind's internal kubeconfig, whose server is
# the node's address on that network.
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/burstable.test" ./internal/daemon
kind_sandbox_internal_kubeconfig "$work/kubeconfig-internal"
result=$work/result
docker run --rm --name "$cluster-driver" --network kind --user "$(id -u):$(id -g)" \
  -v "$work:$work" -e HOME=/tmp \
  -e LEGION_E2E_KUBECONFIG="$work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$kind_context" \
  -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$work" -e LEGION_E2E_RESULT="$result" \
  mirror.gcr.io/library/busybox:1.37 sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=40m -test.v -test.run "^TestBurstableRoleRendersABurstablePodLive\$"' "$work/burstable.test" ||
  fail "the launch driver failed"
[ -s "$result" ] || fail "the launch driver recorded no pod"
pass

begin qos-class
while read -r name pod config; do
  ns=${pod%%/*} pod=${pod#*/}
  qos=$(kind_k -n "$ns" get pod "$pod" -o jsonpath='{.status.qosClass}')
  scheduled=$(kind_k -n "$ns" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].status}')
  node_name=$(kind_k -n "$ns" get pod "$pod" -o jsonpath='{.spec.nodeName}')
  runtime_class=$(kind_k -n "$ns" get pod "$pod" -o jsonpath='{.spec.runtimeClassName}')
  roles=$(kind_k -n "$ns" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="roles")].resources}')
  kind_k -n "$ns" get pod "$pod" -o yaml >"$evidence/$name-pod.yaml"
  cp "$config" "$evidence/$name-legion.yaml"
  note "$name: kubectl get pod $pod -o jsonpath='{.status.qosClass}' → $qos (PodScheduled=$scheduled on $node_name, runtimeClassName $runtime_class)"
  note "$name: roles container resources $roles"
  [ -n "$roles" ] || fail "$name's pod $pod has no roles container"
  want=Guaranteed
  [ "$name" = burstable ] && want=Burstable
  [ "$qos" = "$want" ] || fail "$name's pod $pod is $qos, want $want"
  [ "$scheduled" = True ] || fail "$name's pod $pod is not scheduled"
  [ "$runtime_class" = gvisor ] || fail "$name's pod $pod runs under $runtime_class, not gvisor"
done <"$result"
pass

begin check-config
go -C "$root/packages/daemon" build -o "$work/legion" ./cmd/legion
while read -r name _ config; do
  out=$("$work/legion" start --check-config --config "$config") || fail "--check-config refused $name's file: $out"
  note "$name: $(tr '\n' '|' <<<"$out")"
  if [ "$name" = burstable ]; then
    grep -qx 'resources: Burstable (a request below the limit): the issue pod; every other pod is Guaranteed' <<<"$out" ||
      fail "--check-config on the burstable file does not name the Burstable issue pod"
  else
    ! grep -q '^resources:' <<<"$out" || fail "--check-config on the default file names a Burstable pod: $out"
  fi
done <"$result"
config=$(awk '$1 == "burstable" {print $3}' "$result")
for broken in 'memory_request: 40Gi|runtime.kubernetes.resources.issue.memory_request 40Gi exceeds memory 32Gi' \
  'memory_request: lots|runtime.kubernetes.resources.issue.memory_request must be a positive Kubernetes quantity (e.g. 20Gi or 500m)'; do
  value=${broken%%|*} want=${broken#*|}
  sed "s/memory_request: 8Gi/$value/" "$config" >"$(dirname "$config")/broken.yaml"
  if refused=$("$work/legion" start --check-config --config "$(dirname "$config")/broken.yaml" 2>&1); then
    fail "--check-config passed a file with $value"
  fi
  [ "$refused" = "legion start: $want" ] || fail "--check-config refused $value with: $refused"
  note "$value: refused, exit 1: $refused"
done
pass
