#!/usr/bin/env bash
# Proves, on a booted server, that the real envoy-dispatch binary answers a question over HTTP and
# streams it: it builds ./cmd/dispatch, starts it against DATABASE_URL with DISPATCH_ANSWERER=endpoint
# pointed at a fake chat-completions model this script starts itself, seeds a project and an issue
# through the API, asks a question of that issue over POST /api/v1/going/requests, and prints every
# frame of GET /api/v1/going/requests/{id}/stream as `<elapsed ms> <event> <data>`, so two builds'
# runs diff and the first text frame's arrival is a number on its own line.
#
# The model is a fake and no cloud model is ever called. It serves TLS, because
# DISPATCH_CHAT_ENDPOINT is an absolute https URL with a host and no query or fragment
# (cmd/dispatch/answerer.go, chatEndpoint): the script makes a self-signed certificate for
# 127.0.0.1 with the IP in a subject alternative name, serves it from a small python3 server, and
# gives the server SSL_CERT_FILE, which Go's crypto/x509 reads. The fake refuses any call that does
# not present the bearer DISPATCH_CHAT_TOKEN_FILE holds, so a run that reports OK is one whose
# token file reached the model.
#
# The server runs with a throwaway HOME, a made-up agent token and a trusted identity header, so it
# reads no operator configuration and holds no credential; NATS is off. DATABASE_URL names the
# server: the run makes a database of its own on it and drops it at the end, both because the
# binary migrates whatever database it is given and because the pod's own test server
# (LEGION_TEST_PG_DSN) hands its role no table of the database that DSN names.
set -euo pipefail

usage() {
  printf 'usage: DATABASE_URL=<postgres server> %s [--binary <envoy-dispatch>] [--model-status <code>] [--no-model]\n' "$0" >&2
  exit 2
}

binary=""
model_status=0
no_model=0
while (($# > 0)); do
  case "$1" in
    --binary) binary="${2:?}"; shift 2 ;;
    --model-status) model_status="${2:?}"; shift 2 ;;
    --no-model) no_model=1; shift ;;
    *) usage ;;
  esac
done
[[ "$model_status" =~ ^[0-9]+$ ]] || usage
: "${DATABASE_URL:?DATABASE_URL must name a Postgres server a test database may be made on}"
[[ "$DATABASE_URL" =~ ^(postgres(ql)?://[^/]+)/([^?]*)(\?.*)?$ ]] ||
  { printf 'DATABASE_URL is not a postgres:// URL with a database path\n' >&2; exit 2; }
readonly pg_server="${BASH_REMATCH[1]}" pg_params="${BASH_REMATCH[4]}"
for command in curl jq python3 openssl psql; do
  command -v "$command" >/dev/null || { printf '%s is required\n' "$command" >&2; exit 1; }
done

workdir="$(mktemp -d)"
server_pid=""
model_pid=""
database=""
# shellcheck disable=SC2329 # cleanup runs through the EXIT trap below
cleanup() {
  # The run's own status, kept and handed back: waiting on a process this trap killed answers 143
  # (SIGTERM), and under errexit that would end the trap and make 143 the status of a run that
  # passed, so each step of the stopping is allowed to fail.
  local status=$?
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ -n "$model_pid" ]]; then
    kill "$model_pid" 2>/dev/null || true
    wait "$model_pid" 2>/dev/null || true
  fi
  if [[ -n "$database" ]]; then
    psql "$DATABASE_URL" -qtA -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      printf 'going-smoke: the database %s could not be dropped\n' "$database" >&2
  fi
  rm -rf "$workdir"
  exit "$status"
}
trap cleanup EXIT

database="going_smoke_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
psql "$DATABASE_URL" -qtA -c "create database \"$database\"" >/dev/null ||
  { printf 'the database %s could not be made on the server DATABASE_URL names\n' "$database" >&2; exit 1; }
readonly database_url="${pg_server}/${database}${pg_params}"

# refuse ends the run naming what was missing, with both logs, since either side may hold why. A
# log is printed on stderr (>&2 first, so it duplicates the real stderr) and its own absence is
# not: a refusal before either process started has nothing to print and still names its reason.
refuse() {
  local reason
  for reason in "$@"; do
    printf 'going-smoke: %s\n' "$reason" >&2
  done
  printf '\n--- the fake model log ---\n' >&2
  cat "${workdir}/model.log" >&2 2>/dev/null || true
  printf '\n--- the server log ---\n' >&2
  cat "${workdir}/server.log" >&2 2>/dev/null || true
  exit 1
}

if [[ -z "$binary" ]]; then
  binary="${workdir}/envoy-dispatch"
  (cd "$(dirname "$0")/.." && go build -o "$binary" ./cmd/dispatch)
fi
root="$(cd "$(dirname "$0")/../../.." && pwd)"
readonly root
# shellcheck source-path=SCRIPTDIR source=../../../scripts/e2e/lib/port-file.sh
. "${root}/scripts/e2e/lib/port-file.sh"

# The fake model serves TLS, so the endpoint can be the https URL the settings require. The
# certificate is this run's own, in this run's own directory, and carries 127.0.0.1 as a subject
# alternative name, which is what Go checks a served certificate against.
readonly certificate="${workdir}/model-cert.pem"
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
  -keyout "${workdir}/model-key.pem" -out "$certificate" \
  -subj '/CN=127.0.0.1' -addext 'subjectAltName=IP:127.0.0.1' >"${workdir}/openssl.log" 2>&1 || {
  printf 'the self-signed certificate could not be made:\n' >&2
  cat "${workdir}/openssl.log" >&2
  exit 1
}

cat >"${workdir}/model.py" <<'PY'
"""The fake chat-completions model going-smoke.sh answers with: it serves TLS on a port the kernel
chose, answers POST /chat/completions with a streamed completion whose text links the issue the
server named in the items it was given, and refuses any call that does not present the bearer the
script's token file holds, which is how the run proves the token file reached the call."""

import argparse
import http.server
import json
import os
import ssl
import time

parser = argparse.ArgumentParser()
parser.add_argument("--port-file", required=True)
parser.add_argument("--certificate", required=True)
parser.add_argument("--key", required=True)
parser.add_argument("--token", required=True)
parser.add_argument("--refusal", required=True)
parser.add_argument("--status", type=int, default=0)
parser.add_argument("--chunk", type=int, default=18)
parser.add_argument("--pause", type=float, default=0.12)
args = parser.parse_args()


def say(message):
    print("going-smoke model: " + message, flush=True)


def refuse(message):
    """Say what the model refused and leave it where the script reads it after the stream."""
    say("REFUSED " + message)
    with open(args.refusal, "a", encoding="utf-8") as out:
        out.write(message + "\n")


def items_of(messages):
    """The items the server named to the model, as the answerer renders them (going/answerer.go:
    writeItem)."""
    text = "\n".join(message.get("content") or "" for message in messages if message.get("role") == "user")
    found, block, in_body = [], None, False
    for line in text.splitlines():
        if line == "--- item":
            block, in_body = {"body": []}, False
            found.append(block)
            continue
        if block is None:
            continue
        if line == "body:":
            in_body = True
            continue
        if in_body:
            block["body"].append(line)
            continue
        for field in ("kind", "id", "title", "href"):
            if line.startswith(field + ": ") and field not in block:
                block[field] = line[len(field) + 2:]
    return found


def answer_text(item):
    """A short answer that links the item by the href the server gave it, and quotes its own text:
    the first sentence of its body, which is neither its title nor the heading that repeats it."""
    quoted = ""
    for line in item["body"]:
        stripped = line.strip().lstrip("#").strip()
        if len(stripped) > 30 and stripped != item.get("title"):
            sentence, stop, _ = stripped.partition(". ")
            quoted = (sentence + "." if stop else sentence)[:200]
            break
    answer = "Dispatch holds one issue on this: [%s](%s)." % (item["title"], item["href"])
    if quoted:
        answer += " Its own text says: %s" % quoted
    return answer


class Model(http.server.BaseHTTPRequestHandler):
    def log_message(self, template, *values):
        say(template % values)

    def answer(self, code, error):
        body = json.dumps({"error": {"code": error, "message": "the fake model refused this call"}}).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def frame(self, payload):
        self.wfile.write(b"data: " + json.dumps(payload, separators=(",", ":")).encode() + b"\n\n")
        self.wfile.flush()

    def stream(self, text):
        """The completion, in several frames with a pause between them, then [DONE]."""
        say("streaming %d characters in frames of %d, %.0f ms apart" % (len(text), args.chunk, args.pause * 1000))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for start in range(0, len(text), args.chunk):
            self.frame({"choices": [{"delta": {"content": text[start:start + args.chunk]}}]})
            time.sleep(args.pause)
        self.frame({"choices": [{"delta": {}, "finish_reason": "stop"}]})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def do_GET(self):
        self.answer(404, "no_such_route")

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        if self.path != "/chat/completions":
            refuse("a call to %s, which is not /chat/completions" % self.path)
            return self.answer(404, "no_such_route")
        presented = self.headers.get("Authorization", "")
        token = presented[len("Bearer "):] if presented.startswith("Bearer ") else ""
        if not token:
            refuse("a call carrying no Authorization: Bearer header")
            return self.answer(401, "no_bearer")
        if token != args.token:
            refuse("a call whose bearer is not the one DISPATCH_CHAT_TOKEN_FILE holds")
            return self.answer(401, "wrong_bearer")
        say("a call of model %s, presenting the token file's bearer" % body.get("model"))
        if args.status:
            say("answering %d, as --model-status asked" % args.status)
            return self.answer(args.status, "simulated_failure")
        items = items_of(body.get("messages") or [])
        issues = [item for item in items if item.get("kind") == "issue"]
        if not issues:
            say("the items this call carried: %s" % [(item.get("kind"), item.get("id")) for item in items])
            refuse("a call whose items named no issue to cite")
            return self.answer(502, "no_issue_item")
        say("citing %s by the href the server gave it: %s" % (issues[0].get("id"), issues[0].get("href")))
        self.stream(answer_text(issues[0]))


context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(args.certificate, args.key)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Model)
server.socket = context.wrap_socket(server.socket, server_side=True)
port = server.server_address[1]
with open(args.port_file + ".tmp", "w", encoding="ascii") as out:
    out.write("%d\n" % port)
os.rename(args.port_file + ".tmp", args.port_file)
say("serving TLS at https://127.0.0.1:%d" % port)
server.serve_forever()
PY

cat >"${workdir}/stream.py" <<'PY'
"""Prints every frame of one answer stream as `<elapsed ms> <event> <data>` as it arrives, then the
first text frame's elapsed time on its own line, and leaves what the stream came to in the JSON
file named by its one argument, which the script's refusals read."""

import json
import sys
import time

summary_path = sys.argv[1]
start = time.monotonic()
counts = {name: 0 for name in ("text", "item", "cut", "changed", "none", "end", "error")}
summary = {
    "frames": 0,
    "counts": counts,
    "first_text_ms": None,
    "terminal": None,
    "terminal_ms": None,
    "last_ms": 0,
    "items": [],
    "error_code": None,
}
event = ""
for line in iter(sys.stdin.readline, ""):
    line = line.rstrip("\n")
    if line.startswith("event:"):
        event = line[len("event:"):].strip()
        continue
    if not line.startswith("data:"):
        continue
    data = line[len("data:"):].strip()
    elapsed = int((time.monotonic() - start) * 1000)
    print("%d %s %s" % (elapsed, event, data), flush=True)
    summary["frames"] += 1
    summary["last_ms"] = elapsed
    counts[event] = counts.get(event, 0) + 1
    if event == "text" and summary["first_text_ms"] is None:
        summary["first_text_ms"] = elapsed
    elif event == "item":
        summary["items"].append(json.loads(data))
    elif event == "error":
        summary["error_code"] = json.loads(data).get("code")
    if event in ("end", "error"):
        summary["terminal"] = event
        summary["terminal_ms"] = elapsed
with open(summary_path, "w", encoding="utf-8") as out:
    json.dump(summary, out)
if summary["first_text_ms"] is None:
    print("first text frame: none")
else:
    print("first text frame: %d ms" % summary["first_text_ms"])
PY

readonly model_token="going-smoke-not-a-credential-${RANDOM}"
printf '%s\n' "$model_token" >"${workdir}/chat-token"
python3 "${workdir}/model.py" \
  --port-file "${workdir}/model-port" \
  --certificate "$certificate" --key "${workdir}/model-key.pem" \
  --token "$model_token" --refusal "${workdir}/model-refusal" --status "$model_status" \
  >"${workdir}/model.log" 2>&1 &
model_pid=$!

model_port=""
model_waited=0
read_port_file "${workdir}/model-port" "$model_pid" model_port || model_waited=$?
case $model_waited in
  0) ;;
  1)
    printf 'the fake model exited before it served; its log:\n' >&2
    cat "${workdir}/model.log" >&2
    exit 1
    ;;
  *) printf 'the fake model did not serve within %d seconds\n' "$port_file_seconds" >&2; exit 1 ;;
esac
readonly model_origin="https://127.0.0.1:${model_port}"
printf 'going-smoke: the fake model serves %s\n' "$model_origin"

# held-port.py opens 127.0.0.1:0 and hands the socket to the server, so the port the kernel chose
# stays held until the server serves on it; it then runs as the server, so $! is the server's PID.
mkdir "${workdir}/home"
settings=(
  "HOME=${workdir}/home"
  "DATABASE_URL=${database_url}"
  # Go's crypto/x509 reads SSL_CERT_FILE for its roots, which is how the server trusts this run's
  # own certificate without any setting of Dispatch's own.
  "SSL_CERT_FILE=${certificate}"
  DISPATCH_AGENT_TOKEN=going-smoke-not-a-credential
  DISPATCH_IDENTITY=header:X-Dispatch-User
  DISPATCH_IDENTITY_HEADER_TRUSTED=1
  DISPATCH_NATS_DISABLED=1
  DISPATCH_LISTEN_HOST=127.0.0.1
)
if ((no_model == 0)); then
  settings+=(
    DISPATCH_ANSWERER=endpoint
    "DISPATCH_CHAT_ENDPOINT=${model_origin}"
    DISPATCH_CHAT_MODEL=going-smoke-fake
    "DISPATCH_CHAT_TOKEN_FILE=${workdir}/chat-token"
  )
fi
env "${settings[@]}" \
  python3 "${root}/scripts/e2e/lib/held-port.py" exec --port-file "${workdir}/port" \
    --fd-env DISPATCH_LISTEN_FD \
    --env 'DISPATCH_PORT={port}' --env 'DISPATCH_SERVER_URL=http://127.0.0.1:{port}' \
    -- "$binary" >"${workdir}/server.log" 2>&1 &
server_pid=$!

port=""
waited=0
read_port_file "${workdir}/port" "$server_pid" port || waited=$?
case $waited in
  0) ;;
  1)
    printf 'the server exited before its port was held; its log:\n' >&2
    cat "${workdir}/server.log" >&2
    exit 1
    ;;
  *) printf 'no port was held for the server within %d seconds\n' "$port_file_seconds" >&2; exit 1 ;;
esac
readonly base="http://127.0.0.1:${port}"

# The held socket queues a request until the server serves it, so each poll is bounded.
for _ in $(seq 1 300); do
  if curl -fsS --max-time 2 "${base}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    printf 'the server exited; its log:\n' >&2
    cat "${workdir}/server.log" >&2
    exit 1
  fi
  sleep 1
done
curl -fsS "${base}/healthz" >/dev/null
printf 'going-smoke: the server serves %s\n' "$base"

readonly person="going-smoke@example.test"
api() {
  curl -fsS -H "X-Dispatch-User: ${person}" -H 'Content-Type: application/json' "$@"
}

capability="$(api "${base}/api/v1/going/capability" | tr -d '[:space:]')" ||
  refuse 'the capability route could not be read'
[[ "$capability" == '{"model_configured":true}' ]] ||
  refuse "the capability route answered ${capability}, want {\"model_configured\":true}"
printf 'going-smoke: capability %s\n' "$capability"

# The seed is one project and one issue whose own text is worth citing, written through the API as
# the identity header's person, which is who asks the question below.
readonly project="GOING${RANDOM}"
readonly title="The pager handover at the end of the night watch"
readonly spec="# The pager handover

The night watch hands the pager to the morning shift before it leaves, and writes one handover line naming every alarm it silenced.
A watch that leaves with the pager still in hand is the one failure this rule exists to prevent."
# Keyword search ands a query's own words (websearch_to_tsquery), and the issue row holds its
# title, so the question is asked in the title's words: that is what puts the issue itself among
# the hits the answerer shows the model, beside its spec.
readonly question="What is the pager handover at the end of the night watch?"

api -X POST "${base}/api/v1/projects" \
  -d "$(jq -nc --arg key "$project" '{key: $key, name: "Going smoke"}')" >/dev/null ||
  refuse "the project ${project} could not be seeded"
issue="$(api -X POST "${base}/api/v1/issues" \
  -d "$(jq -nc --arg project "$project" --arg title "$title" --arg spec "$spec" \
    '{project: $project, title: $title, spec: $spec}')")" ||
  refuse "the issue of ${project} could not be seeded"
issue_key="$(jq -er '.key' <<<"$issue")" || refuse "the seeded issue answered no key: ${issue}"
readonly issue_key
printf 'going-smoke: seeded %s %s\n' "$issue_key" "$title"

run="$(api -X POST "${base}/api/v1/going/requests" \
  -d "$(jq -nc --arg words "$question" --arg project "$project" '{words: $words, project: $project}')")" ||
  refuse 'the question could not be asked'
run_id="$(jq -er '.id' <<<"$run")" || refuse "the run answered no id: ${run}"
printf 'going-smoke: run %s ends_at %s\n' "$run_id" "$(jq -er '.ends_at' <<<"$run")"

# Every frame as it arrives, so two builds' runs diff.
if ! curl -fsS -N -H "X-Dispatch-User: ${person}" -H 'Accept: text/event-stream' \
  "${base}/api/v1/going/requests/${run_id}/stream" | python3 "${workdir}/stream.py" "${workdir}/stream.json"; then
  refuse "the stream of run ${run_id} could not be read"
fi

if [[ -s "${workdir}/model-refusal" ]]; then
  refuse "the fake model refused a call, so the server did not call it as it must: $(tr '\n' ';' <"${workdir}/model-refusal")"
fi

summary="${workdir}/stream.json"
missing=()
texts="$(jq -r '.counts.text' "$summary")"
((texts >= 1)) || missing+=('the stream carried no text frame')
item_count="$(jq -r '.counts.item' "$summary")"
cited="$(jq -r --arg href "/issues/${issue_key}" '[.items[] | select(.href == $href)] | length' "$summary")"
if ((item_count != 1)) || ((cited != 1)); then
  missing+=("the stream carried ${item_count} item frame(s), ${cited} of them naming ${issue_key}, want exactly one naming it")
fi
terminals="$(jq -r '.counts.end + .counts.error' "$summary")"
if ((terminals != 1)); then
  missing+=("the stream carried ${terminals} terminal frame(s), want exactly one")
fi
if [[ "$(jq -r '.first_text_ms == null or .terminal_ms == null or .first_text_ms >= .terminal_ms' "$summary")" == true ]]; then
  missing+=('no text frame arrived before the stream ended')
fi
if ((${#missing[@]} > 0)); then
  missing+=("the stream ended $(jq -r '"\(.terminal // "with no terminal frame") \(.error_code // "")"' "$summary")")
  refuse "${missing[@]}"
fi

printf 'going-smoke: OK\n'
