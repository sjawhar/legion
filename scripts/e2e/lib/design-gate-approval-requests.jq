# Approval requests reconstructed from an issue's ordered event history. The first ask.opened and
# each ask.edited that raises requested_version are the versions an agent handed to a human.
# AskEditPrevious deliberately has no approval field, so the prior value comes from this ask's
# preceding event rather than payload.previous.
def normalize_approval:
  . + {approval: (.approval + {requested_version: (.approval.requested_version // .approval.version)})};

def approval_requests($artifact):
  reduce (
    .[]?
    | select(.type == "ask.opened" or .type == "ask.edited")
    | .payload as $payload
    | select($payload.kind == "approval" and $payload.approval.artifact_id == $artifact)
    | {type, created_at, ask: ($payload | normalize_approval)}
  ) as $event
    ({seen: {}, requests: []};
      $event.ask.id as $id
      | $event.ask.approval.requested_version as $requested
      | .seen[$id] as $previous
      | .seen[$id] = $requested
      | if $event.type == "ask.opened"
          or ($event.type == "ask.edited" and $previous != null and $requested > $previous)
        then .requests += [$event.ask + {created_at: $event.created_at}]
        else .
        end
    )
  | .requests;

def approval_requested_versions($artifact):
  [approval_requests($artifact)[].approval.requested_version] | unique | .[];
