# Approval requests reconstructed from an issue's ordered event history: each ask.opened and each
# ask.handed_back is a version an agent handed to a human, the one its approval.requested_version
# names. An ask.edited only rewords a request (a version move, a new summary), so it hands nothing to
# the human; a hand-back with a new summary is an ask.edited followed by its ask.handed_back.
def normalize_approval:
  . + {approval: (.approval + {requested_version: (.approval.requested_version // .approval.version)})};

def approval_requests($artifact):
  [
    .[]?
    | select(.type == "ask.opened" or .type == "ask.handed_back")
    | .payload as $payload
    | select($payload.kind == "approval" and $payload.approval.artifact_id == $artifact)
    | ($payload | normalize_approval) + {created_at}
  ];

def approval_requested_versions($artifact):
  [approval_requests($artifact)[].approval.requested_version] | unique | .[];
