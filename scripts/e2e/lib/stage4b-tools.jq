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
# call is not in the session (a session truncated before its call) is no tool's.
def tool_results(name):
  [session_entries] as $entries
  | ([$entries[] | select(.type == "message" and .message.role == "assistant")
      | .message.content[]? | select(calls(name)) | {key: .id, value: (call_arguments | tostring)}] | from_entries) as $calls
  | [$entries[] | select(.type == "message" and .message.role == "toolResult")
      | select((.message.toolCallId | strings) | in($calls))
      | {toolCallId: .message.toolCallId,
         isError: (.message.isError // false),
         text: ([.message.content[]? | select(.type == "text") | .text] | join("\n")),
         arguments: $calls[.message.toolCallId]}];

# tool_result_texts(name) are the texts of the tool's results that are no error, for a checkpoint's
# note to quote.
def tool_result_texts(name): [tool_results(name)[] | select(.isError == false) | .text];

# tool_ran(name) is whether one call of the tool returned without error.
def tool_ran(name): any(tool_results(name)[]; .isError == false);

# tool_result_said(name; $text) is whether one call of the tool returned without error with $text
# in its text, literally and case-sensitively.
def tool_result_said(name; $text): any(tool_result_texts(name)[]; contains($text));

# tool_result_answered(name; $args; $text) is tool_result_said for the calls whose arguments carry
# $args: the hover of one file among a tool's hovers, the read of one skill:// among its reads.
def tool_result_answered(name; $args; $text):
  any(tool_results(name)[]; .isError == false and (.arguments | contains($args)) and (.text | contains($text)));
