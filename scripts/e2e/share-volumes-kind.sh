#!/usr/bin/env bash
# The share volumes live proof on a throwaway kind cluster: an existing network share, named in
# `runtime.kubernetes.pod.volumes` as a PersistentVolumeClaim and as an NFS export, reaches an issue
# pod the daemon's own code launches under gVisor, which reads a file from the claim's share, is
# refused a write there (its mount is read-only by default), and writes a file to the export it
# mounts with `read_only: false` that the NFS server then holds. `legion probe-boot` mounts both
# shares, and refuses a claim that does not exist, an export the server does not have and a server
# that never answers, each naming the volume and its share.
# The pod is built by Legion's own code: the daemon's loader reads the legion.yaml, its
# sandboxOptions builds the Agent Sandbox runtime, which launches the issue's root through Agent
# Sandbox v1.0.3's controller (internal/daemon/share_volumes_live_test.go). The share is an
# NFS-Ganesha server the run starts in the cluster, exporting `/team`, seeded with one file, which
# a PersistentVolume and the claim `team-share` bind, and `/scratch`. Each check prints
# `== <name>`, what it observed and `ok <name> (<s>s)`; the first that fails ends the run non-zero,
# naming it.
#
# Run it from a checkout on an amd64 (x86_64) Linux host — the gVisor release and the static
# binaries it builds are amd64 only — as
#   LEGION_E2E_IMAGE=<worker image@sha256> bash scripts/e2e/share-volumes-kind.sh
# with docker, go, kubectl, curl, zstd, a tar that supports --zstd, and kind (LEGION_E2E_KIND names
# the binary when it is not on PATH) installed. The kind node's own mount.nfs mounts the share, as a
# GKE node's does. The cluster is lib/kind-sandbox.sh's, the run's own, and every kubectl call names
# its kubeconfig and context, so the caller's kubeconfig is never read. The cluster is deleted on
# any exit; SHARE_VOLUMES_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) keeps the
# transcript, the legion.yaml files, the issue pod's manifest as the API server holds it, and each
# probe-boot's output.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${SHARE_VOLUMES_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-share-volumes-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# shellcheck source-path=SCRIPTDIR source=lib/kind-sandbox.sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"

# The NFS server: NFS-Ganesha as the Kubernetes SIG Storage's nfs-provisioner image ships it, run
# with a configuration of the proof's own (two exports, NFSv4.0, no grace period).
nfs_image=registry.k8s.io/sig-storage/nfs-provisioner@sha256:c825f3d5e28bde099bd7a3daace28772d412c9157ad47fa752a9ad0baafc118d

cluster=legion-share-volumes-$$
work=$(mktemp -d "/tmp/legion-e2e-share-volumes.$$.XXXXXXXX")
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

# release_shares STATUS lets the node go. On a pass every pod that mounts the run's share is
# deleted while the NFS server still runs, so the node's kernel client unmounts it cleanly (on a
# failure they are left for kind_sandbox_teardown's evidence). Then any NFS client the node's
# network namespace still holds is shut down (/sys/fs/nfs/<client>/shutdown): a hard NFSv4 mount
# whose server goes first leaves the kernel renewing its lease and closing its files forever, and
# the node's container can never exit. Shutting a client fails its outstanding RPCs and touches
# nothing outside that namespace. It needs passwordless sudo for the namespace; without it a
# leftover node is named, with the command that frees it.
release_shares() {
  [ -n "$kind_node" ] && docker inspect "$kind_node" >/dev/null 2>&1 || return 0
  if [ "$1" -eq 0 ]; then
    kind_k delete sandboxes --all -n legion --wait=true --timeout=120s >/dev/null 2>&1 || true
    kind_k delete pods --all -n legion --wait=true --timeout=120s >/dev/null 2>&1 || true
  fi
  local netns
  netns=$(docker inspect -f '{{.NetworkSettings.SandboxKey}}' "$kind_node" 2>/dev/null) || return 0
  # shellcheck disable=SC2016 # expanded by the namespace's shell
  local shutdown='d=$(mktemp -d) && mount -t sysfs sysfs "$d" && for s in "$d"/fs/nfs/*/shutdown; do [ -e "$s" ] && echo 1 >"$s"; done; umount "$d"; rmdir "$d"'
  if ! sudo -n nsenter --net="$netns" sh -c "$shutdown" 2>/dev/null; then
    echo "could not shut the node's NFS clients down (no passwordless sudo?); if $kind_node will not stop, run: sudo nsenter --net=$netns sh -c '$shutdown'" >&2
  fi
}

cleanup() {
  local status=$?
  docker rm -f "$cluster-driver" "$cluster-probe" >/dev/null 2>&1 || true
  release_shares "$status"
  kind_sandbox_teardown "$status" "$evidence"
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "share volumes kind e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "share volumes kind e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT

# on_kind NAME COMMAND… runs a static binary of the work directory in a container on the kind
# network beside the node: the pods reach the kind network's containers, while a host firewall can
# drop what they send the host at the network's gateway. It reads the cluster through kind's
# internal kubeconfig, whose server is the node's address on that network.
on_kind() {
  local name=$1
  shift
  docker run --rm --name "$cluster-$name" --network kind --user "$(id -u):$(id -g)" \
    -v "$work:$work" -e HOME=/tmp "$@"
}

begin prerequisites
[ "$(uname -m)" = x86_64 ] || fail "the proof runs on amd64 (x86_64) only, since the gVisor release it installs and the binaries it builds are amd64; this host is $(uname -m)"
for tool in docker go kubectl curl zstd tar sha256sum sha512sum "$kind_bin"; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
[[ $(tar --help 2>/dev/null) == *--zstd* ]] || fail "this tar has no --zstd; install GNU tar 1.31 or later"
image=${LEGION_E2E_IMAGE:-}
[[ $image == *@sha256:* ]] || fail "LEGION_E2E_IMAGE must name the worker image by digest (<image>@sha256:<64 hex>)"
note "worker image $image"
pass

begin binaries
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/share-volumes.test" ./internal/daemon
CGO_ENABLED=0 go -C "$root/packages/daemon" build -o "$work/legion" ./cmd/legion
note "$("$work/legion" version | head -1)"
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

begin images
kind_sandbox_load_image "$image"
kind_sandbox_load_image "$nfs_image"
kind_sandbox_internal_kubeconfig "$work/kubeconfig-internal"
pass

begin nfs-server
# The seed is written once, by the server's own start, so its content is the run's own token.
seed="seeded by run $$ at $(date -u +%FT%TZ)"
kind_k create namespace nfs
kind_k apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ganesha, namespace: nfs}
data:
  ganesha.conf: |
    NFS_Core_Param { Protocols = 4; }
    NFSV4 { Grace_Period = 0; Lease_Lifetime = 10; Minor_Versions = 0; }
    EXPORT_DEFAULTS { SecType = sys; Squash = no_root_squash; }
    EXPORT { Export_Id = 1; Path = /export/team; Pseudo = /team; Access_Type = RW; FSAL { Name = VFS; } }
    EXPORT { Export_Id = 2; Path = /export/scratch; Pseudo = /scratch; Access_Type = RW; FSAL { Name = VFS; } }
---
apiVersion: v1
kind: Pod
metadata: {name: ganesha, namespace: nfs, labels: {app: ganesha}}
spec:
  containers:
  - name: ganesha
    image: $nfs_image
    command:
    - sh
    - -c
    - >-
      mkdir -p /export/team /export/scratch /run/dbus /usr/local/var/run/ganesha &&
      printf '%s\n' "\$SEED" >/export/team/seed.txt && chmod 0777 /export/team /export/scratch &&
      rpcbind -w && rpc.statd --port 662 && dbus-daemon --system --nopidfile &&
      exec ganesha.nfsd -F -L /dev/stdout -f /etc/ganesha/ganesha.conf
    env: [{name: SEED, value: "$seed"}]
    securityContext: {capabilities: {add: [DAC_READ_SEARCH, SYS_RESOURCE]}}
    readinessProbe: {tcpSocket: {port: 2049}, periodSeconds: 2}
    volumeMounts: [{name: export, mountPath: /export}, {name: conf, mountPath: /etc/ganesha}]
  volumes:
  - {name: export, emptyDir: {}}
  - {name: conf, configMap: {name: ganesha}}
---
apiVersion: v1
kind: Service
metadata: {name: ganesha, namespace: nfs}
spec: {selector: {app: ganesha}, ports: [{name: nfs, port: 2049}]}
EOF
kind_k -n nfs wait --for=condition=Ready pod/ganesha --timeout=180s
server=$(kind_k -n nfs get service ganesha -o jsonpath='{.spec.clusterIP}')
[ -n "$server" ] || fail "the NFS server's Service has no cluster IP"
docker exec "$kind_node" sh -c "mkdir -p /tmp/share-check && mount -t nfs $server:/team /tmp/share-check && cat /tmp/share-check/seed.txt; umount /tmp/share-check" >"$work/node-read" 2>&1 ||
  fail "the kind node cannot mount $server:/team: $(cat "$work/node-read")"
[ "$(cat "$work/node-read")" = "$seed" ] || fail "the node read $(cat "$work/node-read") from $server:/team, not the seed"
note "NFS-Ganesha at $server exports /team (seed.txt: $seed) and /scratch; the node mounts /team and reads the seed"
kind_k apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata: {name: team-share-pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  nfs: {server: $server, path: /team}
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  claimRef: {namespace: legion, name: team-share}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: team-share, namespace: legion}
spec: {accessModes: [ReadWriteMany], storageClassName: "", volumeName: team-share-pv, resources: {requests: {storage: 1Gi}}}
EOF
kind_k -n legion wait --for=jsonpath='{.status.phase}'=Bound pvc/team-share --timeout=60s
note "kubectl -n legion get pvc team-share: Bound to team-share-pv ($server:/team)"
pass

begin launch
result=$work/result
# shellcheck disable=SC2016 # expanded by the driver container's shell
on_kind driver \
  -e LEGION_E2E_KUBECONFIG="$work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$kind_context" \
  -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$work" -e LEGION_E2E_RESULT="$result" \
  -e LEGION_E2E_SHARE_SERVER="$server" \
  mirror.gcr.io/library/busybox:1.37 sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=30m -test.v -test.run "^TestShareVolumesMountInAnIssuePodLive\$"' "$work/share-volumes.test" ||
  fail "the launch driver failed"
read -r _ pod _ < <(awk '$1 == "shares"' "$result")
[ -n "${pod:-}" ] && [ "$pod" != - ] || fail "the launch driver recorded no pod"
pod=${pod#*/}
for name in shares missing-claim missing-export silent-server; do
  cp "$(awk -v n="$name" '$1 == n {print $3}' "$result")" "$evidence/$name-legion.yaml"
done
pass

begin pod-shares
container=architect
for _ in $(seq 1 90); do
  kind_k -n legion exec "$pod" -c "$container" -- true >/dev/null 2>&1 && break
  sleep 2
done
kind_k -n legion get pod "$pod" -o yaml >"$evidence/shares-pod.yaml"
note "pod $pod: runtimeClassName $(kind_k -n legion get pod "$pod" -o jsonpath='{.spec.runtimeClassName}'), volumes $(kind_k -n legion get pod "$pod" -o jsonpath='{range .spec.volumes[?(@.name=="team")]}team→claim {.persistentVolumeClaim.claimName}{end}{range .spec.volumes[?(@.name=="scratch")]}, scratch→nfs {.nfs.server}:{.nfs.path}{end}')"
kernel=$(kind_k -n legion exec "$pod" -c "$container" -- sh -c 'dmesg 2>/dev/null | head -1')
[[ $kernel == *gVisor* ]] || fail "container $container's kernel is not gVisor's: $kernel"
note "container $container: $kernel"
note "mounts: $(kind_k -n legion exec "$pod" -c "$container" -- sh -c 'grep -E " /mnt/(team|scratch) " /proc/mounts' | tr '\n' ';')"
read_back=$(kind_k -n legion exec "$pod" -c "$container" -- cat /mnt/team/seed.txt) || fail "container $container cannot read /mnt/team/seed.txt"
[ "$read_back" = "$seed" ] || fail "container $container read $read_back from /mnt/team/seed.txt, not the seed"
note "cat /mnt/team/seed.txt → $read_back"
if refused=$(kind_k -n legion exec "$pod" -c "$container" -- sh -c 'echo no >/mnt/team/written-by-pod' 2>&1); then
  fail "container $container wrote to /mnt/team, which the pod block mounts read-only"
fi
[[ $refused == *"Read-only file system"* ]] || fail "the write to /mnt/team failed for another reason: $refused"
note "echo no >/mnt/team/written-by-pod → refused: $(tr '\n' ' ' <<<"$refused")"
token="written by $pod at $(date -u +%FT%TZ)"
kind_k -n legion exec "$pod" -c "$container" -- sh -c "printf '%s\n' '$token' >/mnt/scratch/from-issue-pod && ls -ln /mnt/scratch/from-issue-pod" >"$work/written" ||
  fail "container $container cannot write /mnt/scratch/from-issue-pod: $(cat "$work/written")"
note "printf … >/mnt/scratch/from-issue-pod → $(cat "$work/written")"
held=$(kind_k -n nfs exec ganesha -- cat /export/scratch/from-issue-pod)
[ "$held" = "$token" ] || fail "the NFS server holds $held in /export/scratch/from-issue-pod, not what the pod wrote"
note "the server's /export/scratch/from-issue-pod → $held"
pass

begin check-config
# Each case's own file, beside the token files it names by relative path.
shares_config=$(awk '$1 == "shares" {print $3}' "$result")
out=$("$work/legion" start --check-config --config "$shares_config" 2>&1) ||
  fail "--check-config refused the shares file: $out"
note "shares: $(head -1 <<<"$out")"
broken=$(dirname "$shares_config")/broken.yaml
sed 's|          persistent_volume_claim: {name: team-share}|          persistent_volume_claim: {name: team-share}\n          nfs: {server: '"$server"', path: /team}|' "$shares_config" >"$broken"
if refused=$("$work/legion" start --check-config --config "$broken" 2>&1); then
  fail "--check-config passed a volume naming a claim and an NFS export"
fi
want="legion start: runtime.kubernetes.pod.volumes[0] must set exactly one of secret, config_map, projected, persistent_volume_claim, nfs"
[ "$refused" = "$want" ] || fail "--check-config refused the two-source volume with: $refused"
note "a volume naming a claim and an export: refused, exit 1: $refused"
pass

# probe NAME WANT… runs `legion probe-boot` on the case NAME's legion.yaml, keeps its output as
# evidence in $probed, and holds it to exit 1 carrying every WANT.
probe() {
  local name=$1 status=0
  shift
  probed=$(on_kind probe mirror.gcr.io/library/busybox:1.37 timeout 900 "$work/legion" probe-boot --config "$(awk -v n="$name" '$1 == n {print $3}' "$result")" 2>&1) || status=$?
  printf '%s\n' "$probed" >"$evidence/$name-probe-boot.txt"
  [ "$status" -eq 1 ] || fail "probe-boot on the $name file exited $status, want 1: $(tail -5 <<<"$probed")"
  local want
  for want in "$@"; do
    [[ $probed == *"$want"* ]] || fail "probe-boot on the $name file does not say $want: $(tail -5 <<<"$probed")"
  done
}

begin probe-mounts-shares
# The shares file's probe pod mounts both shares and runs the image's probe container, whose own
# verdict is the image's: the run's agents have no model route, so the image refuses on its
# agents' models, and that refusal, never a `cannot mount`, is the evidence that the probe
# passed every share.
probe shares 'worker image ' 'failed its probe: pod ' ' Failed (container probe terminated'
[[ $probed != *"cannot mount"* && $probed != *"over its mounts"* ]] || fail "probe-boot refused the mountable shares: $(tail -3 <<<"$probed")"
note "shares: probe-boot exit 1 on the image's own verdict, no share refused: $(grep -m1 -o 'failed its probe: pod [^ ]* [A-Za-z]*' <<<"$probed")"
pass


begin probe-missing-claim
probe missing-claim 'cannot mount volume "team", which runtime.kubernetes.pod.volumes gives it from PersistentVolumeClaim "absent-share"' \
  'persistentvolumeclaim "absent-share" not found'
note "missing-claim: probe-boot exit 1: $(grep -m1 'cannot mount' <<<"$probed")"
pass

begin probe-missing-export
probe missing-export 'cannot mount volume "scratch", which runtime.kubernetes.pod.volumes gives it from NFS export '"$server"':/absent' \
  'mount failed: '
note "missing-export: probe-boot exit 1: $(grep -m1 'cannot mount' <<<"$probed")"
pass

begin probe-silent-server
# 192.0.2.10 is RFC 5737's documentation range: no host answers, so the node's mount.nfs times out
# after its own two minutes of retries, and the probe refuses that first failure within one attempt.
probe silent-server 'cannot mount volume "scratch", which runtime.kubernetes.pod.volumes gives it from NFS export 192.0.2.10:/scratch' \
  'mount failed: '
elapsed=$((SECONDS - check_started))
# One attempt is a pod under the probe's default 300 s budget; a refusal later than that came from
# a second attempt, which is the regression round 4 introduced.
[ "$elapsed" -lt 300 ] || fail "silent-server: refused only after ${elapsed}s, past one 300 s attempt"
note "silent-server: probe-boot exit 1 in ${elapsed}s: $(grep -m1 'cannot mount' <<<"$probed" | cut -c1-300)"
pass

begin probe-missing-image
# The shares file with its image's digest swapped for one its repository does not have: the
# registry answers the probe pod's pulls 404, and probe-boot refuses once the kubelet has met that
# answer on two pulls, within its first attempt, naming the image, rather than retrying it as a pull
# that may yet succeed.
shares_config=$(awk '$1 == "shares" {print $3}' "$result")
missing_config=${shares_config%.yaml}-missing-image.yaml
missing_image=${image%@sha256:*}@sha256:0000000000000000000000000000000000000000000000000000000000000000
sed "s|^    image: .*|    image: $missing_image|" "$shares_config" >"$missing_config"
grep -qx "    image: $missing_image" "$missing_config" || fail "the missing-image file names no image $missing_image"
cp "$missing_config" "$evidence/missing-image-legion.yaml"
printf 'missing-image - %s\n' "$missing_config" >>"$result"
probe missing-image "worker image sha256:0000000000000000000000000000000000000000000000000000000000000000 failed its probe: the registry does not have image $missing_image" \
  'rpc error: code = NotFound desc = failed to pull and unpack image ' ': not found'
elapsed=$((SECONDS - check_started))
[ "$elapsed" -lt 300 ] || fail "missing-image: refused only after ${elapsed}s, past one 300 s attempt"
[[ $probed != *"failed transiently"* ]] || fail "missing-image: probe-boot retried the pull before refusing: $(grep -m1 'failed transiently' <<<"$probed" | cut -c1-300)"
note "missing-image: probe-boot exit 1 in ${elapsed}s: $(grep -m1 -o 'the registry does not have image [^ ]*' <<<"$probed")"
pass
