#!/usr/bin/env bash
# Tests for `ci-retry.sh`: what the helper does, measured by running it, never read off its
# source. The attempt count is counted from a file each fake command appends to, the sleeps and
# the per-attempt bound are read from the times in that file, and the floor R1 sets — five
# attempts, the fifth starting at least 120 s after the first — is the measured span between the
# first and the fifth line of it. Every number the cases assert is printed beside the assertion,
# so a reader of the log sees the measurement rather than the expectation.
#
# The helper is sourced into this harness, which runs with `set -euo pipefail` on: that is how a
# workflow step uses it, and it is also the proof that a failing attempt does not end the caller.
# The case that fails twice and then succeeds calls `retry_fetch` bare, with no `set +e` around
# it, so a leaked failure would kill this script at its first failed attempt and no summary would
# be printed at all.
#
# It costs about 190 s, almost all of it in one case: the five attempts against a command that
# hangs for ever are what the 120 s floor is measured from, and measuring it is the point.
#
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/ci-retry.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/ci-retry.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The one call that matters: sourced into a shell that has errexit and pipefail on, exactly as a
# workflow step sources it.
# shellcheck source=.github/scripts/ci-retry.sh
. "$helper"

# run_retry <arguments...>: calls retry_fetch, leaving its merged output in $out, its exit status
# in $status and its wall time in whole seconds in $elapsed.
run_retry() {
  local start
  start=$(date +%s)
  set +e
  out=$(retry_fetch "$@" 2>&1)
  status=$?
  set -e
  elapsed=$(($(date +%s) - start))
}

# near <actual> <expected> <tolerance>: true/false for check.
near() {
  if [ "$1" -ge "$(($2 - $3))" ] && [ "$1" -le "$(($2 + $3))" ]; then echo true; else echo false; fi
}

# The status the local `timeout` returns when it kills: 124 from GNU coreutils, 143 from BusyBox.
# The helper may only call 124 the bound's own kill, so which one this machine gives decides which
# wording the hanging case expects.
set +e
timeout 1 sleep 5
kill_status=$?
set -e
echo "measured: this machine's \`timeout\` exits $kill_status when it kills"

echo "case: sourcing the helper sets no shell option and adds one name"
check "errexit is still on in this harness" "$(contains "$(set +o)" '^set -o errexit$')"
check "pipefail is still on in this harness" "$(contains "$(set +o)" '^set -o pipefail$')"
cat > "$work/probe" <<'PROBE'
#!/usr/bin/env bash
# Sources the helper in a shell that has set no option at all, and reports what sourcing it
# changed: the shell's options, and the function names it added.
before_options=$(set +o)
before_functions=$(declare -F | awk '{print $NF}' | sort)
# shellcheck source=/dev/null
. "$1"
after_options=$(set +o)
after_functions=$(declare -F | awk '{print $NF}' | sort)
if [ "$before_options" = "$after_options" ]; then echo "options: unchanged"; else echo "options: changed"; fi
printf 'added: %s\n' "$(comm -13 <(echo "$before_functions") <(echo "$after_functions") | paste -sd,)"
PROBE
chmod +x "$work/probe"
probe_out=$(bash "$work/probe" "$helper")
indent "$probe_out"
check "an option-less shell has the same options after sourcing" "$(contains "$probe_out" '^options: unchanged$')"
check "and exactly one new function, retry_fetch" "$(contains "$probe_out" '^added: retry_fetch$')"

echo "case: a command that succeeds on the first attempt returns 0 and sleeps nothing"
run_retry "a tool already on disk" 5 -- true
echo "  measured: exit $status after ${elapsed}s"
check "exits 0" "$(is "$status" 0)"
check "costs no sleep" "$([ "$elapsed" -lt 3 ] && echo true || echo false)"
check "prints no failure line" "$(is "$(contains "$out" 'attempt')" false)"
check "and no error annotation" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a command that fails twice and then succeeds makes three attempts, 10 s and 20 s apart"
cat > "$work/flaky" <<FLAKY
#!/usr/bin/env bash
# Records the time this attempt started, then fails until the third one.
date +%s >> "$work/flaky.starts"
[ "\$(wc -l < "$work/flaky.starts")" -ge 3 ]
FLAKY
chmod +x "$work/flaky"
: > "$work/flaky.starts"
flaky_start=$(date +%s)
# Called bare, under this harness's errexit and pipefail: if a failed attempt leaked out of the
# helper, this script would end here and print no summary.
retry_fetch "the twice-failing tool" 5 -- "$work/flaky" > "$work/flaky.log" 2>&1
flaky_elapsed=$(($(date +%s) - flaky_start))
flaky_out=$(cat "$work/flaky.log")
mapfile -t flaky_starts < "$work/flaky.starts"
indent "$flaky_out"
check "the caller's shell survived two failed attempts under set -e" true
check "exactly three attempts ran" "$(is "${#flaky_starts[@]}" 3)"
if [ "${#flaky_starts[@]}" -eq 3 ]; then
  echo "  measured: 3 attempts over ${flaky_elapsed}s, the second $((flaky_starts[1] - flaky_starts[0]))s after the first and the third $((flaky_starts[2] - flaky_starts[1]))s after the second"
  check "the first sleep was 10 s" "$(near "$((flaky_starts[1] - flaky_starts[0]))" 10 2)"
  check "the second was 20 s" "$(near "$((flaky_starts[2] - flaky_starts[1]))" 20 2)"
fi
check "the whole call took about 30 s" "$(near "$flaky_elapsed" 30 3)"
check "it named the first failure, its status and what was left" \
  "$(contains "$flaky_out" '^the twice-failing tool: attempt 1 of 5 failed (exit 1); 4 attempts left$')"
check "and the second" \
  "$(contains "$flaky_out" '^the twice-failing tool: attempt 2 of 5 failed (exit 1); 3 attempts left$')"

echo "case: the helper's own lines go to standard error, so a caller may capture the command's output"
# A caller that reads a download into a variable — `names=$(retry_fetch … -- gh api …)` — gets the
# command's own output and nothing of the helper's, or every such call site would have to strip
# the retry log out of the value it just fetched.
cat > "$work/noisy" <<NOISY
#!/usr/bin/env bash
date +%s >> "$work/noisy.starts"
if [ "\$(wc -l < "$work/noisy.starts")" -lt 2 ]; then exit 7; fi
echo "the payload"
NOISY
chmod +x "$work/noisy"
: > "$work/noisy.starts"
captured=$(retry_fetch "the noisy tool" 5 -- "$work/noisy" 2> "$work/noisy.err")
noisy_err=$(cat "$work/noisy.err")
indent "$noisy_err"
check "standard output carried the command's output alone" "$(is "$captured" 'the payload')"
check "and the failed attempt was reported on standard error" \
  "$(contains "$noisy_err" '^the noisy tool: attempt 1 of 5 failed (exit 7); 4 attempts left$')"
check "and printed no error annotation, since it succeeded" "$(is "$(contains "$flaky_out" '::error::')" false)"

echo "case: a command that hangs for ever makes five bounded attempts, the fifth past the 120 s floor"
cat > "$work/hangs" <<HANGS
#!/usr/bin/env bash
# Records the time this attempt started, then hangs until \`timeout\` kills it. exec, so the kill
# reaches the sleep rather than a parent that would leave it running.
date +%s >> "$work/hangs.starts"
exec sleep 600
HANGS
chmod +x "$work/hangs"
: > "$work/hangs.starts"
run_retry "the stalled tool" 2 -- "$work/hangs"
mapfile -t hangs_starts < "$work/hangs.starts"
indent "$out"
check "exactly five attempts ran" "$(is "${#hangs_starts[@]}" 5)"
if [ "${#hangs_starts[@]}" -eq 5 ]; then
  span=$((hangs_starts[4] - hangs_starts[0]))
  gaps=""
  for i in 1 2 3 4; do gaps="$gaps $((hangs_starts[i] - hangs_starts[i - 1]))s"; done
  echo "  measured: attempt starts$gaps apart, ${span}s from the first attempt to the fifth, ${elapsed}s in all, exit $status"
  # Each gap is this attempt's 2 s bound plus the sleep after it. Against a command that would run
  # for 600 s, a gap of 12 s is the attempt ending at its bound and nowhere else.
  check "the first attempt ended at its 2 s bound and was followed by a 10 s sleep" "$(near "$((hangs_starts[1] - hangs_starts[0]))" 12 2)"
  check "the second, by a 20 s sleep" "$(near "$((hangs_starts[2] - hangs_starts[1]))" 22 2)"
  check "the third, by a 40 s sleep" "$(near "$((hangs_starts[3] - hangs_starts[2]))" 42 2)"
  check "the fourth, by an 80 s sleep" "$(near "$((hangs_starts[4] - hangs_starts[3]))" 82 2)"
  check "the fifth attempt started at least 120 s after the first" \
    "$([ "$span" -ge 120 ] && echo true || echo false)"
fi
check "it returns the last attempt's non-zero status" "$(is "$status" "$kill_status")"
if [ "$kill_status" -eq 124 ]; then
  check "each attempt is reported as having reached its bound" \
    "$(is "$(grep -c 'reached its 2s bound (exit 124)' <<<"$out")" 5)"
else
  check "no attempt is reported as having reached its bound, since this timeout exits $kill_status" \
    "$(is "$(grep -c "failed (exit $kill_status)" <<<"$out")" 5)"
fi
check "its last line is the error annotation, naming the download and the attempts" \
  "$(is "$(tail -n 1 <<<"$out")" '::error::the stalled tool failed to download after 5 attempts (last exit '"$kill_status"')')"

echo "case: a malformed call is refused, naming the mistake"
run_retry "a tool" 5 true
check "a call with no -- separator is refused" "$(is "$status" 2)"
check "and says the separator is missing" "$(contains "$out" '^::error::retry_fetch: no -- separator before the command')"
check "it slept nothing" "$([ "$elapsed" -lt 3 ] && echo true || echo false)"

run_retry "a tool"
check "a call with fewer than three arguments is refused" "$(is "$status" 2)"
check "and says what it takes" "$(contains "$out" '^::error::retry_fetch takes <what> <per-attempt bound in seconds> -- <command\.\.\.>, and got 1 argument')"

run_retry "a tool" 30s -- true
check "a bound that is not a whole number is refused" "$(is "$status" 2)"
check "and names the bound" "$(contains "$out" "^::error::retry_fetch: the per-attempt bound is a positive whole number of seconds, not '30s'\$")"

run_retry "a tool" 0 -- true
check "a bound of zero is refused" "$(is "$status" 2)"
check "and names it" "$(contains "$out" "not '0'\$")"

run_retry "a tool" 5 --
check "a call with nothing after -- is refused" "$(is "$status" 2)"
check "and says so" "$(contains "$out" '^::error::retry_fetch: nothing to run after --')"

summary "ci-retry.sh"
