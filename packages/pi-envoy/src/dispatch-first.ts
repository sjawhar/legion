import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";

/** The text of `message`'s first content block when it is a user message whose first block has
 *  text: where `withDispatchFirst` and `withSessionAnswers` each look for a copy of their own. */
export function leadingUserText(message: unknown): string | undefined {
  if (
    typeof message !== "object" ||
    message === null ||
    !("role" in message) ||
    message.role !== "user" ||
    !("content" in message) ||
    !Array.isArray(message.content)
  ) {
    return undefined;
  }
  const [first]: unknown[] = message.content;
  if (
    typeof first === "object" &&
    first !== null &&
    "text" in first &&
    typeof first.text === "string"
  ) {
    return first.text;
  }
  return undefined;
}

/** A user message of one text block, as `withDispatchFirst` and `withSessionAnswers` add it. */
export function userTextMessage(text: string): unknown {
  return { role: "user", content: [{ type: "text", text }], timestamp: 0 };
}

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
  if (leadingUserText(messages[index])?.startsWith(DISPATCH_FIRST_MARKER) === true) {
    return undefined;
  }
  return [...messages.slice(0, index), userTextMessage(context), ...messages.slice(index)];
}
