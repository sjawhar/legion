# How an Oh My Pi session's assistant calls a tool, read from the toolCall entries of its
# transcript's messages: one definition for every live proof that asks whether an agent called one
# (stage3-4b13b-acceptance.sh's merger_self_posted, stage4b-sandbox-tree.sh's report_after_tick).
# Loaded with `jq -L scripts/e2e/lib` and `include "omp-tool-calls";`.

# calls(name) is whether a message content entry calls the tool name, by any of the ways Oh My Pi
# gives the model: the tool itself, a write whose path is the tool's device, xd://<name>, alone,
# eval code that calls tool.<name>(...), or eval code whose tool.write(...) call itself names the
# device xd://<name> before the call's own closing brace (the two ways a model can reach a device
# from eval). The device must fall inside that write call's own arguments, not merely anywhere in
# the code string, so a comment or an unrelated tool.write to another device never counts.
def calls(name):
  .type? == "toolCall" and (
    .name == name
    or (.name == "write" and ((.arguments.path? // "") | tostring | test("^\\s*xd://" + name + "\\s*$")))
    or (.name == "eval" and ((.arguments.code? // "") | tostring | (
         test("\\btool\\." + name + "\\s*\\(")
         or test("\\btool\\.write\\s*\\([^}]*xd://" + name + "\\b")
       )))
  );

# device_arguments are the arguments a write to a tool's device hands the tool: its content, which
# is the arguments' JSON; {} when the content is none or no JSON object.
def device_arguments: (.arguments.content? // "") | tostring | fromjson? // {} | if type == "object" then . else {} end;

# call_arguments are the arguments a call that names its tool's arguments hands it, the tool's own or
# its device's (device_arguments); {} for eval code, whose arguments are its code.
def call_arguments:
  if .name == "write" then device_arguments
  elif .name == "eval" then {}
  else .arguments // {} end;
