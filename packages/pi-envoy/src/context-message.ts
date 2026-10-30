/** The text of `message`'s first content block when `message` has `role` and that block has text:
 *  where `withDispatchFirst` and `withSessionAnswers` each look for a copy of their own, and how
 *  `withSessionAnswers` reads the host's own prompts. */
export function leadingText(message: unknown, role: string): string | undefined {
  if (
    typeof message !== "object" ||
    message === null ||
    !("role" in message) ||
    message.role !== role ||
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
