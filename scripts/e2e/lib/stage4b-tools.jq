# What a tool returned in a role's Oh My Pi session, read from the toolResult entries of its
# transcript: Stage 4b's proof that a tool ran and answered (stage4b-sandbox-tree.sh's tool_ran and
# tool_result_said), where assistant_said only proves the model said so. A result is judged by the
# call it answers, never by the toolName it records: a write to a tool's device, xd://<name>, records
# `toolName: "write"`, and the device's name is only in the paired toolCall's arguments.path, so each
# result is paired with its call by toolCallId and the call is judged with omp-tool-calls.jq's
# calls(name), one rule for every way the model calls a tool. Loaded with `jq -R -s -L
# scripts/e2e/lib` over the session's text and `include "stage4b-tools";`.
include "omp-tool-calls";

# session_entries are the entries of a session read as one raw string (`jq -R -s`), one a line; a
# line that is no JSON (the last, still being written) is skipped.
def session_entries: split("\n")[] | fromjson?;

# tool_results(name) are the toolResults of the session whose paired toolCall, the one with the
# result's toolCallId in an assistant message's content, calls(name), in session order, each
# {toolCallId, isError, text, arguments}: isError false when the result records none, text the
# result's text blocks joined by newlines (an image or any other block contributes nothing), and
# arguments the paired call's call_arguments as JSON text, so a checkpoint can tell which call a
# result answered (the hover among lsp's results, the control among codegraph's). A result whose
# call is not in the session (a session truncated before its call) is no tool's, and a call whose id
# is no string is no call (from_entries would refuse it).
def tool_results(name):
  [session_entries] as $entries
  | ([$entries[] | select(.type == "message" and .message.role == "assistant")
      | .message.content[]? | select(calls(name)) | select(.id | strings) | {key: .id, value: (call_arguments | tostring)}] | from_entries) as $calls
  | [$entries[] | select(.type == "message" and .message.role == "toolResult")
      | select((.message.toolCallId | strings) | in($calls))
      | {toolCallId: .message.toolCallId,
         isError: (.message.isError // false),
         text: ([.message.content[]? | select(.type == "text") | .text] | join("\n")),
         arguments: $calls[.message.toolCallId]}];

# tool_result_quote($lead; $max) is one of tool_results' entries on one line, for a fail line or a
# note to quote: `error ` when the result is one, then $lead (the call's arguments, or nothing),
# then its text with each run of whitespace one space, cut to $max characters.
def tool_result_quote($lead; $max):
  (if .isError then "error " else "" end) + $lead + (.text | gsub("\\s+"; " ") | .[0:$max]);

# tool_result_texts(name) are the texts of the tool's results that are no error, for a checkpoint's
# note to quote.
def tool_result_texts(name): [tool_results(name)[] | select(.isError == false) | .text];

# arguments_carry(name; $args) is whether one of tool_results' entries answers a call whose
# arguments carry $args, judged on the argument that carries the call's target and never on the
# model's `i` intent, which Oh My Pi puts in every tool's arguments (an intent naming a command the
# call never ran, or naming skill://<name> on a read of the skill's file path, must not satisfy a
# bound assertion): bash is bound on its command, read on its path, and every other tool on its
# arguments object with `i` removed, as JSON text (the hover among lsp's results by its action and
# file in the device's content; NoSuchSymbol in a codegraph call; an edit's `input`, which carries
# the file's path and the ops, since Oh My Pi's edit has no path argument). A write's stored
# arguments are its content (call_arguments reads every write as a possible device write), which
# carries no `i`, so for a tool called through xd://<name> the object matched is the content itself,
# unchanged. A bash call dispatched through eval (call_arguments is {} for eval, omp-tool-calls.jq)
# or through a write to xd://bash whose content is no JSON object (call_arguments is {} then too)
# has no command, so a bound bash assertion does not see it, while tool_result_said("bash"; …),
# whose $args is "", still counts every bash call, since "" is in every string. Arguments that are
# no JSON text, or no object, are read as {} the same way, so one odd call never aborts the program.
def arguments_carry(name; $args):
  (.arguments | fromjson? // {} | if type == "object" then . else {} end)
  | (if name == "bash" then .command elif name == "read" then .path else del(.i) | tojson end) // ""
  | tostring | contains($args);

# tool_ran(name) is whether one call of the tool returned without error.
def tool_ran(name): any(tool_results(name)[]; .isError == false);

# tool_result_answered(name; $args; $text) is whether one call of the tool whose arguments carry
# $args (arguments_carry: the command for bash, the path for read, the arguments object without
# the intent for every other tool) returned without error with $text in its text, literally and
# case-sensitively: the hover of one file among a tool's hovers, the read of one skill:// among its
# reads.
def tool_result_answered(name; $args; $text):
  any(tool_results(name)[]; .isError == false and arguments_carry(name; $args) and (.text | contains($text)));

# tool_result_said(name; $text) is tool_result_answered for every call of the tool, whatever its
# arguments: "" is in every command and every JSON text, so it still counts every call of the tool,
# the eval-dispatched and device-written bash calls included.
def tool_result_said(name; $text): tool_result_answered(name; ""; $text);
