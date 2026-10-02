# The stage 4b design-gate verdict on the spec whose document id is $artifact, which a human
# approved at $version. Input, slurped: the issue's asks, its events, then each version of the spec
# an approval request was handed back at (GET /api/v1/issues/{key}/asks and /events, then
# GET /api/v1/artifacts/{id}/versions/{number}).
#   request     the question of the approval request at $version, or null when none names it
#   summarized  whether that question carries a summary after "Approve <name> (version N)?"
#   blocks      how many of the spec's decision blocks a human answered
#   early       each approval request made too early, as "version N: <block question>"
#   open_in_version: the version a request named still held one of the spec's blocks open. The
#   request's `requested_version` is the version the agent handed to the human. An approval ask
#   follows later versions in place, so the shared event selector records only its initial
#   ask.opened and each ask.edited that raises requested_version.
include "design-gate-approval-requests";
def ts: (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?Z$") // error("not an RFC3339 UTC time: \(.)")) | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | "0." + . | tonumber);
.[0] as $asks | .[1] as $events | .[2:] as $versions
| [$asks[] | select(.kind == "question" and .block_id != null and .block_artifact.id == $artifact)] as $blocks
| def was_answered($request):
    any($events[]?;
      .type == "ask.answered"
      and .payload.id == $request.id
      and (.payload | normalize_approval).approval.requested_version == $request.approval.requested_version
    );
  ($events | approval_requests($artifact)
    | map(. + {state: (if was_answered(.) then "answered" else "open" end)})) as $requests
| ([$requests[] | select(.approval.requested_version == $version)] | last | .question) as $request
| def open_in_version: .approval.requested_version as $v
    | ([$versions[] | select(.number == $v)] | first // error("version \($v) of the spec was not read"))
    | .markdown | match(":::ask\\{(?:#(?<id>[^\\s}]+))?[^}\\n]*\\bstate=\"open\""; "g") | .captures[0].string as $id
    | $blocks[] | select(.block_id == $id) | .question;
  def answered_after: select(.state != "answered") | (.created_at | ts) as $at
    | $blocks[] | select(.answer != null and (.answer.at | ts) > $at) | .question;
  {
    request: $request,
    summarized: ($request != null and ($request | test("^Approve .+ \\(version [0-9]+\\)\\? \\S"))),
    blocks: [$blocks[] | select(.answer != null)] | length,
    early: [$requests[] | .approval.requested_version as $v | (open_in_version, answered_after) | "version \($v): \(.)"] | unique
  }
