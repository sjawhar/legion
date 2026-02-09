import type { Message, Part } from "@opencode-ai/sdk";

interface MessageWithParts {
  info: Message;
  parts: Part[];
}

function isThinkingPart(part: Part): boolean {
  const type = (part as { type: string }).type;
  return type === "thinking" || type === "reasoning";
}

function hasContentParts(parts: Part[]): boolean {
  if (!parts || parts.length === 0) return false;
  return parts.some((p) => {
    const type = (p as { type: string }).type;
    return type === "tool" || type === "tool_use" || type === "text";
  });
}

export function thinkingBlockValidatorHook(
  _input: Record<string, never>,
  output: { messages: MessageWithParts[] }
): void {
  const { messages } = output;
  if (!messages || messages.length === 0) return;

  for (const msg of messages) {
    if (msg.info.role !== "assistant") continue;
    if (!msg.parts || msg.parts.length < 2) continue;
    if (!hasContentParts(msg.parts)) continue;

    // Stable sort: thinking/reasoning first, everything else preserves order
    msg.parts.sort((a, b) => {
      const aVal = isThinkingPart(a) ? 0 : 1;
      const bVal = isThinkingPart(b) ? 0 : 1;
      return aVal - bVal;
    });
  }
}
