#!/usr/bin/env bash
# scripts/kind-smoke/up.sh — bring up one instance of the kind smoke. See README.md.
set -euo pipefail
# shellcheck source=scripts/kind-smoke/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

require_tools() {
  local missing=() t
  for t in docker kind kubectl go bun curl jq tmux ss openssl shred setsid mise; do
    command -v "$t" >/dev/null 2>&1 || missing+=("$t")
  done
  [ "${#missing[@]}" -eq 0 ] || fail "missing required tools: ${missing[*]} — see docs/kubernetes.md, Runbook: the kind smoke, Prerequisites"
}

validate_inputs() {
  image="${SMOKE_WORKER_IMAGE:-}"
  [ -n "$image" ] || fail "SMOKE_WORKER_IMAGE is unset: the digest reference of the worker image to run (README.md, Finding a digest)"
  [[ "$image" =~ ^[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]] || fail "SMOKE_WORKER_IMAGE must be pinned by digest (…@sha256:<64 hex>); got $image"
  repo="${SMOKE_REPO:-sjawhar/legion-smoke}"
  [[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*/[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail "SMOKE_REPO must be owner/repo; got $repo"
  root_issue_count="${SMOKE_ROOT_ISSUES:-1}"
  [[ "$root_issue_count" =~ ^[1-9][0-9]*$ ]] || fail "SMOKE_ROOT_ISSUES must be a positive integer; got $root_issue_count"
  worker_cap="${SMOKE_WORKER_CAP:-6}"
  [[ "$worker_cap" =~ ^[1-9][0-9]*$ ]] || fail "SMOKE_WORKER_CAP must be a positive integer; got $worker_cap"
  # the resync probe is what resurrects a crashed root (checkpoint kill-pod-resume); short, so the
  # replacement arrives within a minute or two instead of the daemon's default interval
  resync_interval="${SMOKE_RESYNC_INTERVAL:-60}"
  [[ "$resync_interval" =~ ^[1-9][0-9]*$ ]] || fail "SMOKE_RESYNC_INTERVAL must be a positive integer of seconds; got $resync_interval"
  # a finished worker's pod lingers idle this long before the daemon retires it; the daemon's default
  # (600) for a plain run, shorter for a worker-cap run so idle pods stop hiding the running count
  worker_idle_retire="${SMOKE_WORKER_IDLE_RETIRE:-600}"
  [[ "$worker_idle_retire" =~ ^[1-9][0-9]*$ ]] || fail "SMOKE_WORKER_IDLE_RETIRE must be a positive integer of seconds; got $worker_idle_retire"
  session_store="${SMOKE_SESSION_STORE:-pvc}"
  case "$session_store" in
    pvc) ;;
    postgres)
      grep -q 'session_dsn_secret' "$repo_root/packages/daemon/src/daemon/config.ts" ||
        fail "SMOKE_SESSION_STORE=postgres needs runtime.kubernetes.session_store in packages/daemon/src/daemon/config.ts (pull request #1108); this checkout has no such key"
      ;;
    *) fail "SMOKE_SESSION_STORE must be pvc or postgres; got $session_store" ;;
  esac
  github_ingress="${SMOKE_GITHUB_INGRESS:-none}"
  case "$github_ingress" in
    none | envoy) ;;
    *) fail "SMOKE_GITHUB_INGRESS must be none or envoy; got $github_ingress" ;;
  esac
  implement_app_id="${LEGION_IMPLEMENT_APP_ID:-3202636}"
  review_app_id="${LEGION_REVIEW_APP_ID:-3202653}"
  [[ "$implement_app_id$review_app_id" =~ ^[0-9]+$ ]] || fail "LEGION_IMPLEMENT_APP_ID and LEGION_REVIEW_APP_ID must be numeric"
  [ -n "${ANTHROPIC_API_KEY:-}${GEMINI_API_KEY:-}${OPENAI_API_KEY:-}" ] ||
    fail "no provider key in the environment (ANTHROPIC_API_KEY, GEMINI_API_KEY, or OPENAI_API_KEY): run up.sh through 'secrets ANTHROPIC_API_KEY … -- bash scripts/kind-smoke/up.sh' or, on a box that has it, '/home/legion/.local/bin/legion-pane-env bash scripts/kind-smoke/up.sh'"
  app_key_source implement GH_AGENT_APP_PRIVATE_KEY_B64 SMOKE_IMPLEMENT_APP_KEY_FILE
  app_key_source review GH_REVIEW_APP_PRIVATE_KEY_B64 SMOKE_REVIEW_APP_KEY_FILE
}
app_key_source() { # app_key_source ROLE B64_VAR FILE_VAR → sets app_key_<role>_b64 or app_key_<role>_file
  local role="$1" b64="${!2:-}" file="${!3:-}"
  if [ -n "$b64" ]; then
    printf -v "app_key_${role}_b64" '%s' "$b64"
    return
  fi
  if [ -n "$file" ]; then
    [ -r "$file" ] || fail "$3 names $file, which is not readable"
    printf -v "app_key_${role}_file" '%s' "$file"
    return
  fi
  fail "$2 is unset and $3 is unset: supply the ${role} App private key as base64 in $2 (secrets …) or as a PEM path in $3 (e.g. /etc/legion/${role}er.pem on the dev box)"
}

write_mode_records() {
  record_write instance "$instance"
  record_write port-base "$port_base"
  record_write image "$image"
  record_write repo "$repo"
  record_write github-ingress "$github_ingress"
  record_write session-store "$session_store"
  record_write worker-cap "$worker_cap"
  record_write root-issue-count "$root_issue_count"
  record_write resync-interval "$resync_interval"
  record_write worker-idle-retire "$worker_idle_retire"
  record_write project demo
}

# Owner checks: a listener that is this instance's own recorded process or container is reused.
nats_owned() { container_owned_running "$nats_container"; }
postgres_owned() { container_owned_running "$postgres_container"; }
listener_owned() { pid_is_live listener; }
dispatch_owned() { pid_is_live dispatch; }
port_forward_owned() { pid_is_live port-forward; }
container_owned_running() { # container_owned_running NAME → 0 when running and labelled ours
  [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ] &&
    [ "$(docker inspect -f '{{index .Config.Labels "legion-smoke.instance"}}' "$1" 2>/dev/null)" = "$instance" ]
}
check_ports() {
  assert_port_free nats "$port_nats" nats_owned
  assert_port_free listener "$port_listener" listener_owned
  assert_port_free dispatch "$port_dispatch" dispatch_owned
  assert_port_free postgres "$port_postgres" postgres_owned
  assert_port_free daemon "$port_daemon" port_forward_owned
}

# SMOKE_STOP_AFTER=<phase> is a harness seam: up.sh exits 0 after the named phase
# (host-services | overlay | daemon | controller).
stop_after() {
  if [ "${SMOKE_STOP_AFTER:-}" = "$1" ]; then
    note "stopped after $1 (SMOKE_STOP_AFTER)"
    exit 0
  fi
}

# ---- the cluster and the gateway ---------------------------------------------------------------

ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -Fxq -- "$cluster"; then
    [ -s "$state/kubeconfig" ] || kind export kubeconfig --name "$cluster" --kubeconfig "$state/kubeconfig"
    note "REUSED cluster $cluster"
  else
    local args=(create cluster --name "$cluster" --kubeconfig "$state/kubeconfig" --wait 120s)
    [ -n "${SMOKE_KIND_NODE_IMAGE:-}" ] && args+=(--image "$SMOKE_KIND_NODE_IMAGE")
    kind "${args[@]}" || fail "kind create cluster $cluster failed (nothing else was created)"
    note "CREATED cluster $cluster"
  fi
  chmod 0600 "$state/kubeconfig"
  record_write cluster "$cluster"
  record_write kubeconfig "$state/kubeconfig"
}
# kind creates the `kind` docker network on the first cluster create; its IPv4 gateway is the one
# address both the pods (through the node) and the host reach. Chosen by regex: on some boxes the
# IPv6 IPAM entry comes first, so `index .IPAM.Config N` is wrong.
resolve_gateway() {
  gateway="$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}}{{"\n"}}{{end}}' | grep -E '^[0-9]+(\.[0-9]+){3}$' | head -n1 || true)"
  [ -n "$gateway" ] || fail "the kind docker network has no IPv4 gateway (docker network inspect kind); kind creates that network on cluster create — did kind create cluster succeed?"
  record_write gateway "$gateway"
}

# ---- the two containers: NATS and Postgres, published on the gateway address only --------------

ensure_container() { # ensure_container NAME RECORD HOST_PORT CONTAINER_PORT IMAGE-AND-ARGS…
  local name="$1" record="$2" hostport="$3" cport="$4" label
  shift 4
  if container_owned_running "$name"; then
    note "REUSED container $name"
  elif docker inspect "$name" >/dev/null 2>&1; then
    label="$(docker inspect -f '{{index .Config.Labels "legion-smoke.instance"}}' "$name")"
    [ "$label" = "$instance" ] || fail "container $name exists but is not this instance's (label legion-smoke.instance is '$label'); choose another SMOKE_INSTANCE"
    docker start "$name" >/dev/null
    note "STARTED container $name"
  else
    docker run -d --name "$name" --label "legion-smoke.instance=$instance" -p "$gateway:$hostport:$cport" "$@" >/dev/null
    note "CREATED container $name"
  fi
  record_write "$record" "$name"
}
ensure_nats() { ensure_container "$nats_container" nats-container "$port_nats" 4222 nats:2.10 -js; }
ensure_postgres() {
  generate_secret postgres-password
  local envfile="$state/secrets/postgres.env"
  (umask 077; printf 'POSTGRES_USER=legion\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=dispatch\n' "$(<"$state/secrets/postgres-password")" >"$envfile")
  ensure_container "$postgres_container" postgres-container "$port_postgres" 5432 --env-file "$envfile" postgres:16
  poll 60 "postgres to accept connections" docker exec "$postgres_container" pg_isready -U legion -q ||
    fail "postgres container $postgres_container did not become ready; docker logs $postgres_container"
  if [ "$session_store" = postgres ]; then
    docker exec "$postgres_container" psql -U legion -d dispatch -tAc "SELECT 1 FROM pg_database WHERE datname='sessions'" | grep -q 1 ||
      docker exec "$postgres_container" createdb -U legion sessions
  fi
}

# ---- the two binaries built from the checkout, and the two host processes ----------------------

build_binaries() {
  (cd "$repo_root/packages/envoy" && go build -o "$state/bin/envoy-listener" ./cmd/listener && go build -o "$state/bin/envoy-dispatch" ./cmd/dispatch) ||
    fail "go build of packages/envoy cmd/listener and cmd/dispatch failed"
}
# Secret-bearing variables are prefix assignments on the start_process call: bash exports them to
# the children the function spawns, so they land in the process environment and never in argv.
start_listener() {
  generate_secret envoy-token
  ENVOY_API_TOKEN="$(<"$state/secrets/envoy-token")" \
    start_process listener env PORT="$port_listener" ENVOY_LISTEN_HOST="$gateway" ENVOY_MACHINE_ID="legion-smoke-$instance" \
    NATS_URLS="nats://$gateway:$port_nats" "$state/bin/envoy-listener"
  poll 60 "the Envoy listener readiness gate" listener_ready || fail "the Envoy listener did not become ready; see $state/logs/listener.log"
}
listener_ready() {
  pid_is_live listener || fail "listener exited; see $state/logs/listener.log"
  curl -fsS --max-time 5 -H "@$(auth_header_file envoy-token)" "http://$gateway:$port_listener/v1/sessions" >/dev/null 2>&1
}
# The scratch Dispatch server: its own HOME under the state directory, so it reads no operator
# envoy.json and writes its signing key nowhere shared; header identity for the one human login.
start_dispatch() {
  generate_secret dispatch-token
  record_write dispatch-login smoke
  mkdir -p "$state/dispatch-home"
  chmod 0700 "$state/dispatch-home"
  DATABASE_URL="postgres://legion:$(<"$state/secrets/postgres-password")@$gateway:$port_postgres/dispatch?sslmode=disable" \
    DISPATCH_AGENT_TOKEN="$(<"$state/secrets/dispatch-token")" ENVOY_TOKEN="$(<"$state/secrets/envoy-token")" \
    start_process dispatch env -C "$state/dispatch-home" HOME="$state/dispatch-home" \
    DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_ALLOWED_LOGINS=smoke \
    DISPATCH_LISTEN_HOST="$gateway" DISPATCH_PORT="$port_dispatch" DISPATCH_SERVER_URL="http://$gateway:$port_dispatch" \
    NATS_URLS="nats://$gateway:$port_nats" ENVOY_URL="http://$gateway:$port_listener" "$state/bin/envoy-dispatch"
  poll 60 "the Dispatch server" dispatch_ready || fail "the Dispatch server did not answer GET /api/v1; see $state/logs/dispatch.log"
}
dispatch_ready() {
  pid_is_live dispatch || fail "dispatch exited; see $state/logs/dispatch.log"
  curl -fsS --max-time 5 "http://$gateway:$port_dispatch/api/v1" >/dev/null 2>&1
}

# ---- the scratch Dispatch project and the repository mapping (human-only routes, by header) ----

seed_dispatch() {
  if dispatch_get projects | jq -e --arg k "$project_key" 'map(select(.key == $k)) | length > 0' >/dev/null 2>&1; then
    note "REUSED Dispatch project $project_key"
  else
    dispatch_human POST projects "$(jq -cn --arg k "$project_key" --arg n "Kind smoke $instance" '{key:$k,name:$n}')" >/dev/null ||
      fail "could not create the Dispatch project $project_key; see $state/logs/dispatch.log"
    note "CREATED Dispatch project $project_key"
  fi
  dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg k "$project_key" '{project:$k}')" >/dev/null ||
    fail "could not map $repo to the Dispatch project $project_key"
  record_write dispatch-project "$project_key"
}

# ---- the overlay: the checkout's kind overlay copied and filled for this instance -----------------
# The copy tracks the checkout (a secretGenerator or .example file the checkout gains arrives with
# it); legion.yaml, the egress patch, and instructions.md are regenerated wholesale, and exactly two
# things are rewritten in the copied kustomization.yaml: the placeholder digest and the base path.
# Optional daemon keys are emitted only when the checkout's daemon carries them, so an image older
# than the checkout refuses the unknown key and crash-loops — reported by the daemon-wait step.

write_overlay() {
  local src="$repo_root/deploy/kubernetes/daemon" o="$state/overlay" digest="${image#*@}"
  rm -rf "$state/base" "$o"
  mkdir -p "$state/base" "$o"
  cp -R "$src/base/." "$state/base/"
  cp -R "$src/overlays/kind/." "$o/"
  mkdir -p "$o/secrets"
  chmod 0700 "$o/secrets"
  local has_operator=0
  grep -q '^operator_token_file:' "$src/base/legion.yaml" && has_operator=1
  {
    cat <<EOF
# Generated by scripts/kind-smoke/up.sh for instance $instance; do not edit — rerun up.sh.
project: demo
runtime:
  kubernetes:
    namespace: legion
    image: $image
EOF
    [ "$session_store" = postgres ] && printf '    session_store: postgres\n    session_dsn_secret: SESSION_DSN\n'
    cat <<'EOF'
    resources:
      small:  { requests: { cpu: 500m, memory: 1Gi, ephemeral_storage: 2Gi },  limits: { cpu: "2", memory: 3Gi,  ephemeral_storage: 8Gi } }
      medium: { requests: { cpu: "1",  memory: 2Gi, ephemeral_storage: 10Gi }, limits: { cpu: "4", memory: 6Gi,  ephemeral_storage: 30Gi } }
      large:  { requests: { cpu: "2",  memory: 4Gi, ephemeral_storage: 20Gi }, limits: { cpu: "6", memory: 12Gi, ephemeral_storage: 60Gi } }
    role_profiles: { architect: small, planner: small, implementer: medium, tester: large, reviewer: small, merger: small }
bind: 0.0.0.0
daemon_url: http://legion-daemon-demo.legion.svc:13370
port: 13370
worker_stream_port: 13371
state_dir: /var/lib/legion
instructions: /etc/legion/instructions.md
EOF
    cat <<EOF
envoy_url: http://$gateway:$port_listener
envoy_token_file: /var/run/legion/providers/ENVOY_TOKEN
nats_urls:
  - nats://$gateway:$port_nats
dispatch_url: http://$gateway:$port_dispatch
dispatch_project: $project_key
repos:
  - $repo
gates:
  design: off
worker_cap: $worker_cap
worker_idle_retire_seconds: $worker_idle_retire
resync_interval_seconds: $resync_interval
EOF
    [ "$has_operator" = 1 ] && printf 'operator_token_file: /var/run/legion/operator/OPERATOR_TOKEN\n'
    cat <<EOF
github_apps:
  implement:
    app_id: "$implement_app_id"
    private_key_command: cat /var/run/legion/daemon/github-app-implement.pem
  review:
    app_id: "$review_app_id"
    private_key_command: cat /var/run/legion/daemon/github-app-review.pem
EOF
  } >"$o/legion.yaml"
  # the checkpoints read the profile table from this record, never from config.ts defaults
  jq -n '{role_profiles:{architect:"small",planner:"small",implementer:"medium",tester:"large",reviewer:"small",merger:"small"},
          resources:{small:{requests:{cpu:"500m",memory:"1Gi","ephemeral-storage":"2Gi"},limits:{cpu:"2",memory:"3Gi","ephemeral-storage":"8Gi"}},
                     medium:{requests:{cpu:"1",memory:"2Gi","ephemeral-storage":"10Gi"},limits:{cpu:"4",memory:"6Gi","ephemeral-storage":"30Gi"}},
                     large:{requests:{cpu:"2",memory:"4Gi","ephemeral-storage":"20Gi"},limits:{cpu:"6",memory:"12Gi","ephemeral-storage":"60Gi"}}}}' >"$records/profiles.json"
  printf -- '- op: add\n  path: /spec/egress/1/ports/-\n  value:\n    port: %s\n' "$port_nats" "$port_listener" "$port_dispatch" >"$o/networkpolicy-egress.yaml"
  cat >"$o/instructions.md" <<EOF
# Deployment instructions (demo)

This daemon is a throwaway kind smoke instance ($instance) driven by scripts/kind-smoke. The repository $repo is a sandbox: keep every change to the one file the issue names, one commit, no rebases unless GitHub reports a conflict. The design gate is off. Dispatch is the instance's own scratch server; nobody reads it. Do the phase, write the handoff, report completion.
EOF
  local zero='sha256:0000000000000000000000000000000000000000000000000000000000000000'
  [ "$(grep -c -- "$zero" "$o/kustomization.yaml")" = 1 ] || fail "expected exactly one placeholder digest in $o/kustomization.yaml"
  [ "$(grep -c -- '^  - \.\./\.\./base$' "$o/kustomization.yaml")" = 1 ] || fail "expected exactly one '- ../../base' resource in $o/kustomization.yaml"
  sed -i -e "s|$zero|$digest|" -e 's|^  - \.\./\.\./base$|  - ../base|' \
    -e 's|^\(\s*\)- legion.yaml$|\1- legion.yaml\n\1- instructions.md|' "$o/kustomization.yaml"
  # secrets: 0600 under the 0700 secrets/; values from this process's environment and the instance's own files
  (
    umask 077
    {
      printf 'DISPATCH_TOKEN=%s\nENVOY_TOKEN=%s\n' "$(<"$state/secrets/dispatch-token")" "$(<"$state/secrets/envoy-token")"
      for k in ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY; do
        [ -n "${!k:-}" ] && printf '%s=%s\n' "$k" "${!k}"
      done
      [ "$session_store" = postgres ] && printf 'SESSION_DSN=postgres://legion:%s@%s:%s/sessions?sslmode=disable\n' "$(<"$state/secrets/postgres-password")" "$gateway" "$port_postgres"
      true
    } >"$o/secrets/providers.env"
    write_pem implement "$o/secrets/github-app-implement.pem"
    write_pem review "$o/secrets/github-app-review.pem"
    if [ -f "$o/secrets/operator.env.example" ]; then
      generate_secret operator-token
      printf 'OPERATOR_TOKEN=%s\n' "$(<"$state/secrets/operator-token")" >"$o/secrets/operator.env"
    fi
  )
  kubectl kustomize "$o" >"$state/rendered.yaml" || fail "kubectl kustomize $o failed"
  ! grep -q -- "$zero" "$state/rendered.yaml" || fail "the rendered overlay still carries the placeholder digest"
}
write_pem() { # write_pem ROLE DEST — from app_key_<role>_b64 or app_key_<role>_file; refuses a non-PEM
  local b64var="app_key_$1_b64" filevar="app_key_$1_file"
  if [ -n "${!b64var:-}" ]; then
    printf '%s' "${!b64var}" | base64 -d >"$2"
  else
    cp -- "${!filevar}" "$2"
  fi
  chmod 0600 "$2"
  head -c 10 "$2" | grep -q -- '-----BEGIN' || fail "the $1 App private key does not begin with -----BEGIN; check its source"
}

# ---- apply, wait for the daemon, keep a port-forward alive, wait for the image probe ------------

apply_and_wait() {
  kc apply -k "$state/overlay" >/dev/null || fail "kubectl apply -k $state/overlay failed"
  if ! kc rollout status deploy/legion-daemon-demo --timeout=120s; then
    kc describe pod -l app.kubernetes.io/name=legion-daemon 2>&1 | tail -n 30 >&2 || true
    kc logs deploy/legion-daemon-demo --tail=50 >&2 || true
    fail "the daemon pod was not Ready within 120s; the cluster $cluster is left for inspection (kubectl --kubeconfig $state/kubeconfig -n legion …)"
  fi
  # kubectl port-forward exits when its connection drops; a setsid'd loop restarts it, recorded as a group
  # shellcheck disable=SC2016  # the loop body is a bash -c script; $1 and $2 are its own arguments
  start_process_group port-forward bash -c 'while :; do kubectl --kubeconfig "$1" -n legion port-forward --address 127.0.0.1 svc/legion-daemon-demo "$2:13370"; sleep 1; done' _ "$state/kubeconfig" "$port_daemon"
  poll 60 "GET /legion/v1/state through the port-forward" daemon_state_ok || fail "the daemon state page did not answer on 127.0.0.1:$port_daemon; see $state/logs/port-forward.log"
  poll "${SMOKE_PROBE_WAIT:-600}" "the daemon's image probe to pass" daemon_probe_passed ||
    fail "the daemon never logged a passed image probe; last log lines:"$'\n'"$(kc logs deploy/legion-daemon-demo --tail=40 2>&1)"
}
daemon_state_ok() { daemon_state >/dev/null 2>&1; }
daemon_probe_passed() {
  local restarts line
  restarts="$(kc get pod -l app.kubernetes.io/name=legion-daemon -o json | jq -r '.items[0].status.containerStatuses[0].restartCount // 0')"
  if [ "$restarts" -gt 0 ]; then
    printf 'error: the daemon pod is crash-looping (%s restarts); its last log lines:\n' "$restarts" >&2
    kc logs deploy/legion-daemon-demo --previous --tail=40 >&2 2>/dev/null || kc logs deploy/legion-daemon-demo --tail=40 >&2 || true
    fail "the daemon refuses to start (an image older than the checkout's legion.yaml keys, or a bad secret); the cluster $cluster is left for inspection"
  fi
  line="$(kc logs deploy/legion-daemon-demo --tail=400 2>/dev/null | grep -E 'probe pod .* passed:|passed its probe at' | tail -n1 || true)"
  [ -n "$line" ] || return 1
  local contract
  contract="$(printf '%s' "$line" | grep -oE 'daemon-api-version=[0-9]+|daemon API contract [0-9]+' | grep -oE '[0-9]+$' | head -n1 || true)"
  record_write probe-contract "$contract"
  return 0
}

# ---- the LEGION-177 keeper ----------------------------------------------------------------------
# On the image's git 2.47 the `credential.interactive=false` that provisioning writes into the
# tree's shared clone at its end makes the NEXT pod's init container fail its `jj git fetch` with
# `unable to get password from user` (GIT_ASKPASS counts as interactive) — every phase worker and
# every resurrected root, not only the first replacement (seen on the 2026-09-15 run: the planner
# failed six generations in a row). Until LEGION-177 lands, a recorded host loop keeps the key
# unset through `kubectl exec` in every Running Legion pod, so each next provisioning gets through;
# down.sh stops it by record. Gated by SMOKE_LEGION_177_WORKAROUND (default 1); the kill checkpoint
# additionally applies the one-shot unset right before the kill and prints it.

start_legion_177_keeper() {
  if [ "${SMOKE_LEGION_177_WORKAROUND:-1}" != 1 ]; then
    record_write legion-177-workaround off
    note "SKIPPED LEGION-177 keeper (SMOKE_LEGION_177_WORKAROUND=${SMOKE_LEGION_177_WORKAROUND})"
    return 0
  fi
  record_write legion-177-workaround keeper
  # shellcheck disable=SC2016  # the loop body is a bash -c script; $1..$3 are its own arguments
  start_process_group legion-177-keeper bash -c '
    while :; do
      for p in $(kubectl --kubeconfig "$1" -n legion get pods -l "legion.dev/project,!legion.dev/probe" --field-selector=status.phase=Running -o jsonpath="{.items[*].metadata.name}" 2>/dev/null); do
        if kubectl --kubeconfig "$1" -n legion exec "$p" -c worker -- git --git-dir="/legion/repos/github.com/$2/.git" config --unset credential.interactive >/dev/null 2>&1; then
          printf "%s unset credential.interactive in the clone through %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$p"
        fi
      done
      sleep "$3"
    done' _ "$state/kubeconfig" "$repo" "${SMOKE_LEGION_177_INTERVAL:-3}"
}
keeper_summary() {
  case "$(record_read legion-177-workaround)" in
    keeper) printf 'keeper (pgid %s, every %ss; LEGION-177 workaround)' "$(<"$state/pids/legion-177-keeper.pid")" "${SMOKE_LEGION_177_INTERVAL:-3}" ;;
    *) printf 'off (SMOKE_LEGION_177_WORKAROUND=%s)' "${SMOKE_LEGION_177_WORKAROUND:-1}" ;;
  esac
}

# ---- SMOKE_GITHUB_INGRESS=envoy: the read-only bridge from production NATS, before the daemon ---
# An unreachable upstream stops the run before anything in the cluster starts.

upstream_nats() { printf '%s' "${SMOKE_UPSTREAM_NATS:-nats://envoy-nats.tailb86685.ts.net:4222}"; }
start_bridge() {
  start_process envoy-bridge env SMOKE_REPO="$repo" SMOKE_RIG_NATS="nats://$gateway:$port_nats" SMOKE_UPSTREAM_NATS="$(upstream_nats)" \
    bun run "$repo_root/scripts/kind-smoke/envoy-bridge.ts"
  poll 60 "the GitHub bridge to report BRIDGE READY" bridge_ready ||
    fail "the GitHub bridge could not subscribe upstream ($(upstream_nats)); see $state/logs/envoy-bridge.log"
}
bridge_ready() {
  if grep -q 'BRIDGE UNHEALTHY' "$state/logs/envoy-bridge.log" 2>/dev/null || ! pid_is_live envoy-bridge; then
    fail "the GitHub bridge could not subscribe upstream ($(upstream_nats)); see $state/logs/envoy-bridge.log"
  fi
  grep -q 'BRIDGE READY' "$state/logs/envoy-bridge.log" 2>/dev/null
}

# ---- the controller: host-side `legion controller start` in the instance's own tmux server ------
# Three ordered checks decide `controller: none`, each recording its reason; only when all pass is
# a pane opened, and the pane is recorded before it is watched.
#   1. the checkout has the command and its controller.yaml.example (pull request #1110);
#   2. the daemon has the controller-secret route: POST with a deliberately wrong bearer answers 403
#      (present) or 404 (absent) — nothing is minted, no incumbent controller's secret is revoked;
#   3. the profile plugin's daemon-API contract equals the image daemon's (from its probe log line).

checkout_has_controller() {
  [ -f "${SMOKE_CONTROLLER_EXAMPLE:-$repo_root/deploy/kubernetes/daemon/controller.yaml.example}" ] &&
    (cd "$repo_root" && bun run packages/daemon/src/cli/index.ts controller start --help >/dev/null 2>&1)
}
require_omp_pin() { # the controller runs the pinned Oh My Pi through mise; refuse early when it is not installed
  local pin
  pin="$(cd "$repo_root" && bun -e 'import { OMP_FORK_PIN } from "./packages/daemon/src/daemon/omp-pin.ts"; console.log(OMP_FORK_PIN);')" ||
    fail "could not read OMP_FORK_PIN from packages/daemon/src/daemon/omp-pin.ts"
  mise where "$pin" >/dev/null 2>&1 || fail "the pinned Oh My Pi is not installed (mise where $pin); run: mise install $pin"
}
decide_controller() {
  local existing
  existing="$(record_read controller)"
  if [[ "$existing" == tmux\ * ]] && tmux -L "$tmux_server" has-session -t controller 2>/dev/null; then
    note "REUSED controller (tmux -L $tmux_server attach)"
    return 0
  fi
  local reason=""
  if ! checkout_has_controller; then
    reason="the checkout has no legion controller start (pull request #1110)"
  else
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST -H 'Authorization: Bearer smoke-route-probe' -H 'content-type: application/json' --data '{}' "http://127.0.0.1:$port_daemon/legion/v1/controller/secret")"
    case "$code" in
      403) ;;
      404) reason="the daemon answers 404 on POST /legion/v1/controller/secret (the image predates legion controller start)" ;;
      *) fail "POST /legion/v1/controller/secret answered $code with a deliberately wrong bearer; expected 403 (route present) or 404 (route absent)" ;;
    esac
    if [ -z "$reason" ]; then
      local manifest plugin_contract image_contract
      manifest="${SMOKE_PLUGIN_MANIFEST:-$HOME/.omp/profiles/${SMOKE_OMP_PROFILE:-legion}/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json}"
      plugin_contract="$(jq -r '.legion.daemonApiVersion // empty' "$manifest" 2>/dev/null || true)"
      image_contract="$(record_read probe-contract)"
      if [ -z "$image_contract" ]; then
        reason="the image's probe line carries no daemon-api-version (its legion CLI predates the check), so the controller's plugin contract cannot be matched"
      elif [ -z "$plugin_contract" ]; then
        reason="no pi-legion-envoy manifest at $manifest"
      elif [ "$plugin_contract" != "$image_contract" ]; then
        reason="installed pi-legion-envoy speaks contract $plugin_contract; the image daemon requires $image_contract"
      fi
    fi
  fi
  if [ -n "$reason" ]; then
    record_write controller "none: $reason"
    note "SKIPPED controller: $reason"
    return 0
  fi
  start_controller
}
start_controller() {
  local c="$state/controller"
  [ -s "$state/secrets/operator-token" ] || fail "the checkout has legion controller start but its kind overlay wrote no operator token (secrets/operator.env.example missing); the daemon was started without one"
  mkdir -p "$c"
  chmod 0700 "$c"
  (
    umask 077
    cp "$state/secrets/operator-token" "$c/operator-token"
    cp "$state/secrets/envoy-token" "$c/envoy-token"
    cp "$state/secrets/dispatch-token" "$c/dispatch-token"
    cp "$state/overlay/instructions.md" "$c/instructions.md"
    chmod 0600 "$c/operator-token" "$c/envoy-token" "$c/dispatch-token"
  )
  {
    printf 'project: demo\ndaemon_url: http://127.0.0.1:%s\noperator_token_file: ./operator-token\nenvoy_url: http://%s:%s\nenvoy_token_file: ./envoy-token\nnats_urls:\n  - nats://%s:%s\ndispatch_url: http://%s:%s\ndispatch_token_file: ./dispatch-token\ninstructions: ./instructions.md\nstate_dir: ./state\n' \
      "$port_daemon" "$gateway" "$port_listener" "$gateway" "$port_nats" "$gateway" "$port_dispatch"
    # SMOKE_OMP_LAUNCH_PREFIX="" omits the key (a box whose profile plugin, not a secrets CLI, supplies the keys)
    local prefix="${SMOKE_OMP_LAUNCH_PREFIX-secrets ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY --}" w
    if [ -n "$prefix" ]; then
      printf 'omp_launch_prefix:\n'
      for w in $prefix; do printf '  - %s\n' "$w"; done
    fi
  } >"$c/controller.yaml"
  tmux -L "$tmux_server" new-session -d -s controller -n controller -c "$repo_root" -e OMP_PROFILE="${SMOKE_OMP_PROFILE:-legion}" \
    "bun run packages/daemon/src/cli/index.ts controller start --config $c/controller.yaml --daemon-url http://127.0.0.1:$port_daemon"
  tmux -L "$tmux_server" pipe-pane -t controller:controller -o "cat >>$state/logs/controller.log"
  record_write controller "tmux $tmux_server controller"
  note "STARTED controller (tmux -L $tmux_server attach)"
  poll 120 "the controller to register (controllerLocator.external)" controller_registered ||
    fail "the controller did not register within 120s; tail of $state/logs/controller.log:"$'\n'"$(tail -n 30 "$state/logs/controller.log" 2>/dev/null)"
}
controller_registered() { daemon_state 2>/dev/null | jq -e '.controllerLocator.external == true' >/dev/null 2>&1; }

# ---- root issues: created and released to `todo` as the human identity ----------------------------

ensure_root_issues() {
  local n key have
  for n in $(seq 1 "$root_issue_count"); do
    have="$(record_read root-issues | sed -n "${n}p")"
    if [ -n "$have" ]; then
      note "REUSED root issue $have"
      continue
    fi
    key="$(dispatch_human POST issues "$(jq -cn --arg p "$project_key" --arg t "Kind smoke $instance: add smoke/$instance-$n.md" --arg s "$(root_issue_spec "$n")" '{project:$p,title:$t,spec:$s,force:true}')" | jq -r '.key // empty')"
    [[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "Dispatch did not return an issue key for root issue $n"
    dispatch_human PATCH "issues/$key" '{"status":"todo"}' >/dev/null || fail "could not release $key to todo"
    record_append root-issues "$key"
    note "CREATED root issue $key (todo)"
  done
}
root_issue_spec() {
  cat <<EOF
# Add smoke/$instance-$1.md

## Summary
Add one Markdown file, smoke/$instance-$1.md, whose only line is: kind smoke $instance $1 $(date -u +%Y-%m-%dT%H:%M:%SZ). Nothing else changes. Done when the pull request is merged.

## Decisions needed
None: this records what was agreed.

## Acceptance
1. The file exists on the pull request's branch with exactly that line. Check: cat smoke/$instance-$1.md.

## Requirements
| requirement | provenance |
| :--- | :--- |
| one new file, one line, no other change | the kind smoke rig (LEGION-26) |

## Design
One commit adding the file. No decomposition into child issues: this is a single-issue root.

## Errors
| condition | behaviour |
| :--- | :--- |
| the file already exists | overwrite it with the one line |

## Testing
| acceptance | proof |
| :--- | :--- |
| 1 | cat the file on the branch |

## Rejected
- Anything larger: this issue exists to drive the pod lifecycle, not to change the repository.
EOF
}

# ---- the summary block --------------------------------------------------------------------------

controller_summary() {
  local c
  c="$(record_read controller)"
  # shellcheck disable=SC2086  # the record is "tmux <server> <window>": split it on purpose
  case "$c" in
    tmux\ *) set -- $c; printf 'tmux -L %s attach (window %s)' "$2" "$3" ;;
    none:\ *) printf 'none (%s)' "${c#none: }" ;;
    *) printf 'unknown' ;;
  esac
}
ingress_summary() {
  case "$github_ingress" in
    envoy) printf 'envoy (bridge pid %s, upstream %s)' "$(<"$state/pids/envoy-bridge.pid")" "$(upstream_nats)" ;;
    *) printf 'none (checkpoint done will report SKIPPED-BLOCKED)' ;;
  esac
}
summary() {
  cat <<EOF

KIND SMOKE READY
instance:        $instance
state dir:       $state
cluster:         $cluster (kubeconfig: $state/kubeconfig; kubectl --kubeconfig $state/kubeconfig -n legion get pods)
gateway:         $gateway
nats:            nats://$gateway:$port_nats (container $nats_container)
listener:        http://$gateway:$port_listener (pid $(<"$state/pids/listener.pid"))
dispatch:        http://$gateway:$port_dispatch (pid $(<"$state/pids/dispatch.pid"); project $project_key; login $(record_read dispatch-login))
postgres:        $gateway:$port_postgres (container $postgres_container)
daemon:          http://127.0.0.1:$port_daemon → svc/legion-daemon-demo:13370 (port-forward pgid $(<"$state/pids/port-forward.pid"))
image:           $image (daemon API contract $(record_read probe-contract))
session store:   $session_store
worker cap:      $worker_cap
controller:      $(controller_summary)
github ingress:  $(ingress_summary)
legion-177:      $(keeper_summary)
root issues:     $(record_read root-issues | paste -sd' ')
records:         $records
EOF
}

main() {
  require_tools
  smoke_init
  validate_inputs
  if checkout_has_controller; then require_omp_pin; fi
  write_mode_records
  check_ports
  build_binaries
  ensure_cluster
  resolve_gateway
  ensure_nats
  ensure_postgres
  start_listener
  start_dispatch
  [ "$github_ingress" = envoy ] && start_bridge
  stop_after host-services
  seed_dispatch
  write_overlay
  stop_after overlay
  apply_and_wait
  start_legion_177_keeper
  stop_after daemon
  decide_controller
  stop_after controller
  ensure_root_issues
  summary
}

main "$@"
