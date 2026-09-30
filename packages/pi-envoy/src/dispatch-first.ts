import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";

/**
 * The messages one provider request carries, with the dispatch-first skill as a user message
 * right after any leading compaction summaries; undefined when the message already there is the
 * skill as this function inserts it (another copy of this extension inserted it first). Only that
 * one position is checked: every copy inserts there, and anything else that quotes the marker (a
 * tool result that read this module, a delivered message, the model's own reply) is conversation,
 * which must not switch the skill off. Oh My Pi hands a `context` handler a copy made for that one
 * request and stores nothing it returns, so the insert is made on every request. Its text is the
 * same each time and it has no id, so the request's bytes up to and through it repeat exactly.
 */
export function withDispatchFirst(
  messages: readonly unknown[],
  context: string
): unknown[] | undefined {
  let index = 0;
  for (const message of messages) {
    const summary =
      typeof message === "object" &&
      message !== null &&
      "role" in message &&
      message.role === "compactionSummary";
    if (!summary) break;
    index += 1;
  }
  const present = messages[index];
  if (
    typeof present === "object" &&
    present !== null &&
    "role" in present &&
    present.role === "user" &&
    "content" in present &&
    Array.isArray(present.content)
  ) {
    const [first]: unknown[] = present.content;
    if (
      typeof first === "object" &&
      first !== null &&
      "text" in first &&
      typeof first.text === "string" &&
      first.text.startsWith(DISPATCH_FIRST_MARKER)
    ) {
      return undefined;
    }
  }
  return [
    ...messages.slice(0, index),
    { role: "user", content: [{ type: "text", text: context }], timestamp: 0 },
    ...messages.slice(index),
  ];
}
