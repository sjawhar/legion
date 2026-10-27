import { isSessionAnswersText } from "@legion/envoy-client/session-answers";

/**
 * Whether the request's last message is the list of people's answers as `withSessionAnswers`
 * appends it: a user message whose first text block starts with the list's marker or the failure
 * line's. Only that one position is checked, as `withDispatchFirst` checks only its own: every
 * copy of this extension appends there, and a tool result, delivered message or reply quoting the
 * marker earlier in the conversation is conversation, which must not switch the list off.
 */
function endsWithSessionAnswers(messages: readonly unknown[]): boolean {
  const last = messages.at(-1);
  if (
    typeof last !== "object" ||
    last === null ||
    !("role" in last) ||
    last.role !== "user" ||
    !("content" in last) ||
    !Array.isArray(last.content)
  ) {
    return false;
  }
  const [first]: unknown[] = last.content;
  return (
    typeof first === "object" &&
    first !== null &&
    "text" in first &&
    typeof first.text === "string" &&
    isSessionAnswersText(first.text)
  );
}

/**
 * The messages one provider request carries, with what people wrote to the session (`read`'s
 * text) appended as a separate user message after the last one; undefined when the last message
 * already is that list (another copy of this extension appended it first, and `read` is not
 * called) or `read` gives no text.
 *
 * It sits at the tail, never at the head, so a change in the list costs only its own tokens:
 * Oh My Pi marks a message a `context` handler adds as per-call, and its Anthropic placement puts
 * no cache anchor on a per-call message, so every byte before the list is the cached prefix of the
 * request before it. It is never merged into the last message, which is the conversation's and
 * carries the request's cache anchor.
 */
export async function withSessionAnswers(
  messages: readonly unknown[],
  read: () => Promise<string | undefined>
): Promise<unknown[] | undefined> {
  if (endsWithSessionAnswers(messages)) return undefined;
  const text = await read();
  if (text === undefined) return undefined;
  return [...messages, { role: "user", content: [{ type: "text", text }], timestamp: 0 }];
}
