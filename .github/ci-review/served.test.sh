#!/usr/bin/env bash
# Tests for `served`, the CI reviewer's check of which model answered: asked, run's refusals and its record and stop,
# and check's verdicts. A stand-in `claude` (a shell function's printf) writes the stream-json events.
#
# Run from anywhere: .github/ci-review/served.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
served="$here/served"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$here/../scripts/test-lib.sh"

model=claude-opus-5-5
asked=anthropic/$model

# stream <model>...: a command that prints one assistant event per model (each its own message id), then a result.
stream() {
  local script="" n=0 m
  for m in "$@"; do
    n=$((n + 1))
    script+="printf '%s\n' '{\"type\":\"assistant\",\"message\":{\"id\":\"msg_$n\",\"model\":\"$m\"}}';"
  done
  script+="printf '%s\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"Verdict: no blocking findings\"}'"
  echo "$script"
}

# served_run <name> <command> [env assignments...]: runs served in a clean environment and a fresh directory under
# $work/<name>, which is also its working directory; sets out and status.
served_run() {
  local dir="$work/$1" command=$2
  shift 2
  mkdir -p "$dir/home"
  set +e
  out=$(cd "$dir" && env -i PATH="$PATH" HOME="$dir/home" "$@" python3 "$served" run "$dir" "$asked" -- \
    bash -c "$command" 2>&1)
  status=$?
  set -e
}

record_check() { # record_check <dir>: sets out and status from check on <dir>/served
  set +e
  out=$(python3 "$served" check < "$1/served")
  status=$?
  set -e
}

echo "asked"
set +e; python3 "$served" asked "$asked"; s=$?; set -e
check "anthropic/<model> is checkable" "$(is "$s" 0)"
set +e; python3 "$served" asked "bedrock/$model" 2> /dev/null; s=$?; set -e
check "another provider is not" "$(is "$s" 2)"

echo "run: the model asked for, and a dated snapshot"
served_run ok "$(stream "$model" "$model-20261001" "$model")"
check "exits 0" "$(is "$status" 0)"
check "records three turns" "$(is "$(cat "$work/ok/served")" "$(printf 'asked %s\nendpoint api.anthropic.com\nanthropic/%s 2\nanthropic/%s-20261001 1' "$asked" "$model" "$model")")"
check "claude.json is the result" "$(contains "$(cat "$work/ok/claude.json")" '"type":"result"')"
check "claude.jsonl is the stream" "$(is "$(wc -l < "$work/ok/claude.jsonl")" 4)"
record_check "$work/ok"
check "check passes" "$(is "$status" 0)"
check "check names the turns" "$(is "$out" "firstParty/$model (3 turns)")"

echo "run: an API error message is no turn"
served_run apierr "printf '%s\n' '{\"type\":\"assistant\",\"is_api_error_message\":true,\"message\":{\"id\":\"x\",\"model\":\"<synthetic>\"}}'; $(stream "$model")"
check "exits 0" "$(is "$status" 0)"
record_check "$work/apierr"
check "check counts one turn" "$(is "$out" "firstParty/$model (1 turns)")"

echo "run: another model stops it"
served_run other "$(stream "$model" claude-haiku-9 "$model")"
check "exits 4" "$(is "$status" 4)"
check "says which model" "$(contains "$out" 'claude-haiku-9 answered')"
check "never reads the third event" "$(is "$(wc -l < "$work/other/claude.jsonl")" 2)"
record_check "$work/other"
check "check refuses it" "$(is "$status" 1)"

echo "run: refusals"
for case in "ANTHROPIC_BASE_URL=https://proxy.internal.example" "ANTHROPIC_BETAS=x" "CLAUDE_CODE_USE_BEDROCK=1" \
  "https_proxy=http://proxy.internal.example" "HTTPS_PROXY=http://proxy.internal.example" "ALL_PROXY=socks5://x" \
  "NODE_TLS_REJECT_UNAUTHORIZED=0" "NODE_EXTRA_CA_CERTS=/tmp/ca" "SSL_CERT_FILE=/tmp/ca" "SSL_CERT_DIR=/tmp"; do
  name=refuse-${case%%=*}
  served_run "$name" "touch started; $(stream "$model")" "$case"
  check "$case is refused with 4" "$(is "$status" 4)"
  check "$case starts nothing" "$(is "$([ -e "$work/$name/started" ] && echo yes || echo no)" no)"
done
served_run allowed "$(stream "$model")" ANTHROPIC_BASE_URL=https://api.anthropic.com/ ANTHROPIC_FEDERATION_RULE_ID=x
check "Anthropic's own URL and a federation id are allowed" "$(is "$status" 0)"
for settings in home/.claude/settings.json .claude/settings.json .claude/settings.local.json; do
  name=settings-${settings//\//-}
  mkdir -p "$(dirname "$work/$name/$settings")"
  echo '{"env":{"ANTHROPIC_BASE_URL":"https://proxy.internal.example"}}' > "$work/$name/$settings"
  served_run "$name" "touch started; $(stream "$model")"
  check "$settings's env is read and refused" "$(is "$status" 4)"
  check "$settings's refusal starts nothing" "$(is "$([ -e "$work/$name/started" ] && echo yes || echo no)" no)"
done

echo "run: Claude Code's own failure"
served_run fails "$(stream "$model"); exit 7"
check "exits with its status" "$(is "$status" 7)"

echo "check"
check_text() { set +e; out=$(printf '%s' "$1" | python3 "$served" check); status=$?; set -e; }
check_text "asked $asked
endpoint proxy.internal.example
anthropic/$model 2
"
check "another endpoint exits 1" "$(is "$status" 1)"
check_text "asked $asked
endpoint api.anthropic.com
"
check "no turn exits 3" "$(is "$status" 3)"
check_text ""
check "an empty record exits 3" "$(is "$status" 3)"

summary "served"
