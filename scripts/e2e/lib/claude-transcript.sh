# shellcheck shell=bash disable=SC2154,SC2034 # the variables below are the caller's (listed below)
# What a Claude Code backend proof reads in its implementer's transcript: Claude Code's own session
# JSONL, one entry per line, that the implementer's generation writes as it works. Shared by
# claude-backend-tmux.sh, whose transcript is a file on the devbox, and claude-backend-sandbox.sh,
# whose transcript is a file on the implementer pod's own disk. Each judges the same entries; only
# the way it reads the file's bytes is its own.
#
# Sourced after lib/workflow.sh (both callers source both), whose role_handoff handoff_recorded
# reads. The caller sets (beside note):
#   issue         the proof's issue, whose implementer handoff handoff_recorded looks for
#   stall_offset  the transcript's size in bytes when the proof told the implementer to stall,
#                 which report_stall_entries reports from
# defines:
#   transcript_since OFFSET
#     prints the transcript's bytes past OFFSET bytes, and fails when it cannot read them. Every
#     predicate here reads the transcript through it alone, past an offset, so a poll reads the new
#     entries alone, not a session that grows all run; OFFSET 0 is the whole transcript.
# and reads, after sourcing:
#   stall_reminder_needle  the reminder's wording the predicates look for, which the caller names
#                          in its own notes

# The opening of what a worker whose turn ended with its phase still open is told (REMINDER in
# packages/pi-shared/src/phase-stall.ts), which the implementer's `legion` MCP server answers its
# `Stop` hook with. A fragment of that paragraph, not the whole of it, so a later rewording of the
# rest does not break a proof silently; this sentence is in no other message a worker is given.
stall_reminder_needle="Your turn ended with your Legion phase still open"
# claude_entries OFFSET prints one line per transcript entry written past OFFSET bytes: `assistant`
# for an assistant message, and `user <its text>` for a user one — its content, a string or the text
# of its blocks, every run of whitespace squeezed to one space. One line is one entry, so the
# predicates below decide on one message rather than on a line of one.
claude_entries() {
  transcript_since "$1" | jq -R -r '
    fromjson?
    | if .type == "assistant" then "assistant"
      elif .type == "user" then
        "user " + ((.message.content // "")
          | if type == "string" then . else [.[]? | select(.type == "text") | .text] | join(" ") end
          | gsub("\\s+"; " "))
      else empty end'
}
# stop_hook_reminded OFFSET: past OFFSET bytes the session holds Claude Code's own `Stop hook
# feedback`, carrying the reminder — the `{"decision":"block","reason":…}` the implementer's legion
# MCP server answered the `Stop` hook with (packages/claude-envoy/src/legion-phase-hook.ts), which
# Claude Code turns into a user turn. Both in the one entry: the feedback is what makes it the
# hook's own reminder rather than any other line written into the session.
# reminded_or_completed OFFSET is the stall check's wait: the reminder reached the implementer
# (stop_hook_reminded), or the daemon recorded its handoff first, which leaves no open phase to
# remind it of. The check then tells the two apart, so a worker that did its phase from the task that
# started it, against the proof's instructions, is a named failure and never a timeout.
reminded_or_completed() { stop_hook_reminded "$1" || handoff_recorded; }
stop_hook_reminded() {
  local entries
  entries=$(claude_entries "$1") || return 1
  awk -v needle="$stall_reminder_needle" \
    'index($0, "Stop hook feedback") && index($0, needle) { found = 1 } END { exit !found }' <<<"$entries"
}
# reminded_turn_continued OFFSET: an assistant message follows that feedback, so the block carried
# the turn on in the same session rather than ending it.
reminded_turn_continued() {
  local entries
  entries=$(claude_entries "$1") || return 1
  awk -v needle="$stall_reminder_needle" '
    index($0, "Stop hook feedback") && index($0, needle) { reminded = 1; next }
    reminded && $0 == "assistant" { continued = 1 }
    END { exit !continued }' <<<"$entries"
}
# report_stall_entries is that step's timeout_hook: what the transcript held when the wait gave up.
report_stall_entries() {
  note "the implementer's transcript since the stall instruction: $(claude_entries "$stall_offset" | cut -c1-200 | tr '\n' '|')"
}
# legion_handoff_in: the transcript holds a call of the `legion` MCP tool whose input names
# handoff_complete, and a tool result for it that is not an error.
legion_handoff_in() {
  transcript_since 0 | jq -R -s -e '
    [split("\n")[] | fromjson? | select(.message.content | type == "array") | .message.content[]] as $blocks
    | [$blocks[] | select(.type == "tool_use" and (.name | endswith("__legion")) and (.input | tostring | contains("handoff_complete"))) | .id] as $calls
    | any($blocks[]; .type == "tool_result" and (.tool_use_id as $id | $calls | index($id)) and (.is_error != true))' >/dev/null
}
# resumed_reply_names OFFSET MARKER: an assistant text written past OFFSET bytes quotes MARKER.
resumed_reply_names() {
  local text
  text=$(transcript_since "$1" | jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text')
  grep -Fq -- "$2" <<<"$text"
}
# handoff_recorded: the daemon recorded the implementer's handoff of the issue's implementing phase.
handoff_recorded() { [ -n "$(role_handoff "$issue" implementer implementing)" ]; }
