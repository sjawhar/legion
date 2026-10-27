import { describe, expect, test } from "bun:test";
import {
  existsSync,
  linkSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import type { ActiveDispatchConfig } from "../dispatch-config";
import type * as SessionAnswersModule from "../session-answers";
import {
  NO_ANSWERS_ROUTE_REASON,
  readSessionAnswers,
  renderSessionAnswerItem,
  renderSessionAnswers,
  renderSessionAnswersList,
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
  sessionAnswersSettled,
} from "../session-answers";

const NOW = Date.parse("2026-10-27T12:00:00.000Z");
const hoursAgo = (hours: number) => new Date(NOW - hours * 3_600_000).toISOString();
const stateDir = () => mkdtempSync(join(tmpdir(), "session-answers-"));

function message(id: string, at: string, text: string): SessionAnswer {
  return {
    kind: "message",
    relation: "sent_you_a_message",
    id,
    at,
    author: { kind: "user", id: "cy@example.com" },
    text,
    owner: null,
    ref: null,
    href: "/agents/ses/live",
  };
}

const answer: SessionAnswer = {
  kind: "answer",
  relation: "answered_your_ask",
  id: "ask-1",
  at: "2026-10-27T11:00:00Z",
  author: { kind: "user", id: "ana@example.com" },
  text: "Use JSON lines, one per item.",
  selected: ["JSON", "Newest first"],
  ask: { id: "ask-1", question: "Which format should the list use?" },
  owner: { issue: { key: "INFRA-148", title: "People's answers on every request" } },
  ref: "dispatch://INFRA-148/ask/ask-1",
  href: "/issues/INFRA-148/asks/ask-1",
};

const comment: SessionAnswer = {
  kind: "comment",
  relation: "commented_on_a_document",
  id: "c-1",
  at: "2026-10-27T10:30:00Z",
  edited_at: "2026-10-27T10:45:00Z",
  author: { kind: "user", id: "ben@example.com" },
  text: "Step 3 needs the staging URL.",
  owner: { document: { project: "CORE", slug: "runbook", name: "Runbook" } },
  ref: "dispatch://CORE/artifact/runbook/comment/c-1",
  href: "/projects/CORE/artifacts/runbook#comment-c-1",
};

const direct = message("m-1", "2026-10-27T10:00:00Z", "Stop after the migration lands.");

const leftOut = (count: number) =>
  `${count} older ${count === 1 ? "item" : "items"} left out to stay under 8,000 characters: run \`dispatch answers\` to read every one in full.`;

/** A message item whose list, rendered alone (with the left-out line `after` older items take),
 *  is exactly `target` characters long. */
function sizedItem(id: string, target: number, after = 0): SessionAnswer {
  const probe = message(id, hoursAgo(1), "x");
  const tail = after === 0 ? 0 : 2 + leftOut(after).length;
  const length = `${SESSION_ANSWERS_MARKER}\n\n${renderSessionAnswerItem(probe)}`.length + tail;
  return message(id, hoursAgo(1), "x".repeat(1 + target - length));
}

describe("renderSessionAnswers", () => {
  test("prints each item's time, person, relation, reference and owner, question, choices and words", () => {
    expect(renderSessionAnswers([answer, comment, direct], { now: NOW })).toBe(
      [
        SESSION_ANSWERS_MARKER,
        "",
        "2026-10-27T11:00:00Z · user ana@example.com · answered your ask",
        "dispatch://INFRA-148/ask/ask-1 · INFRA-148: People's answers on every request",
        "Question: Which format should the list use?",
        "Selected: JSON, Newest first",
        "Use JSON lines, one per item.",
        "",
        "2026-10-27T10:30:00Z · user ben@example.com · commented on a document · edited 2026-10-27T10:45:00Z",
        "dispatch://CORE/artifact/runbook/comment/c-1 · CORE / Runbook",
        "Step 3 needs the staging URL.",
        "",
        "2026-10-27T10:00:00Z · user cy@example.com · sent you a message",
        "Direct message m-1, on no issue",
        "Stop after the migration lands.",
      ].join("\n")
    );
  });

  test("an answer that chose nothing says Selected: none; one that replaced another says so; a long question is cut at 200", () => {
    const question = `${"q".repeat(199)}rest of the question`;
    expect(
      renderSessionAnswerItem({
        ...answer,
        selected: undefined,
        replaced: true,
        ask: { id: "ask-1", question },
      })
    ).toBe(
      [
        "2026-10-27T11:00:00Z · user ana@example.com · answered your ask (replaces an earlier answer)",
        "dispatch://INFRA-148/ask/ask-1 · INFRA-148: People's answers on every request",
        `Question: ${"q".repeat(199)}r…`,
        "Selected: none",
        "Use JSON lines, one per item.",
      ].join("\n")
    );
  });

  test("a list of 7,999 or 8,000 characters is whole; at 8,001 its one item is left out", () => {
    for (const target of [7_999, 8_000]) {
      const item = sizedItem("fits", target);
      const rendered = renderSessionAnswers([item], { now: NOW });
      expect(rendered).toBe(`${SESSION_ANSWERS_MARKER}\n\n${renderSessionAnswerItem(item)}`);
      expect(rendered?.length).toBe(target);
    }
    const over = renderSessionAnswers([sizedItem("over", 8_001)], { now: NOW });
    expect(over).toBe(`${SESSION_ANSWERS_MARKER}\n\n${leftOut(1)}`);
  });

  test("the left-out line counts in the cap: an item fits only with the line its older items need", () => {
    const older = message("older", hoursAgo(2), "older words");
    const exact = sizedItem("exact", 8_000, 1);
    const rendered = renderSessionAnswers([exact, older], { now: NOW });
    expect(rendered).toBe(
      `${SESSION_ANSWERS_MARKER}\n\n${renderSessionAnswerItem(exact)}\n\n${leftOut(1)}`
    );
    expect(rendered?.length).toBe(8_000);
    expect(renderSessionAnswers([sizedItem("past", 8_001, 1), older], { now: NOW })).toBe(
      `${SESSION_ANSWERS_MARKER}\n\n${leftOut(2)}`
    );
  });

  test("a 9,000-character newest item is left out whole, and every older item with it", () => {
    const rendered = renderSessionAnswers(
      [
        message("big", hoursAgo(1), "b".repeat(9_000)),
        message("small-1", hoursAgo(2), "small one"),
        message("small-2", hoursAgo(3), "small two"),
      ],
      { now: NOW }
    );
    expect(rendered).toBe(`${SESSION_ANSWERS_MARKER}\n\n${leftOut(3)}`);
  });

  test("past the cap it shows a newest-first prefix, and every item left out is older than every item shown", () => {
    const items = Array.from({ length: 12 }, (_, index) =>
      message(`item-${index}`, hoursAgo(index + 1), `${index}:${"w".repeat(1_000)}`)
    );
    const rendered = renderSessionAnswers(items, { now: NOW }) ?? "";
    const shown = items.filter((item) => rendered.includes(`Direct message ${item.id},`));
    expect(shown.length).toBeGreaterThan(0);
    expect(shown.length).toBeLessThan(items.length);
    expect(shown).toEqual(items.slice(0, shown.length));
    expect(rendered.length).toBeLessThanOrEqual(8_000);
    expect(rendered.endsWith(`\n\n${leftOut(items.length - shown.length)}`)).toBe(true);
    expect(rendered).toContain("run `dispatch answers` to read every one in full.");
  });

  test("prints no left-out line when everything fits, and nothing at all when there is nothing", () => {
    const rendered = renderSessionAnswers([answer, direct], { now: NOW });
    expect(rendered).not.toContain("left out");
    expect(renderSessionAnswers([], { now: NOW })).toBeUndefined();
  });

  test("counts the items a server's max_chars did not send, unless the window has moved past them", () => {
    const items = [message("a", hoursAgo(1), "one"), message("b", hoursAgo(2), "two")];
    expect(renderSessionAnswers(items, { now: NOW, total: 9 })).toEndWith(`\n\n${leftOut(7)}`);
    // Once a sent item is past 72 hours, every unsent one, older still, is too.
    const aged = [message("a", hoursAgo(1), "one"), message("b", hoursAgo(73), "two")];
    expect(renderSessionAnswers(aged, { now: NOW, total: 9 })).not.toContain("left out");
  });

  test("filters a cached item by the 72 hours again at render", () => {
    const items = [message("recent", hoursAgo(71), "recent"), message("old", hoursAgo(73), "old")];
    const rendered = renderSessionAnswers(items, { now: NOW }) ?? "";
    expect(rendered).toContain("Direct message recent,");
    expect(rendered).not.toContain("Direct message old,");
    expect(renderSessionAnswers(items, { now: NOW + 2 * 3_600_000 })).toBeUndefined();
  });

  test("a cached list keeps the first line and says when it was read on the second", () => {
    const rendered = renderSessionAnswers([direct], {
      now: NOW,
      cachedAt: "2026-10-27T11:59:00.000Z",
    });
    expect(rendered?.split("\n").slice(0, 2)).toEqual([
      SESSION_ANSWERS_MARKER,
      "As read at 2026-10-27T11:59:00.000Z; Dispatch did not answer now.",
    ]);
  });
});

describe("renderSessionAnswersList", () => {
  test("prints every item of the 72 hours with no cap", () => {
    const items = Array.from({ length: 12 }, (_, index) =>
      message(`item-${index}`, hoursAgo(index + 1), "w".repeat(1_000))
    );
    const listed = renderSessionAnswersList([...items, message("old", hoursAgo(80), "old")], {
      now: NOW,
    });
    expect(listed).toBe(
      [
        "12 items people wrote to this session in the last 72 hours, newest first, word for word:",
        ...items.map(renderSessionAnswerItem),
      ].join("\n\n")
    );
    expect(renderSessionAnswersList([], { now: NOW })).toBe(
      "No person wrote to this session in the last 72 hours."
    );
  });
});

interface FakeCall {
  readonly url: URL;
  readonly signal: AbortSignal | undefined;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function fakeDispatch(handler: (call: FakeCall) => Response | Promise<Response>) {
  const calls: FakeCall[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: new URL(String(input)), signal: init?.signal ?? undefined };
    calls.push(call);
    return handler(call);
  }) as typeof fetch;
  return { calls, fetchImpl };
}

let dispatchCount = 0;
/** A Dispatch of its own per test: the 404 back-off is process-wide, by URL. */
function dispatchAt(): ActiveDispatchConfig {
  dispatchCount += 1;
  return { enabled: true, url: `http://answers-${dispatchCount}.test`, token: "test-token" };
}
let sessionCount = 0;
/** A session of its own per test: the read state is process-wide, by session. */
const newSession = () => {
  sessionCount += 1;
  return `ses-answers-${sessionCount}`;
};

const listed = (items: readonly SessionAnswer[], total = items.length) =>
  json({ session_id: "ses", since: hoursAgo(72), total, items });
const recent = [
  message("m-new", hoursAgo(1), "newest words"),
  message("m-old", hoursAgo(2), "older words"),
];

function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

describe("readSessionAnswers", () => {
  test("reads the list with max_chars, renders it, and keeps it in answers.json", async () => {
    const directory = stateDir();
    const session = newSession();
    const { calls, fetchImpl } = fakeDispatch(() => listed(recent));
    const read = await readSessionAnswers({
      session,
      dispatch: dispatchAt(),
      stateDirectory: directory,
      fetchImpl,
      now: () => NOW,
    });
    expect(read).toEqual({
      text: renderSessionAnswers(recent, { now: NOW }),
      source: "fresh",
      readAt: new Date(NOW).toISOString(),
    });
    expect(calls.map((call) => call.url.pathname + call.url.search)).toEqual([
      `/api/v1/agents/${session}/answers?max_chars=8000`,
    ]);
    expect(JSON.parse(readFileSync(join(directory, "answers.json"), "utf-8"))).toEqual({
      read_at: new Date(NOW).toISOString(),
      total: 2,
      items: recent,
    });
  });

  test("with no list ever read, a failed read carries the failure line naming why", async () => {
    const { fetchImpl } = fakeDispatch(() =>
      json({ code: "INTERNAL", error: "database is down" }, 500)
    );
    const read = await readSessionAnswers({
      session: newSession(),
      dispatch: dispatchAt(),
      stateDirectory: stateDir(),
      fetchImpl,
      now: () => NOW,
    });
    expect(read).toEqual({
      text: "[Dispatch: people's answers from the last 72 hours could not be read (database is down); run `dispatch answers`.]",
      source: "failure",
      reason: "database is down",
    });
    expect(read.text).toBe(sessionAnswersFailureLine("database is down"));
  });

  test("a 200 whose body is no answers list is a failed read, not a cache and not a throw", async () => {
    const directory = stateDir();
    const { fetchImpl } = fakeDispatch(() => json({ session_id: "s", topics: [] }));
    const read = await readSessionAnswers({
      session: newSession(),
      dispatch: dispatchAt(),
      stateDirectory: directory,
      fetchImpl,
      now: () => NOW,
    });
    const reason = "Dispatch answered with something that is not an answers list";
    expect(read).toEqual({ text: sessionAnswersFailureLine(reason), source: "failure", reason });
    expect(existsSync(join(directory, "answers.json"))).toBe(false);
  });

  test("a failed read after a good one carries the cached list, saying when it was read", async () => {
    const session = newSession();
    const dispatch = dispatchAt();
    let fail = false;
    const { fetchImpl } = fakeDispatch(() =>
      fail ? json({ code: "INTERNAL", error: "database is down" }, 500) : listed(recent)
    );
    const options = { session, dispatch, stateDirectory: stateDir(), fetchImpl };
    await readSessionAnswers({ ...options, now: () => NOW });
    fail = true;
    const later = NOW + 60_000;
    const read = await readSessionAnswers({ ...options, now: () => later });
    const readAt = new Date(NOW).toISOString();
    expect(read).toEqual({
      text: renderSessionAnswers(recent, { now: later, cachedAt: readAt }),
      source: "cache",
      readAt,
      reason: "database is down",
    });
    expect(read.text?.split("\n").slice(0, 2)).toEqual([
      SESSION_ANSWERS_MARKER,
      `As read at ${readAt}; Dispatch did not answer now.`,
    ]);
  });

  test("loads answers.json when memory holds no list, as after a restart", async () => {
    const directory = stateDir();
    const readAt = hoursAgo(0.5);
    writeFileSync(
      join(directory, "answers.json"),
      JSON.stringify({ read_at: readAt, total: 2, items: recent })
    );
    const { fetchImpl } = fakeDispatch(() => {
      throw new TypeError("Unable to connect");
    });
    const read = await readSessionAnswers({
      session: newSession(),
      dispatch: dispatchAt(),
      stateDirectory: directory,
      fetchImpl,
      now: () => NOW,
    });
    expect(read.source).toBe("cache");
    expect(read.text).toBe(renderSessionAnswers(recent, { now: NOW, cachedAt: readAt }));
  });

  // The deadline is a real timer in the code under test, so this test waits on real time.
  test("waits at most the deadline, does not wait again on a read past it, and the late answer fills the cache", async () => {
    const session = newSession();
    const dispatch = dispatchAt();
    const hung = deferred<Response>();
    let respond: () => Response | Promise<Response> = () => hung.promise;
    const { calls, fetchImpl } = fakeDispatch(() => respond());
    const options = { session, dispatch, fetchImpl, now: () => NOW };

    let started = performance.now();
    const first = await readSessionAnswers({ ...options, deadlineMs: 50 });
    expect(performance.now() - started).toBeLessThan(500);
    expect(first).toEqual({
      text: sessionAnswersFailureLine("Dispatch did not answer within 50 ms"),
      source: "failure",
      reason: "Dispatch did not answer within 50 ms",
    });

    // The read in flight already cost one request its deadline: the next does not wait on it.
    started = performance.now();
    const second = await readSessionAnswers({ ...options, deadlineMs: 2_000 });
    expect(performance.now() - started).toBeLessThan(1_000);
    expect(second.source).toBe("failure");
    expect(calls).toHaveLength(1);

    // The background read answers, and the cache holds its list.
    hung.resolve(listed(recent));
    await sessionAnswersSettled(session);
    respond = () => json({ code: "INTERNAL", error: "database is down" }, 500);
    const third = await readSessionAnswers({ ...options, deadlineMs: 2_000 });
    expect(calls).toHaveLength(2);
    expect(third.source).toBe("cache");
    expect(third.text).toBe(
      renderSessionAnswers(recent, { now: NOW, cachedAt: new Date(NOW).toISOString() })
    );
  });

  test("the request's own abort ends its wait at once, and the shared read goes on", async () => {
    const session = newSession();
    const hung = deferred<Response>();
    let respond: () => Response | Promise<Response> = () => hung.promise;
    const { calls, fetchImpl } = fakeDispatch(() => respond());
    const controller = new AbortController();
    const options = { session, dispatch: dispatchAt(), fetchImpl, now: () => NOW };
    const waiting = readSessionAnswers({ ...options, signal: controller.signal });
    controller.abort();
    expect((await waiting).source).toBe("failure");
    expect(calls[0]?.signal?.aborted).toBe(false);
    hung.resolve(listed(recent));
    await sessionAnswersSettled(session);
    respond = () => json({ code: "INTERNAL", error: "database is down" }, 500);
    expect((await readSessionAnswers(options)).source).toBe("cache");
  });

  test("every request of a session, from any copy of the module, joins the one read in flight", async () => {
    const session = newSession();
    const dispatch = dispatchAt();
    const pending = deferred<Response>();
    const { calls, fetchImpl } = fakeDispatch(() => pending.promise);
    // A query string loads a module of its own, as Oh My Pi loads each extension instance; this
    // test is of that loading boundary, so the import is dynamic.
    const source = "../session-answers.ts";
    const copy: typeof SessionAnswersModule = await import(`${source}?copy`);
    expect(copy.readSessionAnswers).not.toBe(readSessionAnswers);
    const options = { session, dispatch, fetchImpl, now: () => NOW, deadlineMs: 2_000 };
    const parent = readSessionAnswers(options);
    const subagent = copy.readSessionAnswers(options);
    pending.resolve(listed(recent));
    const [one, two] = await Promise.all([parent, subagent]);
    expect(calls).toHaveLength(1);
    expect(one).toEqual(two);
    expect(one.source).toBe("fresh");
  });

  test("a Dispatch that answers 404 has no answers route, and is not asked again for 5 minutes", async () => {
    const dispatch = dispatchAt();
    const { calls, fetchImpl } = fakeDispatch((call) =>
      json({ code: "NOT_FOUND", error: `no route for GET ${call.url.pathname}` }, 404)
    );
    let clock = NOW;
    const options = { dispatch, fetchImpl, now: () => clock };
    const first = await readSessionAnswers({ ...options, session: newSession() });
    expect(first).toEqual({
      text: sessionAnswersFailureLine(NO_ANSWERS_ROUTE_REASON),
      source: "failure",
      reason: "this Dispatch build has no answers route",
    });
    // Any session of the process, for that Dispatch URL.
    clock = NOW + 5 * 60_000 - 1;
    const second = await readSessionAnswers({ ...options, session: newSession() });
    expect(second.reason).toBe(NO_ANSWERS_ROUTE_REASON);
    expect(calls).toHaveLength(1);
    clock = NOW + 5 * 60_000 + 1;
    await readSessionAnswers({ ...options, session: newSession() });
    expect(calls).toHaveLength(2);
  });

  test("Dispatch off with an error is a failed read with that reason; off with a notice reads nothing", async () => {
    const { calls, fetchImpl } = fakeDispatch(() => listed(recent));
    const notRun =
      "/home/u/envoy.json: dispatch.token's command has not run in this process yet, and this read does not run it";
    const failed = await readSessionAnswers({
      session: newSession(),
      dispatch: { enabled: false, notice: null, error: notRun },
      fetchImpl,
    });
    expect(failed).toEqual({
      text: sessionAnswersFailureLine(notRun),
      source: "failure",
      reason: notRun,
    });
    const off = await readSessionAnswers({
      session: newSession(),
      dispatch: { enabled: false, notice: "Dispatch is off", error: null },
      fetchImpl,
    });
    expect(off).toEqual({ text: undefined, source: "off", reason: "Dispatch is off" });
    expect(calls).toHaveLength(0);
  });

  test("writes answers.json atomically: a new file renamed over the old, owner-only, no temporary left", async () => {
    const directory = stateDir();
    const file = join(directory, "answers.json");
    const old = JSON.stringify({ read_at: hoursAgo(1), total: 0, items: [] });
    writeFileSync(file, old);
    // A second name for the old file: a write in place would change what it reads too.
    linkSync(file, join(directory, "answers.old"));
    const { fetchImpl } = fakeDispatch(() => listed(recent));
    await readSessionAnswers({
      session: newSession(),
      dispatch: dispatchAt(),
      stateDirectory: directory,
      fetchImpl,
      now: () => NOW,
    });
    expect(readFileSync(join(directory, "answers.old"), "utf-8")).toBe(old);
    expect(JSON.parse(readFileSync(file, "utf-8")).items).toEqual(recent);
    expect(statSync(file).mode & 0o777).toBe(0o600);
    expect(readdirSync(directory).sort()).toEqual(["answers.json", "answers.old"]);
  });
});
