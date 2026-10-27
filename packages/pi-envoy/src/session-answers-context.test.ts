import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  readSessionAnswers,
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import { withSessionAnswers } from "./session-answers-context";

// Message shapes as Oh My Pi hands them to a `context` handler.
const LIST = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const list = { role: "user", content: [{ type: "text", text: LIST }], timestamp: 0 };
const skill = {
  role: "user",
  content: [{ type: "text", text: "<dispatch-first-skill>\nSearch Dispatch first." }],
  timestamp: 0,
};
const ask = { role: "user", content: [{ type: "text", text: "Fix the bug." }], timestamp: 1 };
const readCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-1", name: "read", arguments: { path: "x.ts" } }],
  timestamp: 2,
};
const readResult = {
  role: "toolResult",
  toolCallId: "call-1",
  toolName: "read",
  content: [{ type: "text", text: LIST }],
  isError: false,
  timestamp: 3,
};
const reply = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 4,
};

// What Oh My Pi 18.12.2 adds after the conversation for a side turn (`runEphemeralTurn`) and for
// the handoff, as the binary builds them.
const sideTurnNotice = {
  role: "developer",
  content: [
    {
      type: "text",
      text: "<system-reminder>\nEphemeral side-channel turn; reuses current conversation context.\nTool catalog attached only to keep prompt cache warm; tools NOT available this turn.\nDo NOT emit tool calls; reply plain text only. Tool calls discarded without execution.\n</system-reminder>\n",
    },
  ],
  attribution: "agent",
  timestamp: 10,
};
const sidePrompt = {
  role: "user",
  content: [{ type: "text", text: "<btw>\nQuestion:\nWhat is up?\n</btw>" }],
  attribution: "agent",
  timestamp: 11,
};
const handoffPrompt = {
  role: "user",
  content: [
    {
      type: "text",
      text: "<critical>\nWrite a handoff document for another instance of yourself.\nThe handoff MUST be sufficient for seamless continuation without access to this conversation.\n</critical>",
    },
  ],
  attribution: "agent",
  timestamp: 12,
};

/** The session's branch entries for `messages`, as `sessionManager.getBranch` lists them. */
function branchOf(...messages: readonly unknown[]): () => readonly unknown[] {
  return () => messages.map((message, index) => ({ type: "message", id: `e${index}`, message }));
}

const noBranch = branchOf();

/** A read that records each call and answers `text`. */
function reading(text: string | undefined): {
  readonly read: () => Promise<string | undefined>;
  readonly calls: () => number;
} {
  let calls = 0;
  return {
    read: async () => {
      calls += 1;
      return text;
    },
    calls: () => calls,
  };
}

test("the list goes after the request's last message, as a message of its own", async () => {
  const { read } = reading(LIST);
  expect(await withSessionAnswers([skill, ask], { read, branch: noBranch })).toEqual([
    skill,
    ask,
    list,
  ]);
});

// The tool result quotes the marker: it is conversation, not a list another copy appended.
test("after a tool round the list follows the tool result, which keeps its own content", async () => {
  const { read } = reading(LIST);
  const messages = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers(messages, { read, branch: branchOf(ask, readCall, readResult) })
  ).toEqual([...messages, list]);
});

test("the failure line goes where the list would", async () => {
  const line = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
  const { read } = reading(line);
  expect(await withSessionAnswers([ask], { read, branch: noBranch })).toEqual([
    ask,
    { role: "user", content: [{ type: "text", text: line }], timestamp: 0 },
  ]);
});

test("no text to carry adds no message", async () => {
  const { read, calls } = reading(undefined);
  expect(await withSessionAnswers([skill, ask], { read, branch: noBranch })).toBeUndefined();
  expect(calls()).toBe(1);
});

test("a request another copy of the extension already gave the list gets no second one, and no read", async () => {
  const { read, calls } = reading(LIST);
  expect(await withSessionAnswers([skill, ask, list], { read, branch: noBranch })).toBeUndefined();
  const failure = {
    role: "user",
    content: [{ type: "text", text: sessionAnswersFailureLine("no route") }],
    timestamp: 0,
  };
  expect(await withSessionAnswers([ask, failure], { read, branch: noBranch })).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a request another copy of the extension read no text for is not read again", async () => {
  const { read, calls } = reading(undefined);
  // The host hands each copy's handler the same array when the one before returned nothing.
  const messages = [skill, ask];
  expect(await withSessionAnswers(messages, { read, branch: noBranch })).toBeUndefined();
  expect(await withSessionAnswers(messages, { read, branch: noBranch })).toBeUndefined();
  expect(calls()).toBe(1);
  // Another request is read again.
  expect(await withSessionAnswers([skill, ask], { read, branch: noBranch })).toBeUndefined();
  expect(calls()).toBe(2);
});

// The empty-list case with the shared read: two copies, a Dispatch that answers in 600 ms, one
// HTTP read and a request that waits under one second. The delay is real on purpose: the read's
// 800 ms deadline is a platform timer inside `readSessionAnswers`, and the case times the request
// that two copies of the handler make together against it.
const stateRoots: string[] = [];
afterEach(() => {
  for (const root of stateRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

test("two copies of the extension and an empty list: one HTTP read, under one second", async () => {
  const stateRoot = mkdtempSync(join(tmpdir(), "session-answers-context-"));
  stateRoots.push(stateRoot);
  let fetches = 0;
  const fetchImpl = (async (input: string | URL | Request) => {
    fetches += 1;
    await Bun.sleep(600);
    const session = new URL(input.toString()).pathname.split("/")[4] ?? "";
    return Response.json({
      session_id: session,
      since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
      total: 0,
      items: [],
    });
  }) as typeof fetch;
  const read = async () =>
    (
      await readSessionAnswers({
        session: "ses_two_copies_empty",
        dispatch: { enabled: true, url: "http://dispatch.test", token: "token" },
        env: { DISPATCH_STATE_DIR: stateRoot },
        fetchImpl,
      })
    ).text;
  const messages = [skill, ask];

  const started = performance.now();
  // Copy A's handler, then copy B's on the array A passed on.
  expect(await withSessionAnswers(messages, { read, branch: noBranch })).toBeUndefined();
  expect(await withSessionAnswers(messages, { read, branch: noBranch })).toBeUndefined();
  const elapsed = performance.now() - started;

  expect(fetches).toBe(1);
  expect(elapsed).toBeLessThan(1_000);
});

test("a list quoted earlier in the conversation does not switch the list off", async () => {
  const { read } = reading(LIST);
  const messages = [list, ask, readCall, readResult];
  expect(await withSessionAnswers(messages, { read, branch: noBranch })).toEqual([
    ...messages,
    list,
  ]);
});

test("a side turn keeps its prompt last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  const conversation = [skill, ask, reply];
  const branch = branchOf(ask, reply);
  expect(
    await withSessionAnswers([...conversation, sideTurnNotice, sidePrompt], { read, branch })
  ).toBeUndefined();
  // A /btw follow-up: the earlier question and answer sit between the notice and the prompt.
  const earlier = { ...sidePrompt, timestamp: 8 };
  const earlierAnswer = { ...reply, content: [{ type: "text", text: "Not much." }], timestamp: 9 };
  expect(
    await withSessionAnswers(
      [...conversation, sideTurnNotice, earlier, earlierAnswer, sidePrompt],
      {
        read,
        branch,
      }
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a host reminder before a host prompt in the main loop is no side turn", async () => {
  const { read } = reading(LIST);
  const reminder = {
    role: "developer",
    content: [{ type: "text", text: "<system-reminder>\nFinish the todos.\n</system-reminder>" }],
    attribution: "agent",
    timestamp: 5,
  };
  const messages = [skill, ask, reply, reminder, { ...sidePrompt, timestamp: 6 }];
  expect(await withSessionAnswers(messages, { read, branch: branchOf(ask, reply) })).toEqual([
    ...messages,
    list,
  ]);
});

test("the handoff prompt stays last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers([skill, ask, reply, handoffPrompt], {
      read,
      branch: branchOf(ask, reply),
    })
  ).toBeUndefined();
  expect(calls()).toBe(0);
  // The same words from the person are their prompt.
  const typed = { ...handoffPrompt, attribution: "user" };
  expect(
    await withSessionAnswers([skill, ask, reply, typed], { read, branch: branchOf(ask, reply) })
  ).toEqual([skill, ask, reply, typed, list]);
});

// Anthropic remote compaction converts the conversation up to its cut, counts it, and keeps that
// many messages of the whole conversation's conversion.
test("a conversion that ends before the branch's last message is not read for; the whole conversation is", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(ask, readCall, readResult, reply);
  expect(await withSessionAnswers([skill, ask, readCall, readResult], { read, branch })).toBe(
    undefined
  );
  expect(calls()).toBe(0);
  const whole = [skill, ask, readCall, readResult, reply];
  expect(await withSessionAnswers(whole, { read, branch })).toEqual([...whole, list]);
  expect(calls()).toBe(1);
});

test("a retry after a failed reply still carries the list", async () => {
  const { read } = reading(LIST);
  const failed = { ...reply, content: [], stopReason: "error", timestamp: 5 };
  const aborted = { ...reply, content: [], stopReason: "aborted", timestamp: 6 };
  expect(
    await withSessionAnswers([skill, ask], { read, branch: branchOf(ask, failed, aborted) })
  ).toEqual([skill, ask, list]);
});
