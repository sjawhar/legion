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
#     therefore a small Python program (scripts/airlock/memscan.py) that starts
#     `agent-secrets` itself and scans the descendant it started.
#   - k15 needs no stack at all: it builds the Lean model, prints its theorem statements and
#     their axioms, runs the comparison fuzz test for ten minutes, and runs every mutant
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
#        <id> is k1, k2, k3, k4, k5, k6, k7, k8a, k9 or k15. --keep-stack leaves the stack and
#        the work directory up for a person to look at a failure with. --help prints this usage.
#        A usage error exits 2.
#
# Needs bash, curl, jq, go, python3, openssl, node and psql; k2 and k7 also need gh (the last
# released agent-secrets comes from a GitHub release, and k6's and k7's consumers come from
# sjawhar/dotfiles), k6 and k7 need bun, wget, git, uv and the clients scripts/airlock-clients.sh
# installs (az, gcloud, pulumi and boto3), k9 needs git, and k15 needs lake from the Lean
# toolchain this environment names. DEV_BROKER_POSTGRES_URL is passed to dev-airlock.sh as it
# is, so a pod with no container runtime names its test server there.
#
# k9 is a long run: ten minutes of connections and two stacks, about twenty-five minutes in all.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
LEAN_DIR="$ENVOY_DIR/internal/broker/airlock/lean"

usage() {
	cat <<'EOF'
usage: airlock-check.sh <id> [--keep-stack]

Runs one before_merge check of dispatch://INFRA-1352/artifact/verification-md and prints, for
each property the check's outcome sentence names, what it expected beside what it got.

  <id>           k1, k2, k3, k4, k5, k6, k7, k8a, k9 or k15
  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

It exits 0 only when every property passed, 1 when one did not and 2 on a usage error. k9 takes
about twenty-five minutes; every other check takes a few.
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
k1 | k2 | k3 | k4 | k5 | k6 | k7 | k8a | k9 | k15) ;;
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
	# The one binary dev-airlock.sh builds of its own, under the work directory its banner named.
	# Three of this script's steps are subcommands of it.
	STANDS_BIN="$AIRLOCK_DIR/bin/dev-airlock-stands"
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

# broker_env answers, in BROKER_ENV, the command prefix that runs a program with the environment
# of a broker like the stack's own and the caller's overrides on top. It is read out of the first
# stack broker's /proc/<pid>/environ rather than spelled here, so that a broker this script
# starts can never lack a setting dev-airlock.sh gave the stack's. That script's own launcher is
# the one that has to spell the environment: it starts the first broker and has nothing to copy
# from. The entries are NUL-separated, and each becomes one argument of env, so a value holding
# a space or a quote reaches the broker unchanged; env applies its assignments in order, so an
# override the caller names replaces the copy.
broker_env() { # <VAR=value>...
	local pid
	pid="$(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG" | head -n 1)"
	local entries=()
	mapfile -d '' -t entries <"/proc/$pid/environ"
	BROKER_ENV=(env -i "${entries[@]}" "$@")
}

# start_extra_broker starts one more broker process beside the stack's, on its database, rules
# file, certificate authority and stand-ins, with the environment the caller adds on top, and
# answers its proxy address in EXTRA_PROXY_ADDR. It is how a control that turns one proxy setting
# off is run: the stack's own brokers keep the setting, and the command's HTTPS_PROXY is pointed
# at this one for that step alone.
start_extra_broker() { # <name> <VAR=value>...
	local name="$1"
	shift
	local log="$WORK_DIR/broker-$name.log"
	: >"$log"
	# Two ports the system picks, so this one never collides with the broker it was copied from.
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" "$@"
	"${BROKER_ENV[@]}" "$BROKER_BIN" >"$log" 2>&1 &
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
	python3 "$SCRIPT_DIR/airlock/memscan.py" "$dir/ready" "$dir/needles" "$dir/scan.json" "$dir" \
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
	github_app="$(stored_value "${STACK_SECRETS_PREFIX}airlock-github-app")"
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
	old_out="$("$OLD_CLI" AIRLOCK_DEMO_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
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
	old_out="$("$OLD_CLI" AIRLOCK_PLAIN_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
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
say "tsnet=\$(curl -sS --max-time 10 https://host.example-tailnet.ts.net/ 2>&1 | tr '\n' ' ')"
say "tsnet_exit=\$?"
say "curl10=\$(curl -sS --max-time 10 https://10.11.12.13/ 2>&1 | tr '\n' ' ')"
say "curl10_exit=\$?"
say "go10=\$($STANDS_BIN probe-private 2>&1 | tr '\n' ' ')"
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
# k6
###############################################################################################
#
# k6 drives real clients: the Azure CLI through sjawhar/dotfiles' scripts/az-broker, and the AWS
# CLI and boto3 against the stack's stand-in AWS service. A Legion issue pod has neither az nor
# boto3, so the check installs them with scripts/airlock-clients.sh, which pins a version and
# checks its SHA-256 before anything of it runs, and reads that script's exports.
#
# The stack's stand-ins play the two clouds whole, so both clients run as they do in anger: the
# Microsoft stand answers the tenant's OpenID configuration document and the token endpoint,
# the same host plays the Resource Manager an access token is spent at, and the AWS stand is
# told the session token the stack's own AWS secret holds, so a signature it verifies is one
# AWS would verify. az is driven through its own private cloud with MSAL's instance discovery
# turned off (AZURE_CORE_INSTANCE_DISCOVERY=false, the setting Azure Stack deployments use):
# that discovery asks login.microsoftonline.com whether a cloud's authority is a Microsoft one,
# which no stand-in authority can ever be, and it is the one az step no stand could answer.

# k6_clients reads the exports of scripts/airlock-clients.sh, which installs az and boto3 at a
# pinned, checked version. Its own lines go to this script's stderr; its stdout is the exports.
k6_clients() {
	echo "airlock-check: installing the pinned clients k6 drives (scripts/airlock-clients.sh)..." >&2
	local exports
	exports="$(bash "$SCRIPT_DIR/airlock-clients.sh")" || {
		echo "airlock-check: scripts/airlock-clients.sh could not install the clients" >&2
		exit 1
	}
	eval "$exports"
	CLIENT_VERSIONS="az $(az version --output json 2>/dev/null | jq -r '."azure-cli"'), boto3 $("$AIRLOCK_BOTO3_PYTHON" -c 'import boto3; print(boto3.__version__)' 2>/dev/null), aws $(aws --version 2>&1 | cut -d' ' -f1)"
}

# approve_record approves one pending request on the broker's own page, as the person does: the
# page sends a browser to the stack's issuer, which signs the approver in with no form to fill,
# and the decision is posted back with an Origin header, which the page requires. curl's own
# cookie jar cannot be used - both cookies are Secure and this broker is on http - so each
# Set-Cookie is read out of the headers and sent back by hand.
approve_record() { # <record id>
	local rec="$1" d="$WORK_DIR/approve"
	mkdir -p "$d"
	curl -sS -D "$d/start" -o /dev/null "$AGENT_SECRETS_URL/approve/$rec"
	local authorize signin callback session
	authorize="$(sed -n 's/^Location: //p' "$d/start" | tr -d '\r')"
	signin="$(sed -n 's/^Set-Cookie: \(__Host-broker-signin=[^;]*\).*/\1/p' "$d/start" | tr -d '\r')"
	[ -n "$authorize" ] || return 1
	curl -sS -D "$d/authorize" -o /dev/null "$authorize"
	callback="$(sed -n 's/^Location: //p' "$d/authorize" | tr -d '\r')"
	curl -sS -D "$d/callback" -o /dev/null -H "Cookie: $signin" "$callback"
	session="$(sed -n 's/^Set-Cookie: \(__Host-broker-session=[^;]*\).*/\1/p' "$d/callback" | tr -d '\r')"
	[ -n "$session" ] || return 1
	curl -sS -H "Cookie: $session" -o "$d/page.html" "$AGENT_SECRETS_URL/approve/$rec"
	local field
	field() { sed -n "s/.*name=\"$1\" value=\"\([^\"]*\)\".*/\1/p" "$d/page.html" | head -n 1; }
	curl -sS -o "$d/decided.html" -w '%{http_code}' -H "Cookie: $session" -H "Origin: $AGENT_SECRETS_URL" \
		--data-urlencode "decision=approve" --data-urlencode "record=$(field record)" \
		--data-urlencode "printed=$(field printed)" --data-urlencode "token=$(field token)" \
		--data-urlencode "expires=$(field expires)" "$AGENT_SECRETS_URL/approve" >>"$d/decisions"
	echo >>"$d/decisions"
}

# approve_watch is the approver sitting at the page while a command waits: it reads the record
# ids out of the CLI's own stderr, where each pending request prints its page, and approves each
# one once. A pod session has no operator and the broker takes this decision on its page alone,
# so this is the only way a human-tier name is released to a check.
approve_watch() { # <the file the CLI's stderr goes to>
	local log="$1" seen=" " rec
	while :; do
		for rec in $(grep -o '/approve/[0-9a-f]\{64\}' "$log" 2>/dev/null | sed 's|/approve/||' | sort -u); do
			case "$seen" in *" $rec "*) continue ;; esac
			seen="$seen$rec "
			approve_record "$rec" >/dev/null 2>&1 || true
		done
		sleep 1
	done
}

# k6_azure_wrapper writes the `az` this check puts first on PATH. The stand-in Microsoft
# endpoint and the stand-in Resource Manager are .test names of this stack's own, and az reaches
# the endpoints of a cloud, so az has to be told a cloud whose endpoints are those two names.
# az-broker makes its own AZURE_CONFIG_DIR and logs in at once, leaving no moment to register a
# cloud in it, so the registration is here: this wrapper registers the cloud in whatever
# directory az-broker made and then runs the real az with the arguments it was given.
#
# It also turns MSAL's instance discovery off, which is what an Azure Stack deployment does
# (az's own core.instance_discovery, as AZURE_CORE_INSTANCE_DISCOVERY): that discovery asks
# login.microsoftonline.com whether this cloud's authority is a Microsoft one, and the answer
# for a stand-in authority is and must be no. It is the one call of az's a stand cannot answer,
# since it is made to Microsoft's own host and about the stand rather than to it.
k6_azure_wrapper() {
	mkdir -p "$WORK_DIR/k6/bin"
	cat >"$WORK_DIR/k6/bin/az" <<EOF
#!/usr/bin/env bash
set -euo pipefail
export AZURE_CORE_INSTANCE_DISCOVERY=false
if [ -n "\${AZURE_CONFIG_DIR:-}" ] && [ ! -f "\$AZURE_CONFIG_DIR/clouds.config" ]; then
  "$REAL_AZ" cloud register -n airlock --skip-endpoint-discovery \\
    --endpoint-active-directory "$AIRLOCK_MICROSOFT_URL" \\
    --endpoint-active-directory-resource-id "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-active-directory-graph-resource-id "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-resource-manager "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-management "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-gallery "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --suffix-storage-endpoint core.windows.net >/dev/null
  "$REAL_AZ" cloud set -n airlock >/dev/null
fi
exec "$REAL_AZ" "\$@"
EOF
	chmod +x "$WORK_DIR/k6/bin/az"
}

# k6_azure runs the five az-broker runs and then the exchange and the five Resource Manager
# calls, and leaves what it saw in K6_AZ_*.
k6_azure() {
	local d="$WORK_DIR/k6"
	mkdir -p "$d"
	gh api repos/sjawhar/dotfiles/contents/scripts/az-broker -H 'Accept: application/vnd.github.raw' >"$d/az-broker" || {
		echo "airlock-check: sjawhar/dotfiles scripts/az-broker could not be fetched" >&2
		return 1
	}
	chmod +x "$d/az-broker"
	REAL_AZ="$(command -v az)"
	k6_azure_wrapper

	local microsoft_before
	microsoft_before="$(records_count microsoft)"
	: >"$d/cli.err"
	approve_watch "$d/cli.err" &
	local watcher=$!
	HELPER_PIDS+=("$watcher")

	# The five runs. AZ_BROKER_INNER=1 and AZURE_ADMIN are what az-broker's own first hop
	# produces: that hop is `secret-run AZURE_ADMIN -- az-broker`, and secret-run is check k7's
	# subject and needs a dotfiles checkout this pod has not got, so the check runs the half of
	# az-broker that drives az, under the release that hop would have made.
	#
	# What each run does once az-broker has logged it in: `az group list`, the admin command,
	# and then `az account get-access-token`, which prints the access token az itself holds -
	# the one the broker's exchange handed it, which has to be a placeholder and never the real
	# token the Resource Manager is sent.
	cat >"$d/az-steps.sh" <<EOF
set -u
az group list -o json
az account get-access-token -o json >$d/access.json
EOF
	local run
	: >"$d/runs.log"
	for run in 1 2 3 4 5; do
		echo "=== run $run ===" >>"$d/runs.log"
		PATH="$d/bin:$PATH" agent-secrets AIRLOCK_AZURE_LOGIN --ttl 1h --wait 5m -- sh -c '
			export AZURE_ADMIN="$AIRLOCK_AZURE_LOGIN" AZ_BROKER_INNER=1
			exec bash '"$d"'/az-broker -- bash '"$d"'/az-steps.sh' >>"$d/runs.log" 2>>"$d/cli.err"
		echo "exit $?" >>"$d/runs.log"
	done
	kill "$watcher" 2>/dev/null || true
	K6_AZ_RUNS="$(grep -c '^exit ' "$d/runs.log")"
	K6_AZ_FAILED="$(grep -c '^exit [^0]' "$d/runs.log" || true)"
	K6_AZ_ERROR="$(grep -m1 '^ERROR' "$d/cli.err" | tr -d '\r')"

	local recs
	recs="$(records microsoft | jq --argjson n "$microsoft_before" '.[$n:]')"
	K6_AZ_REACHED="$(printf '%s' "$recs" | jq -r '[.[] | .method + " " + .path] | unique | join("; ")')"
	K6_AZ_ASSERTION="$(printf '%s' "$recs" | jq -r '[.[] | select(.path | endswith("/token")) | .body | capture("client_assertion=(?<a>[^&]*)") | .a] | .[0] // ""')"
	# The form's own percent-encoding: a JWT holds none of the characters a form escapes but
	# the "=" of its padding, and the comparison is against the stack's own token.
	K6_AZ_ASSERTION="${K6_AZ_ASSERTION//%3D/=}"
	K6_AZ_ACCESS="$(jq -r '.accessToken // ""' "$d/access.json" 2>/dev/null)"
	mapfile -t K6_AZ_ARM_HEADERS < <(printf '%s' "$recs" | jq -r '.[] | select(.path == "/subscriptions") | .header.Authorization // [] | .[]')
	K6_AZ_ARM_STATUS="$(grep '^exit ' "$d/runs.log" | tr '\n' ' ')"
	K6_AZ_EXCHANGES="$(printf '%s' "$recs" | jq '[.[] | select(.path | endswith("/token"))] | length')"
	K6_AZ_SERVED="$(for log in "$AIRLOCK_DIR"/broker*.log; do printf '%s:%s ' "${log##*/}" "$(grep -c 'a call to a bound host' "$log")"; done)"
}

# k6_aws runs the AWS CLI's three calls and boto3's two against the stand-in AWS service.
k6_aws() {
	local d="$WORK_DIR/k6"
	mkdir -p "$d"
	head -c 102400 /dev/urandom >"$d/100k.bin"
	# boto3's own two steps are scripts/airlock/aws-boto3.py, run where it lies.
	local aws_before
	aws_before="$(records_count aws)"
	cat >"$d/aws-steps.sh" <<EOF
set -u
out=$d/aws-out
: >"\$out"
export AWS_DEFAULT_REGION=us-east-1
say() { printf '%s\n' "\$1" >>"\$out"; }
say "sts=\$(aws --endpoint-url $AIRLOCK_AWS_URL sts get-caller-identity 2>&1 | tail -n 1 | tr -d '\r')"
ls_out=\$(aws --endpoint-url $AIRLOCK_AWS_URL s3 ls 2>&1); ls_exit=\$?
say "ls=\$(printf '%s' "\$ls_out" | tail -n 1 | tr -d '\r')"
say "ls_exit=\$ls_exit"
say "cp=\$(aws --endpoint-url $AIRLOCK_AWS_URL s3 cp $d/100k.bin s3://airlock-bucket/k6-100k.bin 2>&1 | tail -n 1 | tr -d '\r')"
$AIRLOCK_BOTO3_PYTHON $SCRIPT_DIR/airlock/aws-boto3.py $AIRLOCK_AWS_URL $d/boto3.json >$d/boto3.log 2>&1
echo "boto3 exit \$?" >>$d/boto3.log
EOF
	agent-secrets AIRLOCK_AWS_KEY --ttl 30m -- bash "$d/aws-steps.sh" >>"$d/runs.log" 2>>"$d/cli.err"

	declare -gA K6AWS
	local key value
	while IFS='=' read -r key value; do K6AWS["$key"]="$value"; done <"$d/aws-out"
	K6_AWS_RECS="$(records aws | jq --argjson n "$aws_before" '.[$n:]')"
	K6_AWS_CALLS="$(printf '%s' "$K6_AWS_RECS" | jq 'length')"
	K6_AWS_TOKENS="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .header["X-Amz-Security-Token"] // [] | .[]] | unique | length')"
	K6_AWS_KEYIDS="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .header.Authorization // [] | .[] | capture("Credential=(?<k>[^/]*)/") | .k] | unique | join(",")')"
	K6_AWS_PLACEHOLDERS="$(printf '%s' "$K6_AWS_RECS" | jq '[.[] | select(((.header | tostring) + .query) | contains("agent-secrets-placeholder"))] | length')"
	K6_AWS_REFUSED="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .path] | join(" ")')"
	K6_AWS_CHUNK="$(jq -r '.chunk_answer // .chunk_signed // ""' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_PRESIGNED="$(jq -r '.presigned // ""' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_BOTO3="$(jq -c '{boto3, botocore, chunks, decoded_length, presigned_query}' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_BOTO3_LOG="$(tail -n 3 "$d/boto3.log" 2>/dev/null | tr '\n' ' ')"
}

check_k6() {
	expect "the stand-in token endpoint gets the real federated token"
	expect "az gets a placeholder access token"
	expect "the stand-in Resource Manager gets a real access token in Authorization on all five runs, whichever process served each call"
	expect "the stand-in AWS service accepts each signature of the STS call, the list and the upload, with the real session token, and finds no placeholder in them"
	expect "the list is answered, not only signed: aws s3 ls exits 0 through the airlock"
	expect "the chunk-signed upload and the presigned URL get an error that names them"
	expect "the negative control, with the exchange and the signing turned off (dev-airlock.sh --no-kind), gives the token endpoint the placeholder and makes the stand-in AWS service reject every signature"

	k6_clients
	start_stack
	stack_facts
	observe "the clients this run drove: $CLIENT_VERSIONS, each installed by scripts/airlock-clients.sh at a pinned version whose SHA-256 it checked"

	k6_azure
	local federated access_shape
	federated="$(cat "$AIRLOCK_DIR/azure-identity-token")"
	access_shape="$(shape "$K6_AZ_ACCESS")"
	observe "the five az-broker runs: $K6_AZ_RUNS runs, $K6_AZ_FAILED of them non-zero ($K6_AZ_ARM_STATUS); what they reached on the Microsoft stand-in was $K6_AZ_REACHED, and the first ERROR line az wrote, if any, was: ${K6_AZ_ERROR:-none}"
	observe "the exchange: the token endpoint was asked $K6_AZ_EXCHANGES times and its client_assertion was $(shape "$K6_AZ_ASSERTION") against the stack's own federated token $(shape "$federated") (compared by SHA-256 and length)"
	observe "the access token az itself holds after its login (az account get-access-token) is $access_shape, which opens 'agent-secrets-placeholder': $(contains agent-secrets-placeholder "$K6_AZ_ACCESS" && echo yes || echo no)"
	observe "the Resource Manager calls az made, one per run, and their Authorization headers: $(for h in "${K6_AZ_ARM_HEADERS[@]+"${K6_AZ_ARM_HEADERS[@]}"}"; do printf '%s; ' "$(shape "$h")"; done)"
	observe "the brokers' own logs of calls to a bound host, which is how many each process served: $K6_AZ_SERVED"

	judge "$([ "$K6_AZ_EXCHANGES" -gt 0 ] && contains "$federated" "$K6_AZ_ASSERTION" && echo 0 || echo 1)" \
		"the stand-in token endpoint gets the real federated token" \
		"its client_assertion is the stack's own federated token by SHA-256 and length ($(shape "$K6_AZ_ASSERTION")), over $K6_AZ_EXCHANGES exchanges"

	judge "$([ "$K6_AZ_RUNS" = 5 ] && [ "$K6_AZ_FAILED" = 0 ] && contains agent-secrets-placeholder "$K6_AZ_ACCESS" && echo 0 || echo 1)" \
		"az gets a placeholder access token" \
		"all five az-broker runs logged in against the stand-in cloud and listed resource groups ($K6_AZ_FAILED of $K6_AZ_RUNS exited non-zero), and the token az holds is $access_shape, which opens 'agent-secrets-placeholder': $(contains agent-secrets-placeholder "$K6_AZ_ACCESS" && echo yes || echo no)"

	local arm_calls=0 arm_real=0 header
	for header in "${K6_AZ_ARM_HEADERS[@]+"${K6_AZ_ARM_HEADERS[@]}"}"; do
		arm_calls=$((arm_calls + 1))
		contains "agent-secrets-placeholder" "$header" || arm_real=$((arm_real + 1))
	done
	judge "$([ "$arm_calls" = 5 ] && [ "$arm_real" = 5 ] && [ "$(echo "$K6_AZ_SERVED" | tr ' ' '\n' | grep -c ':[1-9]')" -ge 2 ] && echo 0 || echo 1)" \
		"the stand-in Resource Manager gets a real access token in Authorization on all five runs, whichever process served each call" \
		"$arm_real of $arm_calls calls carried a token that is no placeholder, each answered by whichever broker the front gave its connection to ($K6_AZ_SERVED); the five calls are az's own, one per run, made with the azure-access placeholder the exchange handed it"

	k6_aws
	local aws_secret aws_session
	aws_secret="$(stored_value "${STACK_SECRETS_PREFIX}airlock-aws-key")"
	aws_session="$(printf '%s' "$aws_secret" | jq -r .session_token)"
	observe "the AWS CLI: sts said '${K6AWS[sts]}'; s3 ls said '${K6AWS[ls]}' and exited ${K6AWS[ls_exit]}; s3 cp said '${K6AWS[cp]}'"
	observe "the stand-in AWS service recorded $K6_AWS_CALLS calls on $K6_AWS_REFUSED; the Credential of each named the key id '$K6_AWS_KEYIDS' and the real key id is '$AIRLOCK_AWS_KEY_ID'; $K6_AWS_PLACEHOLDERS of them carried a placeholder in a header or the query"
	observe "boto3: $K6_AWS_BOTO3 (its own last lines: $K6_AWS_BOTO3_LOG)"
	observe "the chunk-signed upload got: $K6_AWS_CHUNK"
	observe "the presigned URL got: $K6_AWS_PRESIGNED"

	local accepted
	accepted="$(printf '%s' "$K6_AWS_RECS" | jq '[.[] | select(.path != "")] | length')"
	judge "$([ "$K6_AWS_PLACEHOLDERS" = 0 ] && [ "$accepted" -ge 3 ] && contains "$AIRLOCK_AWS_KEY_ID" "$K6_AWS_KEYIDS" && ! contains "403" "${K6AWS[sts]}" && echo 0 || echo 1)" \
		"the stand-in AWS service accepts each signature of the STS call, the list and the upload, with the real session token, and finds no placeholder in them" \
		"$accepted calls were verified against the real key id $K6_AWS_KEYIDS and the stack's own session token ($(shape "$aws_session")), with $K6_AWS_PLACEHOLDERS placeholders among them; the three calls said '${K6AWS[sts]}' / '${K6AWS[ls]}' / '${K6AWS[cp]}'"

	# The list is read for its own answer as well as its signature. A stand-in that verifies a
	# signature and then answers a line of prose makes the AWS CLI report a 500, and a check that
	# reads the signature alone would pass on a call its own client calls broken: the stand-in
	# answers an empty ListAllMyBuckets now, and this is what holds it to that.
	judge "$([ "${K6AWS[ls_exit]}" = 0 ] && ! contains "error" "${K6AWS[ls]}" && echo 0 || echo 1)" \
		"the list is answered, not only signed: aws s3 ls exits 0 through the airlock" \
		"aws s3 ls exited ${K6AWS[ls_exit]} and its last line was '${K6AWS[ls]}'"

	judge "$(contains "refusing a chunk-signed upload" "$K6_AWS_CHUNK" && contains "refusing a presigned URL" "$K6_AWS_PRESIGNED" && echo 0 || echo 1)" \
		"the chunk-signed upload and the presigned URL get an error that names them" \
		"the upload was refused '$(printf '%s' "$K6_AWS_CHUNK" | head -c 120)…' and the presigned URL '$(printf '%s' "$K6_AWS_PRESIGNED" | head -c 120)…'"

	stop_stack

	# The negative control: a stack whose every rules entry reads as having no kind, so the azure
	# exchange and the AWS signing are both off.
	start_stack --no-kind
	stack_facts
	k6_azure
	k6_aws
	local control_federated
	control_federated="$(cat "$AIRLOCK_DIR/azure-identity-token")"
	control "with --no-kind (BROKER_AIRLOCK_TEST_NO_KIND=true) the token endpoint's client_assertion was $(shape "$K6_AZ_ASSERTION"), against the real federated token $(shape "$control_federated") and a placeholder: it opens 'agent-secrets-placeholder': $(contains agent-secrets-placeholder "$K6_AZ_ASSERTION" && echo yes || echo no)"
	control "with --no-kind the stand-in AWS service recorded $K6_AWS_CALLS calls, $K6_AWS_PLACEHOLDERS of them carrying a placeholder in a header or the query, and the key ids their Credentials named were '$K6_AWS_KEYIDS' against the real '$AIRLOCK_AWS_KEY_ID'; sts said '${K6AWS[sts]}'"
	judge "$(contains "agent-secrets-placeholder" "$K6_AZ_ASSERTION" && [ "$K6_AWS_PLACEHOLDERS" = "$K6_AWS_CALLS" ] && [ "$K6_AWS_CALLS" -gt 0 ] && contains "placeholder" "${K6AWS[sts]}" && echo 0 || echo 1)" \
		"negative control: with the exchange and the signing turned off the token endpoint gets the placeholder and the AWS service rejects every signature" \
		"the assertion the endpoint got was the grant's placeholder, and all $K6_AWS_PLACEHOLDERS of the $K6_AWS_CALLS calls the stand-in recorded carried a placeholder, where the same runs with the kinds on carry none; the stand-in refused each of them, sts with '${K6AWS[sts]}'"

	stop_stack
}

###############################################################################################
# k7
###############################################################################################
#
# k7 drives the consumers as they run, against real services, with fake values: the refusal each
# real service answers is the evidence, not a failure. The consumers come from sjawhar/dotfiles
# at main and nothing of them is rewritten; what this check adds around them is a DOTFILES_DIR
# of its own holding the files they read of each other (scripts/secret-run-reason and
# agent-secrets/broker-url.sh), because secret-run resolves both relative to its own directory.
#
# The names are the consumers' own - GITHUB_ADMIN_PAT, GCLOUD_SAMI, GWS_THEOREM_READ_OAUTH and
# the rest - and the hosts are the real ones, so this check needs a rules file the stack's does
# not hold. That is one broker process of this check's own, on the stack's own database, store,
# certificate authority and stand-ins, with its rules file, its own two ports and its own public
# URL changed - the same device k4's control uses for one setting, with the public URL changed
# too because this broker must serve the release as well as the proxy, and a session's proof is
# verified against the public URL it was signed for. Its whole environment is the first stack
# broker's own, read out of /proc/<pid>/environ, so nothing of the stack's settings is left out.
#
# The pod sets NO_PROXY=* and GIT_CONFIG_COUNT=5 with a GitHub credential helper. NO_PROXY=* is
# left exactly as it is: the CLI must drop it, and that it does is part of what this check
# proves. The git credential settings are cleared for the git step alone, which brings its own
# helper - the one that answers the placeholder - and the proxy in git's own configuration.

# k7_dotfiles fetches the consumers from sjawhar/dotfiles at main and lays them out as a
# dotfiles checkout, which is how secret-run finds its reason builder and its broker URL file.
k7_dotfiles() {
	local d="$WORK_DIR/k7/dotfiles" path
	mkdir -p "$d/scripts" "$d/agent-secrets" "$d/shims" "$d/gws"
	for path in scripts/secret-run scripts/secret-run-reason scripts/gcloud-broker scripts/gh-agent-config scripts/public-text-gate shims/gh gws/accounts.json agent-secrets/broker-url.sh; do
		gh api "repos/sjawhar/dotfiles/contents/$path" -H 'Accept: application/vnd.github.raw' >"$d/$path" || {
			echo "airlock-check: sjawhar/dotfiles $path could not be fetched" >&2
			return 1
		}
	done
	chmod +x "$d/scripts/secret-run" "$d/scripts/secret-run-reason" "$d/scripts/gcloud-broker" \
		"$d/scripts/gh-agent-config" "$d/scripts/public-text-gate" "$d/shims/gh"
	K7_DOTFILES="$d"
	K7_FETCHED="$(cd "$d" && find . -type f | sed 's|^\./||' | sort | tr '\n' ' ')"
}

# k7_broker starts the one broker this check's steps run through: a broker like the stack's,
# whose environment comes from the running one, with the rules file, the two listening addresses
# and the public URL replaced.
k7_broker() {
	local d="$WORK_DIR/k7" api proxy
	api="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	proxy="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	K7_API_URL="http://127.0.0.1:$api"
	K7_PROXY_URL="http://127.0.0.1:$proxy"
	broker_env \
		BROKER_AIRLOCK_RULES_FILE="$d/rules.json" \
		BROKER_LISTEN_ADDR="127.0.0.1:$api" \
		BROKER_PUBLIC_URL="$K7_API_URL" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:$proxy" \
		BROKER_AIRLOCK_PROXY_URL="$K7_PROXY_URL"
	: >"$d/broker.log"
	"${BROKER_ENV[@]}" "$BROKER_BIN" >"$d/broker.log" 2>&1 &
	local started=$!
	EXTRA_BROKER_PIDS+=("$started")
	await "k7's broker" 120 "$d/broker.log" 'airlock proxy listening addr=' "$started"
}

# k7_names is the rules file and the fake secrets this check adds: one name per consumer, bound
# to the real hosts that consumer calls, and a fake value for each. The stack's own entries are
# kept, so the seven plain clients still reach S.
k7_names() {
	local d="$WORK_DIR/k7"
	mkdir -p "$d"
	jq '.secrets += [
	     {"name": "SLACK_BOT_TOKEN", "hosts": ["slack.com"]},
	     {"name": "GITHUB_ADMIN_PAT", "hosts": ["api.github.com", "github.com"]},
	     {"name": "TAILSCALE_API_KEY", "hosts": ["api.tailscale.com"]},
	     {"name": "GCLOUD_SAMI", "hosts": ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com"]},
	     {"name": "GWS_THEOREM_READ_OAUTH", "hosts": ["www.googleapis.com", "oauth2.googleapis.com"]},
	     {"name": "GWS_THEOREM_ADMIN_OAUTH", "hosts": ["admin.googleapis.com"]}
	   ]' "$AIRLOCK_RULES_FILE" >"$d/rules.json"
	K7_SLACK="xoxb-the-fake-slack-token-$RANDOM$RANDOM"
	K7_GITHUB="ghp_thefakegithubadminpat$RANDOM$RANDOM"
	K7_TAILSCALE="tskey-api-the-fake-tailscale-key-$RANDOM$RANDOM"
	add_fake_secret "${STACK_SECRETS_PREFIX}slack-bot-token" '{"owner":"legion-daemon","tier":"agent"}' "$K7_SLACK"
	add_fake_secret "${STACK_SECRETS_PREFIX}github-admin-pat" '{"owner":"legion-daemon","tier":"agent"}' "$K7_GITHUB"
	add_fake_secret "${STACK_SECRETS_PREFIX}tailscale-api-key" '{"owner":"legion-daemon","tier":"agent"}' "$K7_TAILSCALE"
	# The three Google logins are minted kinds: the broker mints each at the stack's stand-in
	# Google token endpoint, so what the consumer holds a placeholder for is a minted value.
	local google login
	google='{"owner":"legion-daemon","tier":"agent","kind":"google-login"}'
	login='{"type":"authorized_user","client_id":"the-stand-in-client","client_secret":"the-stand-in-client-secret","refresh_token":"the-stand-in-refresh-token"}'
	add_fake_secret "${STACK_SECRETS_PREFIX}gcloud-sami" "$google" "$login"
	add_fake_secret "${STACK_SECRETS_PREFIX}gws-theorem-read-oauth" "$google" "$login"
	add_fake_secret "${STACK_SECRETS_PREFIX}gws-theorem-admin-oauth" "$google" "$login"
}

# k7_pulumi writes the Pulumi program the Tailscale provider's getDevices is called from: a YAML
# program, which needs no language runtime of its own, and a local state directory.
k7_pulumi() {
	local d="$WORK_DIR/k7/pulumi"
	mkdir -p "$d/state"
	cat >"$d/Pulumi.yaml" <<EOF
name: airlock-k7
runtime: yaml
description: one tailscale:getDevices call, which the real Tailscale API refuses a fake key
variables:
  devices:
    fn::invoke:
      function: tailscale:index:getDevices
outputs:
  devices: \${devices}
EOF
}

# k7_steps writes every consumer's own command into one file, as k3's and k4's steps are
# written, so one grant of the seven names covers them all.
k7_steps() {
	local d="$WORK_DIR/k7"
	cat >"$d/steps.sh" <<EOF
set -u
out=$d/out
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
export DOTFILES_DIR=$K7_DOTFILES
export PATH="$K7_DOTFILES/scripts:$K7_DOTFILES/shims:\$PATH"
# shims/gh builds a gh configuration directory of its own with scripts/gh-agent-config, under
# XDG_CONFIG_HOME; this run's is under its work directory, never the person's home.
export XDG_CONFIG_HOME=$d/xdg
mkdir -p \$XDG_CONFIG_HOME
say "no_proxy=\$NO_PROXY"
say "https_proxy=\$(printf '%s' "\$HTTPS_PROXY" | sed 's/:[^:@]*@/:***@/')"

# The slack_post poster (scripts/airlock/slack.ts): bun, the Bearer a Slack app's token goes in,
# and auth.test.
say "slack=\$(bun run $SCRIPT_DIR/airlock/slack.ts 2>&1 | tail -n 1 | tr -d '\r')"

# gh api under secret-run for GITHUB_ADMIN_PAT: the admin line of shims/gh itself, with the
# directory of the real GitHub CLI first on PATH for this one step. That line sets GH_TOKEN,
# and the only other gh in this pod is Legion's wrapper, which refuses a call whose GH_TOKEN
# is set and refuses to run at all when another gh is ahead of it on PATH; \$LEGION_GH_PATH is
# the real CLI the wrapper itself runs. Nothing else of the step changes, so what GitHub is
# sent is the consumer's own command with the consumer's own fake value.
say "gh=\$(secret-run --reason 'k7: one gh api call with a fake admin token' GITHUB_ADMIN_PAT -- sh -c 'PATH=$K7_GH_DIR:\$PATH GH_TOKEN=\$GITHUB_ADMIN_PAT GITHUB_TOKEN=\$GITHUB_ADMIN_PAT exec gh api user' 2>&1 | tail -n 1 | tr -d '\r')"

# pulumi preview of a program whose Tailscale provider calls tailscale:getDevices.
say "pulumi=\$(cd $d/pulumi && export PULUMI_CONFIG_PASSPHRASE= PULUMI_BACKEND_URL=file://$d/pulumi/state TAILSCALE_API_KEY="\$TAILSCALE_API_KEY" TAILSCALE_TAILNET=example.com && pulumi stack init k7 >/dev/null 2>&1; pulumi preview --stack k7 --non-interactive 2>&1 | grep -iE 'error|refus|unauthor|401|403' | head -n 1 | tr -d '\r')"

# gcloud-broker with gcloud projects list.
say "gcloud=\$(GCLOUD_BROKER_INNER=1 GCLOUD_SAMI="\$GCLOUD_SAMI" bash $K7_DOTFILES/scripts/gcloud-broker -- gcloud projects list --format=json 2>&1 | grep -iE 'ERROR|reason:' | head -n 1 | tr -d '\r')"

# One gws call under the theorem read tier: that tier's own command out of gws/accounts.json,
# running the Drive call gws makes, since gws itself is no file of that repository.
say "gws_tier=\$(jq -r '.theorem.tiers.read.command | @tsv' $K7_DOTFILES/gws/accounts.json 2>&1 | tr '\t' ' ')"
say "gws=\$(secret-run --reason 'gws theorem read' GWS_THEOREM_READ_OAUTH -- sh -c 'curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$GWS_THEOREM_READ_OAUTH" "https://www.googleapis.com/drive/v3/about?fields=user"' 2>&1 | tail -n 1 | tr -d '\r')"

# The google-workspace skill's Bearer call to admin.googleapis.com.
say "admin=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$GWS_THEOREM_ADMIN_OAUTH" "https://admin.googleapis.com/admin/directory/v1/users?domain=theorem.dev" 2>&1 | tail -n 1)"

# git push --dry-run through scripts/airlock/credential-helper, which answers the placeholder,
# with the proxy in git's own configuration. The pod's own GIT_CONFIG_* settings are cleared for
# this step alone: they name a credential helper of the pod's, which would answer in place of it.
rm -rf $d/repo
git init -q $d/repo
(cd $d/repo && git -c user.email=k7@airlock.test -c user.name=k7 commit -q --allow-empty -m k7)
say "git=\$(cd $d/repo && env -u GIT_CONFIG_COUNT -u GIT_CONFIG_KEY_0 -u GIT_CONFIG_VALUE_0 -u GIT_CONFIG_KEY_1 -u GIT_CONFIG_VALUE_1 -u GIT_CONFIG_KEY_2 -u GIT_CONFIG_VALUE_2 -u GIT_CONFIG_KEY_3 -u GIT_CONFIG_VALUE_3 -u GIT_CONFIG_KEY_4 -u GIT_CONFIG_VALUE_4 git -c http.proxy="\$HTTPS_PROXY" -c credential.helper=$SCRIPT_DIR/airlock/credential-helper -c http.sslCAInfo="\$GIT_SSL_CAINFO" push --dry-run https://github.com/cybermonklabs/legion HEAD:refs/heads/k7-airlock-never-pushed 2>&1 | tail -n 2 | tr '\n' ' ')"

# The plain clients to S, each with the fake value in the header it uses.
say "curl=\$(curl -sS -o /dev/null -w %{http_code} -H "Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/curl)"
say "wget=\$(wget -q -O /dev/null --header="Authorization: Bearer \$AIRLOCK_DEMO_TOKEN" $AIRLOCK_S_URL/wget && echo 200 || echo failed)"
say "node=\$(node -e 'fetch("$AIRLOCK_S_URL/node", {headers: {authorization: "Bearer " + process.env.AIRLOCK_DEMO_TOKEN}}).then(r => console.log(r.status)).catch(e => console.log("error", e.message))')"
say "bun=\$(bun -e 'const r = await fetch("$AIRLOCK_S_URL/bun", {headers: {authorization: "Bearer " + process.env.AIRLOCK_DEMO_TOKEN}}); console.log(r.status)' 2>&1 | tail -n 1)"
say "urllib=\$($AIRLOCK_BOTO3_PYTHON -c 'import os, urllib.request
print(urllib.request.urlopen(urllib.request.Request("$AIRLOCK_S_URL/urllib", headers={"Authorization": "Bearer " + os.environ["AIRLOCK_DEMO_TOKEN"]})).status)' 2>&1 | tail -n 1)"
say "requests=\$($AIRLOCK_BOTO3_PYTHON -c 'import os, requests
print(requests.get("$AIRLOCK_S_URL/requests", headers={"Authorization": "Bearer " + os.environ["AIRLOCK_DEMO_TOKEN"]}).status_code)' 2>&1 | tail -n 1)"
say "go=\$($STANDS_BIN probe-bound)"
EOF
}

check_k7() {
	expect "each call went through the proxy port and the broker swapped the fake value into the header that the consumer uses"
	expect "each real service refused the fake value"
	expect "the gh consumer's own call is refused the fake value by GitHub"
	expect "S got the fake value in each client's call"
	expect "the command's exit status is 3, and the CLI's stderr has the line agent-secrets: NAME from …"
	expect "the negative control, the same runs with a CLI built without the change, shows no call through the proxy port"

	# The real GitHub CLI, which the gh step puts first on PATH: LEGION_GH_PATH names it, and
	# it is the binary Legion's own gh wrapper runs. Without it there is no gh in this pod the
	# consumer's admin line can run - the wrapper refuses a call whose GH_TOKEN is set - and
	# the step is refused rather than run against the wrapper, whose refusal is its own and
	# says nothing about the airlock.
	K7_GH_DIR=""
	if [ -n "${LEGION_GH_PATH:-}" ] && [ -x "${LEGION_GH_PATH:-}" ]; then
		K7_GH_DIR="$(dirname "$LEGION_GH_PATH")"
	fi

	k6_clients
	start_stack
	stack_facts
	k7_dotfiles
	observe "the consumers, fetched from sjawhar/dotfiles at main: $K7_FETCHED"
	k7_names
	k7_broker
	observe "this check's own broker is the stack's first broker's environment with its rules file, its two ports and its public URL changed: api $K7_API_URL, proxy $K7_PROXY_URL, names $(jq -c '[.secrets[].name]' "$WORK_DIR/k7/rules.json")"

	k7_pulumi
	k7_steps

	local s_before
	s_before="$(records_count s)"
	# AGENT_SECRETS_URL is this check's own broker, the one whose rules name the real hosts.
	AGENT_SECRETS_URL="$K7_API_URL" agent-secrets \
		SLACK_BOT_TOKEN GITHUB_ADMIN_PAT TAILSCALE_API_KEY GCLOUD_SAMI \
		GWS_THEOREM_READ_OAUTH GWS_THEOREM_ADMIN_OAUTH AIRLOCK_DEMO_TOKEN \
		--ttl 30m -- bash "$WORK_DIR/k7/steps.sh" >"$WORK_DIR/k7/cli.out" 2>"$WORK_DIR/k7/cli.err"
	declare -A K7
	local key value
	while IFS='=' read -r key value; do K7["$key"]="$value"; done <"$WORK_DIR/k7/out"

	local recs clients=0 with_value=0 client demo_value
	recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	for client in curl wget node bun urllib requests go; do
		clients=$((clients + 1))
		if [ "$(printf '%s' "$recs" | jq --arg p "/$client" --arg v "$demo_value" '[.[] | select(.path == $p) | .header.Authorization // [] | .[] | select(contains($v))] | length')" -ge 1 ]; then
			with_value=$((with_value + 1))
		fi
	done

	observe "the command's inherited NO_PROXY was ${K7[no_proxy]-} — the pod's own NO_PROXY is '*', which the CLI had to drop — and its proxy was ${K7[https_proxy]-}"
	observe "slack.com/api/auth.test under bun: ${K7[slack]-}"
	observe "gh api user under secret-run for GITHUB_ADMIN_PAT: ${K7[gh]-}"
	observe "pulumi preview with the Tailscale provider calling tailscale:getDevices: ${K7[pulumi]-}"
	observe "gcloud projects list under gcloud-broker: ${K7[gcloud]-}"
	observe "the gws theorem read tier (${K7[gws_tier]-}) answered: ${K7[gws]-}"
	observe "the google-workspace skill's Bearer call to admin.googleapis.com answered: ${K7[admin]-}"
	observe "git push --dry-run through a credential helper answering the placeholder: ${K7[git]-}"
	observe "the plain clients to S: curl ${K7[curl]-}, wget ${K7[wget]-}, node ${K7[node]-}, bun ${K7[bun]-}, python urllib ${K7[urllib]-}, python requests ${K7[requests]-}, Go ${K7[go]-}"
	observe "S recorded the fake value (by SHA-256 and length, $(shape "$demo_value")) in $with_value of the $clients plain clients' calls"

	# Each real service's own refusal. A fake value is refused with an unauthorized answer, and
	# what each consumer printed of it is what is read here.
	local refused=0 refusals=""
	k7_refused() { # <label> <what it said>
		if printf '%s' "$2" | grep -qiE '401|403|not_authed|invalid_auth|unauthor|unauthenticated|forbid|refus|denied|Bad credentials|permission|authentication failed|invalid username or token|CREDENTIALS_MISSING|ACCESS_TOKEN_TYPE'; then
			refused=$((refused + 1))
			refusals="$refusals; $1 refused it"
		else
			refusals="$refusals; $1 did NOT refuse it: $2"
		fi
	}
	k7_refused slack "${K7[slack]-}"
	k7_refused tailscale "${K7[pulumi]-}"
	k7_refused gcloud "${K7[gcloud]-}"
	k7_refused gws "${K7[gws]-}"
	k7_refused admin "${K7[admin]-}"
	k7_refused git "${K7[git]-}"
	observe "what each real service answered the fake value${refusals}"
	observe "the gh consumer: $(printf '%s' "${K7[gh]-}" | head -c 300)"

	# What the broker logged: one line per call to a bound host, which is how a call is known to
	# have reached the proxy port and been decided there.
	local logged swapped
	logged="$(grep -c 'a call to a bound host' "$WORK_DIR/k7/broker.log" || true)"
	swapped="$(grep -c 'a call to a bound host.*allowed=true' "$WORK_DIR/k7/broker.log" || true)"
	observe "this check's broker logged $logged calls to a bound host, $swapped of them allowed; the last three:"
	observe "$(grep 'a call to a bound host' "$WORK_DIR/k7/broker.log" | tail -n 3 | sed 's/^/    /')"
	judge "$([ "$logged" -ge 10 ] && [ "$swapped" -ge 10 ] && echo 0 || echo 1)" \
		"each call went through the proxy port and the broker swapped the fake value into the header that the consumer uses" \
		"$logged calls to a bound host were logged by the proxy, $swapped of them allowed and so swapped"
	judge "$([ "$refused" = 6 ] && echo 0 || echo 1)" \
		"each real service refused the fake value" "$refused of the 6 consumers that can run in this pod refused it${refusals}"
	if [ -z "$K7_GH_DIR" ]; then
		fail "the gh consumer's own call is refused the fake value by GitHub — this step did not run: LEGION_GH_PATH is unset or names no runnable file in this pod, and it names the only real GitHub CLI here. shims/gh's own admin line is \`secret-run … GITHUB_ADMIN_PAT -- sh -c 'GH_TOKEN=\$GITHUB_ADMIN_PAT exec gh …'\`, and the other gh on PATH is Legion's wrapper, which refuses a call whose GH_TOKEN is set and refuses to run at all when another gh is ahead of it, so running the step against the wrapper would prove nothing about the airlock. Set LEGION_GH_PATH to the real CLI and run this check again."
	else
		judge "$(printf '%s' "${K7[gh]-}" | grep -qiE '401|403|Bad credentials|unauthor' && echo 0 || echo 1)" \
			"the gh consumer's own call is refused the fake value by GitHub" \
			"the real GitHub CLI ($LEGION_GH_PATH, first on PATH for that one step) sent the fake admin token to api.github.com through the proxy and GitHub answered: $(printf '%s' "${K7[gh]-}" | head -c 200)"
	fi
	judge "$([ "$with_value" = "$clients" ] && echo 0 || echo 1)" \
		"S got the fake value in each client's call" \
		"$with_value of $clients clients' calls carried it, compared by SHA-256 and length"

	# The exit status and the CLI's line: a command that exits 3 under the CLI leaves this run
	# with 3, and the CLI says on its stderr which source each name came from.
	local three_status three_err
	AGENT_SECRETS_URL="$K7_API_URL" agent-secrets GITHUB_ADMIN_PAT --ttl 5m -- sh -c 'exit 3' >/dev/null 2>"$WORK_DIR/k7/three.err"
	three_status=$?
	three_err="$(tr '\n' ' ' <"$WORK_DIR/k7/three.err")"
	observe "a command that exits 3 under the CLI left exit $three_status, and the CLI's stderr said: $three_err"
	judge "$([ "$three_status" = 3 ] && contains "agent-secrets: GITHUB_ADMIN_PAT from " "$three_err" && echo 0 || echo 1)" \
		"the command's exit status is 3, and the CLI's stderr has the line agent-secrets: NAME from …" \
		"it exited $three_status and said '$three_err'"

	# The negative control: the last released CLI, which is the CLI without this change. It is
	# released no value for a name the rules name, so nothing it runs reaches the proxy port.
	fetch_old_cli
	local before_control after_control old_out old_status
	before_control="$(grep -c 'a call to a bound host' "$WORK_DIR/k7/broker.log" || true)"
	old_out="$(AGENT_SECRETS_URL="$K7_API_URL" "$OLD_CLI" GITHUB_ADMIN_PAT -- sh -c 'curl -sS -o /dev/null -w %{http_code} https://api.github.com/user' 2>&1)"
	old_status=$?
	after_control="$(grep -c 'a call to a bound host' "$WORK_DIR/k7/broker.log" || true)"
	control "the last released agent-secrets ($OLD_CLI_TAG, the CLI without this change) exited $old_status saying '$(printf '%s' "$old_out" | tail -n 1)'"
	control "the proxy logged $((after_control - before_control)) further calls to a bound host while it ran"
	judge "$([ "$((after_control - before_control))" = 0 ] && echo 0 || echo 1)" \
		"negative control: with a CLI built without the change no call goes through the proxy port" \
		"$((after_control - before_control)) calls to a bound host were logged while the old CLI ran, which exited $old_status"

	stop_stack
}

###############################################################################################
# k8a
###############################################################################################

# k8a_start_broker starts a broker on one rules file and answers 0 when it refused to start for
# that file, leaving why in K8A_WHY. Its environment is the running stack broker's own, with the
# rules file and the two addresses replaced, so that what it refuses on is the rules file and
# not a setting this function left out. It runs in the foreground under timeout, because what it
# is here to see is a broker that never comes up.
k8a_start_broker() { # <rules file> <label>
	local log="$WORK_DIR/k8a-broker-$2.log"
	broker_env \
		BROKER_PUBLIC_URL="http://127.0.0.1:0" \
		BROKER_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_AIRLOCK_RULES_FILE="$1" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0"
	"${BROKER_ENV[@]}" timeout 25 "$BROKER_BIN" >"$log" 2>&1
	local status=$?
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
		out="$("$BROKER_BIN" airlock-check "$d/$label.json" --against "$d/valid.json" 2>&1)"
		status=$?
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

	out="$("$BROKER_BIN" airlock-check "$d/valid.json" --against "$d/valid.json" 2>&1)"
	status=$?
	control "the valid file against itself exited $status saying: $(printf '%s' "$out" | tail -n 1)"
	judge "$([ "$status" = 0 ] && contains "nothing changed" "$out" && echo 0 || echo 1)" \
		"negative control: on the valid file the command exits 0 and lists no change" \
		"it exited $status and said '$(printf '%s' "$out" | tail -n 1)'"

	stop_stack
}

###############################################################################################
# k9
###############################################################################################
#
# A ten-minute load, and everything else this check asks of one stack, takes about twenty-five
# minutes end to end: two stack starts of about three minutes each, ten minutes of connections,
# a minute of baseline through a plain tunnel, two ninety-second gaps (one here, one in the
# control) and the upload, the clone and the two broker steps between them.
#
# The figures the check names are the broker container's own limits, so they are read of the
# broker processes and of nothing else: each process's CPU time from utime and stime in
# /proc/<pid>/stat, against the wall time of the same window, and its peak memory from
# /proc/<pid>/status' VmRSS, sampled every two seconds across the ten minutes and kept at its
# highest. Not ru_maxrss: getrusage answers for the calling process and its waited-for children,
# and these two are dev-airlock.sh's children and not this script's, so no getrusage of this run
# can see them. Not VmHWM either, which is the kernel's own high-water mark and would need no
# sampling: a Legion pod runs gVisor, whose /proc/<pid>/status has VmSize, VmRSS and VmData and
# no VmHWM at all. The sampled maximum is what is reported, and a peak between two samples is
# what it can miss.
#
# GOMAXPROCS=1 for each broker: dev-airlock.sh has no option for it and its start_broker names
# no GOMAXPROCS, so the brokers inherit it from the environment this script starts the stack
# with, which is where this check sets it. Each broker's /proc/<pid>/environ is read back to
# show that it arrived.

# k9_cpu answers one process's CPU time in seconds, utime and stime of /proc/<pid>/stat added.
k9_cpu() { # <pid>
	awk -v ticks="$(getconf CLK_TCK)" '{ sub(/^[0-9]+ \(.*\) /, ""); print ($12 + $13) / ticks }' "/proc/$1/stat" 2>/dev/null || echo 0
}

# k9_sample_rss appends one line per broker process every two seconds, which is how this check
# reads a peak resident set on a kernel whose /proc/<pid>/status has no VmHWM (see the block
# above). It runs until it is killed.
k9_sample_rss() { # <file> <pid>...
	local file="$1" pid
	shift
	while :; do
		for pid in "$@"; do
			awk -v p="$pid" '/^VmRSS:/ { print p, $2 }' "/proc/$pid/status" 2>/dev/null >>"$file"
		done
		sleep 2
	done
}

# k9_peak_kb is the highest resident set the sampler saw for one process, in kilobytes.
k9_peak_kb() { # <file> <pid>
	awk -v p="$2" '$1 == p && $2 > max { max = $2 } END { print max + 0 }' "$1" 2>/dev/null
}

check_k9() {
	expect "no call fails in the ten minutes of 50 connections a second"
	expect "the proxy adds at most 100 milliseconds at the median, against the same calls through a plain tunnel"
	expect "each broker process's CPU time is at most half its wall time and its peak memory at most 1 GiB"
	expect "the 50 MiB upload, the answer with a 90-second gap in it and the shallow clone finish"
	expect "the WebSocket upgrade reaches S with the real value in its header, and frames pass both ways"
	expect "the stream on the stopped process ends after the drain, and the next call succeeds"
	expect "the negative control, a stack with the proxy's idle limit at 60 seconds, cuts the answer with the 90-second gap"

	# Each broker with GOMAXPROCS=1: dev-airlock.sh has no option for it, so it is exported into
	# the environment the stack is started with and each broker inherits it.
	export GOMAXPROCS=1
	start_stack
	stack_facts
	# This check's own files. The load driver is a mode of dev-airlock-stands, which
	# dev-airlock.sh built when it started the stack, so there is nothing to build here.
	mkdir -p "$WORK_DIR/k9"

	local pids pid environs=""
	mapfile -t pids < <(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG")
	for pid in "${pids[@]}"; do
		environs="$environs $pid:$(tr '\0' '\n' <"/proc/$pid/environ" | grep -c '^GOMAXPROCS=1$')"
	done
	observe "the stack's broker processes are ${pids[*]}, and each one's /proc/<pid>/environ holds GOMAXPROCS=1 (pid:matches)$environs"

	release AIRLOCK_DEMO_TOKEN 60m
	local proxy demo_value
	proxy="$(proxy_for "${AIRLOCK_PROXY_URL##*://}")"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"

	# The baseline: the same calls through a tunnel that reads nothing of what it carries. It is
	# a minute rather than ten: a median needs no more, and the ten minutes are the load's.
	"$STANDS_BIN" load tunnel --listen 127.0.0.1:0 --to 127.0.0.2:443 >"$WORK_DIR/k9/tunnel.addr" 2>"$WORK_DIR/k9/tunnel.log" &
	HELPER_PIDS+=($!)
	local plain=""
	for _ in $(seq 1 40); do
		plain="$(head -n 1 "$WORK_DIR/k9/tunnel.addr" 2>/dev/null)"
		[ -z "$plain" ] || break
		sleep 0.25
	done
	echo "airlock-check: k9 is measuring the baseline through a plain tunnel for 60 seconds..." >&2
	"$STANDS_BIN" load load --proxy "http://$plain" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--rate 50 --seconds 60 --out "$WORK_DIR/k9/baseline.json" >/dev/null 2>"$WORK_DIR/k9/baseline.err"

	local before=() after=() wall_before wall_after rss="$WORK_DIR/k9/rss.log"
	: >"$rss"
	k9_sample_rss "$rss" "${pids[@]}" &
	local sampler=$!
	HELPER_PIDS+=("$sampler")
	for pid in "${pids[@]}"; do before+=("$(k9_cpu "$pid")"); done
	wall_before="$(date +%s.%N)"
	echo "airlock-check: k9 is opening 50 connections a second through the airlock for ten minutes..." >&2
	"$STANDS_BIN" load load --proxy "$proxy" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header "$PLACEHOLDER" --rate 50 --seconds 600 --out "$WORK_DIR/k9/load.json" \
		>/dev/null 2>"$WORK_DIR/k9/load.err"
	wall_after="$(date +%s.%N)"
	for pid in "${pids[@]}"; do after+=("$(k9_cpu "$pid")"); done
	kill "$sampler" 2>/dev/null || true

	local calls failed p50 base_p50 added
	calls="$(jq -r '.calls' "$WORK_DIR/k9/load.json")"
	failed="$(jq -r '.failed' "$WORK_DIR/k9/load.json")"
	p50="$(jq -r '.p50_ms' "$WORK_DIR/k9/load.json")"
	base_p50="$(jq -r '.p50_ms' "$WORK_DIR/k9/baseline.json")"
	added="$(awk -v a="$p50" -v b="$base_p50" 'BEGIN { printf "%.1f", a - b }')"
	observe "the ten minutes: $calls calls, $failed of them failed; the median was $p50 ms through the airlock and $base_p50 ms through a plain tunnel over $(jq -r .calls "$WORK_DIR/k9/baseline.json") calls, so the proxy added $added ms at the median"
	observe "what failed, if anything: $(jq -c '.failures' "$WORK_DIR/k9/load.json")"
	judge "$([ "$failed" = 0 ] && [ "$calls" -gt 0 ] && echo 0 || echo 1)" \
		"no call fails in the ten minutes" "$failed of $calls calls failed"
	judge "$(awk -v x="$added" 'BEGIN { exit !(x <= 100) }' && awk -v b="$base_p50" 'BEGIN { exit !(b > 0) }' && echo 0 || echo 1)" \
		"the proxy adds at most 100 milliseconds at the median" \
		"the median was $p50 ms against the plain tunnel's $base_p50 ms over $(jq -r .calls "$WORK_DIR/k9/baseline.json") baseline calls ($(jq -r .failed "$WORK_DIR/k9/baseline.json") of them failed), which is $added ms added"

	local i wall cpu peak limits="" worst_cpu=0 over=0
	wall="$(awk -v a="$wall_before" -v b="$wall_after" 'BEGIN { printf "%.1f", b - a }')"
	for i in "${!pids[@]}"; do
		cpu="$(awk -v a="${before[$i]}" -v b="${after[$i]}" 'BEGIN { printf "%.1f", b - a }')"
		peak="$(k9_peak_kb "$rss" "${pids[$i]}")"
		limits="$limits; pid ${pids[$i]}: $cpu s of CPU over $wall s of wall (a share of $(awk -v c="$cpu" -v w="$wall" 'BEGIN { printf "%.2f", c / w }')), peak memory $(awk -v k="$peak" 'BEGIN { printf "%.0f", k / 1024 }') MiB"
		awk -v c="$cpu" -v w="$wall" 'BEGIN { exit !(c <= w / 2) }' || over=$((over + 1))
		[ "${peak:-0}" -le 1048576 ] || over=$((over + 1))
		worst_cpu="$cpu"
	done
	observe "each broker process over the ten minutes${limits}"
	judge "$([ "$over" = 0 ] && echo 0 || echo 1)" \
		"each process's CPU time is at most half its wall time and its peak memory at most 1 GiB" \
		"${limits#; } (the last CPU figure read $worst_cpu s)"

	# The upload, the answer with a gap in it, the upgrade and the clone.
	echo "airlock-check: k9 is uploading 50 MiB, waiting out a 90-second gap and cloning..." >&2
	"$STANDS_BIN" load upload --proxy "$proxy" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header "$PLACEHOLDER" --size $((50 * 1024 * 1024)) --out "$WORK_DIR/k9/upload.json" >/dev/null 2>&1
	"$STANDS_BIN" load gap --proxy "$proxy" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header "$PLACEHOLDER" --gap 90 --out "$WORK_DIR/k9/gap.json" >/dev/null 2>&1
	local s_mark
	s_mark="$(records_count s)"
	"$STANDS_BIN" load upgrade --proxy "$proxy" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header "$PLACEHOLDER" --out "$WORK_DIR/k9/upgrade.json" >/dev/null 2>&1

	# The clone goes through the same proxy on an ordinary CONNECT: github.com is no bound host,
	# so nothing is swapped into it and what is proved is that the tunnel carries a clone.
	local clone_status clone_out
	rm -rf "$WORK_DIR/k9/clone"
	clone_out="$(GIT_CONFIG_COUNT=2 \
		GIT_CONFIG_KEY_0=http.proxy GIT_CONFIG_VALUE_0="$proxy" \
		GIT_CONFIG_KEY_1=http.sslCAInfo GIT_CONFIG_VALUE_1="$AIRLOCK_CA_CERT_FILE" \
		git clone --depth 1 --quiet https://github.com/cybermonklabs/legion "$WORK_DIR/k9/clone" 2>&1 | tail -n 2 | tr '\n' ' ')"
	clone_status=$?
	local cloned=0
	[ -d "$WORK_DIR/k9/clone/.git" ] && cloned=1

	observe "the 50 MiB upload: $(cat "$WORK_DIR/k9/upload.json" | tr -d '\n ')"
	observe "the answer with a 90-second gap in it: $(cat "$WORK_DIR/k9/gap.json" | tr -d '\n ')"
	observe "the shallow clone of cybermonklabs/legion through the tunnel exited $clone_status and left a checkout: $cloned ($clone_out)"
	local upload_ok gap_ok
	upload_ok="$(jq -r '.status // 0' "$WORK_DIR/k9/upload.json")"
	gap_ok="$(jq -r '.second // 0' "$WORK_DIR/k9/gap.json")"
	judge "$([ "$upload_ok" = 200 ] && [ "$gap_ok" = 200 ] && [ "$cloned" = 1 ] && echo 0 || echo 1)" \
		"the upload, the answer with the gap and the clone finish" \
		"the upload answered $upload_ok, the second event after the 90-second gap answered $gap_ok, and the clone left a checkout: $cloned"

	local upgrade_recs upgrade_header upgrade_status
	upgrade_recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	upgrade_header="$(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Authorization // [] | .[]] | .[0] // ""')"
	upgrade_status="$(jq -r '.status // 0' "$WORK_DIR/k9/upgrade.json")"
	observe "the WebSocket upgrade: $(cat "$WORK_DIR/k9/upgrade.json" | tr -d '\n' | head -c 300); S recorded its Upgrade header as $(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Upgrade // [] | .[]] | .[0] // ""') and its Authorization as $(shape "$upgrade_header") against the stored $(shape "$demo_value") (compared by SHA-256 and length)"
	judge "$([ "$upgrade_status" = 101 ] && contains "$demo_value" "$upgrade_header" && contains "the service read" "$(jq -r '.frames // ""' "$WORK_DIR/k9/upgrade.json")" && echo 0 || echo 1)" \
		"the WebSocket upgrade reaches S with the real value in its header, and frames pass both ways" \
		"S answered $upgrade_status, its Authorization carried the real value ($(shape "$upgrade_header") against the stored $(shape "$demo_value"), compared by SHA-256 and length), and the frame that came back was: $(jq -r '.frames // ""' "$WORK_DIR/k9/upgrade.json")"

	# One broker stopped while a connection through it is open, and then the next call. The
	# connection is this run's own gap connection on the second broker's proxy port, so what is
	# stopped is the process carrying it.
	local stopped="${pids[-1]}" after_stop
	"$STANDS_BIN" load gap --proxy "http://agent-secrets:$PROXY_PASSWORD@${BROKER_PROXY_ADDRS[-1]}" \
		--host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" --header "$PLACEHOLDER" --gap 8 \
		--out "$WORK_DIR/k9/stopped.json" >/dev/null 2>&1 &
	local gap_pid=$!
	sleep 3
	kill "$stopped" 2>/dev/null || true
	wait "$gap_pid" 2>/dev/null || true
	# Four calls, not one: the stack's proxy front is a round-robin over both brokers' ports
	# that reads its backends from a file and asks nobody whether a backend is alive, so every
	# other connection still goes to the stopped process and is reset. In a cluster the Service
	# drops a terminating pod from its EndpointSlice; here nothing does, so what this reads is
	# that a call through the front reaches the broker that is still up.
	local after_stop="" status ok=0 i
	for i in 1 2 3 4; do
		status="$(curl -sS -o /dev/null -w '%{http_code}' --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
			--proxy "$proxy" -H "Authorization: Bearer $PLACEHOLDER" "$AIRLOCK_S_URL/ok" 2>&1 | tr -d '\n')"
		after_stop="$after_stop $status"
		[ "$status" = 200 ] && ok=$((ok + 1))
	done
	local still_up=0
	kill -0 "$stopped" 2>/dev/null && still_up=1
	observe "with broker $stopped stopped while it carried a connection: that connection answered $(cat "$WORK_DIR/k9/stopped.json" | tr -d '\n ') and the four calls after it answered$after_stop (the stopped process is still up: $still_up)"
	judge "$([ "$ok" -gt 0 ] && echo 0 || echo 1)" \
		"the stream on the stopped process ends after the drain, and the next call succeeds" \
		"the connection on the stopped process ended $(jq -r 'if .second then "with its second event answering " + (.second|tostring) else "with " + (.second_error // .error // "no answer") end' "$WORK_DIR/k9/stopped.json"), and $ok of the four calls after it answered 200 ($after_stop); the ones that did not are the connections the front still handed to the stopped process, which it keeps in its backends file with no health check of its own"

	stop_stack

	# The negative control: a stack of its own whose proxy cuts an idle connection at 60 seconds,
	# which is what the 90-second gap is measured against.
	start_stack --idle-seconds 60
	stack_facts
	release AIRLOCK_DEMO_TOKEN 30m
	"$STANDS_BIN" load gap --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" --host s.airlock.test \
		--ca "$AIRLOCK_CA_CERT_FILE" --header "$PLACEHOLDER" --gap 90 \
		--out "$WORK_DIR/k9/control-gap.json" >/dev/null 2>&1
	local control_second
	control_second="$(jq -r '.second // ""' "$WORK_DIR/k9/control-gap.json")"
	control "with --idle-seconds 60 the same answer with a 90-second gap in it: $(cat "$WORK_DIR/k9/control-gap.json" | tr -d '\n ')"
	judge "$([ -z "$control_second" ] && echo 0 || echo 1)" \
		"negative control: with the idle limit at 60 seconds the answer with the 90-second gap is cut" \
		"its second event $(jq -r 'if .second then "answered " + (.second|tostring) else "did not arrive: " + (.second_error // .error // "no answer") end' "$WORK_DIR/k9/control-gap.json")"

	stop_stack
}

###############################################################################################
# k15
###############################################################################################

check_k15() {
	# The theorems are the ones Axioms.lean names: it holds one `#print axioms` line for each,
	# and this check reads that list rather than writing a second copy of it, so a theorem
	# added there is one k15 prints, counts and expects with nothing edited here.
	local theorems count
	theorems="$(sed -n 's/^#print axioms  *//p' "$LEAN_DIR/Axioms.lean")"
	count="$(printf '%s\n' "$theorems" | grep -c .)"
	if [ "$count" = 0 ]; then
		echo "airlock-check: $LEAN_DIR/Axioms.lean names no theorem, so k15 has nothing to print or count" >&2
		exit 1
	fi

	expect "the Lean model builds on the head"
	expect "its $count theorems are there and are printed with their statements"
	expect "they depend on no sorry and on no axiom outside propext, Classical.choice and Quot.sound"
	expect "the comparison fuzz test passes for ten minutes"
	expect "the negative control, the six mutants of scripts/airlock-mutants.sh, is caught: each Go mutant fails the comparison and each model mutant fails lake build or that same comparison"

	if ! command -v lake >/dev/null 2>&1; then
		echo "airlock-check: k15 needs lake from the Lean toolchain; set ELAN_HOME and PATH" >&2
		exit 1
	fi

	local build_log="$WORK_DIR/lake-build.log"
	echo "airlock-check: building the Lean model..." >&2
	(cd "$LEAN_DIR" && lake build) >"$build_log" 2>&1
	local build_status=$?
	local model="$LEAN_DIR/.lake/build/bin/airlock-model"
	observe "lake build exited $build_status: $(tail -n 1 "$build_log")"
	judge "$([ "$build_status" = 0 ] && [ -x "$model" ] && echo 0 || echo 1)" \
		"the model builds" "lake build exited $build_status and wrote $model"

	# The statements, printed from a file of this run's own so nothing is added to the tree.
	# Its list is the one read from Axioms.lean above, each `#print axioms X` written out as
	# `#check @X`.
	{
		echo "import Airlock"
		printf '%s\n' "$theorems" | sed 's/^/#check @/'
	} >"$WORK_DIR/Statements.lean"
	(cd "$LEAN_DIR" && lake env lean "$WORK_DIR/Statements.lean") >"$WORK_DIR/statements.txt" 2>&1
	local statements_status=$?
	# scripts/airlock-lean-axioms.sh is the one copy of the axiom check, and ci.yaml's
	# airlock-lean job runs the same one: it runs lake env lean Axioms.lean, keeps the axiom
	# text where this block reads it back, and says on its standard error what is wrong.
	local bad
	bad="$(bash "$SCRIPT_DIR/airlock-lean-axioms.sh" "$LEAN_DIR" --axioms-file "$WORK_DIR/axioms.txt" 2>&1 >/dev/null)"
	local axioms_status=$?
	echo
	echo "the model's theorem statements:"
	sed 's/^/  /' "$WORK_DIR/statements.txt"
	echo
	echo "the theorems' axioms:"
	sed 's/^/  /' "$WORK_DIR/axioms.txt"
	observe "lake env lean printed the $count theorem statements (exit $statements_status) and scripts/airlock-lean-axioms.sh read their axioms (exit $axioms_status); both are above this block"
	# `#check @X` opens its line with "@Airlock.X" for a theorem with implicit arguments and
	# with "Airlock.X" for one without, and a statement runs over several lines, so the count
	# is of the lines that open one.
	local printed
	printed="$(grep -cE '^@?Airlock\.' "$WORK_DIR/statements.txt")"
	judge "$([ "$statements_status" = 0 ] && [ "$printed" = "$count" ] && echo 0 || echo 1)" \
		"its $count theorems are there and are printed with their statements" \
		"$printed of the $count theorems Axioms.lean names type-checked and printed"

	observe "the axioms each theorem rests on: $(tr '\n' ' ' <"$WORK_DIR/axioms.txt")"
	judge "$axioms_status" \
		"the theorems depend on no sorry and on no axiom outside the three Lean admits" \
		"${bad:-every one of the $count rests on propext, Classical.choice and Quot.sound alone, and none on sorryAx}"

	echo "airlock-check: fuzzing the Go airlock against the model for ten minutes..." >&2
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -run FuzzAirlock ./internal/broker/airlock/) >"$WORK_DIR/seed-corpus.log" 2>&1
	local seed_status=$?
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -fuzz FuzzAirlock -fuzztime=600s -run FuzzAirlock ./internal/broker/airlock/) >"$WORK_DIR/fuzz.log" 2>&1
	local fuzz_status=$?
	observe "the seed corpus comparison exited $seed_status: $(tail -n 2 "$WORK_DIR/seed-corpus.log" | tr '\n' ' ')"
	observe "ten minutes of fuzzing exited $fuzz_status: $(grep -E '^(ok|FAIL|--- FAIL|fuzz:)' "$WORK_DIR/fuzz.log" | tail -n 3 | tr '\n' ' ')"
	judge "$([ "$seed_status" = 0 ] && [ "$fuzz_status" = 0 ] && echo 0 || echo 1)" \
		"the comparison fuzz test passes for ten minutes" \
		"the seed corpus exited $seed_status and -fuzztime=600s exited $fuzz_status"

	echo "airlock-check: running the mutants..." >&2
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" bash "$SCRIPT_DIR/airlock-mutants.sh") >"$WORK_DIR/mutants.log" 2>&1
	local mutants_status=$?
	echo
	echo "the mutants:"
	grep -E '^(PASS|FAIL):' "$WORK_DIR/mutants.log" | sed 's/^/  /'
	# The check's words name six mutants, which is a floor and not a count: the tree holds every
	# mutant of the decision anyone has written, and each must be caught. So this reads two
	# things — that airlock-mutants.sh caught every mutant it found, and that the six the check
	# names are among them.
	local caught named missing=""
	caught=$(grep -cE '^PASS:' "$WORK_DIR/mutants.log" || true)
	for named in go-extra-header go-no-host-check go-judge-over-900 go-carries-value lean-all-headers lean-carries-value; do
		grep -qE "^PASS: $named " "$WORK_DIR/mutants.log" || missing="$missing $named"
	done
	control "scripts/airlock-mutants.sh exited $mutants_status: $caught mutant(s) caught, the check's six among them"
	control "$(grep -E '^(PASS|FAIL):' "$WORK_DIR/mutants.log" | tr '\n' ' ')"
	judge "$([ "$mutants_status" = 0 ] && [ -z "$missing" ] && echo 0 || echo 1)" \
		"negative control: each of the six mutants is caught" \
		"airlock-mutants.sh exited $mutants_status with $caught caught and none of the six missing${missing:+; missing:$missing}"
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run: a check must report every one of its properties, and a refused request, a command that
# exited 1 and a grep that found nothing are all ordinary answers a step reads. Everything the
# run needs before this point - its arguments, its work directory and its stack - is still
# under set -e, and await ends the run itself when a stack does not come up.
#
# No check below turns it back on. A `set -e` in one of them re-arms it for every step of every
# check after it, and the step it would then end the run at is the one a failing run most needs
# reported: k15's statement count against a model that did not build, say, which would leave the
# run with no result block at all.
set +e

case "$CHECK_ID" in
k1) check_k1 ;;
k2) check_k2 ;;
k3) check_k3 ;;
k4) check_k4 ;;
k5) check_k5 ;;
k6) check_k6 ;;
k7) check_k7 ;;
k8a) check_k8a ;;
k9) check_k9 ;;
k15) check_k15 ;;
esac

report
