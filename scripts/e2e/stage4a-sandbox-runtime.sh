#!/usr/bin/env bash
# Stage 4a's gate for the Go coordinator: the Agent Sandbox runtime (packages/daemon/internal/
# runtime/sandbox) proven on a real cluster, in namespace `legion`, from a host the cluster's pods
# can reach: the devbox beside the production cluster, or a pod in the cluster itself (run through
# lib/proof-pod.sh run, scripts/e2e/README.md "From a pod"). A Go harness (sandbox/live_test.go,
# build tag e2e) drives the runtime through the Legion daemon's own restricted identity and hosts
# the worker stream on LEGION_E2E_STREAM_HOST; the pods it launches run the worker image under test
# and dial it. Operator steps (exec, PVC reads, a Secret's hash, the namespace list) use the
# operator context. Each check prints what it observed, naming the identity, then
# `CHECK <name>: PASS`; the first that fails ends the run non-zero, naming it.
#
# Every pod carries the operator route's pod (deploy/kubernetes/operator-route/pod.yml): its
# model route, overlay, ServiceAccount and projected token. Legion holds none of it. The run
# creates its own copy of the ConfigMap it mounts, named for the run's project, its
# models.yml pointed at LEGION_E2E_MODEL_GATEWAY_URL, and its own providers Secret
# (legion-<project>-providers, one key provider_keys names), before the harness runs; the teardown
# deletes both with the rest of the run's objects.
#
# Everything the run creates carries its own project label, s4a-<run id>, and lib/namespace-rig.sh
# owns it: on any exit the teardown deletes by exact name every Sandbox the harness recorded, then
# everything labelled with that exact project — never by label existence — and namespace-clean
# compares the namespace with the snapshot taken before the run, restricted to objects of this run
# or of no project, so another tree's objects cannot fail it. Nothing outside `legion` is touched.
#
# Inputs, each required (none defaults to a deployment): LEGION_E2E_RUNTIME_KUBECONFIG and
# LEGION_E2E_RUNTIME_CONTEXT name the restricted identity; LEGION_E2E_OPERATOR_CONTEXT the
# operator's, in the kubeconfig kubectl reads (KUBECONFIG or ~/.kube/config); LEGION_E2E_IMAGE the
# worker image by digest; LEGION_E2E_MODEL_GATEWAY_URL the model gateway's Anthropic endpoint, which
# the run substitutes for the operator route's models.yml placeholder;
# LEGION_E2E_MODEL_GATEWAY_AUDIENCE the audience that gateway accepts on a worker's projected token,
# which the run substitutes for the operator route's pod.yml placeholder; LEGION_E2E_STREAM_HOST the
# address the harness binds the worker stream on and every pod dials (the devbox's private address,
# or the proof pod's own, from the downward API); LEGION_E2E_REPO the repository each claim's
# workspace clones; LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_IMPLEMENT_APP_KEY_FILE the implement
# App the harness mints that repository's provisioning token from, its key a PEM file only its
# owner can read.
# Optional: LEGION_E2E_RUNTIME_SERVICE_ACCOUNT (<namespace>/<name>, which lib/proof-pod.sh run sets)
# makes the identity check require that ServiceAccount; unset, it requires the assumed IAM role in
# group legion-daemon the devbox's restricted context authenticates as. STAGE4A_FROM a development
# entry point, which is never the proof; STAGE4A_EVIDENCE_DIR where the transcript and the
# runtime's log go (default a fresh /tmp directory, kept and printed).
#
# The secrets-* checks are optional and print CHECK <name>: SKIPPED-BLOCKED when
# unconfigured: LEGION_E2E_AGENT_SECRETS_URL, LEGION_E2E_AGENT_SECRETS_OPERATOR (the email of the
# person an attended machine login is approved by, approved on the Dispatch credential page during
# the run), and LEGION_E2E_AGENT_SECRETS_AUTO_SHA256. secrets-approval-ask's own credential request is
# approved by that same LEGION_E2E_AGENT_SECRETS_OPERATOR, attended the same way as the machine
# login: the harness polls, prints STAGE4A: approve credential request …, and waits up to 10
# minutes for the operator's real approval. See scripts/e2e/README.md's Stage 4a section.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
namespace=legion
# The rigs' worker-stream port, beside the production daemon's 13370/13371. Stage 4b binds the same
# pair, and each stage refuses to start while the other holds it.
port=13373
repo=${LEGION_E2E_REPO:-}
app_id=${LEGION_E2E_IMPLEMENT_APP_ID:-}
app_key_file=${LEGION_E2E_IMPLEMENT_APP_KEY_FILE:-}
stream_host=${LEGION_E2E_STREAM_HOST:-}
operator=${LEGION_E2E_OPERATOR_CONTEXT:-}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
runtime_service_account=${LEGION_E2E_RUNTIME_SERVICE_ACCOUNT:-}
image=${LEGION_E2E_IMAGE:-}
agent_secrets_url=${LEGION_E2E_AGENT_SECRETS_URL:-}
agent_secrets_operator=${LEGION_E2E_AGENT_SECRETS_OPERATOR:-}
agent_secrets_auto_sha=${LEGION_E2E_AGENT_SECRETS_AUTO_SHA256:-}
from=${STAGE4A_FROM:-}
evidence=${STAGE4A_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e4a-evidence.XXXXXXXX)}
work=$(mktemp -d /tmp/legion-e2e4a.XXXXXXXX)
label_prefix=s4a-
project="${label_prefix}$(date -u +%Y%m%d%H%M%S)-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
run_label=$project
operator_route=$root/deploy/kubernetes/operator-route
route_configmap=legion-operator-route-$project
providers_secret=legion-$project-providers
record=$work/sandboxes
check=setup
torn_down=
snapshotted=
compared=
ok=

mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
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
  # Nothing exists to tear down before the snapshot, and the operator context may not be known yet.
  [ -z "$snapshotted" ] || teardown
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
for tool in go kubectl ss diff stat timeout; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -n "$runtime_context" ] || fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the runtime must run as the Legion daemon's restricted identity, never the operator's"
[ -n "$runtime_kubeconfig" ] || fail "LEGION_E2E_RUNTIME_KUBECONFIG is unset: it names the kubeconfig file holding $runtime_context"
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
[ -n "$operator" ] || fail "LEGION_E2E_OPERATOR_CONTEXT is unset: it names the operator's context, which runs the operator steps"
case "$image" in *@sha256:*) ;; *) fail "LEGION_E2E_IMAGE must be the worker image pinned by digest (…@sha256:…), not '$image'" ;; esac
gateway=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway URL the operator route's models.yml can name (the reason is above)"
gateway_audience=$(bash "$root/scripts/e2e/lib/model-gateway-audience.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_AUDIENCE is not a token audience the operator route's pod.yml can carry (the reason is above)"
# The pods dial the stream's address, so it is one they can reach: never loopback or unspecified.
case "$stream_host" in
  "") fail "LEGION_E2E_STREAM_HOST is unset: it is the address the harness binds the worker stream on and every pod dials" ;;
  0.0.0.0 | 127.* | localhost | *[!A-Za-z0-9.-]*) fail "LEGION_E2E_STREAM_HOST=$stream_host is not an address the cluster's pods can dial" ;;
esac
[[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "LEGION_E2E_REPO='$repo' is not <owner>/<name>: it names the repository each claim's workspace clones"
[[ $app_id =~ ^[0-9]+$ ]] || fail "LEGION_E2E_IMPLEMENT_APP_ID='$app_id' is not a GitHub App id"
[ -n "$app_key_file" ] || fail "LEGION_E2E_IMPLEMENT_APP_KEY_FILE is unset: it names the implement App's private key, a PEM file"
[ -f "$app_key_file" ] && [ -r "$app_key_file" ] || fail "LEGION_E2E_IMPLEMENT_APP_KEY_FILE names $app_key_file, which is not a readable file"
# The mode is the file the harness reads, so a symlink is followed: a link's own mode is always 777.
app_key_mode=$(stat -L -c %a -- "$app_key_file")
case $app_key_mode in
  *00) ;;
  *) fail "LEGION_E2E_IMPLEMENT_APP_KEY_FILE names $app_key_file, whose group or others have access (mode $app_key_mode): make it 0600" ;;
esac
if [ -n "$runtime_service_account" ] && ! [[ $runtime_service_account =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$ ]]; then
  fail "LEGION_E2E_RUNTIME_SERVICE_ACCOUNT=$runtime_service_account is not <namespace>/<name>"
fi
if [ -n "$(ss -Hltn "sport = :$port")" ]; then
  fail "port $port is taken on this host: $(ss -Hltnp "sport = :$port")"
fi
op get namespace "$namespace" -o name >/dev/null || fail "the operator context $operator cannot read namespace $namespace"
note "run project $project (every object's legion.dev/project label)"
note "image $image"
note "worker stream tcp://$stream_host:$port"
note "runtime identity: context $runtime_context in $runtime_kubeconfig${runtime_service_account:+, ServiceAccount $runtime_service_account}; operator: context $operator"
note "repository $repo, provisioned with the implement App $app_id's token"
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root") || fail "lib/built-from.sh could not read the source revision"
while IFS= read -r line; do note "$line"; done <<<"$built"
[ -z "$from" ] || note "STAGE4A_FROM=$from: a development run, never the proof"
if [ -n "$agent_secrets_url" ]; then
  note "agent-secrets: $agent_secrets_url"
else
  note "agent-secrets: none (the secrets-* checks report SKIPPED-BLOCKED)"
fi

begin snapshot
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
note "[operator] $(wc -l <"$evidence/namespace-before.txt") objects in $namespace carry no project label or project $project"

begin operator-route
# shellcheck disable=SC2016  # the operator route's literal placeholder, not an expansion
placeholder='${MODEL_BASE_URL}'
models=$(<"$operator_route/models.yml")
printf '%s\n' "${models//"$placeholder"/"$gateway"}" >"$work/models.yml"
grep -qFx "    baseUrl: $gateway" "$work/models.yml" || fail "the operator route's models.yml has no baseUrl $placeholder to point at the gateway"
# shellcheck disable=SC2016  # the operator route's literal placeholder, not an expansion
placeholder='${MODEL_TOKEN_AUDIENCE}'
pod=$(<"$operator_route/pod.yml")
printf '%s\n' "${pod//"$placeholder"/"$gateway_audience"}" >"$work/pod.yml"
grep -qF "audience: \"$gateway_audience\"" "$work/pod.yml" || fail "the operator route's pod.yml has no token audience $placeholder to fill with the gateway's"
op create configmap "$route_configmap" --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$operator_route/overlay.yml" \
  --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create ConfigMap $route_configmap"
note "[operator] ConfigMap $route_configmap: models.yml (baseUrl from LEGION_E2E_MODEL_GATEWAY_URL) and overlay.yml from $operator_route, label legion.dev/project=$run_label"
note "operator pod: $work/pod.yml, the operator route's with its token audience from LEGION_E2E_MODEL_GATEWAY_AUDIENCE"
# The run's providers Secret, named as the runtime names it (ProvidersSecretName), holding one key no
# model route reads: provider_keys hands it to every agent's Oh My Pi, and provider-key checks where
# it arrives.
op create secret generic "$providers_secret" --from-literal=stage4a="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" \
  --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create Secret $providers_secret"
note "[operator] Secret $providers_secret: one key, stage4a (a random value no route reads), label legion.dev/project=$run_label"
pass

begin build
go -C "$root/packages/daemon" test -c -tags e2e -o "$work/stage4a.test" ./internal/runtime/sandbox
go -C "$root/packages/envoy" build -o "$work/agent-secrets" ./cmd/agent-secrets
note "built the e2e harness and agent-secrets from the checkout"

harness_ok=
if env \
  LEGION_E2E_RUNTIME_KUBECONFIG="$runtime_kubeconfig" \
  LEGION_E2E_RUNTIME_CONTEXT="$runtime_context" \
  LEGION_E2E_RUNTIME_SERVICE_ACCOUNT="$runtime_service_account" \
  LEGION_E2E_OPERATOR_CONTEXT="$operator" \
  LEGION_E2E_NAMESPACE="$namespace" \
  LEGION_E2E_PROJECT="$project" \
  LEGION_E2E_IMAGE="$image" \
  LEGION_E2E_REPO="$repo" \
  LEGION_E2E_STREAM_HOST="$stream_host" \
  LEGION_E2E_STREAM_PORT="$port" \
  LEGION_E2E_IMPLEMENT_APP_ID="$app_id" \
  LEGION_E2E_IMPLEMENT_APP_KEY_FILE="$app_key_file" \
  LEGION_E2E_RECORD="$record" \
  LEGION_E2E_WORK="$evidence" \
  LEGION_E2E_FROM="$from" \
  LEGION_E2E_OPERATOR_POD="$work/pod.yml" \
  LEGION_E2E_OPERATOR_CONFIGMAP="$route_configmap" \
  LEGION_E2E_AGENT_SECRETS_URL="$agent_secrets_url" \
  LEGION_E2E_AGENT_SECRETS_OPERATOR="$agent_secrets_operator" \
  LEGION_E2E_AGENT_SECRETS_AUTO_SHA256="$agent_secrets_auto_sha" \
  LEGION_E2E_AGENT_SECRETS_BIN="$work/agent-secrets" \
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
