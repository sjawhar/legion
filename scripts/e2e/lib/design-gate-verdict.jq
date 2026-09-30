# The stage 4b design-gate verdict on the spec whose document id is $artifact, which a human
# approved at $version. Input, slurped: the issue's asks in every state
# (GET /api/v1/issues/{key}/asks), then each version of the spec an approval request named
# (GET /api/v1/artifacts/{id}/versions/{number}).
#   request     the question of the approval request at $version, or null when none names it
#   summarized  whether that question carries a summary after "Approve <name> (version N)?"
#   blocks      how many of the spec's decision blocks a human answered
#   early       each decision block still open in a version an approval request named, as
#               "version N: <block question>". Retracted requests count: a request made early and
#               retracted when the block's answer wrote a new version was still made too early.
# The version is what the human was asked to approve, and it records each block's state. Times
# cannot order the two: Dispatch indexes a block as an ask when it settles the document, after the
# edit that wrote it, so an approval requested in the same turn predates the block's ask.
.[0] as $asks | .[1:] as $versions
| [$asks[] | select(.kind == "question" and .block_id != null and (.block_artifact.id // $artifact) == $artifact)] as $blocks
| [$asks[] | select(.kind == "approval" and .approval.artifact_id == $artifact)] as $requests
| ([$requests[] | select(.approval.version == $version)] | last | .question) as $request
| {
    request: $request,
    summarized: ($request != null and ($request | test("^Approve .+ \\(version [0-9]+\\)\\? \\S"))),
    blocks: [$blocks[] | select(.answer != null)] | length,
    early: [$requests[] | .approval.version as $v
      | ([$versions[] | select(.number == $v)] | first // error("version \($v) of the spec was not read"))
      | .markdown | match(":::ask\\{(?:#(?<id>[^\\s}]+))?[^}\\n]*\\bstate=\"open\""; "g") | .captures[0].string as $id
      | ([$blocks[] | select(.block_id == $id) | .question] | first // "a decision block Dispatch has not indexed")
      | "version \($v): \(.)"]
  }
