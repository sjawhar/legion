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
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: e2e-local.sh [--help]

Boot a throwaway NATS and the local Envoy listener, submit signed public GitHub
fixtures plus one direct message, and write raw and rendered acceptance evidence
to packages/envoy/out/e2e/.
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
readonly nats_port listener_port session_port
listener_url="http://127.0.0.1:${listener_port}"
readonly listener_url
nats_url="nats://127.0.0.1:${nats_port}"
readonly nats_url
webhook_secret="e2e-local"
readonly webhook_secret
session_id="ses_e2e_local"
readonly session_id

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

listener_pid=""
session_pid=""
subscriber_pid=""
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
  if [[ "$nats_started" -eq 1 ]]; then
    docker stop envoy-e2e-nats >/dev/null 2>&1 || true
    docker rm envoy-e2e-nats >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

for command in bun curl docker go jq openssl; do
  command -v "$command" >/dev/null || {
    printf 'ERR: required command is unavailable: %s\n' "$command" >&2
    exit 3
  }
done

if docker container inspect envoy-e2e-nats >/dev/null 2>&1; then
  printf 'ERR: envoy-e2e-nats already exists; leave it untouched and choose a free Docker daemon.\n' >&2
  exit 3
fi

mkdir -p "$out_dir"
rm -f "$envelopes_file" "$session_prompts_file" "$rendered_ts_file" "$rendered_go_file" \
  "$direct_response_file" "$unwired_response_file" "$unheld_role_response_file" "$role_claim_response_file" "$held_role_response_file" \
  "$listener_log_file" "$session_log_file" "$subscriber_log_file" "$listener_binary" "$session_ready_fifo" "$subscriber_ready_fifo"
: >"$envelopes_file"
: >"$session_prompts_file"
mkfifo "$session_ready_fifo" "$subscriber_ready_fifo"

printf 'starting NATS: envoy-e2e-nats on 127.0.0.1:%s\n' "$nats_port"
docker run --name envoy-e2e-nats -d -p "${nats_port}:4222" nats:2.10-alpine -js >/dev/null
nats_started=1

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

post_github pull_request "${fixture_dir}/pull-request-closed-merged.json"
post_github workflow_run "${fixture_dir}/workflow-run-with-pr.json"
post_github workflow_run "${fixture_dir}/workflow-run-without-pr.json"
post_github push "${fixture_dir}/push.json"
wait_for_prompt_count 12

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
    require(checks.length === 3, `checks count = ${checks.length}, want 3`);
    const checksBySHA = new Map();
    for (const item of checks) {
      const data = payload(item);
      require(data.kind === "checks", "checks envelope has wrong payload kind");
      require(Number.isInteger(data.latest_check_run_id) && data.latest_check_run_id > 0, "checks payload lacks a positive integer latest_check_run_id");
      const forSHA = checksBySHA.get(data.sha) ?? [];
      forSHA.push(data);
      checksBySHA.set(data.sha, forSHA);
    }
    const firstSHA = "1111111111111111111111111111111111111111";
    const secondSHA = "2222222222222222222222222222222222222222";
    require(checksBySHA.get(firstSHA)?.length === 1, "first head must settle exactly once");
    require(checksBySHA.get(secondSHA)?.length === 2, "second head must settle once then re-settle once");
    for (const [sha, values] of checksBySHA) {
      require(values.length === 1 || values.slice(1).every((value) => value.superseded_settlement === "true"), `duplicate checks for ${sha} lack superseded_settlement`);
      require(values.every((value, index) => index === 0 || value.latest_check_run_id >= values[index - 1].latest_check_run_id), `checks latest_check_run_id for ${sha} regressed`);
    }
    require(checksBySHA.get(secondSHA)?.filter((value) => value.superseded_settlement === "true").length === 1, "second head re-settlement flag is missing");

    const workflow = withTopic(topic("workflow.ci_yml.completed"));
    require(workflow.length === 1 && payload(workflow[0]).run_id === "3002", "only the PR-less workflow run may publish");
    require(withTopic(topic("push.branch.main")).length === 1, "main push topic is missing");

    const obsolete = ["pr.42.check", "pr.42.ci", "pr.42.merged", "pr.42.closed"];
    for (const suffix of obsolete) require(withTopic(topic(suffix)).length === 0, `obsolete topic was published: ${suffix}`);

    const tsFirst = readFileSync(renderedTSFile, "utf8").split("\n\nenvoy:")[0];
    const goFirst = readFileSync(renderedGoFile, "utf8").split("\n[NOTIFICATION")[0];
    const assertDirectRender = (rendered, fullMessage, renderer) => {
      const summaryAt = rendered.indexOf(directSummary);
      const bodyAt = rendered.indexOf(fullMessage);
      require(summaryAt >= 0 && summaryAt < bodyAt, `${renderer} direct summary is not first`);
      require(rendered.split(fullMessage).length === 2, `${renderer} did not render the full direct message exactly once`);
    };
    assertDirectRender(tsFirst, `message: ${JSON.stringify(directBody)}`, "TypeScript");
    assertDirectRender(goFirst, `Message:\n${directBody}`, "Go");

    console.log("PASS: topic set, structured payloads, checks settlement, and both renderers match the local acceptance contract");
  '
