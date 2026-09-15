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

main() {
  require_tools
  smoke_init
  validate_inputs
  write_mode_records
  check_ports
  note "preflight OK"
}

main "$@"
