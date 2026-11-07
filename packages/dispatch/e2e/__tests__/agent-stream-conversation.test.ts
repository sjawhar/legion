import { expect, test } from "bun:test";
import { AGENT_STREAM_LIMITS } from "@legion/contracts";

import { agentStreamConversationAtLimit } from "../api";

// The seed behind every session a check of what an open live view costs opens (INFRA-812): the
// heaviest transcript a session will ever answer a replay request with. Both of the publisher's
// caps bound it, so the arithmetic that sizes a turn is held here rather than discovered as a
// refused publish in the browser suite.

const SESSION = "11111111-1111-4111-8111-111111111111";

interface MessageFrame {
  kind: string;
  message: { id: string; parts: { text: string; type: string }[]; role: string };
  seq: number;
}

function conversation(): { frames: MessageFrame[]; session_id: string; v: number } {
  return agentStreamConversationAtLimit(SESSION) as {
    frames: MessageFrame[];
    session_id: string;
    v: number;
  };
}

test("the conversation holds as many turns as a session keeps for replay", () => {
  const replay = conversation();

  expect(replay.session_id).toBe(SESSION);
  expect(replay.frames).toHaveLength(AGENT_STREAM_LIMITS.historyMessages);
  expect(replay.frames.map((frame) => frame.seq)).toEqual(
    Array.from({ length: AGENT_STREAM_LIMITS.historyMessages }, (_, index) => index + 1)
  );
  // The person's turns and the session's, alternating, each naming itself.
  expect(replay.frames[0]?.message.role).toBe("user");
  expect(replay.frames[1]?.message.role).toBe("assistant");
  expect(replay.frames[0]?.message.parts[0]?.text).toStartWith("Turn 0: ");
  expect(replay.frames.at(-1)?.message.id).toBe(`m${AGENT_STREAM_LIMITS.historyMessages - 1}`);
});

test("its prose fills the session's replay budget without passing it", () => {
  const replay = conversation();
  const bytes = new TextEncoder().encode(JSON.stringify(replay)).length;

  expect(bytes).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.historyBytes);
  // At the limit, not merely under it: a transcript a tenth of the budget would prove nothing
  // about what an open view costs.
  expect(bytes).toBeGreaterThan(AGENT_STREAM_LIMITS.historyBytes * 0.95);
  // And within what the publish route takes, which is this one body (maxJSONRequestBytes, 1 MiB).
  expect(bytes).toBeLessThan(1024 * 1024);
});

test("every turn is capped prose a session could really have published", () => {
  for (const frame of conversation().frames) {
    expect(frame.kind).toBe("message");
    expect(frame.message.parts).toHaveLength(1);
    expect(frame.message.parts[0]?.type).toBe("text");
    expect(frame.message.parts[0]?.text.length).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.partChars);
  }
});
