# The stage 4b design-gate verdict on the spec whose document id is $artifact, which a human
# approved at $version. Input, slurped: the issue's asks, its events, then each version of the spec
# an approval request was handed back at (GET /api/v1/issues/{key}/asks and /events, then
# GET /api/v1/artifacts/{id}/versions/{number}).
#   request     the question of the approval request at $version, or null when none names it
#   summarized  whether that question carries a summary after "Approve <name> (version N)?"
#   blocks      how many of the spec's decision blocks a human answered
#   early       each approval hand-back made too early, as "version N: <block question>", by either
#               rule below. A request follows later versions in place, so the shared event selector
#               reads its ask.opened and each ask.handed_back as hand-backs, never an ask.edited,
#               which only rewords it.
# open_in_version: the version a hand-back named in requested_version, the version the agent handed
# to the human, still held one of the spec's blocks open. The version records each block's state;
# the block's ask cannot order that, since Dispatch indexes a block as an ask when it settles the
# document, after the edit that wrote it. Only a block whose id is one of the spec's block asks
# counts, so an :::ask quoted in code (a fence, or a list item that opens with one) is never read as
# a block. It judges every hand-back, answered or not.
# answered_after: a hand-back came before a human answered one of the spec's blocks, and that block
# was raised before the human's first turn on the hand-back: an answer to its request, or a human's
# reply in the request's thread, before the agent handed the same request back again. That covers
# a request made while the choice was still prose, before its block existed, and one sent in
# parallel with the edit that wrote the block; an answer's time is written as the human answers,
# and a block's ask is indexed as the document settles. A block raised after that turn takes up
# what the human said, so the flows the dispatch skill and legion-architect prescribe pass: the
# human replies in the request's thread (or answers Request changes), the revision raises the
# block, the human answers it, and the architect hands the request back (or requests again, which
# opens a new request since Request changes answered the old one).
include "design-gate-approval-requests";
def ts: (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?Z$") // error("not an RFC3339 UTC time: \(.)")) | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | "0." + . | tonumber);
.[0] as $asks | .[1] as $events | .[2:] as $versions
| [$asks[] | select(.kind == "question" and .block_id != null and .block_artifact.id == $artifact)] as $blocks
| ($events | approval_requests($artifact)) as $requests
| ([$requests[] | select(.approval.requested_version == $version)] | last | .question) as $request
| def open_in_version: .approval.requested_version as $v
    | ([$versions[] | select(.number == $v)] | first // error("version \($v) of the spec was not read"))
    | .markdown | match(":::ask\\{(?:#(?<id>[^\\s}]+))?[^}\\n]*\\bstate=\"open\""; "g") | .captures[0].string as $id
    | $blocks[] | select(.block_id == $id) | .question;
  def first_turn: .id as $id | (.created_at | ts) as $at
    | ([$requests[] | select(.id == $id) | .created_at | ts | select(. > $at)] | min) as $next
    | [$events[]
        | select((.type == "ask.answered" and .payload.id == $id)
            or (.type == "comment.created" and .actor.kind == "user" and .payload.ask_id == $id))
        | .created_at | ts | select(. >= $at and ($next == null or . < $next))]
    | min;
  def answered_after: (.created_at | ts) as $at | first_turn as $turn
    | $blocks[] | select(.answer != null and (.answer.at | ts) > $at)
    | select($turn == null or (.created_at | ts) < $turn) | .question;
  {
    request: $request,
    summarized: ($request != null and ($request | test("^Approve .+ \\(version [0-9]+\\)\\? \\S"))),
    blocks: [$blocks[] | select(.answer != null)] | length,
    early: [$requests[] | .approval.requested_version as $v | (open_in_version, answered_after) | "version \($v): \(.)"] | unique
  }
