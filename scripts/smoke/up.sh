#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly smoke_dir="${SMOKE_DIR:-/tmp/legion-smoke}"
readonly gh_config_dir="${smoke_dir}/gh-config"
readonly nats_name="legion-smoke-nats"
readonly nats_port="${NATS_PORT:-14222}"
readonly listener_port="${ENVOY_PORT:-19020}"
readonly dispatch_port="${DISPATCH_PORT:-18766}"
readonly daemon_port="${LEGION_DAEMON_PORT:-19370}"
readonly nats_url="nats://127.0.0.1:${nats_port}"
readonly omp_pin="github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841"
LEGION_IMPLEMENT_APP_ID="${LEGION_IMPLEMENT_APP_ID:-3202636}"
readonly LEGION_IMPLEMENT_APP_ID
LEGION_REVIEW_APP_ID="${LEGION_REVIEW_APP_ID:-3202653}"
readonly LEGION_REVIEW_APP_ID
LEGION_APP_LOGINS="${LEGION_APP_LOGINS:-legion-implementer[bot],legion-reviewer[bot]}"
readonly LEGION_APP_LOGINS
readonly webhook_events="${SMOKE_WEBHOOK_EVENTS:-issues,issue_comment,sub_issues,pull_request,pull_request_review,check_run}"
readonly webhook_forwarder_url="https://webhook-forwarder.github.com/hook"

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

require_env() {
  [[ -n "${!1:-}" ]] || fail "$1 is required"
}
webhook_ingress_block_reason() {
  printf '%s\n' \
    'SMOKE_WEBHOOK_MODE=none: live GitHub events do not flow to Envoy; checkpoints that require live delivery are blocked; resync-driven checkpoints 1-4 remain usable'
}

resolve_webhook_mode() {
  case "${SMOKE_WEBHOOK_MODE:-}" in
    forward)
      gh webhook forward --help >/dev/null 2>&1 ||
        fail "SMOKE_WEBHOOK_MODE=forward requires gh webhook forward"
      printf 'forward\n'
      ;;
    envoy)
      printf 'envoy\n'
      ;;
    none)
      printf 'none\n'
      ;;
    "")
      if gh webhook forward --help >/dev/null 2>&1; then
        printf 'forward\n'
      else
        printf 'none\n'
      fi
      ;;
    *)
      fail "SMOKE_WEBHOOK_MODE must be forward, envoy, or none"
      ;;
  esac
}

normalize_github_webhook_secret() {
  local original_secret="$GITHUB_WEBHOOK_SECRET"

  GITHUB_WEBHOOK_SECRET="${GITHUB_WEBHOOK_SECRET#"${GITHUB_WEBHOOK_SECRET%%[![:space:]]*}"}"
  GITHUB_WEBHOOK_SECRET="${GITHUB_WEBHOOK_SECRET%"${GITHUB_WEBHOOK_SECRET##*[![:space:]]}"}"
  if [[ "$GITHUB_WEBHOOK_SECRET" != "$original_secret" ]]; then
    printf 'WARNING: GITHUB_WEBHOOK_SECRET stored secret contains whitespace; trimmed leading/trailing whitespace\n' >&2
  fi
  export GITHUB_WEBHOOK_SECRET
}


require_numeric() {
  [[ "${!1:-}" =~ ^[0-9]+$ ]] || fail "$1 must be numeric"
}

process_start_time() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $22}' "/proc/${pid}/stat"
}
process_group_id() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $5}' "/proc/${pid}/stat"
}


pid_is_live() {
  local pid_file="$1"
  local start_file="${pid_file%.pid}.start"
  local pid
  local expected_start
  local actual_start

  [[ -r "$pid_file" && -r "$start_file" ]] || return 1
  pid="$(<"$pid_file")"
  expected_start="$(<"$start_file")"
  kill -0 "$pid" 2>/dev/null || return 1
  actual_start="$(process_start_time "$pid")" || return 1
  [[ "$actual_start" == "$expected_start" ]]
}
assert_port_free() {
  local name="$1"
  local port="$2"
  local pid_file="${3:-}"

  if ! [[ "$port" =~ ^[0-9]+$ ]] || ! ((port > 0 && port <= 65535)); then
    fail "${name} port must be between 1 and 65535"
  fi
  if [[ -n "$pid_file" ]] && pid_is_live "$pid_file"; then
    return
  fi
  [[ -z "$(ss -H -ltn "sport = :${port}")" ]] ||
    fail "${name} port ${port} is already in use"
}

start_process() {
  local name="$1"
  local pid_file="${smoke_dir}/${name}.pid"
  local start_file="${smoke_dir}/${name}.start"
  local log_file="${smoke_dir}/${name}.log"
  local pid
  local start_time
  shift

  if pid_is_live "$pid_file"; then
    printf 'REUSED %s (pid %s)\n' "$name" "$(<"$pid_file")"
    return
  fi
  : >"$log_file"

  rm -f "$pid_file" "$start_file"
  "$@" >>"$log_file" 2>&1 &
  pid="$!"
  start_time="$(process_start_time "$pid")" || fail "${name} exited before its ownership record was written"
  printf '%s\n' "$pid" >"$pid_file"
  printf '%s\n' "$start_time" >"$start_file"
  printf 'STARTED %s (pid %s)\n' "$name" "$pid"
}
start_process_group() {
  local name="$1"
  local pid_file="${smoke_dir}/${name}.pid"
  local start_file="${smoke_dir}/${name}.start"
  local log_file="${smoke_dir}/${name}.log"
  local pid
  local pgid
  local start_time
  shift

  if pid_is_live "$pid_file"; then
    printf 'REUSED %s (pgid %s)\n' "$name" "$(<"$pid_file")"
    return
  fi
  : >"$log_file"

  rm -f "$pid_file" "$start_file"
  setsid "$@" >>"$log_file" 2>&1 &
  pid="$!"
  start_time="$(process_start_time "$pid")" || fail "${name} exited before its ownership record was written"
  pgid="$(process_group_id "$pid")" || fail "${name} exited before its process group was recorded"
  [[ "$pgid" == "$pid" ]] || fail "${name} did not start in its own process group"
  printf '%s\n' "$pgid" >"$pid_file"
  printf '%s\n' "$start_time" >"$start_file"
  printf 'STARTED %s (pgid %s)\n' "$name" "$pgid"
}


wait_for_http() {
  local name="$1"
  local url="$2"
  local pid_file="${3:-}"
  local response
  local attempt

  for ((attempt = 1; attempt <= 60; attempt += 1)); do
    [[ -z "$pid_file" ]] || pid_is_live "$pid_file" || fail "${name} exited; inspect ${smoke_dir}"
    if response="$(curl --fail --silent --show-error "$url" 2>/dev/null)"; then
      printf 'GREEN %s: %s\n' "$name" "$response"
      return
    fi
    sleep 1
  done

  fail "$name did not become healthy; inspect ${smoke_dir}"
}

wait_for_json() {
  local name="$1"
  local url="$2"
  local filter="$3"
  local pid_file="${4:-}"
  local response
  local attempt

  for ((attempt = 1; attempt <= 60; attempt += 1)); do
    [[ -z "$pid_file" ]] || pid_is_live "$pid_file" || fail "${name} exited; inspect ${smoke_dir}"
    if response="$(curl --fail --silent --show-error "$url" 2>/dev/null)" &&
      jq -e "$filter" >/dev/null <<<"$response"; then
      printf 'GREEN %s: %s\n' "$name" "$response"
      return
    fi
    sleep 1
  done

  fail "$name did not become healthy; inspect ${smoke_dir}"
}

project_owner() {
  printf '%s\n' "${SMOKE_PROJECT%/*}"
}

repo_owner() {
  printf '%s\n' "${SMOKE_REPO%/*}"
}

project_number() {
  printf '%s\n' "${SMOKE_PROJECT##*/}"
}

project_slug() {
  local project="$SMOKE_PROJECT"
  project="${project,,}"
  project="${project//[^a-z0-9]/}"
  printf '%s\n' "$project"
}

app_jwt() {
  local app_id="$1"
  local private_key_variable="$2"
  local private_key_b64="${!private_key_variable}"

  # shellcheck disable=SC2016 # JavaScript template literals must reach Bun unchanged.
  GH_APP_ID="$app_id" GH_APP_PRIVATE_KEY_B64="$private_key_b64" bun -e '
    import { createPrivateKey, sign } from "node:crypto";
    const encode = (value) => Buffer.from(value).toString("base64url");
    const now = Math.floor(Date.now() / 1000);
    const header = encode(JSON.stringify({ alg: "RS256", typ: "JWT" }));
    const payload = encode(JSON.stringify({ iat: now - 60, exp: now + 540, iss: process.env.GH_APP_ID }));
    const signature = sign("RSA-SHA256", Buffer.from(`${header}.${payload}`), createPrivateKey(Buffer.from(process.env.GH_APP_PRIVATE_KEY_B64, "base64").toString("utf8"))).toString("base64url");
    process.stdout.write(`${header}.${payload}.${signature}`);
  '
}

app_installation_token() {
  local app_id="$1"
  local private_key_variable="$2"
  local owner="${3:-$(project_owner)}"
  local jwt
  local installation_id

  jwt="$(app_jwt "$app_id" "$private_key_variable")"
  installation_id="$(env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$gh_config_dir" gh api -H "Authorization: Bearer ${jwt}" app/installations | jq -r --arg owner "$owner" '[.[] | select(.account.login == $owner) | .id] | first // empty')"
  [[ -n "$installation_id" ]] || fail "GitHub App ${app_id} is not installed for ${owner}"
  env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$gh_config_dir" gh api -H "Authorization: Bearer ${jwt}" -X POST "app/installations/${installation_id}/access_tokens" --jq '.token'
}

write_daemon_config() {
  local board_config=" []"

  if [[ -n "${SMOKE_PROJECT_ID:-}" ]]; then
    [[ "$SMOKE_PROJECT_ID" =~ ^PVT_ ]] || fail "SMOKE_PROJECT_ID must be a Projects V2 node ID"
    board_config=$'\n  - '"${SMOKE_PROJECT_ID}"
  fi

  cat >"${smoke_dir}/legion.yaml" <<EOF
project: ${SMOKE_PROJECT}
port: ${daemon_port}
envoy_url: http://127.0.0.1:${listener_port}
nats_urls:
  - ${nats_url}
dispatch_url: http://127.0.0.1:${dispatch_port}
board_project_ids:${board_config}
app_logins:
$(printf '%s\n' "$LEGION_APP_LOGINS" | tr ',' '\n' | sed 's/^/  - /')
admission_cap: 4
worker_budget: 6
max_recursion_depth: 8
linger_hours: 72
ci_quiet_ms: 30000
max_fix_attempts: 3
resync_interval_seconds: 600
state_dir: ${smoke_dir}/daemon
omp_invocation: mise x ${omp_pin} -- omp
gates:
  design: root-issues
  merge: human
github_apps:
  implement:
    app_id: "${LEGION_IMPLEMENT_APP_ID}"
    private_key_command: 'printf "%s" "\$GH_AGENT_APP_PRIVATE_KEY_B64" | base64 --ignore-garbage --decode'
  review:
    app_id: "${LEGION_REVIEW_APP_ID}"
    private_key_command: 'printf "%s" "\$GH_REVIEW_APP_PRIVATE_KEY_B64" | base64 --ignore-garbage --decode'
EOF
}
write_dispatch_config() {
  mkdir -p "${smoke_dir}/dispatch-home/.config/opencode"
  jq -n --arg nats_url "$nats_url" --arg repo "$SMOKE_REPO" \
    '{natsUrls: [$nats_url], dispatch: {defaultRepo: $repo}}' \
    >"${smoke_dir}/dispatch-home/.config/opencode/envoy.json"
}


ensure_nats() {
  if docker container inspect "$nats_name" >/dev/null 2>&1; then
    [[ "$(docker port "$nats_name" 4222/tcp)" == *":${nats_port}"* ]] ||
      fail "NATS container ${nats_name} is not mapped to configured port ${nats_port}"
    if [[ "$(docker container inspect --format '{{.State.Running}}' "$nats_name")" == "true" ]]; then
      printf 'REUSED NATS container %s\n' "$nats_name"
    else
      docker start "$nats_name" >/dev/null
      printf 'STARTED NATS container %s\n' "$nats_name"
    fi
  else
    assert_port_free NATS "$nats_port"
    docker run -d --name "$nats_name" -p "${nats_port}:4222" nats:2.10 -js >/dev/null
    printf 'STARTED NATS container %s\n' "$nats_name"
  fi
}
ensure_labels() {
  local token="$1"
  local label
  for label in needs-approval human-approved legion-child legion-backlog; do
    GH_TOKEN="$token" GH_CONFIG_DIR="$gh_config_dir" gh label create "$label" -R "$SMOKE_REPO" --force >/dev/null
  done
  printf 'GREEN sandbox labels present\n'
}

configure_branch_protection() {
  local endpoint="repos/${SMOKE_REPO}/branches/main/protection"
  local protection_file="${smoke_dir}/protection.json"

  jq -n '{
    required_status_checks: null,
    enforce_admins: false,
    required_pull_request_reviews: {
      dismissal_restrictions: {users: [], teams: [], apps: []},
      dismiss_stale_reviews: false,
      require_code_owner_reviews: false,
      required_approving_review_count: 1,
      require_last_push_approval: false,
      bypass_pull_request_allowances: {users: [], teams: [], apps: []}
    },
    restrictions: null,
    required_linear_history: false,
    allow_force_pushes: false,
    allow_deletions: false,
    block_creations: false,
    required_conversation_resolution: false,
    lock_branch: false,
    allow_fork_syncing: false
  }' >"$protection_file"

  gh api -X PUT "$endpoint" --input "$protection_file" >/dev/null
  printf 'GREEN branch protection requires one approving review\n'
}

add_human_approval_check() {
  local endpoint="repos/${SMOKE_REPO}/branches/main/protection/required_status_checks"
  local current
  local payload

  current="$(gh api "$endpoint")"
  payload="$(jq -nc --argjson current "$current" '{
    strict: ($current.strict // true),
    contexts: (($current.contexts // []) + ["legion-human-approval"] | unique)
  }')"
  gh api -X PATCH "$endpoint" --input - <<<"$payload" >/dev/null
  printf 'GREEN branch protection requires legion-human-approval\n'
}

measure_app_approval() {
  local existing_pr=""
  local branch
  local timestamp
  local base_sha
  local reviewer_token
  local review_decision

  if [[ -r "${smoke_dir}/protection-probe-pr" ]]; then
    existing_pr="$(<"${smoke_dir}/protection-probe-pr")"
    if gh pr view "$existing_pr" -R "$SMOKE_REPO" --json number >/dev/null 2>&1; then
      printf 'REUSED App-approval probe PR #%s\n' "$existing_pr"
      review_decision="$(gh pr view "$existing_pr" -R "$SMOKE_REPO" --json reviewDecision --jq '.reviewDecision')"
      printf '%s\n' "$review_decision" >"${smoke_dir}/protection-probe-review-decision"
      if [[ "$review_decision" == "APPROVED" ]]; then
        add_human_approval_check
      fi
      return
    fi
  fi

  timestamp="$(date +%s)"
  branch="legion-smoke-approval-${timestamp}"
  base_sha="$(gh api "repos/${SMOKE_REPO}/git/ref/heads/main" --jq '.object.sha')"
  gh api -X POST "repos/${SMOKE_REPO}/git/refs" -f "ref=refs/heads/${branch}" -f "sha=${base_sha}" >/dev/null
  gh api -X PUT "repos/${SMOKE_REPO}/contents/.legion-smoke/approval-${timestamp}.txt" \
    -f "message=chore(smoke): app approval probe" \
    -f "content=$(printf 'approval probe %s\n' "$timestamp" | base64 -w 0)" \
    -f "branch=${branch}" >/dev/null

  gh pr create -R "$SMOKE_REPO" --base main --head "$branch" \
    --title 'chore(smoke): app approval probe' \
    --body 'Disposable branch-protection capability probe.' >/dev/null
  existing_pr="$(gh pr view "$branch" -R "$SMOKE_REPO" --json number --jq '.number')"
  printf '%s\n' "$existing_pr" >"${smoke_dir}/protection-probe-pr"
  printf '%s\n' "$branch" >"${smoke_dir}/protection-probe-branch"

  reviewer_token="$(app_installation_token "$LEGION_REVIEW_APP_ID" GH_REVIEW_APP_PRIVATE_KEY_B64)"
  GH_TOKEN="$reviewer_token" gh api -X POST "repos/${SMOKE_REPO}/pulls/${existing_pr}/reviews" -f event=APPROVE >/dev/null
  review_decision="$(gh pr view "$existing_pr" -R "$SMOKE_REPO" --json reviewDecision --jq '.reviewDecision')"
  printf '%s\n' "$review_decision" >"${smoke_dir}/protection-probe-review-decision"

  if [[ "$review_decision" == "APPROVED" ]]; then
    add_human_approval_check
  else
    printf 'GREEN reviewer-App approval result: %s; legion-human-approval is not required\n' "$review_decision"
  fi
}
remove_recorded_forwarder_hook() {
  local name="$1"
  local hook_file="${smoke_dir}/${name}.hook"
  local hook_endpoint

  [[ -r "$hook_file" ]] || return 0
  hook_endpoint="$(<"$hook_file")"
  rm -f "$hook_file"
  [[ "$hook_endpoint" =~ ^(repos|orgs)/[^/]+(/[^/]+)?/hooks/[0-9]+$ ]] ||
    fail "invalid recorded ${name} hook endpoint"
  gh api -X DELETE "$hook_endpoint" >/dev/null
}

record_forwarder_hook() {
  local name="$1"
  local hooks_endpoint="$2"
  local hook_id

  hook_id="$(gh api "$hooks_endpoint" | jq -r --arg url "$webhook_forwarder_url" '[.[] | select(.name == "cli" and .active and .config.url == $url) | .id] | if length == 1 then .[0] else empty end')"
  [[ "$hook_id" =~ ^[0-9]+$ ]] || fail "could not identify the managed ${name} hook"
  printf '%s/%s\n' "$hooks_endpoint" "$hook_id" >"${smoke_dir}/${name}.hook"
}

wait_for_webhook_forwarder() {
  local name="${1:-webhook-forward}"
  local pid_file="${smoke_dir}/${name}.pid"
  local log_file="${smoke_dir}/${name}.log"
  local attempt

  for ((attempt = 1; attempt <= 60; attempt += 1)); do
    pid_is_live "$pid_file" || fail "webhook forwarder exited; inspect ${log_file}"
    if [[ -r "$log_file" && "$(<"$log_file")" == *"Forwarding Webhook events from GitHub..."* ]]; then
      printf 'GREEN %s: tunnel-ready signal received\n' "$name"
      return
    fi
    sleep 1
  done

  fail "webhook forwarder did not report a tunnel-ready signal; inspect ${log_file}"
}

wait_for_envoy_bridge() {
  local pid_file="${smoke_dir}/envoy-bridge.pid"
  local log_file="${smoke_dir}/envoy-bridge.log"
  local log
  local reason
  local attempt

  for ((attempt = 1; attempt <= 60; attempt += 1)); do
    if [[ -r "$log_file" ]]; then
      log="$(<"$log_file")"
      if [[ "$log" == *"BRIDGE UNHEALTHY "* ]]; then
        reason="${log##*BRIDGE UNHEALTHY }"
        reason="${reason%%$'\n'*}"
        fail "Envoy bridge unhealthy: ${reason}"
      fi
      if [[ "$log" == *"BRIDGE READY "* ]] && pid_is_live "$pid_file"; then
        printf 'GREEN Envoy bridge: upstream subscription is ready\n'
        return
      fi
    fi
    pid_is_live "$pid_file" || fail "Envoy bridge exited; inspect ${log_file}"
    sleep 1
  done

  fail "Envoy bridge did not become ready; inspect ${log_file}"
}
assert_webhook_round_trip() {
  local payload='{"zen":"legion smoke round-trip"}'
  local signature
  local status

  signature="$(SMOKE_WEBHOOK_PAYLOAD="$payload" bun -e '
    import { createHmac } from "node:crypto";
    process.stdout.write(createHmac("sha256", process.env.GITHUB_WEBHOOK_SECRET).update(process.env.SMOKE_WEBHOOK_PAYLOAD).digest("hex"));
  ')" || fail "could not sign local GitHub ping"
  status="$(curl --connect-timeout 2 --max-time 10 --output /dev/null --silent --show-error \
    --write-out '%{http_code}' \
    --request POST \
    --header 'Content-Type: application/json' \
    --header "X-GitHub-Delivery: smoke-round-trip-$(date +%s%N)" \
    --header 'X-GitHub-Event: ping' \
    --header "X-Hub-Signature-256: sha256=${signature}" \
    --data-binary "$payload" \
    "http://127.0.0.1:${listener_port}/webhook/github")" ||
    fail "could not deliver signed local GitHub ping to listener"

  case "$status" in
    200)
      printf 'GREEN webhook round-trip: listener accepted signed ping\n'
      ;;
    401)
      fail 'listener rejected signed webhook round-trip (HTTP 401): secret mismatch'
      ;;
    *)
      fail "listener rejected signed webhook round-trip (HTTP ${status})"
      ;;
  esac
}


main() {
  require_command base64
  require_command bun
  require_command curl
  require_command docker
  require_command gh
  require_command jq
  require_command mise
  require_command go
  require_command sed
  require_command tr
  require_command ss
  require_command awk
  require_command setsid
  local board_scope
  local owner_type
  local dispatch_bearer
  local label_bearer
  local webhook_mode

  require_env SMOKE_REPO
  require_env SMOKE_PROJECT
  require_env GITHUB_WEBHOOK_SECRET
  normalize_github_webhook_secret
  require_env GITHUB_WEBHOOK_SECRET
  require_env GH_AGENT_APP_PRIVATE_KEY_B64
  require_env GH_REVIEW_APP_PRIVATE_KEY_B64
  require_numeric LEGION_IMPLEMENT_APP_ID
  require_numeric LEGION_REVIEW_APP_ID

  [[ "$SMOKE_REPO" =~ ^[^/]+/[^/]+$ ]] || fail "SMOKE_REPO must be <owner>/<repo>"
  [[ "$SMOKE_PROJECT" =~ ^[^/]+/[0-9]+$ ]] || fail "SMOKE_PROJECT must be <owner>/<number>"
  webhook_mode="$(resolve_webhook_mode)"

  mkdir -p "$smoke_dir" "${smoke_dir}/daemon" "${smoke_dir}/dispatch-home" \
    "${smoke_dir}/xdg-data" "${smoke_dir}/xdg-state/legion" "$gh_config_dir"
  printf '%s\n' "$webhook_mode" >"${smoke_dir}/webhook-mode"
  assert_port_free 'Envoy listener' "$listener_port" "${smoke_dir}/listener.pid"
  assert_port_free dispatch "$dispatch_port" "${smoke_dir}/dispatch.pid"
  assert_port_free 'Legion daemon' "$daemon_port" "${smoke_dir}/daemon.pid"
  write_daemon_config
  write_dispatch_config
  dispatch_bearer="${LEGION_DISPATCH_BEARER:-$(app_installation_token "$LEGION_IMPLEMENT_APP_ID" GH_AGENT_APP_PRIVATE_KEY_B64)}"
  board_scope="${SMOKE_BOARD_SCOPE:-}"
  if [[ -z "$board_scope" ]]; then
    owner_type="$(GH_TOKEN="$dispatch_bearer" GH_CONFIG_DIR="$gh_config_dir" gh api "users/$(project_owner)" --jq '.type')"
    board_scope=$([[ "$owner_type" == "Organization" ]] && printf org || printf none)
  fi
  [[ "$board_scope" == "org" || "$board_scope" == "none" ]] ||
    fail "SMOKE_BOARD_SCOPE must be org or none"
  printf '%s\n' "$board_scope" >"${smoke_dir}/board-scope"
  (
    cd "${repo_root}/packages/envoy"
    go build -o out/envoy-listener ./cmd/listener
    go build -o out/envoy-dispatch ./cmd/dispatch
  )

  ensure_nats

  start_process listener env \
    PORT="$listener_port" \
    ENVOY_MACHINE_ID="legion-smoke" \
    NATS_URLS="$nats_url" \
    ENVOY_HOST_BRIDGE="127.0.0.1" \
    ENVOY_WEBHOOKS="github" \
    ENVOY_GITHUB_WEBHOOK_SECRET="$GITHUB_WEBHOOK_SECRET" \
    ENVOY_REVIEWER_APP_ID="$LEGION_REVIEW_APP_ID" \
    "${repo_root}/packages/envoy/out/envoy-listener"

  start_process dispatch env \
    HOME="${smoke_dir}/dispatch-home" \
    DISPATCH_PORT="$dispatch_port" \
    NATS_URLS="$nats_url" \
    "${repo_root}/packages/envoy/out/envoy-dispatch"

  wait_for_json 'Envoy listener' "http://127.0.0.1:${listener_port}/healthz" '.status == "healthy"' "${smoke_dir}/listener.pid"
  wait_for_http 'dispatch' "http://127.0.0.1:${dispatch_port}/healthz" "${smoke_dir}/dispatch.pid"
  assert_webhook_round_trip
  if [[ "$webhook_mode" == "forward" ]]; then
    if pid_is_live "${smoke_dir}/webhook-forward.pid" &&
      [[ -r "${smoke_dir}/webhook-forward.log" && "$(<"${smoke_dir}/webhook-forward.log")" == *"Forwarding Webhook events from GitHub..."* ]]; then
      printf 'REUSED webhook forwarder (pgid %s)\n' "$(<"${smoke_dir}/webhook-forward.pid")"
    else
      remove_recorded_forwarder_hook webhook-forward
      start_process_group webhook-forward gh webhook forward --repo "$SMOKE_REPO" \
        --events "$webhook_events" \
        --secret "$GITHUB_WEBHOOK_SECRET" \
        --url "http://127.0.0.1:${listener_port}/webhook/github"
    fi
    wait_for_webhook_forwarder
    record_forwarder_hook webhook-forward "repos/${SMOKE_REPO}/hooks"
    if [[ "$board_scope" == "org" ]]; then
      if pid_is_live "${smoke_dir}/board-webhook-forward.pid" &&
        [[ -r "${smoke_dir}/board-webhook-forward.log" && "$(<"${smoke_dir}/board-webhook-forward.log")" == *"Forwarding Webhook events from GitHub..."* ]]; then
        printf 'REUSED board webhook forwarder (pgid %s)\n' "$(<"${smoke_dir}/board-webhook-forward.pid")"
      else
        remove_recorded_forwarder_hook board-webhook-forward
        start_process_group board-webhook-forward gh webhook forward --org "$(project_owner)" \
          --events projects_v2_item \
          --secret "$GITHUB_WEBHOOK_SECRET" \
          --url "http://127.0.0.1:${listener_port}/webhook/github"
      fi
      wait_for_webhook_forwarder board-webhook-forward
      record_forwarder_hook board-webhook-forward "orgs/$(project_owner)/hooks"
    fi
  elif [[ "$webhook_mode" == "envoy" ]]; then
    printf 'GREEN webhook ingress: production Envoy NATS bridge will forward only %s\n' "$SMOKE_REPO"
  else
    printf 'SKIPPED-BLOCKED webhook ingress: %s\n' "$(webhook_ingress_block_reason)"
  fi

  start_process daemon env \
    ENVOY_NATS_URL="$nats_url" \
    ENVOY_URL="http://127.0.0.1:${listener_port}" \
    LEGION_DAEMON_PORT="$daemon_port" \
    LEGION_DISPATCH_URL="http://127.0.0.1:${dispatch_port}" \
    LEGION_DISPATCH_BEARER="$dispatch_bearer" \
    LEGION_STATE_DIR="${smoke_dir}/daemon" \
    XDG_DATA_HOME="${smoke_dir}/xdg-data" \
    XDG_STATE_HOME="${smoke_dir}/xdg-state" \
    bun run "${repo_root}/packages/daemon/src/cli/index.ts" start "$SMOKE_PROJECT" --config "${smoke_dir}/legion.yaml"

  wait_for_json 'Legion daemon' "http://127.0.0.1:${daemon_port}/legion/v1/state" 'type == "object"' "${smoke_dir}/daemon.pid"
  if [[ "$webhook_mode" == "envoy" ]]; then
    start_process envoy-bridge env \
      SMOKE_REPO="$SMOKE_REPO" \
      SMOKE_RIG_NATS="$nats_url" \
      SMOKE_UPSTREAM_NATS="${SMOKE_UPSTREAM_NATS:-}" \
      bun run "${repo_root}/scripts/smoke/envoy-bridge.ts"
    wait_for_envoy_bridge
  fi
  label_bearer="$(app_installation_token "$LEGION_IMPLEMENT_APP_ID" GH_AGENT_APP_PRIVATE_KEY_B64 "$(repo_owner)")"
  ensure_labels "$label_bearer"
  if [[ -z "${SMOKE_PROJECT_ID:-}" ]]; then
    printf 'SKIPPED-BLOCKED board ingress: SMOKE_PROJECT_ID is required after the sandbox Projects V2 board exists\n'
  fi
  case "${SMOKE_BRANCH_PROTECTION:-}" in
    1)
      configure_branch_protection
      measure_app_approval
      ;;
    "")
      printf 'SKIPPED-BLOCKED merge gate: set SMOKE_BRANCH_PROTECTION=1 only after branch protection/ruleset is available\n'
      ;;
    *)
      fail "SMOKE_BRANCH_PROTECTION must be 1 when set"
      ;;
  esac
  wc -c <"${smoke_dir}/daemon.log" >"${smoke_dir}/daemon.log.offset"

  printf 'RIG READY\n'
}

main "$@"
