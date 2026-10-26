#!/usr/bin/env bash
# Stage 4a's gate for the Go coordinator: the Agent Sandbox runtime (packages/daemon/internal/
# runtime/sandbox) proven on a real cluster, in namespace `legion`, from the devbox. A Go harness
# (sandbox/live_test.go, build tag e2e) drives the runtime through the Legion daemon's own
# restricted identity and hosts the worker stream on the devbox; the pods it launches run the
# worker image under test and dial it. Operator steps (exec, PVC reads, a Secret's hash, the
# namespace list) use the admin context. Each check prints what it observed, naming the identity,
# then `CHECK <name>: PASS`; the first that fails ends the run non-zero, naming it.
#
# Three explicit inputs, never auto-detected from one another:
#
# LEGION_E2E_PLATFORM names the cluster's own platform (`gke-autopilot` or `eks`), independent of
# identity and reachability below: a cluster could in principle combine any of these differently,
# even though every cluster this harness has run against so far happens to pair gke-autopilot with
# kubernetes-service-account and bridge, and eks with aws-iam-role and aws-private-ip. Every
# platform-specific relaxation the Go harness makes (a gVisor cpu floor, ephemeral-storage
# equalization, the extended-duration-pods NodeAffinity, cgroup-quota-over-nproc) keys off this
# input alone.
#
# LEGION_E2E_POD_REACHABILITY picks how the run's pods reach the devbox's worker stream and
# scratch session Postgres:
#   - `aws-private-ip`: today's shape. The harness binds the devbox's own private address (read
#     from instance metadata), which the cluster's pods already route to directly.
#   - `bridge`: for a cluster whose pods cannot reach the devbox at all (no tailnet, no routable
#     private address — GKE's shape). lib/bridge-tunnel.sh stands up a scratch pod+Service of the
#     run's own in the namespace (operator context, labelled for teardown like everything else)
#     running a chisel reverse-tunnel server, and a reverse tunnel from the devbox through
#     `kubectl port-forward` plus a chisel client built from pinned source; pods dial the Service,
#     which proxies back to the devbox's loopback. A dropped port-forward is recovered, not fatal
#     (lib/bridge-tunnel.sh's watchdog restarts it on the same local port, up to 60s); only a
#     restart that cannot land in time, or the chisel client exhausting its own bounded reconnect
#     budget, fails the whole run loudly.
#
# LEGION_E2E_MODEL_ROUTE picks how the operator's pod reaches a model, independently of
# reachability — a cluster can have a model gateway or not regardless of which one it is:
#   - `gateway`: today's shape, deploy/kubernetes/operator-route/{pod.yml,models.yml}. A projected
#     ServiceAccount token (LEGION_E2E_MODEL_GATEWAY_AUDIENCE) authenticates to
#     LEGION_E2E_MODEL_GATEWAY_URL.
#   - `key-file`: for a cluster with no model gateway in front of Anthropic (GKE's shape).
#     deploy/kubernetes/operator-route/{pod-key-file.yml,models-key-file.yml}: no projected token
#     at all; ANTHROPIC_API_KEY reaches the agent as a `provider_keys` entry, the run's providers
#     Secret's own key, mirroring the live GKE route's shape. LEGION_E2E_MODEL_GATEWAY_URL and
#     LEGION_E2E_MODEL_GATEWAY_AUDIENCE are unused in this mode. No check here makes a real model
#     round trip (the pods run a stub agent, decision 7; Legion's own image probe makes none
#     either), so the key need not be one a real Anthropic call would accept — only in place,
#     reaching the agent's environment exactly as the live route delivers it. The `operator-token`
#     check's ServiceAccount, automount and no-API-token assertions still run and pass; only its
#     one projected-token assertion is skipped, since there is no projected token for it to read.
#
# Every pod carries the operator route's pod fixture for the chosen mode. Legion holds none of it.
# The run creates its own copy of the ConfigMap it mounts, named for the run's project, its
# models.yml pointed at the chosen mode's endpoint, and its own providers Secret
# (legion-<project>-providers, the keys provider_keys and the chosen model route need), before the
# harness runs; the teardown deletes both with the rest of the run's objects.
#
# Everything the run creates carries its own project label, s4a-<run id>, and lib/namespace-rig.sh
# owns it: on any exit the teardown deletes by exact name every Sandbox the harness recorded, then
# everything labelled with that exact project — never by label existence — and namespace-clean
# compares the run's own objects with the snapshot taken before the run (lib/namespace-rig.sh), so
# another system's objects cannot fail it. The bridge's own pod and Service (bridge mode only) are
# torn down by lib/bridge-tunnel.sh before that comparison runs. Nothing outside `legion` is
# touched.
#
# Inputs: LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
# ~/.kube/legion-daemon-production) name the restricted identity; LEGION_E2E_OPERATOR_CONTEXT
# (default production) the admin one; LEGION_E2E_RUNTIME_IDENTITY_KIND (required, `aws-iam-role` or
# `kubernetes-service-account`) what shape that identity's `identity` check expects — an assumed
# IAM role mapped to RBAC group legion-daemon, or a plain system:serviceaccount:<namespace>:
# legion-daemon with no such mapping; LEGION_E2E_IMAGE (required) the worker image by digest;
# LEGION_E2E_PLATFORM (required, `gke-autopilot` or `eks`, above), LEGION_E2E_POD_REACHABILITY and
# LEGION_E2E_MODEL_ROUTE (both required, above);
# LEGION_E2E_STORAGE_CLASS (required) the issue volumes' StorageClass — AWS's gp2, GKE's
# standard-rwo, or whatever else the cluster under test defaults to; LEGION_E2E_MODEL_GATEWAY_URL
# and LEGION_E2E_MODEL_GATEWAY_AUDIENCE (both required under LEGION_E2E_MODEL_ROUTE=gateway, unused
# under key-file) the gateway's Anthropic endpoint and the token audience it accepts, substituted
# for the operator route's placeholders; LEGION_E2E_WORKER_STREAM_PORT (default 13373, the same
# name and default Stage 4b reads for its own worker-stream port, since the two collide on it by
# default) the port the harness binds for the worker stream; LEGION_E2E_SESSION_DB_PORT (required)
# a port the run's scratch Postgres listens on, reachable everywhere the worker-stream port is
# (under aws-private-ip, a port the devbox's security group admits from the Legion nodes; under
# bridge, forwarded through the same tunnel); LEGION_E2E_REPO_SECOND_OWNER (required) `<owner>/<name>`,
# a repository under an owner other than sjawhar on which both Legion Apps are installed, where
# second-owner provisions an issue and proves its pod's gh and git read sjawhar/legion-smoke with
# read-only tokens (nothing is pushed to it); LEGION_E2E_IMPLEMENT_APP_ID and
# LEGION_E2E_IMPLEMENT_APP_KEY_NAME (this deployment's implement App by default) the App's numeric
# id and the secret-store key the `secrets` CLI resolves to its private key (agent tier: no YubiKey
# touch), which prerequisites proves is that id's App against GitHub's GET /app before the run
# writes anything (lib/github-apps.sh; the run acts as no other App); STAGE4A_FROM a development entry point, which is
# never the proof; STAGE4A_EVIDENCE_DIR where the transcript and the runtime's log go (default a
# fresh /tmp directory, kept and printed).
#
# The secrets-* checks are optional and print CHECK <name>: SKIPPED-BLOCKED when
# unconfigured: LEGION_E2E_AGENT_SECRETS_URL, LEGION_E2E_AGENT_SECRETS_OPERATOR (the email of the
# person who approves secrets-approval-ask's credential request on the Dispatch credential page
# during the run), LEGION_E2E_AGENT_SECRETS_AUTO_SHA256, and LEGION_E2E_AGENT_SECRETS_ACCOUNT
# (<namespace>/<name> of a ServiceAccount of the proof's own, which the broker's
# BROKER_SERVICE_LOGINS lets log in as legion-daemon and no pod runs as: each account holds one
# launcher credential, so logging in as a running daemon's account would take that daemon's). The
# run logs in as the daemon does, with that account's token, which the operator context mints with
# `kubectl create token` for the broker's audience, an hour long, and mints again every half hour
# while the run lasts; no person approves it. See scripts/e2e/README.md's Stage 4a section.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
namespace=legion
# The rigs' worker-stream port, within the devbox's admitted range. Overridable
# (LEGION_E2E_WORKER_STREAM_PORT, see Inputs, above) so two runs — two Stage 4a's, or a Stage 4a
# and a Stage 4b — can each hold their own; left at its default, each stage refuses to start while
# the other holds it, exactly as before. A lane's port must be one the devbox's security group
# admits from the Legion nodes (README's port-admission note has the current allocation and how
# many lanes it leaves room for); widening that admitted range is a change to the deployment
# repository's infrastructure, not this one.
port=${LEGION_E2E_WORKER_STREAM_PORT:-13373}
repo=sjawhar/legion-smoke
second_owner_repo=${LEGION_E2E_REPO_SECOND_OWNER:-}
# The scratch session database's image: the pinned Postgres the repository's CI tests against.
session_db_image=pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a
session_db_port=${LEGION_E2E_SESSION_DB_PORT:-}
operator=${LEGION_E2E_OPERATOR_CONTEXT:-production}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-$HOME/.kube/legion-daemon-production}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
runtime_identity_kind=${LEGION_E2E_RUNTIME_IDENTITY_KIND:-}
platform=${LEGION_E2E_PLATFORM:-}
image=${LEGION_E2E_IMAGE:-}
pod_reachability=${LEGION_E2E_POD_REACHABILITY:-}
model_route=${LEGION_E2E_MODEL_ROUTE:-}
storage_class=${LEGION_E2E_STORAGE_CLASS:-}
agent_secrets_url=${LEGION_E2E_AGENT_SECRETS_URL:-}
agent_secrets_operator=${LEGION_E2E_AGENT_SECRETS_OPERATOR:-}
agent_secrets_auto_sha=${LEGION_E2E_AGENT_SECRETS_AUTO_SHA256:-}
agent_secrets_account=${LEGION_E2E_AGENT_SECRETS_ACCOUNT:-}
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
session_db=legion-e2e4a-db-$project
# bridge_name is deterministic from the project alone (lib/bridge-tunnel.sh's pod and Service),
# so the cluster-DNS host pods would dial under LEGION_E2E_POD_REACHABILITY=bridge is known before
# the bridge itself is ever stood up.
bridge_name=bridge-$project
session_db_started=
agent_secrets_reminter=
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
# shellcheck source-path=SCRIPTDIR source=lib/bridge-tunnel.sh
. "$root/scripts/e2e/lib/bridge-tunnel.sh"
# shellcheck source-path=SCRIPTDIR source=lib/teardown.sh
. "$root/scripts/e2e/lib/teardown.sh"
# shellcheck source-path=SCRIPTDIR source=lib/github-apps.sh
. "$root/scripts/e2e/lib/github-apps.sh"

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

begin prerequisites
for tool in go kubectl curl ss secrets diff docker jq bun; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -n "$runtime_context" ] || fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the runtime must run as the Legion daemon's restricted identity, never the operator's"
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
case "$runtime_identity_kind" in
aws-iam-role)
  command -v aws >/dev/null || fail "aws is required: LEGION_E2E_RUNTIME_IDENTITY_KIND=aws-iam-role authenticates the runtime kubeconfig through 'aws eks get-token'"
  ;;
kubernetes-service-account) ;;
*) fail "LEGION_E2E_RUNTIME_IDENTITY_KIND must be aws-iam-role or kubernetes-service-account, not '$runtime_identity_kind'" ;;
esac
case "$platform" in
gke-autopilot | eks) ;;
*) fail "LEGION_E2E_PLATFORM must be gke-autopilot or eks, not '$platform'" ;;
esac
case "$image" in *@sha256:*) ;; *) fail "LEGION_E2E_IMAGE must be the worker image pinned by digest (…@sha256:…), not '$image'" ;; esac
[ -n "$storage_class" ] || fail "LEGION_E2E_STORAGE_CLASS is unset: name the issue volumes' StorageClass (the cluster has no default to fall back on)"
case "$model_route" in
gateway)
  gateway=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
    fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway URL the operator route's models.yml can name (the reason is above)"
  gateway_audience=$(bash "$root/scripts/e2e/lib/model-gateway-audience.sh") ||
    fail "LEGION_E2E_MODEL_GATEWAY_AUDIENCE is not a token audience the operator route's pod.yml can carry (the reason is above)"
  ;;
key-file)
  gateway=
  gateway_audience=
  ;;
*) fail "LEGION_E2E_MODEL_ROUTE must be gateway or key-file, not '$model_route'" ;;
esac
case "$pod_reachability" in
aws-private-ip)
  imds=$(curl -sf -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60') ||
    fail "instance metadata is unreachable; the harness binds the devbox's private address, read from it"
  bind_host=$(curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4) ||
    fail "instance metadata has no local-ipv4"
  unset imds
  advertise_host=$bind_host
  ;;
bridge)
  # The devbox binds loopback; the reverse-tunnel bridge (begin bridge, below) is what makes
  # advertise_host — the bridge's own ClusterIP Service, deterministic from bridge_name — dialable
  # from inside the namespace.
  bind_host=127.0.0.1
  advertise_host="$bridge_name.$namespace.svc.cluster.local"
  ;;
*) fail "LEGION_E2E_POD_REACHABILITY must be aws-private-ip or bridge, not '$pod_reachability'" ;;
esac
case "$session_db_port" in
'') fail "LEGION_E2E_SESSION_DB_PORT is unset: postgres-resume's scratch Postgres listens on a port every pod reaches as it reaches the worker stream" ;;
*[!0-9]*) fail "LEGION_E2E_SESSION_DB_PORT must be a port number, not '$session_db_port'" ;;
esac
[ "$session_db_port" != "$port" ] || fail "LEGION_E2E_SESSION_DB_PORT is the worker stream's port $port"
[ -n "$second_owner_repo" ] ||
  fail "LEGION_E2E_REPO_SECOND_OWNER is unset: the second-owner check provisions an issue on a repository of another owner (owner/name, never a literal here)"
[[ $second_owner_repo =~ ^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$ ]] || fail "LEGION_E2E_REPO_SECOND_OWNER must be <owner>/<name>, not '$second_owner_repo'"
[ "$(tr '[:upper:]' '[:lower:]' <<<"${second_owner_repo%%/*}")" != "$(tr '[:upper:]' '[:lower:]' <<<"${repo%%/*}")" ] ||
  fail "LEGION_E2E_REPO_SECOND_OWNER $second_owner_repo is under ${repo%%/*}, $repo's owner: the check needs a repository of another owner"
require_app_inputs implement
write_github_apps implement
check_github_apps implement
for taken in "$port" "$session_db_port"; do
  if [ -n "$(ss -Hltn "sport = :$taken")" ]; then
    fail "port $taken is taken on the devbox: $(ss -Hltnp "sport = :$taken")"
  fi
done
op get namespace "$namespace" -o name >/dev/null || fail "the operator context $operator cannot read namespace $namespace"
note "run project $project (every object's legion.dev/project label)"
note "image $image"
note "pod reachability: $pod_reachability; model route: $model_route; storage class: $storage_class"
if [ "$pod_reachability" = aws-private-ip ]; then
  note "worker stream tcp://$bind_host:$port (the devbox's private address)"
  note "scratch session database $bind_host:$session_db_port ($session_db_image)"
else
  note "worker stream tcp://$bind_host:$port (devbox loopback), advertised to pods as tcp://$advertise_host:$port (the bridge's Service)"
  note "scratch session database $bind_host:$session_db_port ($session_db_image), advertised to pods as $advertise_host:$session_db_port"
fi
note "runtime identity: context $runtime_context in $runtime_kubeconfig ($runtime_identity_kind); operator: context $operator"
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root") || fail "lib/built-from.sh could not read the source revision"
while IFS= read -r line; do note "$line"; done <<<"$built"
[ -z "$from" ] || note "STAGE4A_FROM=$from: a development run, never the proof"
if [ -n "$agent_secrets_url" ]; then
  note "agent-secrets: $agent_secrets_url"
else
  note "agent-secrets: none (the secrets-* checks report SKIPPED-BLOCKED)"
fi
agent_secrets_token_file=
if [ -n "$agent_secrets_account" ]; then
  # The run's login token: what the kubelet projects into the daemon's pod, minted here by the
  # operator context for the broker's audience and written owner-only; never printed. The broker
  # refuses a token that lives longer than an hour, so the run mints one every half hour, and its
  # login reads the file afresh each time, as the daemon reads the kubelet's.
  account_namespace=${agent_secrets_account%%/*}
  account_name=${agent_secrets_account#*/}
  [ -n "$account_namespace" ] && [ -n "$account_name" ] && [ "$account_namespace" != "$agent_secrets_account" ] ||
    fail "LEGION_E2E_AGENT_SECRETS_ACCOUNT must be <namespace>/<name>, got $agent_secrets_account"
  account_pods=$(timeout --foreground 300 kubectl --context "$operator" -n "$account_namespace" get pods \
    -o jsonpath="{range .items[?(@.spec.serviceAccountName==\"$account_name\")]}{.metadata.name}{' '}{end}") ||
    fail "the operator context could not list the pods of $account_namespace to check that no daemon runs as $agent_secrets_account"
  [ -z "$account_pods" ] ||
    fail "LEGION_E2E_AGENT_SECRETS_ACCOUNT $agent_secrets_account is the account pod(s) ${account_pods% } run as: a login as it would take that daemon's one launcher credential; list a ServiceAccount of the proof's own in BROKER_SERVICE_LOGINS and name it here"
  agent_secrets_token_file=$work/agent-secrets-token
  mint_agent_secrets_token() {
    (
      umask 077
      timeout --foreground 300 kubectl --context "$operator" -n "$account_namespace" create token "$account_name" --audience agent-secrets --duration 1h >"$agent_secrets_token_file.next"
    ) && mv -f "$agent_secrets_token_file.next" "$agent_secrets_token_file"
  }
  mint_agent_secrets_token || fail "the operator could not mint a token for $agent_secrets_account"
  (
    trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM
    while :; do
      sleep 1800 &
      sleeper=$!
      wait "$sleeper"
      mint_agent_secrets_token || echo "   [operator] could not mint a fresh agent-secrets token for $agent_secrets_account; the run's logins fail once the last one expires"
    done
  ) &
  agent_secrets_reminter=$!
  note "[operator] minted a 1h agent-secrets token for $agent_secrets_account (owner-only file, not printed), minted again every 30 minutes"
fi

begin plugins
# The stale-plugin-install checkpoint's own pack of this checkout's two plugins, through
# lib/pack-plugin.sh as every other proof that installs a branch-built plugin packs: it points
# each manifest at its dist bundle exactly as the release does (scripts/pi-plugin-prepack.sh
# refuses to pack the committed manifest's own source-path omp.extensions), runs `bun pm pack`
# with the prepack's build report on stderr, restores the committed manifest on every exit of its
# own, and prints the tarball's path alone. The checkpoint's negative control names the published
# pi-legion@9.6.1 by exact version with no pack of its own: the daemon-rendered install script
# resolves that version to @sjawhar/pi-legion@9.6.1 and bun fetches it from the registry inside
# the pod. Failing here fails the run before anything is created in the namespace.
plugins_dir="$work/plugins"
pi_envoy_version=$(jq -r .version "$root/packages/pi-envoy/package.json") || fail "reading packages/pi-envoy/package.json's version failed"
pi_legion_version=$(jq -r .version "$root/packages/pi-legion/package.json") || fail "reading packages/pi-legion/package.json's version failed"
pi_envoy_tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$plugins_dir/pi-envoy") ||
  fail "packing packages/pi-envoy (pi-envoy@$pi_envoy_version) failed"
pi_legion_tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-legion "$plugins_dir/pi-legion") ||
  fail "packing packages/pi-legion (pi-legion@$pi_legion_version) failed"
note "packed pi-envoy@$pi_envoy_version ($pi_envoy_tarball) and pi-legion@$pi_legion_version ($pi_legion_tarball) from the checkout"
pass

begin snapshot
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
note "[operator] $(wc -l <"$evidence/namespace-before.txt") of the run's objects in $namespace before it starts (labelled with project $project, or unlabelled and named for it)"

if [ "$pod_reachability" = bridge ]; then
  begin bridge
  bridge_start "$port" "$session_db_port"
  pass
else
  note "LEGION_E2E_POD_REACHABILITY=aws-private-ip: no bridge needed, pods reach $bind_host directly (no bridge check: there is nothing to test)"
fi

begin session-db
# postgres-resume's session database: a throwaway Postgres on the devbox, which pods reach as they
# reach the worker stream (directly in AWS mode, through the bridge in GKE mode). Its password is
# the run's own and reaches docker through an owner-only env file, never an argument; the URL is
# written only to an owner-only file, which the harness and the providers Secret read, and is
# never printed. The harness's own direct connection (postgres-resume's writeSession) needs the
# address the devbox itself can dial — bind_host, loopback under bridge — never advertise_host (a
# bridge-mode devbox cannot resolve the bridge's cluster-DNS name), so a second file carries the
# same database at that address, for the harness alone; the providers Secret and every pod still
# read the advertise_host file above.
session_db_password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
(
  umask 077
  printf 'POSTGRES_USER=legion\nPOSTGRES_DB=sessions\nPOSTGRES_PASSWORD=%s\n' "$session_db_password" >"$work/session-db.env"
  printf 'postgres://legion:%s@%s:%s/sessions?sslmode=disable\n' "$session_db_password" "$advertise_host" "$session_db_port" >"$work/session-dsn"
  printf 'postgres://legion:%s@%s:%s/sessions?sslmode=disable\n' "$session_db_password" "$bind_host" "$session_db_port" >"$work/session-dsn-local"
)
unset session_db_password
session_db_started=1
docker run -d --name "$session_db" --cpus 1 --env-file "$work/session-db.env" -p "$bind_host:$session_db_port:5432" "$session_db_image" >/dev/null ||
  fail "docker could not start the scratch session database $session_db on $bind_host:$session_db_port"
ready=
for _ in $(seq 60); do
  if docker exec "$session_db" pg_isready -q -h 127.0.0.1 -U legion -d sessions; then
    ready=1
    break
  fi
  sleep 1
done
[ -n "$ready" ] || fail "the scratch session database $session_db did not accept connections within 60s: $(docker logs --tail 20 "$session_db" 2>&1)"
note "container $session_db: database sessions on $bind_host:$session_db_port, accepting connections (the URL is in an owner-only file, not printed)"
pass

begin operator-route
anthropic_key_args=()
case "$model_route" in
gateway)
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
  note "[operator] ConfigMap $route_configmap: models.yml (baseUrl from LEGION_E2E_MODEL_GATEWAY_URL) and overlay.yml from $operator_route, label legion.dev/project=$run_label"
  note "operator pod: $work/pod.yml, the operator route's with its token audience from LEGION_E2E_MODEL_GATEWAY_AUDIENCE"
  ;;
key-file)
  cp "$operator_route/models-key-file.yml" "$work/models.yml"
  cp "$operator_route/pod-key-file.yml" "$work/pod.yml"
  # No check here makes a real model round trip (the pods run a stub agent, decision 7; Legion's
  # own image probe makes none either), so this is a locally-generated placeholder, never printed:
  # it proves the key-file plumbing (provider_keys, the shim's export into the agent's
  # environment, models.yml's command reading it back), never a working Anthropic credential.
  (umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$work/anthropic-api-key")
  anthropic_key_args=(--from-file=ANTHROPIC_API_KEY="$work/anthropic-api-key")
  note "[operator] ConfigMap $route_configmap: models-key-file.yml (Anthropic's own endpoint, no gateway) and overlay.yml from $operator_route, label legion.dev/project=$run_label"
  note "operator pod: $work/pod.yml, the key-file fixture (no projected token: operator-token's projected-token assertion is skipped, the rest still run and pass)"
  ;;
esac
op create configmap "$route_configmap" --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$operator_route/overlay.yml" \
  --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create ConfigMap $route_configmap"
# The run's providers Secret, named as the runtime names it (ProvidersSecretName): one key no model
# route reads, the scratch session database's URL, which postgres-resume's runtime keeps sessions
# in (session_dsn_secret stage4a_sessions), and, under LEGION_E2E_MODEL_ROUTE=key-file, a
# placeholder ANTHROPIC_API_KEY.
op create secret generic "$providers_secret" --from-literal=stage4a="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" \
  --from-file=stage4a_sessions="$work/session-dsn" \
  "${anthropic_key_args[@]}" \
  --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
  fail "the operator could not create Secret $providers_secret"
note "[operator] Secret $providers_secret: stage4a (a random value no route reads), stage4a_sessions (the scratch session database's URL, not printed)$([ "$model_route" = key-file ] && echo ", and ANTHROPIC_API_KEY (a placeholder: no check calls the model)"), label legion.dev/project=$run_label"
pass

begin build
go -C "$root/packages/daemon" test -c -tags e2e -o "$work/stage4a.test" ./internal/runtime/sandbox
go -C "$root/packages/envoy" build -o "$work/agent-secrets" ./cmd/agent-secrets
note "built the e2e harness and agent-secrets from the checkout"

harness_ok=
# -test.timeout 150m must stay under the harness's staleRunBranchAge (4 hours): another run's
# cache-lost-volume deletes a cache-b branch older than that as a killed run's leftover.
env \
  LEGION_E2E_RUNTIME_KUBECONFIG="$runtime_kubeconfig" \
  LEGION_E2E_RUNTIME_CONTEXT="$runtime_context" \
  LEGION_E2E_OPERATOR_CONTEXT="$operator" \
  LEGION_E2E_NAMESPACE="$namespace" \
  LEGION_E2E_PROJECT="$project" \
  LEGION_E2E_IMAGE="$image" \
  LEGION_E2E_REPO="$repo" \
  LEGION_E2E_REPO_SECOND_OWNER="$second_owner_repo" \
  LEGION_E2E_STREAM_HOST="$bind_host" \
  LEGION_E2E_STREAM_ADVERTISE_HOST="$advertise_host" \
  LEGION_E2E_STREAM_PORT="$port" \
  LEGION_E2E_STORAGE_CLASS="$storage_class" \
  LEGION_E2E_MODEL_ROUTE="$model_route" \
  LEGION_E2E_RUNTIME_IDENTITY_KIND="$runtime_identity_kind" \
  LEGION_E2E_PLATFORM="$platform" \
  LEGION_E2E_POD_REACHABILITY="$pod_reachability" \
  LEGION_E2E_IMPLEMENT_APP_ID="$implement_app_id" \
  LEGION_E2E_IMPLEMENT_APP_KEY_NAME="$implement_app_key_name" \
  LEGION_E2E_RECORD="$record" \
  LEGION_E2E_WORK="$evidence" \
  LEGION_E2E_FROM="$from" \
  LEGION_E2E_OPERATOR_POD="$work/pod.yml" \
  LEGION_E2E_OPERATOR_CONFIGMAP="$route_configmap" \
  LEGION_E2E_AGENT_SECRETS_URL="$agent_secrets_url" \
  LEGION_E2E_AGENT_SECRETS_OPERATOR="$agent_secrets_operator" \
  LEGION_E2E_AGENT_SECRETS_AUTO_SHA256="$agent_secrets_auto_sha" \
  LEGION_E2E_AGENT_SECRETS_BIN="$work/agent-secrets" \
  LEGION_E2E_AGENT_SECRETS_TOKEN_FILE="$agent_secrets_token_file" \
  LEGION_E2E_SESSION_DSN_FILE="$work/session-dsn" \
  LEGION_E2E_SESSION_DSN_LOCAL_FILE="$work/session-dsn-local" \
  LEGION_E2E_SESSION_DB_ADDRESS="$advertise_host:$session_db_port" \
  LEGION_E2E_PI_LEGION_TARBALL="$pi_legion_tarball" \
  LEGION_E2E_PI_ENVOY_TARBALL="$pi_envoy_tarball" \
  LEGION_E2E_PI_LEGION_VERSION="$pi_legion_version" \
  LEGION_E2E_PI_ENVOY_VERSION="$pi_envoy_version" \
  "$work/stage4a.test" -test.run '^TestStage4aSandboxRuntimeLive$' -test.v -test.timeout 150m &
# Backgrounded and waited on, not run as env's own foreground child: a foreign command's shell
# blocks delivering a trapped signal until that command itself exits, so a bare `if env ... cmd
# ...; then` made the watchdog's own 60s give-up bound (lib/bridge-tunnel.sh) meaningless — its
# SIGTERM would queue for up to the Go test's own 150-minute timeout before this script ever saw
# it. `wait` is interruptible by a trapped signal immediately; harness.pid lets the watchdog and
# cleanup kill the harness itself too, not just stop waiting on it.
harness_pid=$!
echo "$harness_pid" >"$work/harness.pid"
if wait "$harness_pid"; then
  harness_ok=1
fi
check=harness
[ -n "$harness_ok" ] || fail "a harness check failed (above)"

teardown_sequence
if [ -n "$from" ]; then
  echo "stage 4a e2e: every check from $from passed — a development run, never the proof"
else
  echo "stage 4a e2e: PASS"
fi
ok=1
