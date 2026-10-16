# How an Oh My Pi session's assistant calls a tool, read from the toolCall entries of its
# transcript's messages: one definition for every live proof that asks whether an agent called one
# (stage3-4b13b-acceptance.sh's merger_self_posted, stage4b-sandbox-tree.sh's report_after_tick),
# or ran a `dispatch` command through bash.
# Loaded with `jq -L scripts/e2e/lib` and `include "omp-tool-calls";`. A module that includes this
# one (lib/stage4b-tools.jq) may reach only a def that calls no other def of this file: jq 1.8.2
# aborts at compile (`nesting_level: Assertion … failed`, `expand_call_arglist: Unknown function
# type`) on a chain such as bash_command → call_arguments reached through an include, where the same
# chain from a main program and a one-def reach (calls, call_arguments) both compile. So
# call_arguments spells a device write's arguments itself rather than through a def of its own.

# calls(name) is whether a message content entry calls the tool name, by any of the ways Oh My Pi
# gives the model: the tool itself, a write whose path is the tool's device, xd://<name>, alone,
# eval code that calls tool.<name>(...), or eval code that calls the generic tool.write(...) and,
# anywhere in the same code, assigns the device xd://<name> as a quoted string with `:` or `=`
# (covering content built before path, a path keyword argument, or a path held in a variable). A
# consumer that needs more than "this cell could write the device" — the report's issue key, or a
# READY literal — still checks for that itself; calls() only says the device is in play.
def calls(name):
  .type? == "toolCall" and (
    .name == name
    or (.name == "write" and ((.arguments.path? // "") | tostring | test("^\\s*xd://" + name + "\\s*$")))
    or (.name == "eval" and ((.arguments.code? // "") | tostring | (
         test("\\btool\\." + name + "\\s*\\(")
         or (test("\\btool\\.write\\s*\\(") and test("[:=]\\s*[\"'`]xd://" + name + "[\"'`]"))
       )))
  );

# call_arguments are the arguments a call that names its tool's arguments hands it: the tool's own,
# or, for a write to a tool's device (xd://<name>), its content, which is the arguments' JSON ({}
# when the content is none or no JSON object); {} for eval code, whose arguments are its code.
def call_arguments:
  if .name == "write" then ((.arguments.content? // "") | tostring | fromjson? // {} | if type == "object" then . else {} end)
  elif .name == "eval" then {}
  else .arguments // {} end;

# bash_command is the text a call to bash runs: the command the bash tool or its device is handed,
# or, for eval code, the code itself, where the command is a string literal.
def bash_command:
  if .name == "eval" then (.arguments.code? // "") | tostring
  else call_arguments.command? // "" | tostring end;

# runs_dispatch(sub) is whether a message content entry runs `dispatch <sub>` through bash, called any
# of the ways calls counts: in a command, first, after `;`, `&` or `|`, or after variable
# assignments; in eval code (tool.bash(...) or tool.write(...) naming xd://bash), at the start of a
# string literal, after any variable assignments. A check on the command's flags reads
# bash_command, where eval code escapes a quote as `\"`.
def runs_dispatch(sub):
  calls("bash") and (
    (if .name == "eval" then "[`\"']\\s*" else "(^|[;&|])\\s*" end) as $lead
    | bash_command | test($lead + "(\\w+=\\S*\\s+)*dispatch\\s+" + sub + "(\\s|$)"));
