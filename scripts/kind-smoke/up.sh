#!/usr/bin/env bash
# scripts/kind-smoke/up.sh — bring up one instance of the kind smoke. See README.md.
set -euo pipefail
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
  mkdir -p -m 0700 "$state/dispatch-home"
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

main() {
  require_tools
  smoke_init
  validate_inputs
  write_mode_records
  check_ports
  build_binaries
  ensure_cluster
  resolve_gateway
  ensure_nats
  ensure_postgres
  start_listener
  start_dispatch
  stop_after host-services
  note "host services OK"
}

main "$@"
