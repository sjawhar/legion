#!/usr/bin/env bash
# packages/envoy/scripts/airlock-check.sh
#
# Runs one before_merge check of the airlock's verification document
# (dispatch://INFRA-1352/artifact/verification-md) end to end and says whether it passed, so
# that the implementer and the tester run the same command and read the same evidence.
#
# What one run proves is the check's own sentence and nothing wider. The script starts the
# local stack the check needs (scripts/dev-airlock.sh, with the options that check's negative
# control asks for), runs the check's steps against it, runs the negative control, stops the
# stack, and prints:
#
#   - a result block to paste into `dispatch result`: what the check expected, what this run
#     observed, and what the negative control did;
#   - one PASS: or FAIL: line for each property the check's outcome sentence names, and one for
#     its negative control, because a control that does not behave as the check says leaves the
#     check proving nothing;
#   - one last line, `check <id>: passed` or `check <id>: failed (<n> of <m>)`.
#
# No line it prints is a credential. A value is compared by its SHA-256 (the first 16 hex
# digits) and its length, and every line that compares one says so; that is a rule of the
# verification document itself, and it holds for the stand-in values of a local stack too.
#
# Two things this pod measured shape two checks:
#
#   - k1 scans the command's memory, and /proc/sys/kernel/yama/ptrace_scope is 1 here, so only
#     a process that is the command's own ancestor may read its /proc/<pid>/mem. The scan is
#     therefore a small Python program (written into this run's work directory) that starts
#     `agent-secrets` itself and scans the descendant it started.
#   - k15 needs no stack at all: it builds the Lean model, prints its theorem statements and
#     their axioms, runs the comparison fuzz test for ten minutes, and runs the six mutants
#     through scripts/airlock-mutants.sh.
#
# Where a check needs a secret the stack does not hold, this script extends the stack's own
# fake Secrets Manager (the file AIRLOCK_FAKE_SECRETS_FILE names) at run time and makes every
# broker reread it. It writes no rules file of its own and never edits dev-airlock.sh's: a
# control that needs other rules or other proxy settings is a second broker process this
# script starts on the stack's own database, with the stack's rules, certificate authority and
# stand-ins, and that one setting changed.
#
# Usage: packages/envoy/scripts/airlock-check.sh <id> [--keep-stack]
#        <id> is k1, k2, k3, k4, k5, k8a or k15. k6, k7 and k9 are not implemented here yet and
#        exit 3. --keep-stack leaves the stack and the work directory up for a person to look
#        at a failure with. --help prints this usage. A usage error exits 2.
#
# Needs bash, curl, jq, go, python3, openssl, node and psql; k2 also needs gh (the last
# released agent-secrets comes from a GitHub release) and k15 needs lake from the Lean
# toolchain this environment names. DEV_BROKER_POSTGRES_URL is passed to dev-airlock.sh as it
# is, so a pod with no container runtime names its test server there.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
LEAN_DIR="$ENVOY_DIR/internal/broker/airlock/lean"

usage() {
	cat <<'EOF'
usage: airlock-check.sh <id> [--keep-stack]

Runs one before_merge check of dispatch://INFRA-1352/artifact/verification-md and prints, for
each property the check's outcome sentence names, what it expected beside what it got.

  <id>           k1, k2, k3, k4, k5, k8a or k15 (k6, k7 and k9 exit 3: not yet implemented)
  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

It exits 0 only when every property passed, 1 when one did not, 2 on a usage error and 3 for a
check this script does not implement yet.
EOF
}

CHECK_ID=""
KEEP_STACK=false
while [ $# -gt 0 ]; do
	case "$1" in
	--keep-stack) KEEP_STACK=true ;;
	--help | -h)
		usage
		exit 0
		;;
	-*)
		echo "airlock-check: unexpected option $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$CHECK_ID" ]; then
			echo "airlock-check: one check id at a time, not $CHECK_ID and $1" >&2
			usage >&2
			exit 2
		fi
		CHECK_ID="$1"
		;;
	esac
	shift
done

if [ -z "$CHECK_ID" ]; then
	echo "airlock-check: name the check to run" >&2
	usage >&2
	exit 2
fi

case "$CHECK_ID" in
k1 | k2 | k3 | k4 | k5 | k8a | k15) ;;
k6 | k7 | k9)
	echo "airlock-check: check $CHECK_ID is not yet implemented in this script" >&2
	exit 3
	;;
*)
	echo "airlock-check: $CHECK_ID is no check of this document; it holds k1, k2, k3, k4, k5, k6, k7, k8a, k9 and k15" >&2
	usage >&2
	exit 2
	;;
esac

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airlock-check-$CHECK_ID.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""
EXTRA_BROKER_PIDS=()
HELPER_PIDS=()

cleanup() {
	local pid
	for pid in "${HELPER_PIDS[@]+"${HELPER_PIDS[@]}"}" "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "airlock-check: the stack is still up (dev-airlock.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "airlock-check: stop it with kill $STACK_PID, which drops its databases" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-airlock.sh's own EXIT trap is what stops its processes and
	# drops the two databases it made.
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "airlock-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# --- What a run says -----------------------------------------------------------------------
#
# Three lists make the result block, and one pair of counters makes the verdict. A property is
# recorded with pass or fail and nothing else decides the exit status.

EXPECTED=()
OBSERVED=()
CONTROL=()
PASSED=0
TOTAL=0
VERDICTS=()

expect() { EXPECTED+=("$1"); }
observe() { OBSERVED+=("$1"); }
control() { CONTROL+=("$1"); }

pass() {
	TOTAL=$((TOTAL + 1))
	PASSED=$((PASSED + 1))
	VERDICTS+=("PASS: $1")
}

fail() {
	TOTAL=$((TOTAL + 1))
	VERDICTS+=("FAIL: $1")
}

# judge records one property from a condition the caller has already evaluated: "0" passes and
# anything else, an empty answer included, fails. A step that could not be evaluated at all is
# a property that was not proved, which is a failure and never a silent pass.
judge() { # <status> <what the property is> <what was seen>
	if [ "${1:-}" = 0 ]; then
		pass "$2 — $3"
	else
		fail "$2 — $3"
	fi
}

report() {
	echo
	echo "---- result block for check $CHECK_ID ----"
	echo "expected:"
	printf '  %s\n' "${EXPECTED[@]+"${EXPECTED[@]}"}"
	echo "observed:"
	printf '  %s\n' "${OBSERVED[@]+"${OBSERVED[@]}"}"
	echo "negative control:"
	printf '  %s\n' "${CONTROL[@]+"${CONTROL[@]}"}"
	echo "---- end result block ----"
	echo
	printf '%s\n' "${VERDICTS[@]+"${VERDICTS[@]}"}"
	echo
	if [ "$PASSED" -eq "$TOTAL" ]; then
		echo "check $CHECK_ID: passed"
		return 0
	fi
	echo "check $CHECK_ID: failed ($((TOTAL - PASSED)) of $TOTAL)"
	return 1
}

# --- Comparing a value without printing it ---------------------------------------------------
#
# Every comparison in this script is by SHA-256 and length, and every line that makes one says
# so. digest answers the first 16 hex digits of a value's SHA-256, which is far more than two
# values of one run ever share and far too little to be the value.

digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }
shape() { printf 'len=%d sha256=%s' "${#1}" "$(digest "$1")"; }

# contains reports whether haystack holds needle, without either reaching the output.
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

# --- The stack ---------------------------------------------------------------------------------

# await waits for a line a process logs, and ends the run the moment that process dies: a
# stack that never came up proves nothing, and the check's own steps would each fail with a
# reason that is not the check's.
await() { # <what> <seconds> <log> <pattern> <pid>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "airlock-check: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "airlock-check: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

# start_stack runs dev-airlock.sh with the options its caller names, waits for its banner, and
# exports everything that banner names. dev-broker.sh's own export block is in the same log and
# is read first, so where the two name one variable the airlock's wins, as the banner says.
start_stack() {
	echo "airlock-check: starting the stack (dev-airlock.sh $*)..." >&2
	: >"$STACK_LOG"
	bash "$SCRIPT_DIR/dev-airlock.sh" "$@" >"$STACK_LOG" 2>&1 &
	STACK_PID=$!
	await "the stack" 900 "$STACK_LOG" '^dev-airlock: ready\.$' "$STACK_PID"
	eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
	mapfile -t BROKER_API_ADDRS < <(sed -n 's/^  broker [0-9]*: *api \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PROXY_ADDRS < <(sed -n 's/^  broker [0-9]*:.*proxy \([^,]*\),.*/\1/p' "$STACK_LOG")
	STACK_DB_NAME="$(sed -n 's/^dev-airlock: created isolated database \([a-z0-9_]*\).*/\1/p' "$STACK_LOG" | head -n 1)"
	# The brokers' database URL, built exactly as dev-airlock.sh builds it, which is the one
	# way to reach the rows a check sets up by hand (an enrollment's withheld names).
	if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
		STACK_DB_URL="${BASH_REMATCH[1]}/${STACK_DB_NAME}${BASH_REMATCH[3]:-}"
	else
		STACK_DB_URL="postgres://postgres:dispatch@127.0.0.1:55432/${STACK_DB_NAME}?sslmode=disable"
	fi
	BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
	echo "airlock-check: the stack is up (${#BROKER_API_ADDRS[@]} brokers, work directory $AIRLOCK_DIR)" >&2
}

# stop_stack stops this run's own extra brokers first and then dev-airlock.sh. The order
# matters: a broker of this script's still holding a connection to the stack's database keeps
# dev-airlock.sh's own cleanup from dropping it, and the database is left behind on the server.
stop_stack() {
	if [ "$KEEP_STACK" = true ]; then return 0; fi
	local pid
	for pid in "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
	EXTRA_BROKER_PIDS=()
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		echo "airlock-check: stopping the stack..." >&2
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	STACK_PID=""
}

# start_extra_broker starts one more broker process on the stack's own database, rules file,
# certificate authority and stand-ins, with the environment the caller adds on top, and answers
# its proxy address in EXTRA_PROXY_ADDR. It is how a control that turns one proxy setting off is
# run: the stack's own brokers keep the setting, and the command's HTTPS_PROXY is pointed at
# this one for that step alone.
start_extra_broker() { # <name> <VAR=value>...
	local name="$1"
	shift
	local log="$WORK_DIR/broker-$name.log"
	: >"$log"
	env \
		BROKER_DATABASE_URL="$STACK_DB_URL" \
		BROKER_PUBLIC_URL="$AGENT_SECRETS_URL" \
		BROKER_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
		BROKER_SECRETS_PREFIX="$STACK_SECRETS_PREFIX" \
		BROKER_SECRETS_KMS_KEY_ARN="$STACK_KMS_KEY_ARN" \
		BROKER_FAKE_SECRETS_FILE="$AIRLOCK_FAKE_SECRETS_FILE" \
		BROKER_SIGNIN_ISSUER="$AIRLOCK_SIGNIN_URL" \
		BROKER_SIGNIN_CLIENT_ID="dev-broker" \
		BROKER_SIGNIN_CLIENT_SECRET_FILE="$DEV_BROKER_DIR/signin-client-secret" \
		BROKER_SIGNIN_DOMAIN="${AIRLOCK_APPROVER#*@}" \
		BROKER_DRAIN_SECONDS=0 \
		BROKER_AIRLOCK_RULES_FILE="$AIRLOCK_RULES_FILE" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_AIRLOCK_PROXY_URL="$AIRLOCK_PROXY_URL" \
		BROKER_AIRLOCK_CA_CERT_FILE="$AIRLOCK_CA_CERT_FILE" \
		BROKER_AIRLOCK_CA_KEY_FILE="$AIRLOCK_CA_KEY_FILE" \
		BROKER_AIRLOCK_TEST_GITHUB_API_BASE="$AIRLOCK_GITHUB_URL" \
		BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL="$AIRLOCK_GOOGLE_URL/token" \
		BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE="$AIRLOCK_CA_CERT_FILE" \
		BROKER_AIRLOCK_TEST_DIAL_ALLOW="$STACK_DIAL_ALLOW" \
		BROKER_AIRLOCK_TEST_RESOLVE="$STACK_RESOLVE" \
		"$@" \
		"$BROKER_BIN" >"$log" 2>&1 &
	local pid=$!
	EXTRA_BROKER_PIDS+=("$pid")
	await "the $name broker" 120 "$log" 'airlock proxy listening addr=' "$pid"
	EXTRA_PROXY_ADDR="$(sed -n 's/.*airlock proxy listening addr=\([^ ]*\).*/\1/p' "$log" | head -n 1)"
}

# stack_facts reads the few settings a second broker and the fake store need and that the
# banner does not name: the namespace prefix and key the brokers serve (their own /v1/settings
# route), and the resolve and dial-allow lists, which are the stack's bound hosts on their own
# loopback addresses.
stack_facts() {
	local settings
	settings="$(curl -sS "$AGENT_SECRETS_URL/v1/settings")"
	STACK_SECRETS_PREFIX="$(printf '%s' "$settings" | jq -r .secrets_prefix)"
	STACK_KMS_KEY_ARN="$(printf '%s' "$settings" | jq -r '.kms_key_arn // ""')"
	STACK_DIAL_ALLOW=""
	STACK_RESOLVE=""
	local host addr
	while read -r host addr; do
		[ -n "$host" ] || continue
		STACK_DIAL_ALLOW="${STACK_DIAL_ALLOW:+$STACK_DIAL_ALLOW }$host:443"
		STACK_RESOLVE="${STACK_RESOLVE:+$STACK_RESOLVE }$host=$addr"
	done <<-EOF
		s.airlock.test 127.0.0.2
		t.airlock.test 127.0.0.3
		microsoft.airlock.test 127.0.0.4
		azure-resource.airlock.test 127.0.0.4
		aws.airlock.test 127.0.0.5
	EOF
}

# --- Talking to the brokers --------------------------------------------------------------------

# release asks for a grant of one name and releases it as the CLI does, leaving the raw answer
# in RELEASE_JSON and the grant's placeholder, proxy password and id in three variables. The
# placeholder and the password never reach the output.
release() { # <name> <ttl>
	local url proof
	RELEASE_REQUEST="$(agent-secrets request "$1" --ttl "$2" --json 2>"$WORK_DIR/request.err")" || {
		echo "airlock-check: the request for $1 failed:" >&2
		cat "$WORK_DIR/request.err" >&2
		return 1
	}
	GRANT_ID="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.grant_id // ""')"
	[ -n "$GRANT_ID" ] && [ "$GRANT_ID" != null ] || {
		echo "airlock-check: the request for $1 was not granted: $(printf '%s' "$RELEASE_REQUEST" | jq -r .state)" >&2
		return 1
	}
	url="$AGENT_SECRETS_URL/v1/grants/$GRANT_ID/values"
	proof="$(agent-secrets sign --method POST --url "$url")"
	RELEASE_JSON="$(curl -sS -X POST -H "Proof: $proof" "$url")"
	PLACEHOLDER="$(printf '%s' "$RELEASE_JSON" | jq -r --arg n "$1" '.airlock.names[$n].value // ""')"
	PROXY_PASSWORD="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_password // ""')"
}

# proxy_for is the proxy URL of this grant at one address, credential and all. It is passed to
# curl and never printed.
proxy_for() { printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"; }

records() { curl -sS "$AIRLOCK_RECORDS_URL/records/$1"; }
records_count() { records "$1" | jq 'length'; }

# reread_all makes every broker read one secret's name again, which is what the release of a
# new version of a stored value waits on.
reread_all() { # <name>
	local addr
	for addr in "${BROKER_API_ADDRS[@]}"; do
		curl -sS -o /dev/null -X POST "http://$addr/v1/secrets/$1/reread" || true
	done
}

# add_fake_secret appends one secret to the stack's fake Secrets Manager and makes every broker
# read it. It is how a check gets a name the rules file does not hold without writing a rules
# file of its own.
add_fake_secret() { # <store name> <tags json> <value>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --argjson tags "$2" --arg value "$3" --arg kms "$STACK_KMS_KEY_ARN" \
		'.secrets += [{name: $name, kms_key_id: $kms, tags: $tags, value: $value}]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

# set_fake_value replaces one stored secret's value, which is the local stack's way of adding a
# new version and disabling the old: the fake store holds one value for each name.
set_fake_value() { # <store name> <value>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --arg value "$2" \
		'.secrets = [.secrets[] | if .name == $name then .value = $value else . end]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

# stored_value is what the fake store holds for one name, read straight out of the file so a
# comparison is against the store and not against a string this script carries.
stored_value() { jq -r --arg n "$1" '.secrets[] | select(.name == $n) | .value' "$AIRLOCK_FAKE_SECRETS_FILE"; }

# header_of is one recorded request's header, and body_of its body: the two a check reads a
# result back from. The index is jq's, so -1 is the last record.
header_of() { records "$1" | jq -r --argjson i "$2" --arg h "$3" '.[$i].header[$h][0] // ""'; }
record_field() { records "$1" | jq -r --argjson i "$2" --arg f "$3" '.[$i][$f] // ""'; }

psql_stack() { psql "$STACK_DB_URL" -Atq -v ON_ERROR_STOP=1 "$@"; }

###############################################################################################
# k1
###############################################################################################

# The Python program that starts the command and scans the descendant it started.
# /proc/sys/kernel/yama/ptrace_scope is 1 in a Legion pod, so the reader has to be an ancestor
# of what it reads; a scanner started beside the command reads "permission denied". Every needle
# reaches it in a file, never on a command line, which /proc shows to everybody.
write_memscan() {
	cat >"$WORK_DIR/memscan.py" <<'PY'
import json, os, subprocess, sys, time

ready, needles_file, out_file = sys.argv[1], sys.argv[2], sys.argv[3]
cwd, cmd = sys.argv[4], sys.argv[5:]
needles = [bytes.fromhex(line.strip()) for line in open(needles_file) if line.strip()]

child = subprocess.Popen(cmd, cwd=cwd)
# The command writes the ready file once it has made its calls and before it sleeps.
deadline = time.time() + 120
while time.time() < deadline and not os.path.exists(ready) and child.poll() is None:
    time.sleep(0.1)

result = {"pid": child.pid, "scanned": False, "regions": 0, "bytes": 0,
          "memory_hits": [0] * len(needles), "environ_hits": [0] * len(needles),
          "scan_error": ""}
try:
    with open(f"/proc/{child.pid}/environ", "rb") as f:
        environ = f.read()
    for i, n in enumerate(needles):
        result["environ_hits"][i] = environ.count(n)
except OSError as e:
    result["scan_error"] = f"environ: {e}"

try:
    maps = open(f"/proc/{child.pid}/maps").read().splitlines()
    with open(f"/proc/{child.pid}/mem", "rb", 0) as mem:
        for line in maps:
            fields = line.split()
            if len(fields) < 2 or "r" not in fields[1]:
                continue
            lo, hi = (int(x, 16) for x in fields[0].split("-"))
            # [vsyscall] sits above the top of the address space a seek offset can hold, and
            # a very large mapping is a reservation nothing has written to.
            if hi - lo > (1 << 30) or lo >= (1 << 63):
                continue
            try:
                mem.seek(lo)
                data = mem.read(hi - lo)
            except (OSError, OverflowError, ValueError):
                continue
            result["regions"] += 1
            result["bytes"] += len(data)
            for i, n in enumerate(needles):
                result["memory_hits"][i] += data.count(n)
    result["scanned"] = result["regions"] > 0
except OSError as e:
    result["scan_error"] = (result["scan_error"] + " " + f"mem: {e}").strip()

result["exit"] = child.wait()
with open(out_file, "w") as f:
    json.dump(result, f)
PY
}

# k1_case runs one of k1's two commands under the scanner and judges the five places.
# bound is true when the name has a rules entry, which is the check itself; false is its
# negative control, where the same run must find the value in env.txt and in the environment.
k1_case() { # <name> <label> <value> <bound true|false>
	local name="$1" label="$2" value="$3" bound="$4"
	local dir="$WORK_DIR/k1-$label"
	mkdir -p "$dir"
	printf '%s' "$value" | od -An -tx1 -v | tr -d ' \n' >"$dir/needles"
	echo >>"$dir/needles"
	chmod 600 "$dir/needles"
	cat >"$dir/command.sh" <<EOF
curl -fsS -H "Authorization: Bearer \$$name" $AIRLOCK_S_URL/ok >curl.out
env > env.txt
: > ready
sleep 5
EOF
	python3 "$WORK_DIR/memscan.py" "$dir/ready" "$dir/needles" "$dir/scan.json" "$dir" \
		agent-secrets "$name" -- sh "$dir/command.sh" 2>"$dir/cli.err" || true
	K1_EXIT="$(jq -r '.exit' "$dir/scan.json")"
	K1_MEM="$(jq -r '.memory_hits[0]' "$dir/scan.json")"
	K1_ENVIRON="$(jq -r '.environ_hits[0]' "$dir/scan.json")"
	K1_REGIONS="$(jq -r '.regions' "$dir/scan.json")"
	K1_SCANNED_BYTES="$(jq -r '.bytes' "$dir/scan.json")"
	K1_SCAN_ERROR="$(jq -r '.scan_error' "$dir/scan.json")"
	K1_FILES="$(grep -rlF "$value" "$dir" 2>/dev/null | grep -v '/needles$' | sed "s|$dir/||" | tr '\n' ' ')"
	K1_HEADER="$(header_of s -1 Authorization)"
	K1_PATH="$(record_field s -1 path)"
	# The broker's own answer to the CLI for this name, read with a release of its own.
	release "$name" 10m >/dev/null 2>&1 || true
	K1_ANSWER_HITS=0
	if contains "$value" "$RELEASE_JSON"; then K1_ANSWER_HITS=1; fi
	K1_ENV_FILE_HITS="$(grep -cF "$value" "$dir/env.txt" 2>/dev/null || true)"
	[ -n "$K1_ENV_FILE_HITS" ] || K1_ENV_FILE_HITS=0
	if [ "$bound" = true ]; then
		observe "$label: the command exited $K1_EXIT; S's last record is $K1_PATH and its Authorization is $(shape "$K1_HEADER") (compared by SHA-256 and length)"
		observe "$label: the value is $(shape "$value"); scanned $K1_REGIONS readable regions ($K1_SCANNED_BYTES bytes) of the command's memory${K1_SCAN_ERROR:+ (scan error: $K1_SCAN_ERROR)}"
		observe "$label: hits for the value — memory $K1_MEM, /proc/<pid>/environ $K1_ENVIRON, the broker's answer $K1_ANSWER_HITS, env.txt $K1_ENV_FILE_HITS, files under the working directory: ${K1_FILES:-none}"
	else
		# No record of S's is read here: a name with no rules entry has no proxy, so the
		# command's call to a .test name no resolver answers never arrives, and S's last
		# record would be the previous case's.
		control "$label with no rules entry: the command exited $K1_EXIT; the value's hits are $K1_ENV_FILE_HITS in env.txt and $K1_ENVIRON in /proc/<pid>/environ, against $(shape "$value") (compared by SHA-256 and length)"
	fi
}

check_k1() {
	expect "S gets the stored secret's real value, and the github-app secret's minted token, in the Authorization header"
	expect "each command exits 0"
	expect "neither value is found in S's record aside: not in the broker's answer to the CLI, not in /proc/<pid>/environ, not in a scan of the command's readable memory, and not in any file under its working directory"
	expect "the negative control, the same runs with the secret's entry removed, finds both in env.txt and in the command's environment"

	start_stack
	stack_facts
	write_memscan

	local demo_value github_token
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	# The stand-in GitHub App mints ghs_standin_<owner> for each installation it is configured
	# with, and the stack configures acme-org (dev-airlock.sh's GITHUB_OWNER).
	github_token="ghs_standin_acme-org"

	k1_case AIRLOCK_DEMO_TOKEN stored "$demo_value" true
	judge "$([ "$K1_EXIT" = 0 ] && echo 0 || echo 1)" "the stored secret's command exits 0" "it exited $K1_EXIT"
	judge "$(contains "$demo_value" "$K1_HEADER" && echo 0 || echo 1)" \
		"S gets the stored secret's real value in the Authorization header of GET /ok" \
		"the header is $(shape "$K1_HEADER") and the stored value is $(shape "$demo_value"), compared by SHA-256 and length"
	judge "$([ "$K1_SCANNED_BYTES" -gt 0 ] && [ "$K1_MEM" = 0 ] && [ "$K1_ENVIRON" = 0 ] && [ "$K1_ANSWER_HITS" = 0 ] && [ -z "$K1_FILES" ] && echo 0 || echo 1)" \
		"the stored secret's value is in none of the five places" \
		"memory $K1_MEM hits over $K1_SCANNED_BYTES scanned bytes, environ $K1_ENVIRON, the broker's answer $K1_ANSWER_HITS, files ${K1_FILES:-none}"

	k1_case AIRLOCK_GITHUB_APP github "$github_token" true
	judge "$([ "$K1_EXIT" = 0 ] && echo 0 || echo 1)" "the github-app command exits 0" "it exited $K1_EXIT"
	judge "$(contains "$github_token" "$K1_HEADER" && echo 0 || echo 1)" \
		"S gets the minted installation token in the Authorization header" \
		"the header is $(shape "$K1_HEADER") and the minted token is $(shape "$github_token"), compared by SHA-256 and length"
	judge "$([ "$K1_SCANNED_BYTES" -gt 0 ] && [ "$K1_MEM" = 0 ] && [ "$K1_ENVIRON" = 0 ] && [ "$K1_ANSWER_HITS" = 0 ] && [ -z "$K1_FILES" ] && echo 0 || echo 1)" \
		"the minted token is in none of the five places" \
		"memory $K1_MEM hits over $K1_SCANNED_BYTES scanned bytes, environ $K1_ENVIRON, the broker's answer $K1_ANSWER_HITS, files ${K1_FILES:-none}"

	# The negative control: the same two runs for names the rules file does not hold. The stack
	# takes its shape from its own rules file, so the control adds the two secrets to the
	# stack's fake store rather than removing an entry from its rules.
	local plain_value github_app
	plain_value="the-control-airlock-plain-token-$RANDOM$RANDOM"
	add_fake_secret "${STACK_SECRETS_PREFIX}airlock-plain-token" \
		'{"owner":"legion-daemon","tier":"agent"}' "$plain_value"
	github_app="$(jq -r --arg n "${STACK_SECRETS_PREFIX}airlock-github-app" '.secrets[] | select(.name == $n) | .value' "$AIRLOCK_FAKE_SECRETS_FILE")"
	add_fake_secret "${STACK_SECRETS_PREFIX}airlock-plain-github" \
		'{"owner":"legion-daemon","tier":"agent","kind":"github-app","github-owners":"acme-org"}' "$github_app"
	reread_all AIRLOCK_PLAIN_TOKEN
	reread_all AIRLOCK_PLAIN_GITHUB

	k1_case AIRLOCK_PLAIN_TOKEN plain-stored "$plain_value" false
	local plain_env="$K1_ENV_FILE_HITS" plain_environ="$K1_ENVIRON"
	k1_case AIRLOCK_PLAIN_GITHUB plain-github "$github_token" false
	judge "$([ "$plain_env" -gt 0 ] && [ "$plain_environ" -gt 0 ] && [ "$K1_ENV_FILE_HITS" -gt 0 ] && [ "$K1_ENVIRON" -gt 0 ] && echo 0 || echo 1)" \
		"negative control: with no rules entry both values are in env.txt and in the command's environment" \
		"stored: env.txt $plain_env, environ $plain_environ; github-app: env.txt $K1_ENV_FILE_HITS, environ $K1_ENVIRON"

	stop_stack
}

###############################################################################################
# k2
###############################################################################################

# fetch_old_cli puts the last released agent-secrets under the work directory and answers its
# path, checking that it prints the tag the release carries. It is the one check that reaches
# GitHub.
fetch_old_cli() {
	local tag asset
	tag="$(gh release list --repo cybermonklabs/legion --limit 100 --json tagName \
		--jq '[.[] | select(.tagName | startswith("legion-envoy-v"))] | .[0].tagName')"
	[ -n "$tag" ] || {
		echo "airlock-check: no legion-envoy-v* release was listed" >&2
		return 1
	}
	mkdir -p "$WORK_DIR/old-cli"
	gh release download "$tag" --repo cybermonklabs/legion --pattern 'agent-secrets-amd64.tar.gz' \
		--dir "$WORK_DIR/old-cli" --clobber >/dev/null
	asset="$WORK_DIR/old-cli/agent-secrets-amd64.tar.gz"
	tar -xzf "$asset" -C "$WORK_DIR/old-cli"
	# The release's tarball holds agent-secrets/bin/agent-secrets beside its helper and its
	# notices, so the binary is under that directory and is not the archive's own name.
	OLD_CLI="$WORK_DIR/old-cli/agent-secrets/bin/agent-secrets"
	chmod +x "$OLD_CLI"
	OLD_CLI_TAG="$tag"
	OLD_CLI_VERSION="$("$OLD_CLI" --version 2>&1 | tr -d '\n')"
}

check_k2() {
	expect "POST /v1/grants/{id}/values for a grant of a secret the rules name answers the placeholder and the proxy's certificate in their new fields and leaves the name out of values"
	expect "the last released agent-secrets exits without running the command and says that no value was released"
	expect "the negative control, the same call for a grant of a secret the rules do not name, answers the value under the name, and the old CLI runs its command"

	start_stack
	stack_facts
	fetch_old_cli
	observe "the last released agent-secrets is $OLD_CLI_TAG and prints: $OLD_CLI_VERSION"
	judge "$(contains "$OLD_CLI_TAG" "$OLD_CLI_VERSION" && echo 0 || echo 1)" \
		"the old CLI is the last release" "its --version prints $OLD_CLI_VERSION for tag $OLD_CLI_TAG"

	release AIRLOCK_DEMO_TOKEN 30m
	local values_keys airlock_keys ca_len name_value demo_value
	values_keys="$(printf '%s' "$RELEASE_JSON" | jq -c '.values // {} | keys')"
	airlock_keys="$(printf '%s' "$RELEASE_JSON" | jq -c '.airlock | keys')"
	ca_len="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.ca_certificate | length')"
	name_value="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.names.AIRLOCK_DEMO_TOKEN.value // ""')"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	observe "the answer's values keys are $values_keys and its airlock keys are $airlock_keys"
	observe "airlock.names.AIRLOCK_DEMO_TOKEN.value is $(shape "$name_value") and the stored value is $(shape "$demo_value") (compared by SHA-256 and length: a different value, which is what a placeholder is)"
	observe "airlock.ca_certificate is $ca_len bytes and begins $(head -c 27 <<<"$(printf '%s' "$RELEASE_JSON" | jq -r .airlock.ca_certificate)")"

	judge "$([ "$values_keys" = "[]" ] && echo 0 || echo 1)" \
		"the answer leaves the name out of values" "values keys are $values_keys"
	judge "$([ -n "$name_value" ] && ! contains "$demo_value" "$RELEASE_JSON" && echo 0 || echo 1)" \
		"the answer carries the placeholder in its new field and no real value anywhere" \
		"airlock.names.AIRLOCK_DEMO_TOKEN.value is $(shape "$name_value"); the stored value's bytes are not in the answer"
	judge "$(printf '%s' "$RELEASE_JSON" | jq -e '.airlock.ca_certificate | startswith("-----BEGIN CERTIFICATE-----")' >/dev/null && echo 0 || echo 1)" \
		"the answer carries the proxy's certificate in its new field" "airlock.ca_certificate is a $ca_len byte PEM certificate"

	local old_out old_status
	set +e
	old_out="$("$OLD_CLI" AIRLOCK_DEMO_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
	set -e
	observe "the old CLI for AIRLOCK_DEMO_TOKEN exited $old_status and said: $(printf '%s' "$old_out" | tail -n 2 | tr '\n' ' ')"
	judge "$([ "$old_status" -ne 0 ] && ! contains "THE-COMMAND-RAN" "$old_out" && echo 0 || echo 1)" \
		"the old CLI exits without running the command" "exit $old_status, and its output does not hold the command's own line"
	judge "$(contains "no value" "$old_out" && echo 0 || echo 1)" \
		"the old CLI says that no value was released" "it said: $(printf '%s' "$old_out" | tail -n 1)"

	# The negative control: a secret the rules do not name.
	local plain_value
	plain_value="the-control-airlock-plain-token-$RANDOM$RANDOM"
	add_fake_secret "${STACK_SECRETS_PREFIX}airlock-plain-token" \
		'{"owner":"legion-daemon","tier":"agent"}' "$plain_value"
	reread_all AIRLOCK_PLAIN_TOKEN
	release AIRLOCK_PLAIN_TOKEN 30m
	local plain_values plain_airlock released
	plain_values="$(printf '%s' "$RELEASE_JSON" | jq -c '.values // {} | keys')"
	plain_airlock="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock // "null"')"
	released="$(printf '%s' "$RELEASE_JSON" | jq -r '.values.AIRLOCK_PLAIN_TOKEN // ""')"
	control "for a secret the rules do not name the answer's values keys are $plain_values, its airlock field is ${plain_airlock:0:4}, and values.AIRLOCK_PLAIN_TOKEN is $(shape "$released") against the stored $(shape "$plain_value") (compared by SHA-256 and length)"
	set +e
	old_out="$("$OLD_CLI" AIRLOCK_PLAIN_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
	set -e
	control "the old CLI for that name exited $old_status and its output holds the command's own line: $(contains "THE-COMMAND-RAN" "$old_out" && echo yes || echo no)"
	judge "$([ "$released" = "$plain_value" ] && contains "THE-COMMAND-RAN" "$old_out" && echo 0 || echo 1)" \
		"negative control: the answer carries the value under the name and the old CLI runs its command" \
		"values.AIRLOCK_PLAIN_TOKEN matches the stored value by SHA-256 and length, and the old CLI exited $old_status having run the command"

	stop_stack
}

###############################################################################################
# k3
###############################################################################################

check_k3() {
	expect "S gets the real value only in Authorization and X-Key of GET /read"
	expect "X-Other, the query and the body reach S with the placeholder"
	expect "POST /read and GET /other get an error that names the rule, and S records neither"
	expect "T records the placeholder"
	expect "the request whose TLS name and Host name T is refused with an error that names the mismatch, and neither S nor T records a value"
	expect "the request that already carries the value is refused with an error that names it, and S records nothing"
	expect "the broker's log has a line for each call to S, allowed or refused with its reason"
	expect "the negative control, with the entry's allow removed, lets the POST and the GET of /other reach S with the real value in Authorization"

	start_stack
	stack_facts
	local scoped_value s_before t_before log_before
	scoped_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-scoped-token")"
	s_before="$(records_count s)"
	t_before="$(records_count t)"
	log_before="$(grep -h 'a call to a bound host' "$AIRLOCK_DIR"/broker*.log 2>/dev/null | wc -l)"

	cat >"$WORK_DIR/k3-steps.sh" <<EOF
set -u
P="\$AIRLOCK_SCOPED_TOKEN"
out="$WORK_DIR/k3-out"
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "authorization=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/read)"
say "xkey=\$(curl -sS -o /dev/null -w %{http_code} -H "X-Key: \$P" $AIRLOCK_S_URL/read)"
say "xother=\$(curl -sS -o /dev/null -w %{http_code} -H "X-Other: \$P" $AIRLOCK_S_URL/read)"
say "query=\$(curl -sS -o /dev/null -w %{http_code} "$AIRLOCK_S_URL/read?k=\$P")"
say "getbody=\$(curl -sS -o /dev/null -w %{http_code} -X GET --data "k=\$P" $AIRLOCK_S_URL/read)"
say "postbody=\$(curl -sS -o /dev/null -w %{http_code} -X POST --data "k=\$P" $AIRLOCK_S_URL/read)"
say "postbody_error=\$(curl -sS -X POST --data "k=\$P" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "post=\$(curl -sS -o /dev/null -w %{http_code} -X POST -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/read)"
say "post_error=\$(curl -sS -X POST -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "other=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/other)"
say "other_error=\$(curl -sS -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/other 2>&1 | tr '\n' ' ')"
say "t=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$P" $AIRLOCK_T_URL/read)"
say "mismatch_sni=\$(curl -sS --connect-to t.airlock.test:443:s.airlock.test:443 -H "Authorization: Bearer \$P" $AIRLOCK_T_URL/read 2>&1 | tr '\n' ' ')"
say "mismatch_host=\$(curl -sS -H "Host: t.airlock.test" -H "Authorization: Bearer \$P" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "carries=\$(curl -sS -H "X-Key: \$(cat $WORK_DIR/k3-real)" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
EOF
	printf '%s' "$scoped_value" >"$WORK_DIR/k3-real"
	chmod 600 "$WORK_DIR/k3-real"
	agent-secrets AIRLOCK_SCOPED_TOKEN -- bash "$WORK_DIR/k3-steps.sh" >"$WORK_DIR/k3-cli.log" 2>&1 || true
	rm -f "$WORK_DIR/k3-real"
	# shellcheck disable=SC1090
	declare -A K3
	while IFS='=' read -r key value; do K3["$key"]="$value"; done <"$WORK_DIR/k3-out"

	local recs
	recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	local auth_hits key_hits other_hits query_hits body_hits post_records other_records
	auth_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.path == "/read" and .method == "GET") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	key_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.path == "/read" and .method == "GET") | .header["X-Key"] // [] | .[] | select(contains($v))] | length')"
	other_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | .header["X-Other"] // [] | .[] | select(contains($v))] | length')"
	query_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.query | contains($v))] | length')"
	body_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.body | contains($v))] | length')"
	post_records="$(printf '%s' "$recs" | jq '[.[] | select(.method == "POST")] | length')"
	other_records="$(printf '%s' "$recs" | jq '[.[] | select(.path == "/other")] | length')"
	local xother_ph query_ph body_ph
	xother_ph="$(printf '%s' "$recs" | jq -r '[.[] | .header["X-Other"] // [] | .[]] | .[0] // ""')"
	query_ph="$(printf '%s' "$recs" | jq -r '[.[] | select(.query != "") | .query] | .[0] // ""')"
	body_ph="$(printf '%s' "$recs" | jq -r '[.[] | select(.body != "") | .body] | .[0] // ""')"
	local t_recs t_ph
	t_recs="$(records t | jq --argjson n "$t_before" '.[$n:]')"
	t_ph="$(printf '%s' "$t_recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"

	observe "status by step: Authorization ${K3[authorization]}, X-Key ${K3[xkey]}, X-Other ${K3[xother]}, query ${K3[query]}, GET /read with a body ${K3[getbody]}, POST /read with a body ${K3[postbody]}, POST /read ${K3[post]}, GET /other ${K3[other]}, T ${K3[t]}"
	observe "S recorded $(printf '%s' "$recs" | jq length) requests: the real value (by SHA-256 and length, $(shape "$scoped_value")) in $auth_hits Authorization and $key_hits X-Key of GET /read, and in $other_hits X-Other, $query_hits queries and $body_hits bodies"
	observe "what X-Other, the query and the body actually carried: $(shape "$xother_ph"), $(shape "$query_ph"), $(shape "$body_ph") — the placeholder, not the value"
	observe "S recorded $post_records POST and $other_records /other requests"
	observe "how the body step is read: the entry's allow is GET /read alone, and the same check requires POST /read to be refused, so the body carrying the placeholder is read off the GET of /read that carries one; the POST of /read with a placeholder body is refused with the other POST"
	observe "how the mismatch step is read: the proxy answers the CONNECT to S with a leaf for S, so a client whose TLS name is T fails the handshake and nothing is sent; the same request with the Host header alone changed reaches the proxy and is refused 'airlock: host header mismatch'"
	observe "the refusals: POST /read said ${K3[post_error]}; GET /other said ${K3[other_error]}; the TLS-name and Host mismatch said ${K3[mismatch_sni]}; a Host-only mismatch said ${K3[mismatch_host]}; a request already carrying the value said ${K3[carries]}"
	observe "T recorded $(printf '%s' "$t_recs" | jq length) requests and its Authorization was $(shape "$t_ph")"

	judge "$([ "$auth_hits" = 1 ] && [ "$key_hits" = 1 ] && [ "$other_hits" = 0 ] && [ "$query_hits" = 0 ] && [ "$body_hits" = 0 ] && echo 0 || echo 1)" \
		"S gets the real value only in Authorization and X-Key of GET /read" \
		"Authorization $auth_hits, X-Key $key_hits, X-Other $other_hits, query $query_hits, body $body_hits (compared by SHA-256 and length)"
	judge "$(contains "agent-secrets-placeholder" "$xother_ph" && contains "agent-secrets-placeholder" "$query_ph" && contains "agent-secrets-placeholder" "$body_ph" && echo 0 || echo 1)" \
		"X-Other, the query and the body reach S with the placeholder" \
		"each carried the grant's placeholder: X-Other $(shape "$xother_ph"), query $(shape "$query_ph"), body $(shape "$body_ph")"
	judge "$(contains "not allowed by rule" "${K3[post_error]}" && contains "not allowed by rule" "${K3[other_error]}" && [ "$post_records" = 0 ] && [ "$other_records" = 0 ] && echo 0 || echo 1)" \
		"POST /read and GET /other get an error that names the rule, and S records neither" \
		"both answered ${K3[post]} and ${K3[other]} with 'not allowed by rule'; S recorded $post_records POST and $other_records /other"
	judge "$(contains "agent-secrets-placeholder" "$t_ph" && echo 0 || echo 1)" \
		"T records the placeholder" "T's Authorization was $(shape "$t_ph"), the grant's placeholder"
	judge "$( { contains "t.airlock.test" "${K3[mismatch_sni]}" || contains "mismatch" "${K3[mismatch_sni]}"; } && [ "$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.host == "t.airlock.test")] | length')" = 0 ] && echo 0 || echo 1)" \
		"the request whose TLS name and Host name T is refused with an error that names the mismatch, and neither S nor T records a value" \
		"it was refused: ${K3[mismatch_sni]}; the same request with the Host alone changed was refused ${K3[mismatch_host]}"
	judge "$(contains "already carries the value" "${K3[carries]}" && echo 0 || echo 1)" \
		"the request that already carries the value is refused with an error that names it, and S records nothing" \
		"it was refused: ${K3[carries]}"

	# Thirteen of the fourteen requests this command makes reach the proxy on a CONNECT to a
	# bound host and so log a line: the one that does not is the TLS-name mismatch, whose
	# handshake fails before a request exists. The run also checks that both a carried and a
	# refused call are among the lines, each with its reason.
	local log_after log_lines allowed refused
	log_after="$(grep -h 'a call to a bound host' "$AIRLOCK_DIR"/broker*.log 2>/dev/null | wc -l)"
	log_lines="$(grep -h 'a call to a bound host' "$AIRLOCK_DIR"/broker*.log | tail -n 3 | sed 's/^/    /')"
	allowed="$(grep -h 'a call to a bound host' "$AIRLOCK_DIR"/broker*.log | grep -c 'allowed=true')"
	refused="$(grep -h 'a call to a bound host' "$AIRLOCK_DIR"/broker*.log | grep -c 'allowed=false.*reason=')"
	observe "the brokers logged $((log_after - log_before)) lines for a call to a bound host during this run, $allowed of them allowed and $refused refused with a reason; the last three:"
	observe "$log_lines"
	judge "$([ "$((log_after - log_before))" -ge 13 ] && [ "$allowed" -gt 0 ] && [ "$refused" -gt 0 ] && echo 0 || echo 1)" \
		"the broker's log has a line for each call to S, allowed or refused with its reason" \
		"$((log_after - log_before)) lines for the 13 requests of this command that reached the proxy, $allowed allowed and $refused refused with a reason"

	# The negative control. The stack's AIRLOCK_DEMO_TOKEN entry is the scoped entry with its
	# allow removed: the same host and the authorization header, and no allow list.
	local demo_value s_mark
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	s_mark="$(records_count s)"
	cat >"$WORK_DIR/k3-control.sh" <<EOF
set -u
out="$WORK_DIR/k3-control-out"
: >"\$out"
printf 'post=%s\n' "\$(curl -sS -o /dev/null -w %{http_code} -X POST -H "Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/read)" >>"\$out"
printf 'other=%s\n' "\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/other)" >>"\$out"
EOF
	agent-secrets AIRLOCK_DEMO_TOKEN -- bash "$WORK_DIR/k3-control.sh" >>"$WORK_DIR/k3-cli.log" 2>&1 || true
	local crecs cpost cother
	crecs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	cpost="$(printf '%s' "$crecs" | jq --arg v "$demo_value" '[.[] | select(.method == "POST" and .path == "/read") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	cother="$(printf '%s' "$crecs" | jq --arg v "$demo_value" '[.[] | select(.path == "/other") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	control "with an entry that has no allow (the stack's AIRLOCK_DEMO_TOKEN, the same host and header as the scoped entry), $(cat "$WORK_DIR/k3-control-out" | tr '\n' ' ')"
	control "S recorded the real value (by SHA-256 and length) in $cpost POST /read and $cother GET /other"
	judge "$([ "$cpost" = 1 ] && [ "$cother" = 1 ] && echo 0 || echo 1)" \
		"negative control: with the entry's allow removed, the POST and the GET of /other reach S with the real value in Authorization" \
		"POST /read $cpost, GET /other $cother, both carrying the stored value by SHA-256 and length"

	stop_stack
}

###############################################################################################
# k4
###############################################################################################

check_k4() {
	expect "the call to S goes through the proxy port and S's certificate comes from the proxy's authority"
	expect "T presents its own certificate and gets the bytes the command sent"
	expect "the .svc and .ts.net calls and the 10.x calls of curl and of a Go program do not reach the proxy port"
	expect "Node's 10.x call reaches the proxy and is refused with an error that names the private address"
	expect "the connections to 169.254.169.254 and the broker's own loopback port are refused with an error that names the rule"
	expect "both bad proxy passwords, the call after the grant ends and the call after the withholding get 407, and nothing is tunnelled"
	expect "the negative control, with the proxy's refusal of loopback and link-local addresses turned off for it, tunnels the connection to the broker's loopback port"

	start_stack
	stack_facts

	# A stand-in on a .svc name of its own, which the stack does not have. It is this run's
	# own: one Python HTTP server on loopback, reached by the .svc name with curl --resolve,
	# because a Legion pod's /etc/hosts cannot be written.
	local svc_port
	svc_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	mkdir -p "$WORK_DIR/svc"
	echo "the .svc stand-in answered" >"$WORK_DIR/svc/index.html"
	(cd "$WORK_DIR/svc" && exec python3 -m http.server "$svc_port" --bind 127.0.0.1 >"$WORK_DIR/svc.log" 2>&1) &
	HELPER_PIDS+=($!)

	# A Go program of this run's own, built outside the repository so no build of this check
	# touches the workspace's go.work.sum.
	mkdir -p "$WORK_DIR/goprobe"
	cat >"$WORK_DIR/goprobe/go.mod" <<'EOF'
module airlockcheck

go 1.24
EOF
	cat >"$WORK_DIR/goprobe/main.go" <<'EOF'
// A client that reads the proxy from the environment as any Go program does, so what it does
// with a private address literal is what a Go consumer behind the airlock would do.
package main

import (
	"fmt"
	"net/http"
	"time"
)

func main() {
	start := time.Now()
	c := &http.Client{Timeout: 8 * time.Second}
	r, err := c.Get("https://10.11.12.13/")
	if err != nil {
		fmt.Printf("error after %dms: %v\n", time.Since(start).Milliseconds(), err)
		return
	}
	fmt.Printf("status %d after %dms\n", r.StatusCode, time.Since(start).Milliseconds())
}
EOF
	(cd "$WORK_DIR/goprobe" && GOTOOLCHAIN=local GOFLAGS=-mod=mod go build -o "$WORK_DIR/goprobe/goprobe" .) >"$WORK_DIR/goprobe/build.log" 2>&1

	local s_before t_before
	s_before="$(records_count s)"
	t_before="$(records_count t)"

	cat >"$WORK_DIR/k4-steps.sh" <<EOF
set -u
out="$WORK_DIR/k4-out"
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "no_proxy=\$NO_PROXY"
curl -sS -o /dev/null -w "%{certs}" -H "Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/ok >"$WORK_DIR/s-leaf.pem"
say "s_status=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/ok)"
curl -sS -o /dev/null -w "%{certs}" $AIRLOCK_T_URL/read >"$WORK_DIR/t-leaf.pem"
say "t_body=\$(curl -sS -X POST --data 'the-bytes-the-command-sent' $AIRLOCK_T_URL/echo | tr '\n' ' ')"
say "svc=\$(curl -sS --resolve stand.default.svc:$svc_port:127.0.0.1 --max-time 10 http://stand.default.svc:$svc_port/ 2>&1 | tr '\n' ' ')"
say "svc_exit=\$?"
say "tsnet=\$(curl -sS --max-time 10 https://host.tailnet-example.ts.net/ 2>&1 | tr '\n' ' ')"
say "tsnet_exit=\$?"
say "curl10=\$(curl -sS --max-time 10 https://10.11.12.13/ 2>&1 | tr '\n' ' ')"
say "curl10_exit=\$?"
say "go10=\$($WORK_DIR/goprobe/goprobe 2>&1 | tr '\n' ' ')"
node -e '
const t = Date.now();
fetch("https://10.11.12.13/").then(r => console.log("status", r.status, Date.now() - t))
  .catch(e => { const p = []; let c = e.cause; while (c) { p.push((c.code || "") + ":" + (c.message || "")); c = c.cause; } console.log("ms", Date.now() - t, "|", e.message, "|", p.join(" <- ")); });
' >"$WORK_DIR/node10.txt" 2>&1
say "node10=\$(tr '\n' ' ' <"$WORK_DIR/node10.txt")"
say "private=\$(curl -sS --noproxy '' --proxy "\$HTTPS_PROXY" http://10.11.12.13/ 2>&1 | tr '\n' ' ')"
say "metadata=\$(curl -sS --noproxy '' --proxy "\$HTTPS_PROXY" http://169.254.169.254/ 2>&1 | tr '\n' ' ')"
say "broker_port=\$(curl -sS --noproxy '' --proxy "\$HTTPS_PROXY" http://${BROKER_API_ADDRS[0]}/healthz 2>&1 | tr '\n' ' ')"
say "broker_connect=\$(curl -sS --noproxy '' --proxy "\$HTTPS_PROXY" https://${BROKER_API_ADDRS[0]}/healthz 2>&1 | tr '\n' ' ')"
say "no_password=\$(curl -sS --noproxy '' --proxy "http://\${HTTPS_PROXY##*@}" $AIRLOCK_S_URL/ok 2>&1 | tr '\n' ' ')"
say "made_up=\$(curl -sS --noproxy '' --proxy "\${HTTPS_PROXY%@*}-made-up@\${HTTPS_PROXY##*@}" $AIRLOCK_S_URL/ok 2>&1 | tr '\n' ' ')"
EOF
	agent-secrets AIRLOCK_DEMO_TOKEN -- bash "$WORK_DIR/k4-steps.sh" >"$WORK_DIR/k4-cli.log" 2>&1 || true
	declare -A K4
	while IFS='=' read -r key value; do K4["$key"]="$value"; done <"$WORK_DIR/k4-out"

	local s_leaf t_leaf s_own t_own s_issuer
	s_leaf="$(openssl x509 -in "$WORK_DIR/s-leaf.pem" -noout -fingerprint -sha256 | sed 's/.*=//')"
	t_leaf="$(openssl x509 -in "$WORK_DIR/t-leaf.pem" -noout -fingerprint -sha256 | sed 's/.*=//')"
	s_own="$(openssl x509 -in "$AIRLOCK_DIR/stands/s.crt" -noout -fingerprint -sha256 | sed 's/.*=//')"
	t_own="$(openssl x509 -in "$AIRLOCK_DIR/stands/t.crt" -noout -fingerprint -sha256 | sed 's/.*=//')"
	s_issuer="$(openssl x509 -in "$WORK_DIR/s-leaf.pem" -noout -issuer)"
	local s_recs t_recs
	s_recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	t_recs="$(records t | jq --argjson n "$t_before" '.[$n:]')"

	observe "the command's NO_PROXY is ${K4[no_proxy]}"
	observe "S's certificate as the command saw it is $s_leaf and S's own certificate file is $s_own: a different leaf, signed by $s_issuer, which is the proxy minting one of its own"
	observe "T's certificate as the command saw it is $t_leaf and T's own certificate file is $t_own"
	observe "T answered the POST with: ${K4[t_body]}; T's last record's body is $(printf '%s' "$t_recs" | jq -r '[.[] | select(.body != "") | .body] | .[0] // ""')"
	observe ".svc: ${K4[svc]}; .ts.net: ${K4[tsnet]}; curl 10.x: ${K4[curl10]}; Go 10.x: ${K4[go10]}; Node 10.x: ${K4[node10]}"
	observe "169.254.169.254 through the proxy: ${K4[metadata]}"
	observe "the broker's own loopback port through the proxy: ${K4[broker_port]} (and on a CONNECT: ${K4[broker_connect]})"
	observe "the same private address asked of the proxy with curl: ${K4[private]}"
	observe "no proxy password: ${K4[no_password]}; a made-up one: ${K4[made_up]}"

	judge "$([ "${K4[s_status]}" = 200 ] && [ "$s_leaf" != "$s_own" ] && contains "dev-airlock-stands authority" "$s_issuer" && echo 0 || echo 1)" \
		"the call to S goes through the proxy port and S's certificate comes from the proxy's authority" \
		"S answered ${K4[s_status]} and presented a leaf the stand never wrote ($s_leaf against the stand's own $s_own), signed by the authority the proxy holds the key of"
	judge "$([ "$t_leaf" = "$t_own" ] && contains "the-bytes-the-command-sent" "$(printf '%s' "$t_recs" | jq -r '[.[] | .body] | join(" ")')" && echo 0 || echo 1)" \
		"T presents its own certificate and gets the bytes the command sent" \
		"T's leaf is its own certificate file, and its record holds the bytes the command POSTed"
	judge "$(! contains "CONNECT tunnel failed" "${K4[svc]}" && contains "the .svc stand-in answered" "${K4[svc]}" && ! contains "CONNECT tunnel failed" "${K4[tsnet]}" && ! contains "CONNECT tunnel failed" "${K4[curl10]}" && ! contains "Proxy response" "${K4[go10]}" && echo 0 || echo 1)" \
		"the .svc and .ts.net calls and the 10.x calls of curl and of a Go program do not reach the proxy port" \
		".svc reached this run's own stand-in directly; .ts.net, curl's 10.x and Go's 10.x failed to connect or resolve with no answer from a proxy"
	judge "$(contains "Proxy response (403)" "${K4[node10]}" && contains "private" "${K4[private]}" && echo 0 || echo 1)" \
		"Node's 10.x call reaches the proxy and is refused with an error that names the private address" \
		"undici reported '${K4[node10]}', so the proxy answered its CONNECT 403; undici does not show a proxy's body, and the same address asked of the proxy with curl is refused '${K4[private]}'"
	judge "$(contains "link-local" "${K4[metadata]}" && contains "loopback" "${K4[broker_port]}" && echo 0 || echo 1)" \
		"the connections to 169.254.169.254 and the broker's own loopback port are refused with an error that names the rule" \
		"169.254.169.254: ${K4[metadata]}; the broker's port: ${K4[broker_port]}"

	# The three 407s, each driven with a release of its own: the CLI never builds a wrong proxy
	# password, so these are the grant's own release and curl.
	local s_mark short_now short_later withheld_before withheld_after
	s_mark="$(records_count s)"
	release AIRLOCK_DEMO_TOKEN 1m
	short_now="$(curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H "Authorization: Bearer $PLACEHOLDER" "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	sleep 70
	short_later="$(curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H "Authorization: Bearer $PLACEHOLDER" "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"

	release AIRLOCK_DEMO_TOKEN 30m
	withheld_before="$(curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H "Authorization: Bearer $PLACEHOLDER" "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	# Withholding a name from a session is an operator's act, and a pod session has no
	# operator, so the row the broker reads is written here directly.
	psql_stack -c "insert into withheld_secrets (enrollment_id, name) values ('$AIRLOCK_ENROLLMENT', 'AIRLOCK_DEMO_TOKEN')" >/dev/null
	withheld_after="$(curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H "Authorization: Bearer $PLACEHOLDER" "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	psql_stack -c "delete from withheld_secrets where enrollment_id = '$AIRLOCK_ENROLLMENT'" >/dev/null
	local tunnelled
	tunnelled="$(($(records_count s) - s_mark))"

	observe "a one-minute grant: at once '$short_now', after 70 seconds '$short_later'"
	observe "withholding the name mid-grant: before '$withheld_before', after '$withheld_after'"
	observe "S recorded $tunnelled requests across the four refused calls and the two that were meant to succeed"
	judge "$(contains "407" "${K4[no_password]}" && contains "407" "${K4[made_up]}" && contains "407" "$short_later" && contains "407" "$withheld_after" && [ "$tunnelled" = 2 ] && echo 0 || echo 1)" \
		"both bad proxy passwords, the call after the grant ends and the call after the withholding get 407, and nothing is tunnelled" \
		"no password and a made-up one were refused 407; the call after the grant ended was '$short_later' and the one after the withholding '$withheld_after'; S recorded only the $tunnelled calls that were meant to succeed"

	# The negative control: one more broker, on the stack's own database and rules, whose
	# refusal of loopback is turned off for the first broker's API port alone.
	start_extra_broker loopback-allowed \
		BROKER_AIRLOCK_TEST_DIAL_ALLOW="$STACK_DIAL_ALLOW ${BROKER_API_ADDRS[0]}"
	release AIRLOCK_DEMO_TOKEN 30m
	local control_answer
	control_answer="$(curl -sS --noproxy '' --proxy "$(proxy_for "$EXTRA_PROXY_ADDR")" "http://${BROKER_API_ADDRS[0]}/healthz" 2>&1 | tr '\n' ' ')"
	control "a second broker on the same database and rules, started with the first broker's address in BROKER_AIRLOCK_TEST_DIAL_ALLOW — the setting that turns the refusal of a loopback address off for one host — answered the same request: $control_answer"
	judge "$(! contains "loopback" "$control_answer" && echo 0 || echo 1)" \
		"negative control: with the refusal turned off the connection to the broker's loopback port is tunnelled" \
		"the same request through that broker answered '$control_answer', where the stack's own brokers answer '${K4[broker_port]}'"

	stop_stack
}

###############################################################################################
# k5
###############################################################################################

k5_call() { # <placeholder> -> prints the status
	curl -sS -o /dev/null -w '%{http_code}' --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
		--proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H "Authorization: Bearer $1" "$AIRLOCK_S_URL/ok"
}

# k5_stored drives the stored half of k5 against the stack this run started and answers through
# K5_* variables. keep says whether the stack was started with --keep-values.
k5_stored() {
	local s_mark first second new_value old_value
	old_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	s_mark="$(records_count s)"
	release AIRLOCK_DEMO_TOKEN 30m
	first="$(k5_call "$PLACEHOLDER")"
	new_value="the-second-version-of-the-airlock-demo-token-$RANDOM$RANDOM"
	set_fake_value "${STACK_SECRETS_PREFIX}airlock-demo-token" "$new_value"
	reread_all AIRLOCK_DEMO_TOKEN
	sleep 2
	second="$(k5_call "$PLACEHOLDER")"
	local recs got_first got_second
	recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	got_first="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"
	got_second="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[1] // ""')"
	K5_FIRST_STATUS="$first"
	K5_SECOND_STATUS="$second"
	K5_FIRST_VALUE="$got_first"
	K5_SECOND_VALUE="$got_second"
	K5_OLD_VALUE="$old_value"
	K5_NEW_VALUE="$new_value"
}

check_k5() {
	expect "with a grant of a stored secret behind the airlock, the call after a new version of the secret and the broker's reread carries the new value, and every call succeeds"
	expect "with a google-login secret whose stand-in minter issues tokens of 7 minutes, the call 3 minutes after the first carries a freshly minted value"
	expect "the negative control, with the proxy's value kept across the reread and past the 5-minute margin, carries the old value on the second call"

	# One broker, not the stack's two. What this check reads is a pod's own held value: the
	# first call makes the pod hold it, the reread is what ends that, and the second call must
	# read the store again. With two brokers the front gives each connection whichever it
	# pleases, so a second call landing on the pod that never held the value reads the new one
	# whatever the reread did - the check would pass with the drop removed, and its negative
	# control would fail with the drop in place. Neither the check's words nor its control
	# names a second process.
	start_stack --one-broker
	stack_facts
	k5_stored
	observe "the stored secret: the first call answered $K5_FIRST_STATUS carrying $(shape "$K5_FIRST_VALUE") and the second $K5_SECOND_STATUS carrying $(shape "$K5_SECOND_VALUE") (compared by SHA-256 and length)"
	observe "the store held $(shape "$K5_OLD_VALUE") and then $(shape "$K5_NEW_VALUE")"
	# The recorded header is "Bearer <value>", so the comparison is that it carries the value
	# and not that it equals it.
	judge "$(contains "$K5_OLD_VALUE" "$K5_FIRST_VALUE" && contains "$K5_NEW_VALUE" "$K5_SECOND_VALUE" && [ "$K5_FIRST_STATUS" = 200 ] && [ "$K5_SECOND_STATUS" = 200 ] && echo 0 || echo 1)" \
		"the stored secret's second call carries the new value, and every call succeeds" \
		"the first call carried the old value and the second the new one, by SHA-256 and length; the two answered $K5_FIRST_STATUS and $K5_SECOND_STATUS"

	# The google-login half. The stack's Google stand answers one fixed access token and a
	# lifetime of an hour, so nothing here can make the proxy mint again after three minutes,
	# and two mints could not be told apart if it did.
	local google_before google_after first_status second_status s_mark got_first got_second recs
	google_before="$(records_count google)"
	s_mark="$(records_count s)"
	release AIRLOCK_GOOGLE_LOGIN 30m
	first_status="$(k5_call "$PLACEHOLDER")"
	sleep 180
	second_status="$(k5_call "$PLACEHOLDER")"
	google_after="$(records_count google)"
	recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	got_first="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"
	got_second="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[1] // ""')"
	observe "the google-login secret: the call at once answered $first_status carrying $(shape "$got_first"), and the one three minutes later $second_status carrying $(shape "$got_second"); the stand-in minter was asked $((google_after - google_before)) times"
	if [ "$got_first" = "$got_second" ]; then
		fail "the google-login secret's second call carries the new value — this step cannot run on this stack. dev-airlock.sh's Google stand answers one fixed access token ('the-stand-in-google-access-token') with a lifetime of an hour, so the proxy's five-minute margin never falls due within three minutes, and two mints could not be told apart if it did. It needs dev-airlock.sh to pass --google-lifetime 7m to dev-airlock-stands and airlocktest.GoogleStand to answer a different token for each mint. Here the minter was asked $((google_after - google_before)) times, both calls answered $first_status and $second_status, and the two carried one value by SHA-256 and length."
	else
		judge "$([ "$first_status" = 200 ] && [ "$second_status" = 200 ] && echo 0 || echo 1)" \
			"the google-login secret's second call carries the new value, and every call succeeds" \
			"the minter was asked $((google_after - google_before)) times and the two calls carried different values by SHA-256 and length"
	fi

	stop_stack

	# The negative control, which is a stack of its own: --keep-values is the switch that keeps
	# a grant's values across a reread and past a minted token's margin.
	start_stack --one-broker --keep-values
	stack_facts
	k5_stored
	control "with --keep-values (BROKER_AIRLOCK_TEST_KEEP_VALUES=true) the first call answered $K5_FIRST_STATUS carrying $(shape "$K5_FIRST_VALUE") and the second $K5_SECOND_STATUS carrying $(shape "$K5_SECOND_VALUE"); the store held $(shape "$K5_OLD_VALUE") and then $(shape "$K5_NEW_VALUE")"
	judge "$(contains "$K5_OLD_VALUE" "$K5_SECOND_VALUE" && echo 0 || echo 1)" \
		"negative control: with the proxy's value kept across the reread, the second call carries the old value" \
		"the second call carried the old value by SHA-256 and length, where the same run without the switch carried the new one"

	stop_stack
}

###############################################################################################
# k8a
###############################################################################################

# k8a_start_broker starts a broker on one rules file and answers 0 when it refused to start for
# that file, leaving why in K8A_WHY. It is given everything else the broker needs - the stack's
# own database, store and sign-in client - so that what it refuses on is the rules file and not
# a setting this function left out.
k8a_start_broker() { # <rules file> <label>
	local log="$WORK_DIR/k8a-broker-$2.log"
	set +e
	env BROKER_DATABASE_URL="$STACK_DB_URL" \
		BROKER_PUBLIC_URL="http://127.0.0.1:0" \
		BROKER_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
		BROKER_SECRETS_PREFIX="$STACK_SECRETS_PREFIX" \
		BROKER_SECRETS_KMS_KEY_ARN="$STACK_KMS_KEY_ARN" \
		BROKER_FAKE_SECRETS_FILE="$AIRLOCK_FAKE_SECRETS_FILE" \
		BROKER_SIGNIN_ISSUER="$AIRLOCK_SIGNIN_URL" \
		BROKER_SIGNIN_CLIENT_ID="dev-broker" \
		BROKER_SIGNIN_CLIENT_SECRET_FILE="$DEV_BROKER_DIR/signin-client-secret" \
		BROKER_SIGNIN_DOMAIN="${AIRLOCK_APPROVER#*@}" \
		BROKER_AIRLOCK_RULES_FILE="$1" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_AIRLOCK_PROXY_URL="$AIRLOCK_PROXY_URL" \
		BROKER_AIRLOCK_CA_CERT_FILE="$AIRLOCK_CA_CERT_FILE" \
		BROKER_AIRLOCK_CA_KEY_FILE="$AIRLOCK_CA_KEY_FILE" \
		timeout 25 "$BROKER_BIN" >"$log" 2>&1
	local status=$?
	set -e
	K8A_WHY="$(grep -m1 'fatal' "$log" | tr '\n' ' ')"
	# The refusal has to be about this file: a broker that died of a setting this function did
	# not give it would otherwise read as a refusal of the rules.
	[ "$status" -ne 0 ] && [ -n "$K8A_WHY" ] && contains "$1" "$K8A_WHY"
}

check_k8a() {
	expect "the judge-1800 and the no-hosts copies exit non-zero naming the entry"
	expect "the malformed copy names the parse position"
	expect "a local broker refuses to start on each of the three failing copies"
	expect "the new-host, new-header and removed-entry copies exit 0 and list what they add or remove"
	expect "the negative control, the valid file against itself, exits 0 and lists no change"

	start_stack
	stack_facts
	local d="$WORK_DIR/k8a"
	mkdir -p "$d"
	cp "$AIRLOCK_RULES_FILE" "$d/valid.json"
	jq '.secrets[0].judge = 1800' "$d/valid.json" >"$d/judge-1800.json"
	jq '.secrets[0] += {judge: 300} | .secrets[0].hosts = []' "$d/valid.json" >"$d/no-hosts.json"
	jq '.secrets[0].hosts += ["extra.airlock.test"]' "$d/valid.json" >"$d/new-host.json"
	jq '.secrets[0] += {headers: ["authorization", "x-extra"]}' "$d/valid.json" >"$d/new-header.json"
	jq '.secrets = .secrets[1:]' "$d/valid.json" >"$d/removed-entry.json"
	printf '{"secrets": [{"name": "A", "hosts": ["a.airlock.test"]},]}' >"$d/malformed.json"

	local label status out
	declare -A K8A_STATUS K8A_OUT
	for label in judge-1800 no-hosts new-host new-header removed-entry malformed; do
		set +e
		out="$("$BROKER_BIN" airlock-check "$d/$label.json" --against "$d/valid.json" 2>&1)"
		status=$?
		set -e
		K8A_STATUS[$label]=$status
		K8A_OUT[$label]="$out"
		observe "$label: exit ${K8A_STATUS[$label]}; $(printf '%s' "$out" | head -n 3 | tr '\n' ' / ')"
	done

	judge "$([ "${K8A_STATUS[judge-1800]}" -ne 0 ] && contains 'AIRLOCK_DEMO_TOKEN' "${K8A_OUT[judge-1800]}" && [ "${K8A_STATUS[no-hosts]}" -ne 0 ] && contains 'AIRLOCK_DEMO_TOKEN' "${K8A_OUT[no-hosts]}" && echo 0 || echo 1)" \
		"the 1800 and no-hosts copies exit non-zero naming the entry" \
		"judge-1800 exited ${K8A_STATUS[judge-1800]} saying '${K8A_OUT[judge-1800]}'; no-hosts exited ${K8A_STATUS[no-hosts]} saying '${K8A_OUT[no-hosts]}'"
	judge "$([ "${K8A_STATUS[malformed]}" -ne 0 ] && contains 'byte' "${K8A_OUT[malformed]}" && echo 0 || echo 1)" \
		"the malformed copy names the parse position" "it exited ${K8A_STATUS[malformed]} saying '${K8A_OUT[malformed]}'"

	local refusals=0 why=""
	for label in judge-1800 no-hosts malformed; do
		if k8a_start_broker "$d/$label.json" "$label"; then
			refusals=$((refusals + 1))
			why="$why; $label: $K8A_WHY"
		else
			why="$why; $label: the broker did not refuse to start"
		fi
	done
	observe "a local broker on each failing copy${why}"
	judge "$([ "$refusals" = 3 ] && echo 0 || echo 1)" \
		"the local broker refuses to start on each failing copy" "$refusals of 3 refused"

	local listed=0
	for label in new-host new-header removed-entry; do
		if [ "${K8A_STATUS[$label]}" = 0 ] && printf '%s' "${K8A_OUT[$label]}" | grep -qE 'added|removed|→'; then
			listed=$((listed + 1))
		fi
	done
	judge "$([ "$listed" = 3 ] && echo 0 || echo 1)" \
		"the new-host, new-header and removed-entry copies exit 0 and list what they add or remove" \
		"new-host said '$(printf '%s' "${K8A_OUT[new-host]}" | tail -n 1)', new-header '$(printf '%s' "${K8A_OUT[new-header]}" | tail -n 1)', removed-entry '$(printf '%s' "${K8A_OUT[removed-entry]}" | tail -n 1)'"

	set +e
	out="$("$BROKER_BIN" airlock-check "$d/valid.json" --against "$d/valid.json" 2>&1)"
	status=$?
	set -e
	control "the valid file against itself exited $status saying: $(printf '%s' "$out" | tail -n 1)"
	judge "$([ "$status" = 0 ] && contains "nothing changed" "$out" && echo 0 || echo 1)" \
		"negative control: on the valid file the command exits 0 and lists no change" \
		"it exited $status and said '$(printf '%s' "$out" | tail -n 1)'"

	stop_stack
}

###############################################################################################
# k15
###############################################################################################

check_k15() {
	expect "the Lean model builds on the head"
	expect "its five theorems are there and are printed with their statements"
	expect "they depend on no sorry and on no axiom outside propext, Classical.choice and Quot.sound"
	expect "the comparison fuzz test passes for ten minutes"
	expect "the negative control, the six mutants of scripts/airlock-mutants.sh, is caught: each Go mutant fails the comparison and each model mutant fails lake build or that same comparison"

	if ! command -v lake >/dev/null 2>&1; then
		echo "airlock-check: k15 needs lake from the Lean toolchain; set ELAN_HOME and PATH" >&2
		exit 1
	fi

	local build_log="$WORK_DIR/lake-build.log"
	echo "airlock-check: building the Lean model..." >&2
	set +e
	(cd "$LEAN_DIR" && lake build) >"$build_log" 2>&1
	local build_status=$?
	set -e
	local model="$LEAN_DIR/.lake/build/bin/airlock-model"
	observe "lake build exited $build_status: $(tail -n 1 "$build_log")"
	judge "$([ "$build_status" = 0 ] && [ -x "$model" ] && echo 0 || echo 1)" \
		"the model builds" "lake build exited $build_status and wrote $model"

	# The statements, printed from a file of this run's own so nothing is added to the tree.
	cat >"$WORK_DIR/Statements.lean" <<'EOF'
import Airlock
#check @Airlock.T1
#check @Airlock.T1_structure
#check @Airlock.T2
#check @Airlock.T3
#check @Airlock.rewrite_some_iff_allow
EOF
	set +e
	(cd "$LEAN_DIR" && lake env lean "$WORK_DIR/Statements.lean") >"$WORK_DIR/statements.txt" 2>&1
	local statements_status=$?
	(cd "$LEAN_DIR" && lake env lean Axioms.lean) >"$WORK_DIR/axioms.txt" 2>&1
	local axioms_status=$?
	set -e
	echo
	echo "the model's theorem statements:"
	sed 's/^/  /' "$WORK_DIR/statements.txt"
	echo
	echo "the theorems' axioms:"
	sed 's/^/  /' "$WORK_DIR/axioms.txt"
	observe "lake env lean printed the five theorem statements (exit $statements_status) and their axioms (exit $axioms_status); both are above this block"
	# `#check @X` opens its line with "@Airlock.X" for a theorem with implicit arguments and
	# with "Airlock.X" for one without, and a statement runs over several lines, so the count
	# is of the lines that open one.
	local printed
	printed="$(grep -cE '^@?Airlock\.' "$WORK_DIR/statements.txt")"
	judge "$([ "$statements_status" = 0 ] && [ "$printed" = 5 ] && echo 0 || echo 1)" \
		"its five theorems are there and are printed with their statements" \
		"$printed of the five named theorems type-checked and printed"

	local bad
	bad="$(awk -F'[][]' '/^.Airlock\./ { printed++; split($2, a, ", "); for (i in a) if (a[i] != "propext" && a[i] != "Classical.choice" && a[i] != "Quot.sound") print "axiom outside the three: " a[i] } END { if (printed != 5) print "theorems printed: " printed+0 ", expected 5" }' "$WORK_DIR/axioms.txt")"
	observe "the axioms each theorem rests on: $(tr '\n' ' ' <"$WORK_DIR/axioms.txt")"
	judge "$([ "$axioms_status" = 0 ] && [ -z "$bad" ] && ! grep -q sorryAx "$WORK_DIR/axioms.txt" && echo 0 || echo 1)" \
		"the theorems depend on no sorry and on no axiom outside the three Lean admits" \
		"${bad:-every one of the five rests on propext, Classical.choice and Quot.sound alone, and none on sorryAx}"

	echo "airlock-check: fuzzing the Go airlock against the model for ten minutes..." >&2
	set +e
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -run FuzzAirlock ./internal/broker/airlock/) >"$WORK_DIR/seed-corpus.log" 2>&1
	local seed_status=$?
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -fuzz FuzzAirlock -fuzztime=600s -run FuzzAirlock ./internal/broker/airlock/) >"$WORK_DIR/fuzz.log" 2>&1
	local fuzz_status=$?
	set -e
	observe "the seed corpus comparison exited $seed_status: $(tail -n 2 "$WORK_DIR/seed-corpus.log" | tr '\n' ' ')"
	observe "ten minutes of fuzzing exited $fuzz_status: $(grep -E '^(ok|FAIL|--- FAIL|fuzz:)' "$WORK_DIR/fuzz.log" | tail -n 3 | tr '\n' ' ')"
	judge "$([ "$seed_status" = 0 ] && [ "$fuzz_status" = 0 ] && echo 0 || echo 1)" \
		"the comparison fuzz test passes for ten minutes" \
		"the seed corpus exited $seed_status and -fuzztime=600s exited $fuzz_status"

	echo "airlock-check: running the six mutants..." >&2
	set +e
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" bash "$SCRIPT_DIR/airlock-mutants.sh") >"$WORK_DIR/mutants.log" 2>&1
	local mutants_status=$?
	set -e
	echo
	echo "the mutants:"
	grep -E '^(PASS|FAIL):' "$WORK_DIR/mutants.log" | sed 's/^/  /'
	control "scripts/airlock-mutants.sh exited $mutants_status: $(grep -cE '^PASS:' "$WORK_DIR/mutants.log" || true) of 6 mutants were caught"
	control "$(grep -E '^(PASS|FAIL):' "$WORK_DIR/mutants.log" | tr '\n' ' ')"
	judge "$([ "$mutants_status" = 0 ] && [ "$(grep -cE '^PASS:' "$WORK_DIR/mutants.log" || true)" = 6 ] && echo 0 || echo 1)" \
		"negative control: each of the six mutants is caught" \
		"airlock-mutants.sh exited $mutants_status with $(grep -cE '^PASS:' "$WORK_DIR/mutants.log" || true) of 6 caught"
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run: a check must report every one of its properties, and a refused request, a command that
# exited 1 and a grep that found nothing are all ordinary answers a step reads. Everything the
# run needs before this point - its arguments, its work directory and its stack - is still
# under set -e, and await ends the run itself when a stack does not come up.
set +e

case "$CHECK_ID" in
k1) check_k1 ;;
k2) check_k2 ;;
k3) check_k3 ;;
k4) check_k4 ;;
k5) check_k5 ;;
k8a) check_k8a ;;
k15) check_k15 ;;
esac

report
