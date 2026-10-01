#!/usr/bin/env bash
# listener-deploy-probe.test.sh — Proves listener-deploy-probe.sh's per-tick lines, summary and exit
# code over two fake listener tasks: Python http.servers bound to 127.0.0.1 and 127.0.0.2 on one
# port, and a fake getent on PATH that resolves probe.test to both, three ticks a run. The name
# itself resolves nowhere for curl, so every run also lists probe.test as unreached, which names it
# without failing the run.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/listener-deploy-probe.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly output_file="${temporary_dir}/output"
readonly server="${temporary_dir}/listener.py"
readonly dispatch_server="${temporary_dir}/dispatch.py"
mkdir -p "$fake_bin_dir"

server_pids=()
stop_servers() {
  local pid
  for pid in "${server_pids[@]}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  server_pids=()
}
dispatch_pid=""
cleanup() {
  stop_servers
  [[ -z "$dispatch_pid" ]] || kill "$dispatch_pid" 2>/dev/null || true
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

command -v python3 >/dev/null || {
  printf 'ERR: python3 is required\n' >&2
  exit 4
}

cat >"${fake_bin_dir}/getent" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == ahostsv4 && "$2" == probe.test ]] || exit 2
printf '%s\n' '127.0.0.1       STREAM probe.test' '127.0.0.1       DGRAM' '127.0.0.1       RAW' \
  '127.0.0.2       STREAM' '127.0.0.2       DGRAM' '127.0.0.2       RAW'
EOF
chmod +x "${fake_bin_dir}/getent"

# The fake task answers /healthz and GET /v1/sessions as its mode says: ok (both 200), starting
# (main's shape while the durable is held: /healthz 200 "starting", /v1 503 "service starting" at
# every tick), flap (/v1 200, then 503 at its second request, then 200), unauthorized (/v1 401),
# exiting (/v1 200, 200, then the connection dropped with no answer, as a task that stops between a
# tick's two requests leaves it), wedged (/v1 always dropped with no answer). Any /v1 request
# without the probe's bearer is 401 too, so a pass also proves the bearer was sent.
cat >"$server" <<'EOF'
import http.server
import json
import sys

address, port, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
v1_requests = 0


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, code, body):
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        global v1_requests
        if self.path == "/healthz":
            return self.answer(200, {"status": "starting" if mode == "starting" else "healthy"})
        if self.path == "/v1/sessions":
            v1_requests += 1
            if mode == "unauthorized" or self.headers.get("Authorization") != "Bearer probe-token":
                return self.answer(401, {"error": "unauthorized"})
            if mode == "wedged" or (mode == "exiting" and v1_requests == 3):
                self.close_connection = True
                return None
            if mode == "starting" or (mode == "flap" and v1_requests == 2):
                return self.answer(503, {"error": "service starting"})
            return self.answer(200, [])
        return self.answer(404, {"error": "not found"})


http.server.HTTPServer((address, port), Handler).serve_forever()
EOF

port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
readonly port

# start_server ADDRESS MODE starts a fake task on ADDRESS:$port and waits until it answers; the mode
# none starts nothing, so the address refuses every connection.
start_server() {
  local address="$1" mode="$2" attempt
  [[ "$mode" != none ]] || return 0
  python3 "$server" "$address" "$port" "$mode" &
  server_pids+=("$!")
  for attempt in $(seq 50); do
    curl -s -o /dev/null "http://${address}:${port}/healthz" && return 0
    [[ "$attempt" -lt 50 ]] || break
    sleep 0.1
  done
  printf 'FAIL: the fake task on %s:%s never answered\n' "$address" "$port" >&2
  exit 1
}

status=0
# run_case FIRST_MODE SECOND_MODE [PROBE_OPTION...] runs the probe for three ticks against tasks on
# 127.0.0.1 and 127.0.0.2 in those modes, leaving its output in $output_file and its exit code in
# $status.
run_case() {
  local first="$1" second="$2"
  shift 2
  stop_servers
  start_server 127.0.0.1 "$first"
  start_server 127.0.0.2 "$second"
  status=0
  env -u ENVOY_TOKEN_FILE PATH="${fake_bin_dir}:${PATH}" ENVOY_TOKEN=probe-token \
    bash "$driver" --url "http://probe.test:${port}" --interval 1 --duration 3 "$@" >"$output_file" 2>&1 || status=$?
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  cat "$output_file" >&2
  exit 1
}

expect_status() {
  [[ "$status" == "$1" ]] || fail "$2: exit $status, want $1"
}

expect_line() {
  grep -Fqx -- "$1" "$output_file" || fail "$2: no line $(printf '%q' "$1")"
}

expect_text() {
  grep -Fq -- "$1" "$output_file" || fail "$2: no $(printf '%q' "$1")"
}

# (a) Negative control: main's shape. The replacement answers /healthz "starting" and /v1 503 at
# every tick while the old task serves both.
run_case ok starting
expect_status 1 "a task refusing /v1 at every tick"
[[ "$(grep -c $'\t127.0.0.2\t200\tstarting\t503\tservice starting\t-\t-$' "$output_file")" == 3 ]] ||
  fail "a task refusing /v1 at every tick: want three per-tick lines for 127.0.0.2 with 200 starting 503 service starting"
expect_text 'target 127.0.0.2: fail - ' "a task refusing /v1 at every tick"
expect_line '  3 x 503:service starting' "a task refusing /v1 at every tick"
expect_text 'target 127.0.0.1: pass - ' "a task refusing /v1 at every tick"
expect_text '200 at 3; /v1 non-200 at 0 of those' "a task refusing /v1 at every tick"
expect_text 'target probe.test: unreached - ' "a task refusing /v1 at every tick"
printf 'PASS: a task that refuses /v1 while it answers /healthz fails the run (exit 1)\n'

# (b) Both tasks serve /v1 at every tick.
run_case ok ok
expect_status 0 "two tasks serving /v1"
expect_text 'target 127.0.0.1: pass - ' "two tasks serving /v1"
expect_text 'target 127.0.0.2: pass - ' "two tasks serving /v1"
expect_text 'verdict: pass - 2 target(s)' "two tasks serving /v1"
printf 'PASS: two tasks serving /v1 at every tick pass the run (exit 0)\n'

# (c) A flap mid-deploy: /v1 answers 200, 503, 200.
run_case ok flap
expect_status 1 "a task whose /v1 flaps"
expect_text 'target 127.0.0.2: fail - ' "a task whose /v1 flaps"
expect_line '  1 x 503:service starting' "a task whose /v1 flaps"
printf 'PASS: a task whose /v1 refuses one tick fails the run (exit 1)\n'

# (d) Every /v1 answer is 401: the bearer is wrong, not the listener.
run_case unauthorized unauthorized
expect_status 2 "every /v1 answer 401"
expect_text 'verdict: misconfigured - every /v1 answer was 401 or 403' "every /v1 answer 401"
printf 'PASS: a run where every /v1 answer is 401 is a misconfiguration (exit 2)\n'

# (e) The second address refuses every connection, as a stale A record does during the handover.
run_case ok none
expect_status 0 "an unreached address"
expect_text 'target 127.0.0.2: unreached - ' "an unreached address"
expect_text 'target 127.0.0.1: pass - ' "an unreached address"
printf 'PASS: an address that never answers /healthz is named unreached and fails nothing (exit 0)\n'

# (f) The old task stops between a tick's /healthz and its /v1: the /v1 request draws no answer.
# That is the task leaving, not a refusal, and it served /v1 at every other tick.
run_case exiting ok
expect_status 0 "a task that stops mid-tick"
expect_text 'target 127.0.0.1: pass - ' "a task that stops mid-tick"
expect_text '  1 tick(s) where /v1 drew no answer right after /healthz answered 200' "a task that stops mid-tick"
printf 'PASS: a task that stops between a tick'"'"'s two requests is shown, not failed (exit 0)\n'

# (g) A task whose /v1 never answers at all while /healthz does still fails.
run_case wedged ok
expect_status 1 "a task whose /v1 never answers"
expect_text 'target 127.0.0.1: fail - ' "a task whose /v1 never answers"
expect_text '; /v1 never answered 200' "a task whose /v1 never answers"
printf 'PASS: a task whose /v1 never answers while /healthz does fails the run (exit 1)\n'

# (h) Dispatch messages during the run: a fake Dispatch that takes only the request Dispatch's
# POST /api/v1/issues/<KEY>/messages takes from a bearer (the session target, the mode, the bearer's
# session actor) answers the first message with a failed delivery attempt and the second with a
# sent one, in Dispatch's own answer shape.
cat >"$dispatch_server" <<'EOF'
import http.server
import json
import sys

port = int(sys.argv[1])
posts = 0


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, code, body):
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        return self.answer(200, {"status": "ok"})

    def do_POST(self):
        global posts
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path != "/api/v1/issues/LEGION-1/messages" or self.headers.get("Authorization") != "Bearer dispatch-token":
            return self.answer(401, {"code": "UNAUTHORIZED", "error": "unauthorized"})
        if body.get("target") != "session:ses_probe" or body.get("delivery") != "btw" or body.get("actor", {}).get("kind") != "session":
            return self.answer(400, {"code": "MESSAGE_INPUT", "error": "bad message"})
        posts += 1
        state, error = ("failed", "service starting") if posts == 1 else ("sent", None)
        delivery = {"message_id": "m%d" % posts, "attempt": 1, "delivery": "btw", "session_id": "ses_probe",
                    "envelope_id": None, "duplicate": False, "state": state, "error": error, "reply_id": None,
                    "created_at": "2026-10-01T00:00:00Z", "requested_by": body["actor"], "accepted_at": None,
                    "accepted_as": None}
        return self.answer(201, {"id": "m%d" % posts, "issue_key": "LEGION-1", "author": body["actor"],
                                 "body": body["body"], "target": body["target"], "in_reply_to": None,
                                 "broadcast_id": None, "created_at": "2026-10-01T00:00:00Z",
                                 "deliveries": [delivery], "advice": {"status": "in_progress"}})


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
EOF
dispatch_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
printf 'dispatch-token\n' >"${temporary_dir}/dispatch-token"
python3 "$dispatch_server" "$dispatch_port" &
dispatch_pid="$!"
for attempt in $(seq 50); do
  curl -s -o /dev/null "http://127.0.0.1:${dispatch_port}/" && break
  [[ "$attempt" -lt 50 ]] || fail "the fake Dispatch never answered"
  sleep 0.1
done
run_case ok ok --dispatch-url "http://127.0.0.1:${dispatch_port}" --dispatch-token-file "${temporary_dir}/dispatch-token" \
  --dispatch-issue LEGION-1 --dispatch-session ses_probe --dispatch-every 2
kill "$dispatch_pid" 2>/dev/null || true
wait "$dispatch_pid" 2>/dev/null || true
expect_status 0 "Dispatch messages during the run"
[[ "$(grep -c $'\tprobe.test\t000\t-\t000\t-\t-\tfailed:service starting$' "$output_file")" == 1 ]] ||
  fail "Dispatch messages during the run: want the first attempt's failed state on the name's line"
expect_text 'failed:service starting=1' "Dispatch messages during the run"
expect_text 'sent=1' "Dispatch messages during the run"
printf 'PASS: each Dispatch message records its delivery attempt state, on ticks 1 and 3 at every 2\n'

stop_servers
status=0
bash "$driver" >"$output_file" 2>&1 || status=$?
expect_status 2 "no --url"
expect_text 'ERR: --url is required' "no --url"
printf 'PASS: a run without --url is a usage error (exit 2)\n'

mkdir -p "${temporary_dir}/no-curl"
ln -s "$(command -v sed)" "${temporary_dir}/no-curl/sed"
status=0
PATH="${temporary_dir}/no-curl" "$BASH" "$driver" --url "http://probe.test:${port}" --targets 127.0.0.1 >"$output_file" 2>&1 || status=$?
expect_status 4 "no curl"
expect_text 'ERR: missing required command: curl' "no curl"
printf 'PASS: a host without curl is a missing tool (exit 4)\n'
