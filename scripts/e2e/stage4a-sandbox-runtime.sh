#!/usr/bin/env bash
# Stage 4a's gate for the Go coordinator: the Agent Sandbox runtime (packages/daemon-go/internal/
# runtime/sandbox) proven on the production cluster, in namespace `legion`, from the devbox. A Go
# harness (sandbox/live_test.go, build tag e2e) drives the runtime through the Legion daemon's own
# restricted identity and hosts the worker stream on the devbox's private address; the pods it
# launches run the worker image under test and dial it. Operator steps (exec, PVC reads, a Secret's
# hash, the namespace list) use the admin context. Each check prints what it observed, naming the
# identity, then `CHECK <name>: PASS`; the first that fails ends the run non-zero, naming it.
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
# Inputs: LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
# ~/.kube/legion-daemon-production) name the restricted identity; LEGION_E2E_OPERATOR_CONTEXT
# (default production) the admin one; LEGION_E2E_IMAGE (required) the worker image by digest;
# LEGION_E2E_MODEL_GATEWAY_URL (required) the model gateway's Anthropic endpoint, which the run
# substitutes for the operator route's models.yml placeholder; LEGION_E2E_MODEL_GATEWAY_AUDIENCE
# (required) the audience that gateway accepts on a worker's projected token, which the run
# substitutes for the operator route's pod.yml placeholder;
# STAGE4A_FROM a development entry point, which is never the proof; STAGE4A_EVIDENCE_DIR where the
# transcript and the runtime's log go (default a fresh /tmp directory, kept and printed).
#
# The secrets-* checks (AGENTC-393) are optional and print CHECK <name>: SKIPPED-BLOCKED when
# unconfigured: LEGION_E2E_AGENT_SECRETS_URL, LEGION_E2E_AGENT_SECRETS_OPERATOR (the login an
# attended machine login is approved by, approved on the Dispatch credential page during the
# run), and LEGION_E2E_AGENT_SECRETS_AUTO_SHA256. secrets-approval-ask's own credential request is
# approved by that same LEGION_E2E_AGENT_SECRETS_OPERATOR, attended the same way as the machine
# login: the harness polls, prints STAGE4A: approve credential request …, and waits up to 10
# minutes for the operator's real approval. See scripts/e2e/README.md's Stage 4a section.
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
# tee shares the driver's process group, so a signal to the group (Ctrl-C, a closed pane, timeout's
# TERM) would end it before cleanup writes, and cleanup's first write would die of SIGPIPE: tee
# ignores the signals the driver traps, and outlives the driver's last line. Whoever reads the
# driver's output can go first (a supervised launcher's own tee, stopped with it): tee then ignores
# SIGPIPE and, with -p, stops writing that reader and keeps writing the transcript, so it keeps
# draining the driver's output, and cleanup's commands (kubectl delete among them) never fail on a
# write of their own.
exec > >(trap '' HUP INT TERM PIPE && exec tee -p -a "$evidence/transcript.log") 2>&1
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
# shellcheck source-path=SCRIPTDIR source=lib/stage-role-prompts.sh
. "$root/scripts/e2e/lib/stage-role-prompts.sh"

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
gateway=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway URL the operator route's models.yml can name (the reason is above)"
gateway_audience=$(bash "$root/scripts/e2e/lib/model-gateway-audience.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_AUDIENCE is not a token audience the operator route's pod.yml can carry (the reason is above)"
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
  --dry-run=client -o yaml | kubectl label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create ConfigMap $route_configmap"
note "[operator] ConfigMap $route_configmap: models.yml (baseUrl from LEGION_E2E_MODEL_GATEWAY_URL) and overlay.yml from $operator_route, label legion.dev/project=$run_label"
note "operator pod: $work/pod.yml, the operator route's with its token audience from LEGION_E2E_MODEL_GATEWAY_AUDIENCE"
# The run's providers Secret, named as the runtime names it (ProvidersSecretName), holding one key no
# model route reads: provider_keys hands it to every agent's Oh My Pi, and provider-key checks where
# it arrives.
op create secret generic "$providers_secret" --from-literal=stage4a="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" \
  --dry-run=client -o yaml | kubectl label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create Secret $providers_secret"
note "[operator] Secret $providers_secret: one key, stage4a (a random value no route reads), label legion.dev/project=$run_label"
pass

begin build
go -C "$root/packages/daemon-go" test -c -tags e2e -o "$work/stage4a.test" ./internal/runtime/sandbox
stage_role_prompts "$root" "$work"
go -C "$root/packages/envoy" build -o "$work/agent-secrets" ./cmd/agent-secrets
note "built the e2e harness and agent-secrets from the checkout, with role-prompts beside the harness"

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
