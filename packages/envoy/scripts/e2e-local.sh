#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317 # Bun code uses literal template syntax; cleanup is trap-invoked
# e2e-local.sh — Runs the local Envoy webhook, delivery, and renderer acceptance path.
#
# Starts a throwaway NATS server and listener, then leaves evidence in out/e2e/.
# The listener needs ENVOY_HOST_BRIDGE=127.0.0.1 because it runs on this host and
# delivers to the host-local fake session. ENVOY_CI_DEBOUNCE=250ms keeps this
# bounded acceptance run responsive while retaining the production settle logic.
#
# Optional ports: E2E_NATS_PORT (14222), E2E_PORT (19020), E2E_SESSION_PORT (19021).
# Optional NATS container name: E2E_NATS_CONTAINER (envoy-e2e-nats).
# Optional nats-server binary: E2E_NATS_SERVER (the one on PATH), used when Docker is absent.
set -euo pipefail
# This rig's NATS is a throwaway server with no users. nats.go refuses an nkey when the server sends
# no nonce ("nats: nkeys not supported by the server"), so no process here inherits an operator's
# NATS_NKEY_SEED or NATS_NKEY_SEED_FILE.
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE

usage() {
  cat <<'EOF'
Usage: e2e-local.sh [--help]

Boot a throwaway NATS and the local Envoy listener, submit signed public GitHub
fixtures plus one direct message, prove how a send to an unknown session and a
send whose bus is gone are refused, and write raw and rendered acceptance
evidence to packages/envoy/out/e2e/. NATS runs in Docker, or as a local
nats-server process on a host that has no container runtime.
EOF
}

case "${1:-}" in
  "") ;;
  --help|-h)
    usage
    exit 0
    ;;
  *)
    printf 'ERR: unknown option: %s\n' "$1" >&2
    usage >&2
    exit 2
    ;;
esac

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
envoy_dir="$(cd "${script_dir}/.." && pwd)"
readonly envoy_dir
repo_root="$(cd "${envoy_dir}/../.." && pwd)"
readonly repo_root
fixture_dir="${script_dir}/fixtures/github"
readonly fixture_dir
out_dir="${envoy_dir}/out/e2e"
readonly out_dir

nats_port="${E2E_NATS_PORT:-14222}"
listener_port="${E2E_PORT:-19020}"
session_port="${E2E_SESSION_PORT:-19021}"
nats_container="${E2E_NATS_CONTAINER:-envoy-e2e-nats}"
readonly nats_port listener_port session_port nats_container
listener_url="http://127.0.0.1:${listener_port}"
readonly listener_url
nats_url="nats://127.0.0.1:${nats_port}"
readonly nats_url
webhook_secret="e2e-local"
readonly webhook_secret
session_id="ses_e2e_local"
readonly session_id
# The destination of the refused send: an id nothing registers in this run, so the store cannot
# hold it however late the assertion runs.
absent_session_id="ses_e2e_local_absent"
readonly absent_session_id

readonly envelopes_file="${out_dir}/envelopes.jsonl"
readonly session_prompts_file="${out_dir}/session-prompts.jsonl"
readonly rendered_ts_file="${out_dir}/rendered-ts.txt"
readonly rendered_go_file="${out_dir}/rendered-go.txt"
readonly direct_response_file="${out_dir}/direct-send-response.json"
readonly unwired_response_file="${out_dir}/unwired-subscribe-response.json"
readonly unheld_role_response_file="${out_dir}/unheld-role-response.json"
readonly role_claim_response_file="${out_dir}/role-claim-response.json"
readonly held_role_response_file="${out_dir}/held-role-response.json"
readonly listener_log_file="${out_dir}/listener.log"
readonly session_log_file="${out_dir}/session.log"
readonly subscriber_log_file="${out_dir}/subscriber.log"
readonly listener_binary="${out_dir}/listener"
readonly session_ready_fifo="${out_dir}/session-ready.fifo"
readonly subscriber_ready_fifo="${out_dir}/subscriber-ready.fifo"
readonly absent_session_response_file="${out_dir}/absent-session-response.json"
readonly bus_down_response_file="${out_dir}/bus-down-send-response.json"
readonly bus_down_headers_file="${out_dir}/bus-down-send-headers.txt"
readonly bus_down_result_file="${out_dir}/bus-down-send-result.txt"
readonly bus_down_client_file="${out_dir}/bus-down-client-error.json"
readonly nats_log_file="${out_dir}/nats.log"
readonly nats_store_dir="${out_dir}/nats-store"

listener_pid=""
session_pid=""
subscriber_pid=""
nats_pid=""
nats_started=0

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  for pid in "$subscriber_pid" "$session_pid" "$listener_pid"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -f "$session_ready_fifo" "$subscriber_ready_fifo"
  # stop_nats guards on nats_started itself; the guard is repeated here because this trap is armed
  # before that function is defined, and a run that exits on a missing command must not die in its
  # own cleanup.
  if [[ "$nats_started" -eq 1 ]]; then
    stop_nats
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

for command in bun curl go jq openssl; do
  command -v "$command" >/dev/null || {
    printf 'ERR: required command is unavailable: %s\n' "$command" >&2
    exit 3
  }
done

# Which backend gives this run its NATS. Docker is the first choice and behaves exactly as it
# always has: the same image, the same single container this script owns, and the same refusal to
# touch a container that is already there. The nats-server fallback exists because a Legion worker
# pod has no container runtime at all -- no docker, no podman, no socket to reach one -- so a
# container is not something the script can fall back to there, and without the fallback this
# acceptance run cannot be executed where this repository's agents actually run.
# dispatch://INFRA-250 removes the fallback by starting nats-server in process.
nats_backend=docker
nats_server_bin="${E2E_NATS_SERVER:-}"
if ! command -v docker >/dev/null; then
  nats_backend=process
  if [[ -z "$nats_server_bin" ]]; then
    nats_server_bin="$(command -v nats-server || true)"
  fi
  if [[ -z "$nats_server_bin" ]]; then
    printf 'ERR: required command is unavailable: docker (runs the throwaway NATS container) or nats-server (runs the same NATS as a child process; name one with E2E_NATS_SERVER)\n' >&2
    exit 3
  fi
fi
readonly nats_backend nats_server_bin

if [[ "$nats_backend" == docker ]] && docker container inspect "$nats_container" >/dev/null 2>&1; then
  printf 'ERR: %s already exists; leave it untouched and set E2E_NATS_CONTAINER to a free name.\n' "$nats_container" >&2
  exit 3
fi

# wait_for_nats makes both backends ready in one way: the port answers, whoever opened it. Docker
# returns from `run -d` before the server inside has bound, and nats-server returns from the fork
# the same way, so neither start is proof on its own.
wait_for_nats() {
  local deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${nats_port}") 2>/dev/null; then
      return 0
    fi
    sleep 0.25
  done
  printf 'ERR: NATS did not accept connections on 127.0.0.1:%s within 30 seconds.\n' "$nats_port" >&2
  if [[ "$nats_backend" == process ]] && [[ -s "$nats_log_file" ]]; then
    cat "$nats_log_file" >&2
  fi
  return 1
}

start_nats() {
  if [[ "$nats_backend" == docker ]]; then
    printf 'starting NATS: %s on 127.0.0.1:%s\n' "$nats_container" "$nats_port"
    docker run --name "$nats_container" -d -p "${nats_port}:4222" mirror.gcr.io/library/nats:2.10-alpine -js >/dev/null
    nats_started=1
  else
    printf 'starting NATS: %s on 127.0.0.1:%s\n' "$nats_server_bin" "$nats_port"
    "$nats_server_bin" -js -a 127.0.0.1 -p "$nats_port" -sd "$nats_store_dir" >"$nats_log_file" 2>&1 &
    nats_pid=$!
    nats_started=1
  fi
  wait_for_nats
}

# stop_nats ends this run's NATS whichever backend started it, and is the one way it is ended: the
# cleanup trap and the bus-down step below both call it, so a half-stopped rig is not expressible.
stop_nats() {
  if [[ "$nats_started" -ne 1 ]]; then
    return 0
  fi
  nats_started=0
  if [[ "$nats_backend" == docker ]]; then
    docker stop "$nats_container" >/dev/null 2>&1 || true
    docker rm "$nats_container" >/dev/null 2>&1 || true
    return 0
  fi
  if [[ -n "$nats_pid" ]] && kill -0 "$nats_pid" 2>/dev/null; then
    kill "$nats_pid" 2>/dev/null || true
    wait "$nats_pid" 2>/dev/null || true
  fi
  nats_pid=""
}

mkdir -p "$out_dir"
rm -rf "$nats_store_dir"
rm -f "$envelopes_file" "$session_prompts_file" "$rendered_ts_file" "$rendered_go_file" \
  "$direct_response_file" "$unwired_response_file" "$unheld_role_response_file" "$role_claim_response_file" "$held_role_response_file" \
  "$absent_session_response_file" "$bus_down_response_file" "$bus_down_headers_file" "$bus_down_result_file" "$bus_down_client_file" \
  "$listener_log_file" "$session_log_file" "$subscriber_log_file" "$nats_log_file" "$listener_binary" "$session_ready_fifo" "$subscriber_ready_fifo"
: >"$envelopes_file"
: >"$session_prompts_file"
mkfifo "$session_ready_fifo" "$subscriber_ready_fifo"

start_nats

printf 'building listener\n'
(
  cd "$envoy_dir"
  go build -o "$listener_binary" ./cmd/listener
)

printf 'starting listener: %s\n' "$listener_url"
PORT="$listener_port" \
  ENVOY_MACHINE_ID=e2e-local \
  NATS_URLS="$nats_url" \
  ENVOY_WEBHOOKS=github \
  ENVOY_GITHUB_WEBHOOK_SECRET="$webhook_secret" \
  ENVOY_REVIEWER_APP_ID=1 \
  ENVOY_HOST_BRIDGE=127.0.0.1 \
  ENVOY_CI_DEBOUNCE=250ms \
  "$listener_binary" >"$listener_log_file" 2>&1 &
listener_pid=$!

wait_for_health() {
  local deadline=$((SECONDS + 30))
  local body
  while ((SECONDS < deadline)); do
    if body="$(curl -fsS "${listener_url}/healthz" 2>/dev/null)" && [[ "$body" == *'"status":"healthy"'* ]]; then
      return 0
    fi
    sleep 0.25
  done
  printf 'ERR: listener did not become healthy within 30 seconds.\n' >&2
  cat "$listener_log_file" >&2
  return 1
}

printf 'waiting for listener health\n'
wait_for_health

E2E_SESSION_PROMPTS_FILE="$session_prompts_file" E2E_SESSION_PORT="$session_port" \
  bun -e '
    import { appendFileSync } from "node:fs";

    const output = process.env.E2E_SESSION_PROMPTS_FILE;
    const port = Number(process.env.E2E_SESSION_PORT);
    if (output === undefined || !Number.isInteger(port) || port <= 0) {
      throw new Error("fake-session configuration is invalid");
    }
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port,
      async fetch(request) {
        const url = new URL(request.url);
        if (request.method !== "POST" || !url.pathname.endsWith("/prompt_async")) {
          return new Response("not found", { status: 404 });
        }
        appendFileSync(output, `${await request.text()}\n`);
        return new Response(null, { status: 204 });
      },
    });
    process.stdout.write(`ready ${server.port}\n`);
  ' >"$session_ready_fifo" 2>"$session_log_file" &
session_pid=$!

if ! IFS= read -r -t 5 session_ready <"$session_ready_fifo" || [[ "$session_ready" != "ready ${session_port}" ]]; then
  printf 'ERR: fake session did not bind to port %s.\n' "$session_port" >&2
  cat "$session_log_file" >&2
  exit 3
fi

(
  cd "$repo_root"
  exec env E2E_NATS_URL="$nats_url" E2E_ENVELOPES_FILE="$envelopes_file" bun -e '
    import { appendFileSync } from "node:fs";
    import { connect, StringCodec } from "nats";

    const url = process.env.E2E_NATS_URL;
    const output = process.env.E2E_ENVELOPES_FILE;
    if (url === undefined || output === undefined) throw new Error("capture configuration is invalid");
    const codec = StringCodec();
    const connection = await connect({ servers: url, name: "envoy-e2e-capture" });
    const subscription = connection.subscribe("notifications.>");
    process.stdout.write("ready\n");
    for await (const message of subscription) appendFileSync(output, `${codec.decode(message.data)}\n`);
  ' >"$subscriber_ready_fifo" 2>"$subscriber_log_file"
) &
subscriber_pid=$!

if ! IFS= read -r -t 5 subscriber_ready <"$subscriber_ready_fifo" || [[ "$subscriber_ready" != "ready" ]]; then
  printf 'ERR: NATS capture subscriber did not become ready.\n' >&2
  cat "$subscriber_log_file" >&2
  exit 3
fi

subscribe_body="$(jq -nc \
  --arg session_id "$session_id" \
  --arg dir "/tmp/envoy-e2e-local" \
  --arg title "Envoy local acceptance" \
  --arg topic "notifications.github.example-org.example-repo.>" \
  --argjson port "$session_port" \
  '{session_id:$session_id,dir:$dir,title:$title,topics:[$topic],port:$port,driving:true}')"
curl -fsS -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/interests/subscribe" -d "$subscribe_body" >/dev/null

unwired_subscribe_body="$(jq -nc \
  --arg session_id "$session_id" \
  --arg topic "notifications.github.never-seen.e2e-repository.>" \
  '{session_id:$session_id,topics:[$topic]}')"
curl -fsS -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/interests/subscribe" -d "$unwired_subscribe_body" >"$unwired_response_file"
jq -e \
  '.warnings | length == 1 and .[0] == "no GitHub event for never-seen/e2e-repository in the stream'\''s retention window; is the App installed there?"' \
  "$unwired_response_file" >/dev/null || {
  printf 'ERR: never-seen repository did not produce exactly one wiring warning.\n' >&2
  exit 1
}

post_github() {
  local event=$1
  local fixture=$2
  local delivery
  delivery="e2e-${event}-$(basename "$fixture" .json)"
  local signature
  local status

  signature="sha256=$(openssl dgst -sha256 -hmac "$webhook_secret" <"$fixture" | cut -d ' ' -f 2)"
  status="$(curl -sS -o "${out_dir}/response-${delivery}.txt" -w '%{http_code}' \
    -X POST "${listener_url}/webhook/github" \
    -H 'Content-Type: application/json' \
    -H "X-GitHub-Event: ${event}" \
    -H "X-GitHub-Delivery: ${delivery}" \
    -H "X-Hub-Signature-256: ${signature}" \
    --data-binary "@${fixture}")"
  if [[ "$status" != 200 ]]; then
    printf 'ERR: %s fixture was rejected with HTTP %s.\n' "$(basename "$fixture")" "$status" >&2
    cat "${out_dir}/response-${delivery}.txt" >&2
    return 1
  fi
}

wait_for_topic_count() {
  local topic=$1
  local expected=$2
  local deadline=$((SECONDS + 20))
  local count
  while ((SECONDS < deadline)); do
    count="$(jq -rs --arg topic "$topic" '[.[] | select(.topic == $topic)] | length' "$envelopes_file")"
    if ((count >= expected)); then
      return 0
    fi
    sleep 0.25
  done
  printf 'ERR: expected %s captures on %s within 20 seconds.\n' "$expected" "$topic" >&2
  return 1
}

wait_for_prompt_count() {
  local expected=$1
  local deadline=$((SECONDS + 20))
  local count
  while ((SECONDS < deadline)); do
    count="$(wc -l <"$session_prompts_file")"
    if ((count >= expected)); then
      return 0
    fi
    sleep 0.25
  done
  printf 'ERR: expected %s fake-session prompts within 20 seconds.\n' "$expected" >&2
  return 1
}

direct_message=$'Direct acceptance summary.\n\nDirect second paragraph.\n\nDirect third paragraph.'
direct_body="$(jq -nc \
  --arg session_id "$session_id" \
  --arg message "$direct_message" \
  '{source:"agent",source_session:"octocat",target_session:$session_id,message:$message}')"
printf 'posting direct message\n'
curl -fsS -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/messages/send" -d "$direct_body" >"$direct_response_file"
jq -e --arg recipient "$session_id" --arg message "$direct_message" \
  '.recipient == $recipient and .topic == ("notifications.agent." + $recipient) and .payload_summary == "Direct acceptance summary." and .payload == $message' \
  "$direct_response_file" >/dev/null || {
  printf 'ERR: direct send response did not retain the three-paragraph message contract.\n' >&2
  exit 1
}

# A send to a session id nothing registered. The store holds no key for it, so the listener refuses
# the send by name instead of publishing into a topic no one reads: the sender is told which
# session is gone, and can route around that pane. `curl -fsS` cannot assert a refusal (it prints
# no body and exits non-zero), so this takes the status from -w as the unheld-role publish above does.
printf 'posting a direct message to an unregistered session\n'
absent_summary="Absent session acceptance summary."
absent_body="$(jq -nc \
  --arg session_id "$absent_session_id" \
  --arg message "$absent_summary" \
  '{source:"agent",source_session:"octocat",target_session:$session_id,message:$message}')"
absent_status="$(curl -sS -o "$absent_session_response_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/messages/send" -d "$absent_body")"
if [[ "$absent_status" != 404 ]] || ! jq -e --arg session_id "$absent_session_id" \
  '.error == ("no live session " + $session_id)' "$absent_session_response_file" >/dev/null; then
  printf 'ERR: a send to the unregistered session %s answered HTTP %s, not a 404 naming it.\n' \
    "$absent_session_id" "$absent_status" >&2
  cat "$absent_session_response_file" >&2
  exit 1
fi

# Why this step reads the listener's own log, which nothing in this script read before: a refused
# send publishes nothing at all, so no envelope, no stream entry and no "listener received" line
# will ever name it, and this record is the only server-side trace it leaves. The issue exists
# because that trace did not: a send never attempted, one lost on the way and one refused by name
# all left the same silence in the log.
count_listener_records() {
  local selector=$1
  # The records are JSON lines, but the log also carries the Go standard logger's plain lines
  # ("2026/10/25 03:19:15 github sender login=..."), and jq's fromjson reads the leading 2026 of
  # one of those as a number rather than refusing it. Keeping only the objects drops both shapes
  # of non-record line instead of failing the whole read over them.
  jq -R 'fromjson? // empty | select(type == "object")' "$listener_log_file" \
    | jq -s --arg session_id "$session_id" --arg absent_session_id "$absent_session_id" \
      "[.[] | select(${selector})] | length"
}

# Each record is written after its response is sent, so the assertion can reach the log a moment
# before the line lands: wait for the first match rather than racing it, and fail on anything but
# exactly one, a second line being as wrong as none.
wait_for_listener_record() {
  local description=$1
  local selector=$2
  local deadline=$((SECONDS + 10))
  local count=0
  while ((SECONDS < deadline)); do
    count="$(count_listener_records "$selector")"
    if ((count != 0)); then
      break
    fi
    sleep 0.25
  done
  if ((count != 1)); then
    printf 'ERR: expected exactly one %s record in the listener log; found %s.\n' "$description" "$count" >&2
    return 1
  fi
}

printf 'asserting the listener request records\n'
wait_for_listener_record 'accepted send' \
  '.msg == "listener send" and .level == "INFO" and .outcome == "accepted" and .status == 200
   and .target_session == $session_id and .source == "agent" and .source_session == "octocat"
   and .payload_summary == "Direct acceptance summary." and (.event_id | length) > 0'
wait_for_listener_record 'refused send' \
  '.msg == "listener send" and .level == "WARN" and .outcome == "no_live_session" and .status == 404
   and .target_session == $absent_session_id and .source == "agent" and .source_session == "octocat"
   and .payload_summary == "Absent session acceptance summary." and (.event_id | length) > 0'

role="e2e-reviewer"
unheld_status="$(curl -sS -o "$unheld_role_response_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/messages/publish" \
  -d "{\"topic\":\"notifications.role.${role}\",\"message\":\"Role delivery.\"}")"
if [[ "$unheld_status" != 404 ]] || ! jq -e --arg role "$role" '.error == ("no holder for role " + $role)' "$unheld_role_response_file" >/dev/null; then
  printf 'ERR: an unheld role publish was not rejected with 404.\n' >&2
  exit 1
fi
curl -fsS -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/roles/set" \
  -d "{\"session_id\":\"${session_id}\",\"role\":\"${role}\"}" >"$role_claim_response_file"
jq -e --arg session_id "$session_id" '.session_id == $session_id' "$role_claim_response_file" >/dev/null || {
  printf 'ERR: fake session did not claim the role.\n' >&2
  exit 1
}
curl -fsS -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/messages/publish" \
  -d "{\"topic\":\"notifications.role.${role}\",\"message\":\"Role delivery.\"}" >"$held_role_response_file"
jq -e --arg session_id "$session_id" '.holder == $session_id' "$held_role_response_file" >/dev/null || {
  printf 'ERR: held role publish did not return the fake-session holder.\n' >&2
  exit 1
}

post_github issue_comment "${fixture_dir}/issue-comment-created.json"
post_github issue_comment "${fixture_dir}/issue-comment-edited.json"
post_github issue_comment "${fixture_dir}/issue-comment-pr-created.json"
post_github pull_request "${fixture_dir}/pull-request-opened.json"
post_github check_suite "${fixture_dir}/check-suite-first.json"
post_github check_run "${fixture_dir}/check-run-first.json"
wait_for_topic_count "notifications.github.example-org.example-repo.pr.42.checks" 1

post_github pull_request "${fixture_dir}/pull-request-synchronize.json"
post_github check_suite "${fixture_dir}/check-suite-second.json"
post_github check_run "${fixture_dir}/check-run-second.json"
wait_for_topic_count "notifications.github.example-org.example-repo.pr.42.checks" 2

post_github check_run "${fixture_dir}/check-run-cancelled.json"
wait_for_topic_count "notifications.github.example-org.example-repo.pr.42.checks" 3

# The pass-6 trace. Before: build 2002 completed 03:17 (the head's latest
# completion) and integration 2005 cancelled 03:16 (the head's highest run id).
# A newer build attempt 2004 completed at 03:15 — created after 2002 yet
# finished earlier — supersedes 2002: the attempt set advances while both the
# head's highest run id (2005) and its latest completion (03:17 -> 03:16) do
# not rise. A consumer ordering by the set accepts it; one ordering by scalar
# maxima would drop it forever.
post_github check_run "${fixture_dir}/check-run-superseding.json"
wait_for_topic_count "notifications.github.example-org.example-repo.pr.42.checks" 4

post_github pull_request "${fixture_dir}/pull-request-closed-merged.json"
post_github workflow_run "${fixture_dir}/workflow-run-with-pr.json"
post_github workflow_run "${fixture_dir}/workflow-run-without-pr.json"
post_github check_run "${fixture_dir}/check-run-actions-job-without-pr.json"
post_github check_run "${fixture_dir}/check-run-actions-job-merge-queue.json"
post_github push "${fixture_dir}/push.json"
wait_for_prompt_count 14

(
  cd "$repo_root/packages/envoy-client"
  E2E_ENVELOPES_FILE="$envelopes_file" E2E_RENDERED_TS_FILE="$rendered_ts_file" \
    E2E_RENDERED_GO_FILE="$rendered_go_file" E2E_SESSION_PROMPTS_FILE="$session_prompts_file" \
    E2E_SESSION_ID="$session_id" bun -e '
      import { readFileSync, writeFileSync } from "node:fs";
      import { renderInbound } from "@legion/envoy-client/delivery";

      const envelopesFile = process.env.E2E_ENVELOPES_FILE;
      const renderedTSFile = process.env.E2E_RENDERED_TS_FILE;
      const renderedGoFile = process.env.E2E_RENDERED_GO_FILE;
      const promptsFile = process.env.E2E_SESSION_PROMPTS_FILE;
      const sessionID = process.env.E2E_SESSION_ID;
      if ([envelopesFile, renderedTSFile, renderedGoFile, promptsFile, sessionID].some((value) => value === undefined)) {
        throw new Error("renderer configuration is invalid");
      }
      const envelopes = readFileSync(envelopesFile, "utf8").trim().split("\n").filter(Boolean);
      const rendered = envelopes.map((raw) => renderInbound(raw, sessionID).content);
      writeFileSync(renderedTSFile, `${rendered.join("\n\n")}\n`);
      const texts = readFileSync(promptsFile, "utf8").trim().split("\n").filter(Boolean).map((raw) => {
        const prompt = JSON.parse(raw);
        const text = prompt.parts?.find((part) => part.type === "text")?.text;
        if (typeof text !== "string") throw new Error("prompt_async body has no text part");
        return text;
      });
      writeFileSync(renderedGoFile, `${texts.join("\n\n")}\n`);
    '
)

E2E_ENVELOPES_FILE="$envelopes_file" E2E_RENDERED_TS_FILE="$rendered_ts_file" \
  E2E_RENDERED_GO_FILE="$rendered_go_file" E2E_SESSION_ID="$session_id" bun -e '
    import { readFileSync } from "node:fs";

    const envelopesFile = process.env.E2E_ENVELOPES_FILE;
    const renderedTSFile = process.env.E2E_RENDERED_TS_FILE;
    const renderedGoFile = process.env.E2E_RENDERED_GO_FILE;
    const sessionID = process.env.E2E_SESSION_ID;
    if ([envelopesFile, renderedTSFile, renderedGoFile, sessionID].some((value) => value === undefined)) {
      throw new Error("assertion configuration is invalid");
    }
    const envelopes = readFileSync(envelopesFile, "utf8").trim().split("\n").filter(Boolean).map((raw) => JSON.parse(raw));
    const topic = (suffix) => `notifications.github.example-org.example-repo.${suffix}`;
    const require = (condition, message) => {
      if (!condition) throw new Error(message);
    };
    const withTopic = (name) => envelopes.filter((item) => item.topic === name);
    const payload = (item) => JSON.parse(item.payload);

    console.log("topic → summary");
    for (const item of envelopes) console.log(`${item.topic} → ${item.payload_summary}`);

    const directSummary = "Direct acceptance summary.";
    const directBody = `${directSummary}\n\nDirect second paragraph.\n\nDirect third paragraph.`;
    const directTopic = `notifications.agent.${sessionID}`;
    const direct = withTopic(directTopic);
    require(direct.length === 1, `direct envelope count = ${direct.length}, want 1`);
    require(direct[0].source === "agent" && direct[0].source_session === "octocat", "direct envelope loses sender identity");
    require(direct[0].payload_summary === directSummary && direct[0].payload === directBody, "direct envelope loses the three-paragraph body");

    const lifecycle = withTopic(topic("pr.42"));
    require(lifecycle.length === 3, `pr lifecycle count = ${lifecycle.length}, want 3`);
    const actions = lifecycle.map(payload).map((item) => item.action);
    require(actions.join(",") === "opened,synchronize,closed", `pr lifecycle actions = ${actions.join(",")}`);
    const closed = payload(lifecycle[2]);
    require(closed.merged === "true" && closed.head_sha === "2222222222222222222222222222222222222222", "closed lifecycle payload loses merged head facts");

    const comments = withTopic(topic("issue.7.comment"));
    require(comments.length === 2, `issue comment count = ${comments.length}, want 2`);
    for (const item of comments) {
      const data = payload(item);
      require(item.payload_summary.startsWith("comment ") && data.kind === "comment" && data.repo === "example-org/example-repo" && data.number === "7", "issue comment lacks prose or structured payload");
    }

    const prComments = withTopic(topic("pr.42.comment"));
    require(prComments.length === 1, `pr comment count = ${prComments.length}, want 1`);
    const prComment = payload(prComments[0]);
    require(prComments[0].payload_summary === "comment on example-org/example-repo#42 by octocat: Please review the pull request." && prComment.kind === "comment" && prComment.parent_kind === "pr" && prComment.repo === "example-org/example-repo" && prComment.number === "42", "pr comment lacks prose or structured payload");

    const checks = withTopic(topic("pr.42.checks"));
    require(checks.length === 4, `checks count = ${checks.length}, want 4`);
    const checksBySHA = new Map();
    const attemptSet = (data) => new Map(data.check_runs.map((run) => [run.name, run.id]));
    for (const item of checks) {
      const data = payload(item);
      require(data.kind === "checks", "checks envelope has wrong payload kind");
      require(Array.isArray(data.check_runs) && data.check_runs.length > 0, "checks payload lacks a non-empty check_runs attempt set");
      require(data.check_runs.every((run) => typeof run.name === "string" && run.name.length > 0 && Number.isInteger(run.id) && run.id > 0), "check_runs entries need a name and a positive integer id");
      require(data.check_runs.every((run, index) => index === 0 || data.check_runs[index - 1].name < run.name), "check_runs must be sorted by name with one entry per name");
      require(data.latest_check_run_id === undefined && data.latest_completed_at === undefined, "checks payload still carries a scalar ordering field");
      require(Number.isInteger(data.generation) && data.generation >= 0, "checks payload lacks a non-negative integer generation");
      require(typeof data.snapshot === "string" && data.snapshot.length > 0, "checks payload lacks a snapshot");
      const forSHA = checksBySHA.get(data.sha) ?? [];
      forSHA.push(data);
      checksBySHA.set(data.sha, forSHA);
    }
    require(new Set(checks.map((item) => payload(item).snapshot)).size === checks.length, "checks settlements must carry distinct snapshots");
    const firstSHA = "1111111111111111111111111111111111111111";
    const secondSHA = "2222222222222222222222222222222222222222";
    require(checksBySHA.get(firstSHA)?.length === 1, "first head must settle exactly once");
    require(checksBySHA.get(secondSHA)?.length === 3, "second head must settle once then re-settle twice");
    // Consumers order settlements of one commit by the attempt set: per-name ids
    // never decrease, and at an equal set the generation must not decrease.
    const compareSets = (previous, next) => {
      let advanced = false;
      for (const [name, id] of next) {
        const before = previous.get(name);
        if (before === undefined || id > before) advanced = true;
        else if (id < before) return "regressed";
      }
      return advanced ? "advanced" : "equal";
    };
    for (const [sha, values] of checksBySHA) {
      require(values.length === 1 || values.slice(1).every((value) => value.superseded_settlement === "true"), `duplicate checks for ${sha} lack superseded_settlement`);
      for (let index = 1; index < values.length; index += 1) {
        const order = compareSets(attemptSet(values[index - 1]), attemptSet(values[index]));
        require(order !== "regressed", `checks attempt set for ${sha} regressed at settlement ${index}`);
        require(order === "advanced" || values[index].generation > values[index - 1].generation, `checks generation for ${sha} did not advance at an equal attempt set`);
      }
    }
    // The superseding build attempt: exact sets before and after. The set
    // advances (build 2002 -> 2004) while integration 2005 keeps the highest
    // id and the latest completion on the head falls (03:17 -> 03:16); the verdict
    // turns red on the new attempt.
    const secondHead = checksBySHA.get(secondSHA);
    const pairs = (data) => data.check_runs.map((run) => `${run.name}=${run.id}`).join(",");
    require(pairs(secondHead[1]) === "build=2002,integration=2005", `settlement before supersession = ${pairs(secondHead[1])}`);
    require(pairs(secondHead[2]) === "build=2004,integration=2005", `settlement after supersession = ${pairs(secondHead[2])}`);
    require(compareSets(attemptSet(secondHead[1]), attemptSet(secondHead[2])) === "advanced", "superseding attempt did not advance the attempt set");
    require(secondHead[2].failed.checks.join(",") === "build", `superseding settlement failed = ${JSON.stringify(secondHead[2].failed)}`);
    require(secondHead[1].cancelled.checks.join(",") === "integration" && secondHead[2].cancelled.checks.join(",") === "integration", "integration must stay cancelled across the supersession");
    require(checksBySHA.get(secondSHA)?.filter((value) => value.superseded_settlement === "true").length === 2, "second head re-settlement flags are missing");

    const workflow = withTopic(topic("workflow.ci_yml.completed"));
    require(workflow.length === 1 && payload(workflow[0]).run_id === "3002", "only the PR-less workflow run may publish");
    // A finished Actions job of that PR-less run on main, the default branch, publishes on the job
    // topic of the run; the same job of a merge queue commit (run 3003) publishes nothing, and the
    // four check runs above list pull request 42 and publish only through its checks settlement.
    const jobs = envelopes.filter((item) => item.topic.startsWith(topic("job.")));
    require(jobs.length === 1 && jobs[0].topic === topic("job.3002.completed"), `job topics = ${jobs.map((item) => item.topic).join(",")}`);
    const job = payload(jobs[0]);
    require(job.kind === "job" && job.repo === "example-org/example-repo" && job.run_id === "3002" && job.job_id === "3101" && job.name === "deploy / production" && job.conclusion === "success" && job.completed_at === "2026-09-07T03:24:00Z", `job payload = ${jobs[0].payload}`);
    require(jobs[0].payload_summary === "job deploy / production run 3002 completed/success", `job summary = ${jobs[0].payload_summary}`);
    require(withTopic(topic("push.branch.main")).length === 1, "main push topic is missing");

    const obsolete = ["pr.42.check", "pr.42.ci", "pr.42.merged", "pr.42.closed"];
    for (const suffix of obsolete) require(withTopic(topic(suffix)).length === 0, `obsolete topic was published: ${suffix}`);

    const tsFirst = readFileSync(renderedTSFile, "utf8").split("\n\nenvoy:")[0];
    const goFirst = readFileSync(renderedGoFile, "utf8").split("\n[NOTIFICATION")[0];
    // The direct summary is the message head, so both renderers drop the separate summary line
    // and print the full message once: the summary text appears only where the message opens.
    const assertDirectRender = (rendered, fullMessage, renderer) => {
      require(rendered.split(fullMessage).length === 2, `${renderer} did not render the full direct message exactly once`);
      require(rendered.split(directSummary).length === 2, `${renderer} repeats the direct summary outside the message it opens`);
    };
    assertDirectRender(tsFirst, `message: ${JSON.stringify(directBody)}`, "TypeScript");
    assertDirectRender(goFirst, `Message:\n${directBody}`, "Go");

    console.log("PASS: topic set, structured payloads, checks settlement, and both renderers match the local acceptance contract");
  '

# Last of all, because it ends this rig's NATS and every step above needs the bus. A send the bus
# cannot serve has to be refused by name inside the sender's own attempt, rather than left to the
# sender's timeout: a timeout says nothing about whether the message was taken.
printf 'stopping NATS for the bus-down answers\n'
stop_nats

# --max-time 5 is the sending client's own per-attempt deadline (DEFAULT_TIMEOUT_MS in
# packages/envoy-client/src/transport.ts): an answer that arrives after it is, to that caller, no
# answer at all. The listener's own budget (sendRequestBudget, 3.5 s) is what keeps it inside.
printf 'posting a direct message with the bus down\n'
curl --max-time 5 -sS -o "$bus_down_response_file" -D "$bus_down_headers_file" \
  -w '%{http_code}\n%{time_total}\n' \
  -X POST -H 'Content-Type: application/json' \
  "${listener_url}/v1/messages/send" -d "$direct_body" >"$bus_down_result_file"
{ IFS= read -r bus_down_status; IFS= read -r bus_down_seconds; } <"$bus_down_result_file"
if [[ "$bus_down_status" != 503 ]]; then
  printf 'ERR: a send with the bus down answered HTTP %s, not 503.\n' "$bus_down_status" >&2
  cat "$bus_down_response_file" >&2
  exit 1
fi
jq -e '.error | startswith("envoy: the message bus did not answer")' "$bus_down_response_file" >/dev/null || {
  printf 'ERR: the bus-down body does not name the bus as the reason.\n' >&2
  cat "$bus_down_response_file" >&2
  exit 1
}
# Retry-After is what separates a bus that did not answer from bytes or a subject NATS refuses
# however often they are sent: only the first is worth sending down this same route again.
grep -qiE '^retry-after:[[:space:]]*1[[:space:]]*$' "$bus_down_headers_file" || {
  printf 'ERR: the bus-down answer carries no Retry-After: 1 header.\n' >&2
  cat "$bus_down_headers_file" >&2
  exit 1
}
# Whole seconds only: curl prints time_total with a fraction (behind whichever separator the
# locale asks for), and all that is asserted is that the answer beat the client deadline.
if ((${bus_down_seconds%%[.,]*} >= 5)); then
  printf 'ERR: the bus-down answer took %s seconds, past the sending client deadline.\n' "$bus_down_seconds" >&2
  exit 1
fi
printf 'bus-down send: HTTP %s in %s seconds\n' "$bus_down_status" "$bus_down_seconds"

# The same refusal through the client a session actually sends with. It retries a >= 500 answer
# once, after 250 ms (packages/envoy-client/src/transport.ts), so two refusals and one wait are
# expected here; what has to hold is that the send ends in a named EnvoyApiError rather than in a
# timeout, which is why this asserts what was thrown and never how long it took.
printf 'sending through the real client with the bus down\n'
(
  cd "$repo_root/packages/envoy-client"
  E2E_LISTENER_URL="$listener_url" E2E_SESSION_ID="$session_id" \
    E2E_BUS_DOWN_CLIENT_FILE="$bus_down_client_file" bun -e '
      import { writeFileSync } from "node:fs";
      import { createEnvoyClient, EnvoyApiError } from "@legion/envoy-client/transport";

      const baseUrl = process.env.E2E_LISTENER_URL;
      const sessionID = process.env.E2E_SESSION_ID;
      const output = process.env.E2E_BUS_DOWN_CLIENT_FILE;
      if ([baseUrl, sessionID, output].some((value) => value === undefined)) {
        throw new Error("bus-down client configuration is invalid");
      }
      const client = createEnvoyClient({ baseUrl, fetch });
      let thrown;
      try {
        await client.send({
          sourceSessionID: "octocat",
          targetSessionID: sessionID,
          message: "Direct send while the message bus is down.",
        });
      } catch (error) {
        thrown = error;
      }
      if (!(thrown instanceof EnvoyApiError)) {
        const what = thrown === undefined ? "nothing: the send was accepted" : String(thrown);
        throw new Error(`the client threw ${what}, want an EnvoyApiError`);
      }
      const prefix = "envoy: the message bus did not answer";
      if (thrown.details.status !== 503 || !thrown.message.startsWith(prefix)) {
        throw new Error(`the client threw ${thrown.details.status}: ${thrown.message}, want 503 opening with ${JSON.stringify(prefix)}`);
      }
      const record = { name: thrown.name, status: thrown.details.status, message: thrown.message };
      writeFileSync(output, `${JSON.stringify(record, null, 2)}\n`);
      console.log(`client refusal: ${record.name} ${record.status} ${record.message}`);
    '
)

printf 'PASS: an absent destination, the listener request records and a bus that did not answer are each refused by name\n'
