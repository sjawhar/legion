#!/usr/bin/env bash
# The Burstable roles live proof on a throwaway kind cluster: a role given a memory request below
# its limit (`runtime.kubernetes.resources.<role>.memory_request`) runs in an issue pod the API
# server admits, schedules and records as `Burstable`, and a file with no `resources` block keeps
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
# the binary when it is not on PATH) installed. The cluster is the run's own, and every kubectl
# call names its kubeconfig and context, so the caller's kubeconfig is never read. The cluster is
# deleted on any exit;
# BURSTABLE_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) keeps the transcript,
# the two legion.yaml files and each pod's manifest as the API server holds it.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${BURSTABLE_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-burstable-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

# Agent Sandbox's release manifest at the version internal/runtime/sandbox speaks, and gVisor's
# release, each pinned by its digest.
agent_sandbox_url=https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.3/sandbox.yaml
agent_sandbox_sha256=725fafdabe6aac202a89dc57f1cfe0e2e92f3164c8c2bd343fffca52f7039d96
gvisor_url=https://storage.googleapis.com/gvisor/releases/release/20261012.0/x86_64/gvisor.tar.zstd
gvisor_sha512=ce2e1aeb853c104e854dea6644333a1dba0fbc9dd1646af97e1d013fdf810da6d4176dd96f2139d7b055e5382117c0d7c4567ebd64ed9bef47fdd15f80fd67ab

kind_bin=${LEGION_E2E_KIND:-kind}
cluster=legion-burstable-$$
context=kind-$cluster
work=$(mktemp -d "/tmp/legion-e2e-burstable.$$.XXXXXXXX")
kubeconfig=$work/kubeconfig
node=$cluster-control-plane
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
k() { kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"; }

cleanup() {
  local status=$?
  if "$kind_bin" get clusters 2>/dev/null | grep -qx "$cluster"; then
    if [ "$status" -ne 0 ]; then
      # What the cluster held when the run failed, before the cluster goes with it.
      {
        k get pods -A -o wide
        k -n legion get sandboxes -o yaml
        k -n legion describe pods
        k -n legion get events --sort-by=.lastTimestamp
        k -n legion logs --all-containers --prefix --tail=200 -l legion.dev/project 2>&1
      } >"$evidence/cluster-at-failure.txt" 2>&1 || true
      echo "the cluster's pods, Sandboxes, events and container logs at the failure: $evidence/cluster-at-failure.txt" >&2
    fi
    "$kind_bin" delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 || echo "could not delete kind cluster $cluster; delete it with: $kind_bin delete cluster --name $cluster" >&2
  fi
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
note "kind $("$kind_bin" version), worker image $image"
pass

begin cluster
"$kind_bin" create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 180s
# The runtime places every pod on the Legion pool; the one node is that pool.
k label node "$node" legion.dev/pool=legion
k create namespace legion
k get storageclass standard -o name
note "node $(k get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion} {.status.allocatable.cpu} cpu {.status.allocatable.memory} memory')"
pass

begin agent-sandbox
curl -fsSLo "$work/sandbox.yaml" "$agent_sandbox_url"
echo "$agent_sandbox_sha256  $work/sandbox.yaml" | sha256sum -c -
k apply -f "$work/sandbox.yaml"
k -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=300s
pass

begin gvisor
curl -fsSLo "$work/gvisor.tar.zstd" "$gvisor_url"
echo "$gvisor_sha512  $work/gvisor.tar.zstd" | sha512sum -c -
mkdir -p "$work/gvisor"
tar --zstd -xf "$work/gvisor.tar.zstd" -C "$work/gvisor"
for part in runsc containerd-shim-runsc-v1 gvisor-bin; do
  docker cp "$work/gvisor/$part" "$node:/usr/local/bin/$part"
done
docker exec "$node" sh -c 'chown -R root:root /usr/local/bin/gvisor-bin &&
  printf "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\n  runtime_type = \"io.containerd.runsc.v1\"\n" >>/etc/containerd/config.toml &&
  systemctl restart containerd'
k wait --for=condition=Ready "node/$node" --timeout=120s
k apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
k run gvisor-check --image=mirror.gcr.io/library/busybox:1.37 --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gvisor"}}' -- dmesg
k wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-check --timeout=180s
k logs gvisor-check | grep -q 'Starting gVisor' || fail "a gvisor pod's kernel is not gVisor's: $(k logs gvisor-check)"
note "a runtimeClassName: gvisor pod runs under $(docker exec "$node" /usr/local/bin/runsc --version | head -1)"
k delete pod gvisor-check --wait=false
pass

begin launch
# The driver runs in a container on the kind network, beside the node, and binds the worker
# stream on that container's own address: the pods reach the kind network's containers, while a
# host firewall can drop what they send the host at the network's gateway. So the driver is a
# static test binary, and it reads the cluster through kind's internal kubeconfig, whose server is
# the node's address on that network.
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/burstable.test" ./internal/daemon
"$kind_bin" get kubeconfig --internal --name "$cluster" >"$work/kubeconfig-internal"
result=$work/result
docker run --rm --name "$cluster-driver" --network kind --user "$(id -u):$(id -g)" \
  -v "$work:$work" -e HOME=/tmp \
  -e LEGION_E2E_KUBECONFIG="$work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$context" \
  -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$work" -e LEGION_E2E_RESULT="$result" \
  mirror.gcr.io/library/busybox:1.37 sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=40m -test.v -test.run "^TestBurstableRoleRendersABurstablePodLive\$"' "$work/burstable.test" ||
  fail "the launch driver failed"
[ -s "$result" ] || fail "the launch driver recorded no pod"
pass

begin qos-class
while read -r name pod config; do
  ns=${pod%%/*} pod=${pod#*/}
  qos=$(k -n "$ns" get pod "$pod" -o jsonpath='{.status.qosClass}')
  scheduled=$(k -n "$ns" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].status}')
  node_name=$(k -n "$ns" get pod "$pod" -o jsonpath='{.spec.nodeName}')
  runtime_class=$(k -n "$ns" get pod "$pod" -o jsonpath='{.spec.runtimeClassName}')
  implementer=$(k -n "$ns" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="implementer")].resources}')
  k -n "$ns" get pod "$pod" -o yaml >"$evidence/$name-pod.yaml"
  cp "$config" "$evidence/$name-legion.yaml"
  note "$name: kubectl get pod $pod -o jsonpath='{.status.qosClass}' → $qos (PodScheduled=$scheduled on $node_name, runtimeClassName $runtime_class)"
  note "$name: implementer resources $implementer"
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
    grep -qx 'resources: Burstable roles (a request below the limit): implementer, so every issue pod is Burstable; every other pod is Guaranteed' <<<"$out" ||
      fail "--check-config on the burstable file does not name the Burstable role"
  else
    ! grep -q '^resources:' <<<"$out" || fail "--check-config on the default file names a Burstable role: $out"
  fi
done <"$result"
config=$(awk '$1 == "burstable" {print $3}' "$result")
for broken in 'memory_request: 40Gi|runtime.kubernetes.resources.implementer.memory_request 40Gi exceeds memory 32Gi' \
  'memory_request: lots|runtime.kubernetes.resources.implementer.memory_request must be a positive Kubernetes quantity (e.g. 20Gi or 500m)'; do
  value=${broken%%|*} want=${broken#*|}
  sed "s/memory_request: 8Gi/$value/" "$config" >"$(dirname "$config")/broken.yaml"
  if refused=$("$work/legion" start --check-config --config "$(dirname "$config")/broken.yaml" 2>&1); then
    fail "--check-config passed a file with $value"
  fi
  [ "$refused" = "legion start: $want" ] || fail "--check-config refused $value with: $refused"
  note "$value: refused, exit 1: $refused"
done
pass
