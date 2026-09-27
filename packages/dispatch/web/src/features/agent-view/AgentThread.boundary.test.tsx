import { afterEach, expect, test } from "bun:test";
import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import type { AgentStreamFrame } from "@legion/contracts";
import { cleanup, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";

import { AgentThread } from "./AgentThread";
import { applyFrames, EMPTY_CONVERSATION, toThreadMessages } from "./conversation";

/**
 * assistant-ui throws "Unsupported user message part type: reasoning" when a user message carries
 * anything but text, and that throw happens where the runtime is built — above the thread — so it
 * takes the composer with it whatever boundary the thread has. The session's ring re-serves the
 * same frame on every visit, so it would leave the viewer permanently unable to talk to the
 * session.
 *
 * Both guards are in `conversation.ts`: `isRenderableFrame` drops such a frame, and
 * `toThreadMessages` emits text and nothing else for a user message, so a part type a future
 * publisher adds cannot reach the library either. This renders through the real runtime and the
 * real components rather than asserting the shapes, because it is the library's own conversion
 * that has to accept them.
 */

const poisoned = [
  {
    kind: "message",
    message: {
      at: 10,
      id: "u10",
      parts: [
        { text: "the prompt", type: "reasoning" },
        { text: "hello", type: "text" },
      ],
      role: "user",
      streaming: false,
    },
    seq: 1,
    v: 1,
  },
] as unknown as AgentStreamFrame[];

function Harness({ messages }: { messages: ThreadMessageLike[] }): ReactNode {
  const runtime = useExternalStoreRuntime({
    convertMessage: (message: ThreadMessageLike) => message,
    isRunning: false,
    messages,
    onNew: async () => undefined,
  });
  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread placeholder="Message the session…" resetKey="session-1" />
    </AssistantRuntimeProvider>
  );
}

// Each test renders a thread of its own; without this the second one finds both composers.
afterEach(cleanup);

test("a user message the library would refuse never reaches it, and the composer stays", () => {
  const state = applyFrames(EMPTY_CONVERSATION, poisoned);
  // The frame is refused outright, so nothing of it is rendered at all.
  expect(state.messages).toEqual([]);
  expect(() => render(<Harness messages={toThreadMessages(state)} />)).not.toThrow();
  expect(screen.getByTestId("agent-composer")).toBeTruthy();
  expect(screen.getByPlaceholderText("Message the session…")).toBeTruthy();
});

test("a user part type this build does not know is dropped rather than handed to the library", () => {
  // The second guard, for a frame a later publisher sends that this build's validator has no
  // rule for: the conversion emits text and nothing else for a user message.
  const message = {
    at: 10,
    id: "u10",
    parts: [
      { text: "the prompt", type: "reasoning" },
      { text: "hello", type: "text" },
    ],
    role: "user",
    streaming: false,
  };
  const state = {
    applied: {},
    messages: [message],
    results: {},
  } as unknown as Parameters<typeof toThreadMessages>[0];
  const converted = toThreadMessages(state);
  expect(converted[0]?.content).toEqual([{ text: "hello", type: "text" }]);
  expect(() => render(<Harness messages={converted} />)).not.toThrow();
  expect(screen.getByTestId("agent-composer")).toBeTruthy();
});
