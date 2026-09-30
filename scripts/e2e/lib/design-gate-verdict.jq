# The stage 4b design-gate verdict on the spec whose document id is $artifact, which a human
# approved at $version. Input, slurped: the issue's asks in every state
# (GET /api/v1/issues/{key}/asks), then each version of the spec an approval request named
# (GET /api/v1/artifacts/{id}/versions/{number}).
#   request     the question of the approval request at $version, or null when none names it
#   summarized  whether that question carries a summary after "Approve <name> (version N)?"
#   blocks      how many of the spec's decision blocks a human answered
#   early       each approval request made too early, as "version N: <block question>", by either
#               rule below. Retracted requests count: a request made early and retracted when the
#               block's answer wrote a new version was still made too early.
# A request is early when the version it named still held a block open: the version is what the
# human was asked to approve, and it records each block's state. The block's ask cannot order that,
# since Dispatch indexes a block as an ask when it settles the document, after the edit that wrote
# it. A request is also early when a human answered one of the spec's blocks after it: that covers a
# request made while the choice was still prose, before its block existed, and one sent in parallel
# with the edit that wrote the block. An answer's time is written as the human answers.
def ts: (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?Z$") // error("not an RFC3339 UTC time: \(.)")) | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | "0." + . | tonumber);
# The markdown outside fenced code blocks: a fenced :::ask is quoted text, never a block.
def outside_fences:
  reduce split("\n")[] as $line ({fence: null, kept: []};
    ([$line | capture("^ {0,3}(?<m>`{3,}|~{3,})") | .m] | first) as $m
    | if .fence == null then (if $m == null then .kept += [$line] else .fence = $m end)
      elif $m != null and $m[0:1] == .fence[0:1] and ($m | length) >= (.fence | length) and ($line | test("^ {0,3}[`~]+\\s*$")) then .fence = null
      else . end)
  | .kept | join("\n");
.[0] as $asks | .[1:] as $versions
| [$asks[] | select(.kind == "question" and .block_id != null and .block_artifact.id == $artifact)] as $blocks
| [$asks[] | select(.kind == "approval" and .approval.artifact_id == $artifact)] as $requests
| ([$requests[] | select(.approval.version == $version)] | last | .question) as $request
| {
    request: $request,
    summarized: ($request != null and ($request | test("^Approve .+ \\(version [0-9]+\\)\\? \\S"))),
    blocks: [$blocks[] | select(.answer != null)] | length,
    early: [$requests[] | .approval.version as $v | (.created_at | ts) as $at
      | ((([$versions[] | select(.number == $v)] | first // error("version \($v) of the spec was not read"))
          | .markdown | outside_fences | match(":::ask\\{(?:#(?<id>[^\\s}]+))?[^}\\n]*\\bstate=\"open\""; "g") | .captures[0].string as $id
          | [$blocks[] | select(.block_id == $id) | .question] | first // "a decision block Dispatch has not indexed"),
        ($blocks[] | select(.answer != null and (.answer.at | ts) > $at) | .question))
      | "version \($v): \(.)"] | unique
  }
