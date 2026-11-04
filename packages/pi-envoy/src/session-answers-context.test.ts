import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  readSessionAnswers,
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import {
  type CarriedAnswers,
  type SessionAnswersRequest,
  withSessionAnswers,
} from "./session-answers-context";

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

// A delivered Envoy message and a branch summary as the session's branch holds them, and the
// context messages Oh My Pi 18.12.2 makes of them for a compaction (`yu`, `hm`): role `custom` and
// `branchSummary`, each timestamped `new Date(entry.timestamp).getTime()`.
const DELIVERED_AT = "2026-10-27T10:00:05.123Z";
const deliveryEntry = {
  type: "custom_message",
  id: "c1",
  customType: "envoy-message",
  content: "envoy:\n  notice: CI finished",
  display: true,
  timestamp: DELIVERED_AT,
};
const delivery = {
  role: "custom",
  customType: "envoy-message",
  content: "envoy:\n  notice: CI finished",
  display: true,
  timestamp: new Date(DELIVERED_AT).getTime(),
};
const deliveryReply = {
  role: "assistant",
  content: [{ type: "text", text: "CI is green." }],
  stopReason: "stop",
  timestamp: new Date(DELIVERED_AT).getTime() + 1_000,
};
const SUMMARIZED_AT = "2026-10-27T10:00:06.456Z";
const summaryEntry = {
  type: "branch_summary",
  id: "b1",
  summary: "The other branch tried a rewrite.",
  fromId: "e0",
  timestamp: SUMMARIZED_AT,
};
const branchSummary = {
  role: "branchSummary",
  summary: "The other branch tried a rewrite.",
  fromId: "e0",
  timestamp: new Date(SUMMARIZED_AT).getTime(),
};

/** The session's branch entries for `items`, as `sessionManager.getBranch` lists them: a message
 *  in a message entry of its own, an entry (it has a `type`) as it is. */
function branchOf(...items: readonly unknown[]): () => readonly unknown[] {
  return () =>
    items.map((item, index) =>
      typeof item === "object" && item !== null && "type" in item
        ? item
        : { type: "message", id: `e${index}`, message: item }
    );
}

const noBranch = branchOf();

/** What `readSessionAnswers` gives with `text`: the text, and what the people wrote in it (each
 *  item's head and words, as `sessionAnswersWords` keys them), none for the failure line. */
function carry(text: string | undefined): CarriedAnswers | undefined {
  if (text === undefined) return undefined;
  const words = Array.from(
    text.matchAll(/^\d{4}-\S+ · (.+)\n(.+)$/gm),
    (match) => `${match[1]}: ${match[2]}`
  );
  return { text, words };
}

/** A read that records each call and answers `text`. */
function reading(text: string | undefined): {
  readonly read: () => Promise<CarriedAnswers | undefined>;
  readonly calls: () => number;
} {
  let calls = 0;
  return {
    read: async () => {
      calls += 1;
      return carry(text);
    },
    calls: () => calls,
  };
}

/** A request of `session` (a session of its own unless given), not compacting unless told. */
function requestOf(
  read: () => Promise<CarriedAnswers | undefined>,
  branch: () => readonly unknown[],
  session: object = {},
  compacting = false,
  id = "ses_a"
): SessionAnswersRequest {
  return { read, branch, session, id, compacting: () => compacting };
}

test("the list goes after the request's last message, as a message of its own", async () => {
  const { read } = reading(LIST);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toEqual([
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
    await withSessionAnswers(messages, requestOf(read, branchOf(ask, readCall, readResult)))
  ).toEqual([...messages, list]);
});

// What a session's requests carry over turns with the same people's answers. A list sits after the
// request's last message only on the request that first shows an item, and stays there on the
// requests after it, so the model never reads a list with nothing new in it as the newest turn.
const LATER = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const later = { role: "user", content: [{ type: "text", text: LATER }], timestamp: 0 };
const grepCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-2", name: "grep", arguments: { pattern: "bug" } }],
  timestamp: 5,
};
const grepResult = {
  role: "toolResult",
  toolCallId: "call-2",
  toolName: "grep",
  content: [{ type: "text", text: "x.ts:3: the bug" }],
  isError: false,
  timestamp: 6,
};

/** A read that answers each of `texts` in turn, then the last one again. */
function readingEach(...texts: string[]): () => Promise<CarriedAnswers | undefined> {
  let call = 0;
  return async () => carry(texts[Math.min(call++, texts.length - 1)]);
}

test("a list with no new item stays where it first went: the request still ends with its own last message", async () => {
  const session = {};
  const read = readingEach(LIST);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
  ]);
  // Two tool rounds later, with nothing new: the list is where the first request put it.
  const turn = [skill, ask, readCall, readResult, grepCall, grepResult];
  for (let request = 0; request < 3; request += 1) {
    const carried = await withSessionAnswers([...turn], requestOf(read, noBranch, session));
    expect(carried).toEqual([skill, ask, list, readCall, readResult, grepCall, grepResult]);
    expect(carried?.at(-1)).toBe(grepResult);
  }
});

test("a new item goes after the request's last message, and stays there on the requests after it", async () => {
  const session = {};
  const read = readingEach(LIST, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    later,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([...turn, later, grepCall, grepResult]);
});

// Every byte of a request before its last cache breakpoint has to repeat, or the provider writes
// the whole conversation behind the changed byte to its cache again. The list's header names the
// read that produced it ("As read at …; this read failed: …"), which differs from one request to
// the next whenever a read crosses its 800 ms deadline, so a kept place keeps its bytes too.
test("a list that only lost items, or changed its header, keeps its place and its bytes", async () => {
  const session = {};
  const cached = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
  const read = readingEach(LATER, LIST, cached);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    later,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, later, readCall, readResult, grepCall, grepResult]);
});

// The one change that does move the list also re-renders it, so a new item is never hidden behind
// the bytes a kept place holds.
test("an item written after a kept place moves the list, with the text of that read", async () => {
  const session = {};
  const stale = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
  const read = readingEach(LIST, stale, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([...turn, grepCall, grepResult, later]);
});

// A read gives no text when the answer list is empty, or when a server with no answers route has
// nothing cached. Taking the placed list out of the conversation changes the request from its
// place on, as changing its bytes would, and the next read that answers from the cache puts it
// back: the pair alternates, and every request pays for it.
test("a read with no text leaves a placed list where it is", async () => {
  const session = {};
  const read = readingEach(LIST, "", LIST);
  const empty = async (): Promise<CarriedAnswers | undefined> => {
    const next = await read();
    return next?.text === "" ? undefined : next;
  };
  await withSessionAnswers([skill, ask], requestOf(empty, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(empty, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(empty, noBranch, session))
  ).toEqual([skill, ask, list, readCall, readResult, grepCall, grepResult]);
});

test("a list whose place left the conversation, as a compaction leaves it, goes after the last message again", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  // The compaction summarized the question the list followed.
  const compacted = [skill, branchSummary, readCall, readResult];
  expect(await withSessionAnswers([...compacted], requestOf(read, noBranch, session))).toEqual([
    ...compacted,
    list,
  ]);
  expect(
    await withSessionAnswers(
      [...compacted, grepCall, grepResult],
      requestOf(read, noBranch, session)
    )
  ).toEqual([...compacted, list, grepCall, grepResult]);
});

test("a second copy of the extension finds the list where the first put it, and reads nothing", async () => {
  const session = {};
  const first = reading(LIST);
  await withSessionAnswers([skill, ask], requestOf(first.read, noBranch, session));
  const carried = await withSessionAnswers(
    [skill, ask, readCall, readResult],
    requestOf(first.read, noBranch, session)
  );
  expect(carried?.at(-1)).toBe(readResult);
  // The host hands the second copy's handler the array the first returned.
  const second = reading(LIST);
  expect(
    await withSessionAnswers(carried ?? [], requestOf(second.read, noBranch, session))
  ).toBeUndefined();
  expect(second.calls()).toBe(0);
});

// A speculative compaction runs beside the main loop, so the main loop's request can come between
// the compaction's cut and its whole conversion. While the session compacts, every list goes after
// the last message, so the whole conversion's first messages are the cut's, as the compaction
// keeps them.
test("a main-loop request between a compaction's cut and its whole conversion leaves the compaction its messages", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const branch = branchOf(ask, readCall, readResult, grepCall, grepResult);
  const cut = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...cut], requestOf(read, branch, session, true))).toBe(
    undefined
  );
  const whole = [skill, ask, readCall, readResult, grepCall, grepResult];
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  const converted = await withSessionAnswers([...whole], requestOf(read, branch, session, true));
  expect(converted?.slice(0, cut.length)).toEqual(cut);
  // Once the compaction is over, the list is back where it went first.
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    grepCall,
    grepResult,
  ]);
});

test("a retry after an aborted tool call, whose results the host wrote, carries the list", async () => {
  const { read } = reading(LIST);
  const aborted = {
    role: "assistant",
    content: [{ type: "toolCall", id: "call-9", name: "read", arguments: { path: "y.ts" } }],
    stopReason: "aborted",
    timestamp: 5,
  };
  const synthetic = {
    role: "toolResult",
    toolCallId: "call-9",
    toolName: "read",
    content: [{ type: "text", text: "Aborted." }],
    isError: true,
    timestamp: 6,
  };
  expect(
    await withSessionAnswers([skill, ask], requestOf(read, branchOf(ask, aborted, synthetic)))
  ).toEqual([skill, ask, list]);
});

test("each session on one sessionManager keeps its own list's place", async () => {
  const manager = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, manager, false, "ses_a"));
  const other = { ...ask, content: [{ type: "text", text: "Another task." }], timestamp: 50 };
  await withSessionAnswers([skill, other], requestOf(read, noBranch, manager, false, "ses_b"));
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult],
      requestOf(read, noBranch, manager, false, "ses_a")
    )
  ).toEqual([skill, ask, list, readCall, readResult]);
});

test("tool results with one timestamp are told apart by their call ids", async () => {
  const session = {};
  const read = readingEach(LIST);
  const sameTime = { ...grepResult, timestamp: readResult.timestamp };
  const turn = [skill, ask, readCall, readResult];
  await withSessionAnswers([...turn], requestOf(read, noBranch, session));
  expect(
    await withSessionAnswers([...turn, grepCall, sameTime], requestOf(read, noBranch, session))
  ).toEqual([...turn, list, grepCall, sameTime]);
});

test("the failure line goes where the list would", async () => {
  const line = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
  const { read } = reading(line);
  expect(await withSessionAnswers([ask], requestOf(read, noBranch))).toEqual([
    ask,
    { role: "user", content: [{ type: "text", text: line }], timestamp: 0 },
  ]);
});

test("no text to carry adds no message", async () => {
  const { read, calls } = reading(undefined);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(1);
});

// An older copy of the extension put the list after the last message of every request. This copy
// reads nothing for that request, adds no second list, and moves the one there where it belongs.
test("a request an older copy already gave the list gets no second one and no read; its list is placed as this copy's", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  expect(await withSessionAnswers([skill, ask, list], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
  ]);
  // The next request, with the same list after its last message, gets it after the first one's.
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult, list],
      requestOf(read, noBranch, session)
    )
  ).toEqual([skill, ask, list, readCall, readResult]);
  const failure = {
    role: "user",
    content: [{ type: "text", text: sessionAnswersFailureLine("database is down") }],
    timestamp: 0,
  };
  expect(await withSessionAnswers([ask, failure], requestOf(read, noBranch))).toEqual([
    ask,
    failure,
  ]);
  expect(calls()).toBe(0);
});

test("a person's own last message quoting the list's marker is never moved", async () => {
  const { read, calls } = reading(LIST);
  const quoted = { ...list, timestamp: 7 };
  expect(await withSessionAnswers([skill, ask, quoted], requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a request another copy of the extension read no text for is not read again", async () => {
  const { read, calls } = reading(undefined);
  // The host hands each copy's handler the same array when the one before returned nothing.
  const messages = [skill, ask];
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(1);
  // Another request is read again.
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toBeUndefined();
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
    carry(
      (
        await readSessionAnswers({
          session: "ses_two_copies_empty",
          dispatch: { enabled: true, url: "http://dispatch.test", token: "token" },
          env: { DISPATCH_STATE_DIR: stateRoot },
          fetchImpl,
        })
      ).text
    );
  const messages = [skill, ask];

  const started = performance.now();
  // Copy A's handler, then copy B's on the array A passed on.
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  const elapsed = performance.now() - started;

  expect(fetches).toBe(1);
  expect(elapsed).toBeLessThan(1_000);
});

test("a list quoted earlier in the conversation does not switch the list off", async () => {
  const { read } = reading(LIST);
  const messages = [list, ask, readCall, readResult];
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toEqual([
    ...messages,
    list,
  ]);
});

test("a side turn keeps its prompt last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  const conversation = [skill, ask, reply];
  const branch = branchOf(ask, reply);
  expect(
    await withSessionAnswers([...conversation, sideTurnNotice, sidePrompt], requestOf(read, branch))
  ).toBeUndefined();
  // A /btw follow-up: the earlier question and answer sit between the notice and the prompt.
  const earlier = { ...sidePrompt, timestamp: 8 };
  const earlierAnswer = { ...reply, content: [{ type: "text", text: "Not much." }], timestamp: 9 };
  expect(
    await withSessionAnswers(
      [...conversation, sideTurnNotice, earlier, earlierAnswer, sidePrompt],
      requestOf(read, branch)
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
  expect(await withSessionAnswers(messages, requestOf(read, branchOf(ask, reply)))).toEqual([
    ...messages,
    list,
  ]);
});

test("the handoff prompt stays last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers(
      [skill, ask, reply, handoffPrompt],
      requestOf(read, branchOf(ask, reply))
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
  // The same words from the person are their prompt.
  const typed = { ...handoffPrompt, attribution: "user" };
  expect(
    await withSessionAnswers([skill, ask, reply, typed], requestOf(read, branchOf(ask, reply)))
  ).toEqual([skill, ask, reply, typed, list]);
});

// Anthropic remote compaction converts the conversation up to its cut, counts it, and keeps that
// many messages of the whole conversation's conversion.
test("a conversion that ends before the branch's last message is not read for; the whole conversation is", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(ask, readCall, readResult, reply);
  expect(
    await withSessionAnswers([skill, ask, readCall, readResult], requestOf(read, branch))
  ).toBeUndefined();
  expect(calls()).toBe(0);
  const whole = [skill, ask, readCall, readResult, reply];
  expect(await withSessionAnswers(whole, requestOf(read, branch))).toEqual([...whole, list]);
  expect(calls()).toBe(1);
});

test("a retry after a failed reply still carries the list", async () => {
  const { read } = reading(LIST);
  const failed = { ...reply, content: [], stopReason: "error", timestamp: 5 };
  const aborted = { ...reply, content: [], stopReason: "aborted", timestamp: 6 };
  expect(
    await withSessionAnswers([skill, ask], requestOf(read, branchOf(ask, failed, aborted)))
  ).toEqual([skill, ask, list]);
});

// The compaction's messages come from every branch entry, a delivered message and a branch summary
// among them, and its cut may fall right after either. Every Envoy delivery with `triggerTurn` is
// a delivered message the reply follows.
test("a conversion that ends at a delivered message with a reply after it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(ask, reply, deliveryEntry, deliveryReply);
  expect(
    await withSessionAnswers([skill, ask, reply, delivery], requestOf(read, branch))
  ).toBeUndefined();
  expect(calls()).toBe(0);
  const whole = [skill, ask, reply, delivery, deliveryReply];
  expect(await withSessionAnswers(whole, requestOf(read, branch))).toEqual([...whole, list]);
});

test("a conversion that ends at a branch summary with a message after it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(summaryEntry, ask, reply);
  expect(await withSessionAnswers([skill, branchSummary], requestOf(read, branch))).toBe(undefined);
  expect(calls()).toBe(0);
});

test("a delivered message after the request's last message is a later one", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers(
      [skill, ask, reply],
      requestOf(read, branchOf(ask, reply, deliveryEntry))
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a main-loop request that ends at a delivered message, the branch's last, carries the list", async () => {
  const { read } = reading(LIST);
  const messages = [skill, ask, reply, delivery];
  expect(
    await withSessionAnswers(messages, requestOf(read, branchOf(ask, reply, deliveryEntry)))
  ).toEqual([...messages, list]);
});

// A remote compaction that keeps no tail converts the same messages twice, each ending at the
// branch's last message. With no list on the second, it keeps no list whatever the first carried.
test("a compaction that keeps no tail: the repeat of the conversion before it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  const branch = branchOf(ask, reply, deliveryEntry);
  const conversation = [skill, ask, reply, delivery];
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toEqual([...conversation, list]);
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toBeUndefined();
  expect(calls()).toBe(1);
});

test("a repeat carries the list when the session is not compacting, the branch grew, or the session is another", async () => {
  const { read, calls } = reading(LIST);
  const conversation = [skill, ask, reply, delivery];
  const branch = branchOf(ask, reply, deliveryEntry);
  const idle = {};
  await withSessionAnswers([...conversation], requestOf(read, branch, idle));
  expect(await withSessionAnswers([...conversation], requestOf(read, branch, idle))).toEqual([
    ...conversation,
    list,
  ]);
  // A failed reply joined the branch: a retry, which carries the list.
  const failed = { ...reply, content: [], stopReason: "error", timestamp: delivery.timestamp + 1 };
  const retried = {};
  await withSessionAnswers([...conversation], requestOf(read, branch, retried, true));
  expect(
    await withSessionAnswers(
      [...conversation],
      requestOf(read, branchOf(ask, reply, deliveryEntry, failed), retried, true)
    )
  ).toEqual([...conversation, list]);
  // Another session's conversion is not the one before this session's.
  await withSessionAnswers([...conversation], requestOf(read, branch, {}, true));
  expect(await withSessionAnswers([...conversation], requestOf(read, branch, {}, true))).toEqual([
    ...conversation,
    list,
  ]);
  expect(calls()).toBe(6);
});

// A speculative compaction runs beside the main loop: its cut, then its whole conversion, which
// the main loop's next request matches message for message.
test("the main loop's request after a speculative compaction's cut and whole conversion keeps the list", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  const branch = branchOf(ask, readCall, readResult);
  const whole = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([skill, ask], requestOf(read, branch, session, true))).toBe(
    undefined
  );
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  expect(calls()).toBe(2);
});
