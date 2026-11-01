#!/usr/bin/env bash
# listener-deploy-probe.test.sh — Proves listener-deploy-probe.sh's per-tick lines, summary and exit
# code over two fake listener tasks: one Python process per case that serves 127.0.0.1 and
# 127.0.0.2 on one port the kernel chose, and a fake getent on PATH that resolves probe.test to
# both, three ticks a run. The name itself resolves nowhere for curl, so every run also lists
# probe.test as unreached, which names it without failing the run.
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

# The fake tasks answer /healthz and GET /v1/sessions as each address's mode says: ok (both 200),
# starting (main's shape while the durable is held: /healthz 200 "starting", /v1 503 "service
# starting" at every tick), flap (/v1 200, then 503 at its second request, then 200), unauthorized
# (/v1 401), exiting (/v1 200 twice, then the task stops right after it answers the next /healthz,
# so that tick's /v1 finds nothing listening), unhealthy (/healthz 503 and /v1 503 at every tick).
# empty, reset and slow fail /v1 at its first two requests and answer 200 at the third: empty
# closes the connection without an answer and reset resets it, as Go's net/http does after a
# handler panics, and slow answers after 4 s, past the probe's 3 s. none binds the address without
# listening, so it refuses every connection while no other process can take the port. Any /v1
# request without the probe's bearer is 401 too, so a pass also proves the bearer was sent. Each
# address keeps its own mode and request count and serves in its own thread, so a task that exits
# closes only its own socket.
cat >"$server" <<'EOF'
import errno
import http.server
import json
import os
import socket
import struct
import sys
import threading
import time

port_file, modes = sys.argv[1], {"127.0.0.1": sys.argv[2], "127.0.0.2": sys.argv[3]}


def bind_pair():
    # The kernel chooses the port on 127.0.0.1, and 127.0.0.2 takes the same one. Another socket
    # may already hold that port on 127.0.0.2, so a refused second bind closes both and starts over.
    for _ in range(20):
        first = socket.socket()
        first.bind(("127.0.0.1", 0))
        port = first.getsockname()[1]
        second = socket.socket()
        try:
            second.bind(("127.0.0.2", port))
        except OSError as error:
            first.close()
            second.close()
            if error.errno != errno.EADDRINUSE:
                raise
            continue
        return port, {"127.0.0.1": first, "127.0.0.2": second}
    sys.exit("no port was free on both 127.0.0.1 and 127.0.0.2 in 20 attempts")


class Task(http.server.ThreadingHTTPServer):
    def __init__(self, listening, mode):
        super().__init__(listening.getsockname(), Handler, bind_and_activate=False)
        self.socket.close()
        self.socket = listening
        self.mode = mode
        self.v1_requests = 0
        self.lock = threading.Lock()

    def stop(self):
        self.shutdown()
        self.server_close()


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
        task = self.server
        if self.path == "/healthz":
            if task.mode == "unhealthy":
                return self.answer(503, {"status": "unhealthy"})
            with task.lock:
                stopping = task.mode == "exiting" and task.v1_requests == 2
            if stopping:
                # The task stops as Go's server.Shutdown does: it stops listening first, then
                # finishes the request in flight. Only this address's socket closes.
                task.socket.shutdown(socket.SHUT_RDWR)
            self.answer(200, {"status": "starting" if task.mode == "starting" else "healthy"})
            if stopping:
                self.connection.shutdown(socket.SHUT_WR)
                threading.Thread(target=task.stop).start()
            return None
        if self.path == "/v1/sessions":
            with task.lock:
                task.v1_requests += 1
                v1_requests = task.v1_requests
            mode = task.mode
            if mode == "unauthorized" or self.headers.get("Authorization") != "Bearer probe-token":
                return self.answer(401, {"error": "unauthorized"})
            failing = v1_requests <= 2
            if mode == "empty" and failing:
                self.close_connection = True
                return None
            if mode == "reset" and failing:
                self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                self.connection.close()
                self.close_connection = True
                return None
            if mode == "slow" and failing:
                time.sleep(4)
                try:
                    return self.answer(200, [])
                except OSError:
                    # The probe gave up on the request and closed the connection.
                    return None
            if mode in ("starting", "unhealthy") or (mode == "flap" and v1_requests == 2):
                return self.answer(503, {"error": "service starting"})
            return self.answer(200, [])
        return self.answer(404, {"error": "not found"})


port, sockets = bind_pair()
for address, bound in sockets.items():
    if modes[address] != "none":
        bound.listen()
        threading.Thread(target=Task(bound, modes[address]).serve_forever, daemon=True).start()
# Both sockets are in their modes: a listening socket queues connections before its thread serves.
with open(port_file + ".tmp", "w") as out:
    out.write("%d\n" % port)
os.rename(port_file + ".tmp", port_file)
# The process holds both sockets, a none address's included, until the test kills it.
threading.Event().wait()
EOF

# wait_port_file FILE PID WHAT waits until the fake process PID, named WHAT, has written FILE: the
# port it serves, written once its sockets are ready.
wait_port_file() {
  local file="$1" pid="$2" what="$3"
  for _ in $(seq 50); do
    [[ ! -s "$file" ]] || return 0
    if ! kill -0 "$pid" 2>/dev/null; then
      printf 'FAIL: %s exited before it wrote its port\n' "$what" >&2
      exit 1
    fi
    sleep 0.1
  done
  printf 'FAIL: %s never wrote its port\n' "$what" >&2
  exit 1
}

port=""
starts=0
# start_tasks FIRST_MODE SECOND_MODE starts one fake process serving 127.0.0.1 and 127.0.0.2 in
# those modes on one port the kernel chose, and sets $port from the fresh port file it writes.
start_tasks() {
  local port_file
  starts=$((starts + 1))
  port_file="${temporary_dir}/listener-port.${starts}"
  python3 "$server" "$port_file" "$1" "$2" &
  server_pids+=("$!")
  wait_port_file "$port_file" "$!" "the fake tasks"
  read -r port <"$port_file"
}

status=0
# run_case FIRST_MODE SECOND_MODE [PROBE_OPTION...] runs the probe for three ticks against tasks on
# 127.0.0.1 and 127.0.0.2 in those modes, leaving its output in $output_file and its exit code in
# $status.
run_case() {
  local first="$1" second="$2"
  shift 2
  stop_servers
  start_tasks "$first" "$second"
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

# (f) The old task stops between a tick's /healthz and its /v1: the /v1 request finds nothing
# listening. That is the task leaving, not a refusal, and it served /v1 at every other tick.
run_case exiting ok
expect_status 0 "a task that stops mid-tick"
expect_text 'target 127.0.0.1: pass - ' "a task that stops mid-tick"
[[ "$(grep -c $'\t127.0.0.1\t200\thealthy\t000\tno connection\t-\t-$' "$output_file")" == 1 ]] ||
  fail "a task that stops mid-tick: want one per-tick line for 127.0.0.1 with 200 healthy 000 no connection"
expect_text '  1 tick(s) where /v1 found nothing listening right after /healthz answered 200' "a task that stops mid-tick"
printf 'PASS: a task that stops between a tick'"'"'s two requests is shown, not failed (exit 0)\n'

# (g) A task that answers /healthz at every tick but never serves /v1 fails, though no tick's
# /healthz said 200.
run_case unhealthy ok
expect_status 1 "a task whose /v1 never answers 200"
expect_text 'target 127.0.0.1: fail - ' "a task whose /v1 never answers 200"
expect_text '; /v1 never answered 200' "a task whose /v1 never answers 200"
printf 'PASS: a task whose /v1 never answers 200 while /healthz answers fails the run (exit 1)\n'

# (h) Dispatch messages during the run. A fake Dispatch takes only the request Dispatch's
# POST /api/v1/issues/<KEY>/messages takes from a bearer (the session target, the mode, the bearer's
# session actor) and answers it in Dispatch's own shape: in mode sent every delivery attempt is
# sent; in mode refused-first the first attempt is failed with the listener's "service starting",
# as a send to a replacement whose /v1 is closed fails, and the rest are sent. Any other bearer is
# 401 "invalid bearer token", as Dispatch answers it. The run passes only when every message
# records state sent.
cat >"$dispatch_server" <<'EOF'
import http.server
import json
import os
import sys

port_file, mode = sys.argv[1], sys.argv[2]
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
        if self.path != "/api/v1/issues/LEGION-1/messages":
            return self.answer(404, {"code": "NOT_FOUND", "error": "no route for POST " + self.path})
        if self.headers.get("Authorization") != "Bearer dispatch-token":
            return self.answer(401, {"code": "UNAUTHORIZED", "error": "invalid bearer token"})
        if body.get("target") != "session:ses_probe" or body.get("delivery") != "btw" or body.get("actor", {}).get("kind") != "session":
            return self.answer(400, {"code": "MESSAGE_INPUT", "error": "bad message"})
        posts += 1
        state, error = ("failed", "service starting") if mode == "refused-first" and posts == 1 else ("sent", None)
        delivery = {"message_id": "m%d" % posts, "attempt": 1, "delivery": "btw", "session_id": "ses_probe",
                    "envelope_id": None, "duplicate": False, "state": state, "error": error, "reply_id": None,
                    "created_at": "2026-10-01T00:00:00Z", "requested_by": body["actor"], "accepted_at": None,
                    "accepted_as": None}
        return self.answer(201, {"id": "m%d" % posts, "issue_key": "LEGION-1", "author": body["actor"],
                                 "body": body["body"], "target": body["target"], "in_reply_to": None,
                                 "broadcast_id": None, "created_at": "2026-10-01T00:00:00Z",
                                 "deliveries": [delivery], "advice": {"status": "in_progress"}})


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".tmp", "w") as out:
    out.write("%d\n" % server.server_address[1])
os.rename(port_file + ".tmp", port_file)
server.serve_forever()
EOF
printf 'dispatch-token\n' >"${temporary_dir}/dispatch-token"
printf 'wrong-token\n' >"${temporary_dir}/wrong-dispatch-token"

stop_dispatch() {
  [[ -n "$dispatch_pid" ]] || return 0
  kill "$dispatch_pid" 2>/dev/null || true
  wait "$dispatch_pid" 2>/dev/null || true
  dispatch_pid=""
}

# run_dispatch_case MODE TOKEN_FILE runs case (h)'s probe, a Dispatch message at ticks 1 and 3,
# against a fake Dispatch in MODE, with the Dispatch bearer in TOKEN_FILE. Each start of the fake
# binds a port the kernel chose and writes it to a fresh port file.
run_dispatch_case() {
  local mode="$1" token_file="$2" port_file dispatch_port
  starts=$((starts + 1))
  port_file="${temporary_dir}/dispatch-port.${starts}"
  python3 "$dispatch_server" "$port_file" "$mode" &
  dispatch_pid="$!"
  wait_port_file "$port_file" "$dispatch_pid" "the fake Dispatch"
  read -r dispatch_port <"$port_file"
  run_case ok ok --dispatch-url "http://127.0.0.1:${dispatch_port}" --dispatch-token-file "$token_file" \
    --dispatch-issue LEGION-1 --dispatch-session ses_probe --dispatch-every 2
  stop_dispatch
}

run_dispatch_case sent "${temporary_dir}/dispatch-token"
expect_status 0 "every Dispatch message sent"
[[ "$(grep -c $'\tprobe.test\t000\t-\t000\tnot resolved\t-\tsent$' "$output_file")" == 2 ]] ||
  fail "every Dispatch message sent: want each attempt's sent state on the name's line at ticks 1 and 3"
expect_text 'sent=2' "every Dispatch message sent"
expect_text 'verdict: pass - 2 target(s)' "every Dispatch message sent"
printf 'PASS: a run whose every Dispatch message records state sent passes (exit 0)\n'

run_dispatch_case sent "${temporary_dir}/wrong-dispatch-token"
expect_status 1 "a Dispatch that refuses the bearer"
expect_text 'http_401:invalid bearer token=2' "a Dispatch that refuses the bearer"
expect_line 'verdict: fail - 2 of 2 Dispatch message(s) did not record state sent: http_401:invalid bearer token=2' \
  "a Dispatch that refuses the bearer"
printf 'PASS: a run whose Dispatch messages Dispatch refuses (401) fails (exit 1)\n'

run_dispatch_case refused-first "${temporary_dir}/dispatch-token"
expect_status 1 "a Dispatch message whose delivery failed"
[[ "$(grep -c $'\tprobe.test\t000\t-\t000\tnot resolved\t-\tfailed:service starting$' "$output_file")" == 1 ]] ||
  fail "a Dispatch message whose delivery failed: want the first attempt's failed state on the name's line"
expect_text 'target 127.0.0.1: pass - ' "a Dispatch message whose delivery failed"
expect_text 'target 127.0.0.2: pass - ' "a Dispatch message whose delivery failed"
expect_line 'verdict: fail - 1 of 2 Dispatch message(s) did not record state sent: failed:service starting=1' \
  "a Dispatch message whose delivery failed"
printf 'PASS: a Dispatch message whose delivery attempt failed fails the run though /v1 served (exit 1)\n'

# (i) to (k): a task that serves /v1 by its last tick, after its first two /v1 requests drew no
# answer at ticks its /healthz answered 200: a connection closed without an answer (Go's net/http
# after a handler panic), a connection reset, and an answer later than the probe's 3 s. Each is a
# request the listener failed, not a task that had stopped.
run_case empty ok
expect_status 1 "a task whose /v1 closes the connection without an answer"
expect_text 'target 127.0.0.1: fail - ' "a task whose /v1 closes the connection without an answer"
[[ "$(grep -c $'\t127.0.0.1\t200\thealthy\t000\tempty reply\t-\t-$' "$output_file")" == 2 ]] ||
  fail "a task whose /v1 closes the connection without an answer: want two per-tick lines for 127.0.0.1 with 200 healthy 000 empty reply"
expect_line '  2 x 000:empty reply' "a task whose /v1 closes the connection without an answer"
printf 'PASS: a task whose /v1 closes the connection without an answer fails the run (exit 1)\n'

run_case reset ok
expect_status 1 "a task whose /v1 resets the connection"
expect_text 'target 127.0.0.1: fail - ' "a task whose /v1 resets the connection"
expect_line '  2 x 000:connection reset' "a task whose /v1 resets the connection"
printf 'PASS: a task whose /v1 resets the connection fails the run (exit 1)\n'

run_case slow ok
expect_status 1 "a task whose /v1 answers after the probe's 3 s"
expect_text 'target 127.0.0.1: fail - ' "a task whose /v1 answers after the probe's 3 s"
expect_line '  2 x 000:timeout' "a task whose /v1 answers after the probe's 3 s"
printf 'PASS: a task whose /v1 answers too late fails the run (exit 1)\n'

stop_servers
status=0
bash "$driver" >"$output_file" 2>&1 || status=$?
expect_status 2 "no --url"
expect_text 'ERR: --url is required' "no --url"
printf 'PASS: a run without --url is a usage error (exit 2)\n'

mkdir -p "${temporary_dir}/no-curl"
ln -s "$(command -v sed)" "${temporary_dir}/no-curl/sed"
status=0
PATH="${temporary_dir}/no-curl" "$BASH" "$driver" --url "http://probe.test:1" --targets 127.0.0.1 >"$output_file" 2>&1 || status=$?
expect_status 4 "no curl"
expect_text 'ERR: missing required command: curl' "no curl"
printf 'PASS: a host without curl is a missing tool (exit 4)\n'
