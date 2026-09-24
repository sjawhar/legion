#!/usr/bin/env bash
# Proves a tmux stage proof's agents reached the model only through the Hawk model gateway: every
# agent turn its OMP profile recorded, each subagent's included, was served by the anthropic
# provider, the one lib/install-model-gateway.sh routes to the gateway and leaves enabled. Oh My Pi
# falls back without a word: with its key command failing, a Go pane answered as
# amazon-bedrock/us.anthropic.claude-opus-4-8 on the devbox's instance role, and a Stage 3 retro's
# scout subagent ran on amazon-bedrock/openai.gpt-oss-120b. Here a turn on any other provider fails
# the proof, whatever put it there.
#
#   scripts/e2e/lib/check-model-route.sh --sessions <dir> --control <dir>
#
# <sessions> is the profile's agent/sessions directory. Each session is a JSONL file under it; a
# subagent's is the <AgentName>.jsonl in its parent session's own directory. Every assistant turn
# must record message.provider anthropic, and every model_change a model under anthropic/. A line
# that does not parse is skipped: a live agent may be mid-write.
#
# The negative control runs on this run's own evidence: the first session holding a turn is copied
# into <control> with that one turn rewritten as Bedrock's, and the same check must refuse the
# copy. A check that passed it would prove nothing.
#
# Stdout is one line, what was checked. Exit 1 names every session off the route, with what it
# recorded, or a control the check passed; exit 2 refuses the arguments.
set -euo pipefail

me=check-model-route
provider=anthropic
control_model=amazon-bedrock/us.anthropic.claude-opus-4-8

refuse() {
  echo "$me: $*" >&2
  exit 2
}
fail() {
  echo "$me: $*" >&2
  exit 1
}

sessions=
control=
while [ $# -gt 0 ]; do
  case "$1" in
  --sessions)
    [ $# -ge 2 ] || refuse "--sessions needs a directory"
    sessions=$2
    shift 2
    ;;
  --control)
    [ $# -ge 2 ] || refuse "--control needs a directory"
    control=$2
    shift 2
    ;;
  *) refuse "unknown argument '$1'" ;;
  esac
done
[ -n "$sessions" ] || refuse "--sessions is required: the OMP profile's agent/sessions directory"
[ -n "$control" ] || refuse "--control is required: the directory the negative control's copy is written to"
[ -d "$sessions" ] || fail "--sessions $sessions is not a directory"

# off_route FILE… prints "<file>: <what it recorded>" for each turn or model change off the route.
off_route() {
  jq -R -r --arg provider "$provider" '
    fromjson? | select(type == "object")
    | if .type == "message" and .message.role == "assistant" then
        select(.message.provider != $provider)
        | "\(input_filename): turn on \(.message.provider // "no provider")/\(.message.model // "no model")"
      elif .type == "model_change" then
        select((.model // "") | startswith($provider + "/") | not)
        | "\(input_filename): model change to \(.model // "no model")"
      else empty end' "$@" | sort -u
}
turn_count() {
  jq -R -c 'fromjson? | select(type == "object" and .type == "message" and .message.role == "assistant")' "$@" | wc -l
}

mapfile -t files < <(find "$sessions" -type f -name '*.jsonl' | sort)
[ "${#files[@]}" -gt 0 ] || fail "no agent session under $sessions"
subagents=$(find "$sessions" -mindepth 3 -type f -name '*.jsonl' | wc -l)
turns=$(turn_count "${files[@]}")
[ "$turns" -gt 0 ] || fail "no agent session under $sessions holds an assistant turn"
off=$(off_route "${files[@]}")
[ -z "$off" ] || fail "agent sessions off the $provider route (the gateway):"$'\n'"$off"

# The control: the first session holding a turn, that turn alone rewritten as Bedrock's.
first=
for file in "${files[@]}"; do
  if [ "$(turn_count "$file")" -gt 0 ]; then
    first=$file
    break
  fi
done
# Captured whole, then cut in bash: `jq … | head -1` under pipefail exits 141 once jq writes past
# the pipe's buffer after head has gone, on a session of about a thousand turns.
lines=$(jq -R -r 'input_line_number as $n | fromjson? | select(type == "object" and .type == "message" and .message.role == "assistant") | $n' "$first")
line=${lines%%$'\n'*}
mkdir -p "$control"
copy=$control/$(basename "$first")
rewritten=$(sed -n "${line}p" "$first" | jq -c --arg provider "${control_model%%/*}" --arg model "${control_model#*/}" \
  '.message.provider = $provider | .message.model = $model')
REWRITTEN=$rewritten awk -v n="$line" 'NR == n { print ENVIRON["REWRITTEN"]; next } { print }' "$first" >"$copy"
refused=$(off_route "$copy")
[ "$refused" = "$copy: turn on $control_model" ] ||
  fail "the negative control passed: $copy, $first with turn $line rewritten as $control_model, was not refused (the check printed '${refused}')"

echo "$turns assistant turns in ${#files[@]} agent sessions ($subagents of them subagents'), every one on the $provider provider (the gateway); negative control: $copy ($first with turn $line on $control_model) refused"
