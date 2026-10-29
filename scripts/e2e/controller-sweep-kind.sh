#!/usr/bin/env bash
# The controller orphan sweep's live proof on a throwaway kind cluster: the project controller the
# daemon launches (`controller: daemon`) keeps its Sandbox through an orphan sweep at grace 0 whose
# known set was read before the launch, because the store holds its claim unretired, and loses it,
# with its volume, to the first sweep once the claim is retired. The controller is launched by
# Legion's own code: the daemon's loader reads the legion.yaml, boot's factory builds the Agent
# Sandbox runtime over a real Postgres store, and the runtime launches it through Agent Sandbox
# v1.0.3's controller under gVisor (internal/daemon/controller_sweep_live_test.go). The negative
# control runs the same launch and sweep on a driver built from a scratch copy of the checkout
# whose sweep never reads the store (controllerOwned answers false): main's sweep before the fix,
# which must delete the launched controller's Sandbox. Each check prints `== <name>`, what it
# observed and `ok <name> (<s>s)`; the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on an amd64 (x86_64) Linux host — the gVisor release and the static test
# binary it builds are amd64 only — as
#   LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/controller-sweep-kind.sh
# with docker, go, kubectl, curl, zstd, a tar that supports --zstd, and kind (LEGION_E2E_KIND names
# the binary when it is not on PATH) installed. The cluster and the Postgres are the run's own, and
# every kubectl call names the cluster's kubeconfig and context, so the caller's kubeconfig is
# never read. Both are deleted on any exit; CONTROLLER_SWEEP_EVIDENCE_DIR (default a fresh /tmp
# directory, kept and printed) keeps the transcript and, on a failure, what the cluster held.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${CONTROLLER_SWEEP_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-controller-sweep-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# shellcheck source-path=SCRIPTDIR source=lib/kind-sandbox.sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"

# The store's image: the pinned Postgres the repository's CI tests against.
pg_image=mirror.gcr.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11

cluster=legion-controller-sweep-$$
work=$(mktemp -d "/tmp/legion-e2e-controller-sweep.$$.XXXXXXXX")
pg=$cluster-pg
driver=$cluster-driver
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  docker rm -f -v "$driver" "$pg" >/dev/null 2>&1 || true
  kind_sandbox_teardown "$status" "$evidence"
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "controller sweep kind e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "controller sweep kind e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT

# drive EXPECT BINARY runs the driver BINARY, built with -tags e2e, in a container on the kind
# network beside the node, its worker stream bound on that container's own address: the pods reach
# the kind network's containers, while a host firewall can drop what they send the host at the
# network's gateway. It reads the cluster through kind's internal kubeconfig, whose server is the
# node's address on that network, and the store by the Postgres container's name there.
drive() {
  docker run --rm --name "$driver" --cpus 2 --memory 2g --network kind --user "$(id -u):$(id -g)" \
    -v "$work:$work" -e HOME=/tmp \
    -e LEGION_E2E_KUBECONFIG="$work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$kind_context" \
    -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$work" -e LEGION_E2E_EXPECT="$1" \
    -e LEGION_E2E_STORE_DSN="postgres://postgres:p@$pg:5432/postgres?sslmode=disable" \
    mirror.gcr.io/library/busybox:1.37 sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=30m -test.v -test.run "^TestTheControllerOrphanSweepLive\$"' "$2"
}

# left says what the legion namespace holds of Sandboxes and volume claims, and fails unless it
# holds none.
left() {
  local objects
  objects=$(kind_k -n legion get sandboxes,persistentvolumeclaims -o name)
  [ -z "$objects" ] || fail "the legion namespace still holds $(tr '\n' ' ' <<<"$objects")"
  note "kubectl -n legion get sandboxes,persistentvolumeclaims: none"
}

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

begin drivers
# The driver as the checkout builds it, and the negative control's: a scratch copy of the checkout
# whose controllerOwned answers false before it reads the store, which leaves the sweep judging the
# controller's Sandbox by the known set alone, as main's did before the fix.
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/controller-sweep.test" ./internal/daemon
mkdir "$work/reverted"
tar -C "$root" --exclude=node_modules -cf - go.work go.work.sum packages/daemon packages/envoy | tar -C "$work/reverted" -xf -
swept=$work/reverted/packages/daemon/internal/runtime/sandbox/sandbox.go
sed -i '/^func (r \*Runtime) controllerOwned(/a\	return false, nil // the negative control: the sweep before the fix reads no claim' "$swept"
[ "$(sed -n '/^func (r \*Runtime) controllerOwned(/{n;p;}' "$swept")" = $'\treturn false, nil // the negative control: the sweep before the fix reads no claim' ] ||
  fail "the scratch copy's sandbox.go has no controllerOwned to revert"
note "reverted: $(diff "$root/packages/daemon/internal/runtime/sandbox/sandbox.go" "$swept" | tr '\n' ' ')"
CGO_ENABLED=0 go -C "$work/reverted/packages/daemon" test -c -tags e2e -o "$work/controller-sweep-reverted.test" ./internal/daemon
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
kind_sandbox_internal_kubeconfig "$work/kubeconfig-internal"
pass

begin store
docker run -d --name "$pg" --cpus 2 --memory 2g --network kind --tmpfs /var/lib/postgresql -e POSTGRES_PASSWORD=p "$pg_image" >/dev/null
for _ in $(seq 1 60); do
  docker exec "$pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres >/dev/null || fail "Postgres $pg did not accept TCP connections within 60s"
note "Postgres $pg on the kind network: $(docker exec "$pg" postgres --version)"
pass

begin sweep
drive kept "$work/controller-sweep.test" || fail "the driver failed"
left
pass

begin negative-control
drive deleted "$work/controller-sweep-reverted.test" || fail "the negative control's driver failed"
left
pass
