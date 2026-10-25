import { afterEach, expect, jest, test } from "bun:test";
import { act, type RenderHookResult, renderHook } from "@testing-library/react";

import type { Message, MessageDelivery, MessageRead } from "../../api/types";
import { type SessionSwitch, type SwitchRequest, useSessionSwitch } from "./use-session-switch";

const SESSION = "01a0e090-4848-7473-acc5-fc96e6a646d3";
const MODEL_B: SwitchRequest = { kind: "model", value: "anthropic/model-b" };
const HIGH: SwitchRequest = { kind: "thinking", value: "high" };
const NO_ANSWER =
  "No answer came from the session within 30 seconds. The header shows the model and thinking level the session last reported.";

afterEach(() => {
  jest.useRealTimers();
});

function attempt(overrides: Partial<MessageDelivery> = {}): MessageDelivery {
  return {
    attempt: 1,
    created_at: "2026-10-25T10:00:00Z",
    delivery: "steer",
    envelope_id: "envelope-1",
    error: null,
    message_id: "s1",
    reply_id: null,
    session_id: SESSION,
    state: "sent",
    ...overrides,
  };
}

/** The person's switch as Dispatch stored it and answered its send. */
function switchMessage(id: string, body: string, deliveries = [attempt()]): Message {
  return {
    author: { id: "sami", kind: "user" },
    body,
    created_at: "2026-10-25T10:00:00Z",
    deliveries,
    id,
    in_reply_to: null,
    issue_key: null,
    target: `session:${SESSION}`,
  };
}

function reply(id: string, inReplyTo: string, body: string, author = SESSION): Message {
  return {
    author: author === SESSION ? { id: SESSION, kind: "session" } : { id: author, kind: "user" },
    body,
    created_at: "2026-10-25T10:00:01Z",
    deliveries: [],
    id,
    in_reply_to: inReplyTo,
    issue_key: null,
    target: null,
  };
}

type SwitchHook = RenderHookResult<SessionSwitch, { thread: readonly MessageRead[] }>;

function renderSwitch(stored: readonly MessageRead[] = []): SwitchHook {
  return renderHook(({ thread }) => useSessionSwitch(SESSION, thread), {
    initialProps: { thread: stored },
  });
}

/** Picks `request`, Dispatch answering its send with `answer`. */
async function pick(hook: SwitchHook, request: SwitchRequest, answer: Message): Promise<void> {
  await act(async () => {
    await hook.result.current.run(request, async () => answer);
  });
}

test("a switch is pending until the session's reply, which it then quotes exactly", async () => {
  const hook = renderSwitch();
  const sent = switchMessage("s1", "/model anthropic/model-b");
  await pick(hook, MODEL_B, sent);
  expect(hook.result.current.status).toBe("Switching model to anthropic/model-b…");
  expect(hook.result.current.pending).toBe(true);

  // Another person's reply in the thread is not the session's answer.
  hook.rerender({ thread: [{ message: sent, replies: [reply("b1", "s1", "Why?", "bob")] }] });
  expect(hook.result.current.pending).toBe(true);

  hook.rerender({
    thread: [{ message: sent, replies: [reply("r1", "s1", "Model set to anthropic/model-b.")] }],
  });
  expect(hook.result.current.status).toBe("The session answered: Model set to anthropic/model-b.");
  expect(hook.result.current.pending).toBe(false);
});

test("a thinking level switch says which level it asked for", async () => {
  const hook = renderSwitch();
  await pick(hook, HIGH, switchMessage("s1", "/effort high"));
  expect(hook.result.current.status).toBe("Switching thinking level to high…");
});

test("a reply already in the thread when Dispatch answers the send ends the switch", async () => {
  const sent = switchMessage("s1", "/model anthropic/model-b");
  const hook = renderSwitch([
    { message: sent, replies: [reply("r1", "s1", "Model set to anthropic/model-b.")] },
  ]);
  await pick(hook, MODEL_B, sent);
  expect(hook.result.current.status).toBe("The session answered: Model set to anthropic/model-b.");
  expect(hook.result.current.pending).toBe(false);
});

test("a refusal shows the session's own reason", async () => {
  const hook = renderSwitch();
  const sent = switchMessage("s1", "/model openai/model-c");
  await pick(hook, { kind: "model", value: "openai/model-c" }, sent);
  hook.rerender({
    thread: [
      {
        message: sent,
        replies: [reply("r1", "s1", "Failed to set model: No API key for openai/model-c")],
      },
    ],
  });
  expect(hook.result.current.status).toBe(
    "The session answered: Failed to set model: No API key for openai/model-c"
  );
  expect(hook.result.current.pending).toBe(false);
});

test("a send Dispatch answers with a failed attempt ends at once, saying no answer came", async () => {
  const hook = renderSwitch();
  const failed = switchMessage("s1", "/model anthropic/model-b", [
    attempt({ envelope_id: null, error: "no live session", state: "failed" }),
  ]);
  await pick(hook, MODEL_B, failed);
  expect(hook.result.current.status).toBe("No answer came from the session: no live session");
  expect(hook.result.current.pending).toBe(false);

  // The stored thread then holds the same failed attempt, which is not the session answering.
  hook.rerender({ thread: [{ message: failed, replies: [] }] });
  expect(hook.result.current.status).toBe("No answer came from the session: no live session");
});

test("a delivery that later fails with an error is the session's answer", async () => {
  const hook = renderSwitch();
  const sent = switchMessage("s1", "/model anthropic/model-b");
  await pick(hook, MODEL_B, sent);

  // An attempt the session accepted as its user's turn has not failed, whatever came after.
  const accepted = switchMessage("s1", "/model anthropic/model-b", [
    attempt({ accepted_as: "user_turn", error: "listener closed", state: "failed" }),
  ]);
  hook.rerender({ thread: [{ message: accepted, replies: [] }] });
  expect(hook.result.current.pending).toBe(true);

  const failed = switchMessage("s1", "/model anthropic/model-b", [
    attempt({ error: "the session refused the attempt", state: "failed" }),
  ]);
  hook.rerender({ thread: [{ message: failed, replies: [] }] });
  expect(hook.result.current.status).toBe("The session answered: the session refused the attempt");
  expect(hook.result.current.pending).toBe(false);
});

test("no answer in 30 seconds from the send's answer ends the switch; a late reply replaces it", async () => {
  jest.useFakeTimers();
  const hook = renderSwitch();
  const sent = switchMessage("s1", "/model anthropic/model-b");
  await pick(hook, MODEL_B, sent);

  act(() => {
    jest.advanceTimersByTime(29_999);
  });
  expect(hook.result.current.pending).toBe(true);
  act(() => {
    jest.advanceTimersByTime(1);
  });
  expect(hook.result.current.status).toBe(NO_ANSWER);
  expect(hook.result.current.pending).toBe(false);

  hook.rerender({
    thread: [{ message: sent, replies: [reply("r1", "s1", "Model set to anthropic/model-b.")] }],
  });
  expect(hook.result.current.status).toBe("The session answered: Model set to anthropic/model-b.");
});

test("an earlier switch's late reply leaves a newer switch's status alone", async () => {
  jest.useFakeTimers();
  const hook = renderSwitch();
  const first = switchMessage("s1", "/model anthropic/model-b");
  await pick(hook, MODEL_B, first);
  act(() => {
    jest.advanceTimersByTime(30_000);
  });
  expect(hook.result.current.status).toBe(NO_ANSWER);

  const second = switchMessage("s2", "/effort high");
  await pick(hook, HIGH, second);
  hook.rerender({
    thread: [
      { message: first, replies: [reply("r1", "s1", "Model set to anthropic/model-b.")] },
      { message: second, replies: [] },
    ],
  });
  expect(hook.result.current.status).toBe("Switching thinking level to high…");
  expect(hook.result.current.pending).toBe(true);

  // Nor does the first switch's deadline, long gone, end the second early.
  act(() => {
    jest.advanceTimersByTime(29_999);
  });
  expect(hook.result.current.pending).toBe(true);

  hook.rerender({
    thread: [
      { message: first, replies: [reply("r1", "s1", "Model set to anthropic/model-b.")] },
      { message: second, replies: [reply("r2", "s2", "Thinking set to high.")] },
    ],
  });
  expect(hook.result.current.status).toBe("The session answered: Thinking set to high.");
  expect(hook.result.current.pending).toBe(false);
});

test("a send Dispatch refuses drops the switch and hands the refusal back", async () => {
  const hook = renderSwitch();
  let refusal: unknown;
  await act(async () => {
    await hook.result.current
      .run(MODEL_B, async () => {
        throw new Error("only a person can send this");
      })
      .catch((error: unknown) => {
        refusal = error;
      });
  });
  expect(refusal).toEqual(new Error("only a person can send this"));
  expect(hook.result.current.status).toBeUndefined();
  expect(hook.result.current.pending).toBe(false);
});
