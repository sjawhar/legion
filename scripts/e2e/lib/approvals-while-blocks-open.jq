# Every approval request on the document named by $artifact that was made while one of its
# decision blocks was open, retracted requests included, as "version N: <block question>".
# Input: an issue's asks in every state (GET /api/v1/issues/{key}/asks). A request made early and
# retracted when the block's answer wrote a new version is still a request made too early.
# Dispatch writes fractional seconds of varying length, so times compare as whole seconds, and a
# block never answered or resolved is open for ever.
def t: if . == null then infinite else sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 end;
[.[] | select(.kind == "question" and .block_id != null and (.block_artifact.id // $artifact) == $artifact)] as $blocks
| [.[] | select(.kind == "approval" and .approval.artifact_id == $artifact)
    | (.created_at | t) as $at | .approval.version as $version
    | $blocks[] | select((.created_at | t) < $at and ((.answer.at // .resolution.at) | t) > $at)
    | "version \($version): \(.question)"]
