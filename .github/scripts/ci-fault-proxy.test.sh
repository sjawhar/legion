#!/usr/bin/env bash
# Tests for `ci-fault-proxy.py`: each of its three modes over plain HTTP and over CONNECT, a
# host outside --hosts passed on in every one of them, and --bind.
#
# Nothing here touches the real internet. The host that is passed on is an origin this test
# starts, a `python3 -m http.server` on a loopback port, and the host that is faulted is either
# a name under .invalid, which RFC 2606 reserves to resolve nowhere, or that same origin under
# another of its names — `localhost` and `127.0.0.1` reach the same server and the proxy matches
# on the name the request carries, so the recover case can fault a host that still answers once
# the window closes.
#
# CONNECT is driven with `curl --proxytunnel`, which makes curl tunnel a plain-HTTP request
# through a CONNECT rather than speak TLS: it exercises the proxy's CONNECT path, which is what
# is under test, against an origin that needs no certificate.
#
# Every curl here clears no_proxy and NO_PROXY. A value of `*` in either makes curl ignore -x,
# and the test would then pass by never reaching the proxy at all. A GitHub runner sets neither;
# the pods this repository's agents run in set both.
#
# Every process this script starts is killed by its own pid in the EXIT trap, and everything it
# writes goes under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/ci-fault-proxy.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
proxy_script="$script_dir/ci-fault-proxy.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
pids=()
cleanup() {
  local pid
  for pid in ${pids+"${pids[@]}"}; do
    kill "$pid" 2> /dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

# The origin: one file, served on a loopback port the kernel picks. The port is written to a
# file rather than read out of what the server logs, as the proxy's own --port-file is: the
# banner `python3 -m http.server` prints reached its log only when the server exited on the
# runner, so this harness waited ten seconds and then said the origin reported no port (run
# 42353947192 of this repository, the `test` job, whose origin log was empty). A port written
# by the process that bound it cannot be buffered away and cannot be taken by a second process
# between the choice and the bind.
mkdir "$work/origin"
printf 'the origin answered\n' > "$work/origin/hello.txt"
cat > "$work/origin.py" <<'ORIGIN'
import functools, http.server, os, sys

directory, port_file = sys.argv[1], sys.argv[2]
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=directory)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
temporary = port_file + ".part"
with open(temporary, "w", encoding="utf-8") as handle:
    handle.write(f"{server.server_address[1]}\n")
os.replace(temporary, port_file)
server.serve_forever()
ORIGIN
python3 "$work/origin.py" "$work/origin" "$work/origin.port" > "$work/origin.log" 2>&1 &
pids+=("$!")
origin_port=""
for _ in $(seq 200); do
  if [ -s "$work/origin.port" ]; then
    origin_port=$(cat "$work/origin.port")
    break
  fi
  sleep 0.05
done
if [ -z "$origin_port" ]; then
  echo "  FAIL: the origin did not report a port" >&2
  cat "$work/origin.log" >&2
  exit 1
fi
echo "measured: the origin listens on 127.0.0.1:$origin_port"

# start_proxy <name> <arguments...>: starts the proxy on a free port, waits for its port file and
# sets $proxy and $proxy_port. Its pid joins the list the trap kills.
start_proxy() {
  local name=$1
  shift
  local port_file="$work/$name.port" pid port=""
  python3 "$proxy_script" --port 0 --port-file "$port_file" "$@" > "$work/$name.log" 2>&1 &
  pid=$!
  pids+=("$pid")
  for _ in $(seq 200); do
    if [ -s "$port_file" ]; then
      port=$(cat "$port_file")
      break
    fi
    kill -0 "$pid" 2> /dev/null || break
    sleep 0.05
  done
  if [ -z "$port" ]; then
    echo "  FAIL: the $name proxy did not report a port" >&2
    cat "$work/$name.log" >&2
    exit 1
  fi
  proxy_port=$port
  proxy="http://127.0.0.1:$port"
}

# run_curl <arguments...>: curl with this machine's no_proxy and NO_PROXY cleared, leaving its
# merged output in $out, its exit status in $status and its wall time in whole seconds in
# $seconds.
run_curl() {
  local start
  start=$(date +%s)
  set +e
  out=$(env no_proxy= NO_PROXY= curl --silent --show-error "$@" 2>&1)
  status=$?
  set -e
  seconds=$(($(date +%s) - start))
}

echo "case: --mode fail answers 504 to a faulted host, over plain HTTP and over CONNECT"
start_proxy fail --mode fail --hosts fetch.invalid
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: plain HTTP answered $out, curl exit $status"
check "a plain-HTTP request for a faulted host answers 504" "$(is "$out" 504)"
run_curl -x "$proxy" --proxytunnel http://fetch.invalid/tool.tar.gz
echo "  measured: CONNECT gave curl exit $status: $out"
check "a CONNECT for a faulted host is refused with 504, which curl reports as exit 56" "$(is "$status" 56)"
check "and curl's message names the 504 it got from the proxy" "$(contains "$out" '504')"
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin over plain HTTP" "$(is "$out" 'the origin answered')"
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and through CONNECT" "$(is "$out" 'the origin answered')"

echo "case: --mode recover answers 504 inside its window and passes the request on after it"
# A 10 s window, not the 2 s a reader might write: three loopback requests have to finish inside
# it, and a runner that stalls for a second between them would otherwise read the recovered
# answer and report the window as broken. The case costs 11 s and cannot race.
start_proxy recover --mode recover --recover-after 10 --hosts localhost
window_start=$(date +%s)
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' "http://localhost:$origin_port/hello.txt"
inside_plain=$out
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
inside_tunnel_status=$status
echo "  measured: $(($(date +%s) - window_start))s into the 10 s window, plain HTTP answered $inside_plain and CONNECT gave curl exit $inside_tunnel_status"
check "a plain-HTTP request inside the window answers 504" "$(is "$inside_plain" 504)"
check "a CONNECT inside the window is refused" "$(is "$inside_tunnel_status" 56)"
run_curl -x "$proxy" "http://127.0.0.1:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin inside the window" "$(is "$out" 'the origin answered')"
sleep 11
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
echo "  measured: $(($(date +%s) - window_start))s after the window opened, plain HTTP answered '$out' (curl exit $status)"
check "the same plain-HTTP request after the window reaches the origin" "$(is "$out" 'the origin answered')"
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and the same CONNECT reaches it too" "$(is "$out" 'the origin answered')"

echo "case: --mode stall sends nothing, so each attempt ends at the client's own bound"
start_proxy stall --mode stall --hosts fetch.invalid
# The body file is created here, not left to curl: a curl that receives nothing writes no file
# at all, and a check that a file which was never created is empty proves nothing.
: > "$work/stall.body"
run_curl --max-time 2 -o "$work/stall.body" -x "$proxy" http://fetch.invalid/tool.tar.gz
echo "  measured: plain HTTP ended after ${seconds}s with curl exit $status, $(wc -c < "$work/stall.body") bytes from the proxy: $out"
check "a stalled plain-HTTP request ends at curl's --max-time, which curl reports as exit 28" "$(is "$status" 28)"
check "and it took about the 2 s bound" "$([ "$seconds" -ge 2 ] && [ "$seconds" -le 5 ] && echo true || echo false)"
check "and the proxy sent nothing at all" "$([ ! -s "$work/stall.body" ] && echo true || echo false)"
run_curl --max-time 2 -x "$proxy" --proxytunnel http://fetch.invalid/tool.tar.gz
echo "  measured: CONNECT ended after ${seconds}s with curl exit $status: $out"
check "a stalled CONNECT ends at the same bound" "$(is "$status" 28)"
check "and it took about the 2 s bound too" "$([ "$seconds" -ge 2 ] && [ "$seconds" -le 5 ] && echo true || echo false)"
run_curl --max-time 10 -x "$proxy" "http://localhost:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin over plain HTTP" "$(is "$out" 'the origin answered')"
run_curl --max-time 10 -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and through CONNECT" "$(is "$out" 'the origin answered')"

echo "case: --bind listens on another address, which is how a docker build reaches the proxy"
# 127.0.0.2 stands in for the docker bridge gateway: a second address of this machine that the
# default 127.0.0.1 does not answer on, so the case proves both halves of the option — the proxy
# serves where --bind says, and nowhere else. Every Linux kernel routes the whole 127.0.0.0/8 to
# loopback, so this needs no interface of its own.
start_proxy bind --mode fail --hosts fetch.invalid --bind 127.0.0.2
run_curl -x "http://127.0.0.2:$proxy_port" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: the proxy on 127.0.0.2:$proxy_port answered $out, curl exit $status"
check "a request to the bound address is served" "$(is "$out" 504)"
run_curl --max-time 5 -x "http://127.0.0.1:$proxy_port" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: the same port on 127.0.0.1 gave curl exit $status: $out"
check "and 127.0.0.1 is not listening, which curl reports as exit 7" "$(is "$status" 7)"
check "the proxy's first line names the address it bound" \
  "$(contains "$(cat "$work/bind.log")" "^ci-fault-proxy: mode=fail bind=127\.0\.0\.2 port=$proxy_port ")"

echo "case: the proxy logs what it did, and fetched nothing of its own"
fail_log=$(cat "$work/fail.log")
indent "$fail_log"
check "it logged the 504 it answered to the plain-HTTP request" \
  "$(contains "$fail_log" '^ci-fault-proxy: 504 GET http://fetch.invalid/tool.tar.gz$')"
check "and the 504 it answered to the CONNECT" \
  "$(contains "$fail_log" '^ci-fault-proxy: 504 CONNECT fetch.invalid:80$')"
check "and the request it passed on" \
  "$(contains "$fail_log" "^ci-fault-proxy: pass GET http://localhost:$origin_port/hello.txt\$")"
check "no handler failed" "$(is "$(contains "$fail_log" 'handler failed')" false)"

summary "ci-fault-proxy.py"
