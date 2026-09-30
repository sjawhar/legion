import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";

function carriesDispatchFirst(message: unknown): boolean {
  if (typeof message !== "object" || message === null || !("content" in message)) return false;
  const { content } = message;
  if (typeof content === "string") return content.includes(DISPATCH_FIRST_MARKER);
  if (!Array.isArray(content)) return false;
  return content.some(
    (part: unknown) =>
      typeof part === "object" &&
      part !== null &&
      "text" in part &&
      typeof part.text === "string" &&
      part.text.includes(DISPATCH_FIRST_MARKER)
  );
}

/**
 * The messages one provider request carries, with the dispatch-first skill as a user message
 * right after any leading compaction summaries; undefined when a message already carries it
 * (another copy of this extension inserted it first). Oh My Pi hands a `context` handler a copy
 * made for that one request and stores nothing it returns, so the insert is made on every
 * request. Its text is the same each time and it has no id, which keeps its digest, and so the
 * append-only prompt cache's prefix, stable across requests.
 */
export function withDispatchFirst(
  messages: readonly unknown[],
  context: string
): unknown[] | undefined {
  if (messages.some(carriesDispatchFirst)) return undefined;
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
  return [
    ...messages.slice(0, index),
    { role: "user", content: [{ type: "text", text: context }], timestamp: 0 },
    ...messages.slice(index),
  ];
}
