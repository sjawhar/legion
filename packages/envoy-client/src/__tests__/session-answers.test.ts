import { describe, expect, test } from "bun:test";
import {
  existsSync,
  linkSync,
  mkdirSync,
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
  SESSION_ANSWERS_DEADLINE_MS,
  SESSION_ANSWERS_MARKER,
  type SessionAnswersRead,
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
  owner: { issue: { key: "CORE-7", title: "People's answers on every request" } },
  ref: "dispatch://CORE-7/ask/ask-1",
  href: "/issues/CORE-7/asks/ask-1",
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
        "dispatch://CORE-7/ask/ask-1 · CORE-7: People's answers on every request",
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
        "dispatch://CORE-7/ask/ask-1 · CORE-7: People's answers on every request",
        `Question: ${"q".repeat(199)}r…`,
        "Selected: none",
        "Use JSON lines, one per item.",
      ].join("\n")
    );
  });

  test("a list of 7,998 or 7,999 characters is whole; at 8,000 its one item is left out", () => {
    for (const target of [7_998, 7_999]) {
      const item = sizedItem("fits", target);
      const rendered = renderSessionAnswers([item], { now: NOW });
      expect(rendered).toBe(`${SESSION_ANSWERS_MARKER}\n\n${renderSessionAnswerItem(item)}`);
      expect(rendered?.length).toBe(target);
    }
    const over = renderSessionAnswers([sizedItem("over", 8_000)], { now: NOW });
    expect(over).toBe(`${SESSION_ANSWERS_MARKER}\n\n${leftOut(1)}`);
  });

  test("the left-out line counts in the cap: an item fits only with the line its older items need", () => {
    const older = message("older", hoursAgo(2), "older words");
    const exact = sizedItem("exact", 7_999, 1);
    const rendered = renderSessionAnswers([exact, older], { now: NOW });
    expect(rendered).toBe(
      `${SESSION_ANSWERS_MARKER}\n\n${renderSessionAnswerItem(exact)}\n\n${leftOut(1)}`
    );
    expect(rendered?.length).toBe(7_999);
    expect(renderSessionAnswers([sizedItem("past", 8_000, 1), older], { now: NOW })).toBe(
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
    expect(rendered.length).toBeLessThan(8_000);
    expect(rendered.endsWith(`\n\n${leftOut(items.length - shown.length)}`)).toBe(true);
    expect(rendered).toContain("run `dispatch answers` to read every one in full.");
  });

  test("withoutCommand ends the list with a left-out line that names no command, still under the cap", () => {
    const items = Array.from({ length: 12 }, (_, index) =>
      message(`item-${index}`, hoursAgo(index + 1), `${index}:${"w".repeat(1_000)}`)
    );
    const rendered = renderSessionAnswers(items, { now: NOW, withoutCommand: true }) ?? "";
    const shown = items.filter((item) => rendered.includes(`Direct message ${item.id},`));
    expect(shown).toEqual(items.slice(0, shown.length));
    expect(rendered.length).toBeLessThan(8_000);
    const count = items.length - shown.length;
    expect(rendered).toEndWith(`\n\n${count} older items left out to stay under 8,000 characters.`);
    expect(rendered).not.toContain("dispatch answers");
    expect(
      renderSessionAnswers([message("one", hoursAgo(1), "one")], {
        now: NOW,
        total: 2,
        withoutCommand: true,
      })
    ).toEndWith("\n\n1 older item left out to stay under 8,000 characters.");
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

  test("a cached list keeps the first line and names its read time and why this read failed on the second", () => {
    const rendered = renderSessionAnswers([direct], {
      now: NOW,
      cached: { readAt: "2026-10-27T11:59:00.000Z", reason: "token\n  revoked" },
    });
    expect(rendered?.split("\n").slice(0, 2)).toEqual([
      SESSION_ANSWERS_MARKER,
      "As read at 2026-10-27T11:59:00.000Z; this read failed: token revoked.",
    ]);
  });

  test("a reason that already ends a sentence gets no second full stop", () => {
    const secondLine = (reason: string) =>
      renderSessionAnswers([direct], {
        now: NOW,
        cached: { readAt: "2026-10-27T11:59:00.000Z", reason },
      })?.split("\n")[1];
    expect(secondLine("Dispatch is restarting, so a retry may succeed.")).toBe(
      "As read at 2026-10-27T11:59:00.000Z; this read failed: Dispatch is restarting, so a retry may succeed."
    );
    expect(secondLine("Is the computer able to access the url?")).toBe(
      "As read at 2026-10-27T11:59:00.000Z; this read failed: Is the computer able to access the url?"
    );
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

/** The environment `readSessionAnswers` works the session's state directory out from. */
const envAt = (root: string) => ({ DISPATCH_STATE_DIR: root });
/** Where the session's `answers.json` lives under the state root `root`. */
const cacheFile = (root: string, session: string) =>
  join(root, "sessions", session, "answers.json");
const hostOf = (dispatch: ActiveDispatchConfig) => new URL(dispatch.url).host;

/** A page something in Dispatch's place serves, with `status`. */
const page = (status: number, body: string, statusText = "") =>
  new Response(body, { status, statusText, headers: { "Content-Type": "text/html" } });

/** Each way a read fails that keeps the failure line, with the reason it gives (a pattern for an
 *  answer Dispatch did not write, whose reason names the request's URL) and how long a request
 *  waits on it. */
const failedReads: readonly {
  readonly kind: string;
  readonly reason: string | RegExp;
  readonly deadlineMs: number;
  readonly respond: () => Response | Promise<Response>;
}[] = [
  {
    kind: "401",
    reason: "token revoked",
    deadlineMs: 2_000,
    respond: () => json({ code: "UNAUTHORIZED", error: "token revoked" }, 401),
  },
  {
    kind: "5xx",
    reason: "database is down",
    deadlineMs: 2_000,
    respond: () => json({ code: "INTERNAL", error: "database is down" }, 500),
  },
  {
    kind: "Dispatch's own 503",
    reason: "the database is unavailable",
    deadlineMs: 2_000,
    respond: () => json({ code: "UNAVAILABLE", error: "the database is unavailable" }, 503),
  },
  {
    kind: "a hang past the deadline",
    reason: "Dispatch did not answer within 50 ms",
    deadlineMs: 50,
    respond: () => new Promise<Response>(() => undefined),
  },
  {
    kind: "a proxy's 404 page",
    reason:
      /^GET \S+\/answers\?max_chars=8000 answered 404 Not Found with a body that is not Dispatch's error JSON \(.*\), which looks like a proxy or gateway page rather than Dispatch's own answer, so /,
    deadlineMs: 2_000,
    respond: () =>
      page(404, "<html><body><h1>404 Not Found</h1><hr>nginx</body></html>", "Not Found"),
  },
  {
    kind: "a 404 with an empty body",
    reason:
      /^GET \S+\/answers\?max_chars=8000 answered 404 with an empty body, which looks like a proxy or gateway page rather than Dispatch's own answer, so /,
    deadlineMs: 2_000,
    respond: () => new Response("", { status: 404 }),
  },
  {
    kind: "a 200 whose JSON is no answers list",
    reason: "Dispatch answered with something that is not an answers list",
    deadlineMs: 2_000,
    respond: () => json({ session_id: "s", topics: [] }),
  },
  {
    kind: "a 200 HTML sign-in page",
    reason: "Dispatch answered with something that is not an answers list",
    deadlineMs: 2_000,
    respond: () => page(200, "<!doctype html><html><title>Sign in</title><form></form></html>"),
  },
  {
    kind: "a 200 JSON error envelope",
    reason: "Dispatch answered with something that is not an answers list",
    deadlineMs: 2_000,
    respond: () => json({ code: "UNAUTHORIZED", error: "sign in first" }),
  },
  {
    kind: "a 200 with null items",
    reason: "Dispatch answered with something that is not an answers list",
    deadlineMs: 2_000,
    respond: () => json({ session_id: "s", since: hoursAgo(72), total: 0, items: null }),
  },
  {
    kind: "a 200 with an empty body",
    reason: "Dispatch answered with something that is not an answers list",
    deadlineMs: 2_000,
    respond: () => new Response("", { status: 200 }),
  },
];

/** Dispatch's own answer that it has no answers route, as its router answers a build older than
 *  the route. */
const noRoute = (call: FakeCall) =>
  json(
    {
      code: "NOT_FOUND",
      error: `no route for GET ${call.url.pathname}`,
      hint: "GET /api/v1 lists every route",
    },
    404
  );

/** The notice a read gives once per session and URL for a Dispatch with no answers route. */
const noRouteNotice = (url: string) =>
  `Dispatch at ${url} has no answers route (its build is older than the route), so people's answers cannot be read from it`;

/** That `read` is the failure line, naming `reason`, with no notice. */
function expectFailureLine(read: SessionAnswersRead, reason: string | RegExp): void {
  if (typeof reason === "string") expect(read.reason).toBe(reason);
  else expect(read.reason).toMatch(reason);
  expect(read).toEqual({
    text: sessionAnswersFailureLine(read.reason ?? ""),
    source: "failure",
    reason: read.reason,
  });
}

describe("readSessionAnswers", () => {
  test("reads the list with max_chars, renders it, and keeps it in answers.json with its Dispatch URL", async () => {
    const root = stateDir();
    const session = newSession();
    const dispatch = dispatchAt();
    const { calls, fetchImpl } = fakeDispatch(() => listed(recent));
    const read = await readSessionAnswers({
      session,
      dispatch,
      env: envAt(root),
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
    expect(JSON.parse(readFileSync(cacheFile(root, session), "utf-8"))).toEqual({
      dispatch_url: dispatch.url,
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
      env: envAt(stateDir()),
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
    const root = stateDir();
    const session = newSession();
    const { fetchImpl } = fakeDispatch(() => json({ session_id: "s", topics: [] }));
    const read = await readSessionAnswers({
      session,
      dispatch: dispatchAt(),
      env: envAt(root),
      fetchImpl,
      now: () => NOW,
    });
    const reason = "Dispatch answered with something that is not an answers list";
    expect(read).toEqual({ text: sessionAnswersFailureLine(reason), source: "failure", reason });
    expect(existsSync(cacheFile(root, session))).toBe(false);
  });

  for (const { kind, reason, deadlineMs, respond } of failedReads) {
    test(`after an empty read, a failed read (${kind}) carries the failure line naming why`, async () => {
      let fail = false;
      const { fetchImpl } = fakeDispatch(() => (fail ? respond() : listed([])));
      const options = {
        session: newSession(),
        dispatch: dispatchAt(),
        env: envAt(stateDir()),
        fetchImpl,
        now: () => NOW,
      };
      expect(await readSessionAnswers(options)).toEqual({
        text: undefined,
        source: "fresh",
        readAt: new Date(NOW).toISOString(),
      });
      fail = true;
      expectFailureLine(await readSessionAnswers({ ...options, deadlineMs }), reason);
    });

    test(`after a non-empty read, a failed read (${kind}) carries the cached list, its header naming why`, async () => {
      let fail = false;
      const { fetchImpl } = fakeDispatch(() => (fail ? respond() : listed(recent)));
      const options = {
        session: newSession(),
        dispatch: dispatchAt(),
        env: envAt(stateDir()),
        fetchImpl,
      };
      await readSessionAnswers({ ...options, now: () => NOW });
      fail = true;
      const later = NOW + 60_000;
      const read = await readSessionAnswers({ ...options, now: () => later, deadlineMs });
      const readAt = new Date(NOW).toISOString();
      if (typeof reason === "string") expect(read.reason).toBe(reason);
      else expect(read.reason).toMatch(reason);
      const failed = read.reason ?? "";
      expect(read).toEqual({
        text: renderSessionAnswers(recent, { now: later, cached: { readAt, reason: failed } }),
        source: "cache",
        readAt,
        reason: failed,
      });
      expect(read.text?.split("\n")[0]).toBe(SESSION_ANSWERS_MARKER);
      expect(read.text?.split("\n")[1]).toStartWith(`As read at ${readAt}; this read failed: `);
    });
  }

  test("a proxy's 404 is a failed read like any other: Dispatch is asked again, and no notice", async () => {
    const { calls, fetchImpl } = fakeDispatch(() =>
      page(404, "<html><body><h1>404 Not Found</h1></body></html>", "Not Found")
    );
    const options = {
      session: newSession(),
      dispatch: dispatchAt(),
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
    };
    const proxyPage = /answered 404 Not Found with a body that is not Dispatch's error JSON/;
    expectFailureLine(await readSessionAnswers(options), proxyPage);
    expectFailureLine(await readSessionAnswers(options), proxyPage);
    expect(calls).toHaveLength(2);
  });

  test("after an empty read, Dispatch's own no-route 404 adds no line and gives the host a notice", async () => {
    let missing = false;
    const { fetchImpl } = fakeDispatch((call) => (missing ? noRoute(call) : listed([])));
    const dispatch = dispatchAt();
    const options = {
      session: newSession(),
      dispatch,
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
    };
    expect((await readSessionAnswers(options)).source).toBe("fresh");
    missing = true;
    const read = await readSessionAnswers(options);
    expect(read).toEqual({
      text: undefined,
      source: "unavailable",
      reason: NO_ANSWERS_ROUTE_REASON,
      notice: noRouteNotice(dispatch.url),
    });
    expect(read.text).toBeUndefined();
  });

  test("a cached list carried after Dispatch's no-route 404 never names `dispatch answers`, its left-out line included", async () => {
    // Twelve items of about 1,000 characters each: the cap leaves the oldest out.
    const items = Array.from({ length: 12 }, (_, index) =>
      message(`item-${index}`, hoursAgo(index + 1), `${index}:${"w".repeat(1_000)}`)
    );
    let missing = false;
    const { fetchImpl } = fakeDispatch((call) => (missing ? noRoute(call) : listed(items)));
    const options = {
      session: newSession(),
      dispatch: dispatchAt(),
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
    };
    const fresh = await readSessionAnswers(options);
    expect(fresh.text).toContain("run `dispatch answers` to read every one in full.");
    missing = true;
    const read = await readSessionAnswers(options);
    const readAt = new Date(NOW).toISOString();
    expect(read.source).toBe("cache");
    expect(read.text).toBe(
      renderSessionAnswers(items, {
        now: NOW,
        cached: { readAt, reason: NO_ANSWERS_ROUTE_REASON },
        withoutCommand: true,
      })
    );
    expect(read.text?.split("\n").at(-1)).toMatch(
      /^\d+ older items left out to stay under 8,000 characters\.$/
    );
    expect(read.text).not.toContain("dispatch answers");
  });

  test("a failed read carries the failure line once every cached item is past 72 hours", async () => {
    let fail = false;
    const { fetchImpl } = fakeDispatch(() =>
      fail ? json({ code: "UNAUTHORIZED", error: "token revoked" }, 401) : listed(recent)
    );
    const options = {
      session: newSession(),
      dispatch: dispatchAt(),
      env: envAt(stateDir()),
      fetchImpl,
    };
    await readSessionAnswers({ ...options, now: () => NOW });
    fail = true;
    const read = await readSessionAnswers({ ...options, now: () => NOW + 73 * 3_600_000 });
    expect(read).toEqual({
      text: sessionAnswersFailureLine("token revoked"),
      source: "failure",
      reason: "token revoked",
    });
  });

  test("loads answers.json when memory holds no list, as after a restart", async () => {
    const root = stateDir();
    const session = newSession();
    const dispatch = dispatchAt();
    const readAt = hoursAgo(0.5);
    mkdirSync(join(root, "sessions", session), { recursive: true });
    writeFileSync(
      cacheFile(root, session),
      JSON.stringify({ dispatch_url: dispatch.url, read_at: readAt, total: 2, items: recent })
    );
    const { fetchImpl } = fakeDispatch(() => {
      throw new TypeError("Unable to connect");
    });
    const read = await readSessionAnswers({
      session,
      dispatch,
      env: envAt(root),
      fetchImpl,
      now: () => NOW,
    });
    expect(read.source).toBe("cache");
    expect(read.text).toBe(
      renderSessionAnswers(recent, {
        now: NOW,
        cached: { readAt, reason: "Unable to connect" },
      })
    );
  });

  test("a list read from one Dispatch is never carried for another, from memory or answers.json", async () => {
    const root = stateDir();
    const session = newSession();
    const a = dispatchAt();
    const b = dispatchAt();
    let aFails = false;
    const failure = () => json({ code: "INTERNAL", error: "database is down" }, 500);
    const { fetchImpl } = fakeDispatch((call) =>
      call.url.host === hostOf(a) && !aFails ? listed(recent) : failure()
    );
    const options = { env: envAt(root), fetchImpl, now: () => NOW };
    const fromA = await readSessionAnswers({ ...options, session, dispatch: a });
    expect(fromA.source).toBe("fresh");
    const notCarried: SessionAnswersRead = {
      text: sessionAnswersFailureLine("database is down"),
      source: "failure",
      reason: "database is down",
    };
    expect(await readSessionAnswers({ ...options, session, dispatch: b })).toEqual(notCarried);

    // A's answers.json, in a process with no list in memory (another session's directory here).
    const restarted = newSession();
    mkdirSync(join(root, "sessions", restarted), { recursive: true });
    writeFileSync(cacheFile(root, restarted), readFileSync(cacheFile(root, session)));
    aFails = true;
    expect(await readSessionAnswers({ ...options, session: restarted, dispatch: b })).toEqual(
      notCarried
    );
    const carried = await readSessionAnswers({ ...options, session: restarted, dispatch: a });
    expect(carried.source).toBe("cache");
    expect(carried.readAt).toBe(fromA.readAt);
  });

  test("a request to one Dispatch does not join a read in flight to another", async () => {
    const session = newSession();
    const a = dispatchAt();
    const b = dispatchAt();
    const pendingA = deferred<Response>();
    const { calls, fetchImpl } = fakeDispatch((call) =>
      call.url.host === hostOf(a) ? pendingA.promise : listed([direct])
    );
    const options = {
      session,
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
      deadlineMs: 2_000,
    };
    const fromA = readSessionAnswers({ ...options, dispatch: a });
    const fromB = await readSessionAnswers({ ...options, dispatch: b });
    expect(fromB).toEqual({
      text: renderSessionAnswers([direct], { now: NOW }),
      source: "fresh",
      readAt: new Date(NOW).toISOString(),
    });
    expect(calls.map((call) => call.url.host)).toEqual([hostOf(a), hostOf(b)]);
    pendingA.resolve(listed(recent));
    expect((await fromA).text).toBe(renderSessionAnswers(recent, { now: NOW }));
  });

  test("a session id that is not one plain path segment keeps its list in memory only", async () => {
    const root = stateDir();
    let fail = false;
    const { fetchImpl } = fakeDispatch(() =>
      fail ? json({ code: "INTERNAL", error: "database is down" }, 500) : listed(recent)
    );
    const options = {
      session: `../${newSession()}`,
      dispatch: dispatchAt(),
      env: envAt(root),
      fetchImpl,
      now: () => NOW,
    };
    expect((await readSessionAnswers(options)).source).toBe("fresh");
    expect(readdirSync(root)).toEqual([]);
    fail = true;
    expect((await readSessionAnswers(options)).source).toBe("cache");
  });

  // The deadline is a real timer in the code under test, so this test waits on real time.
  test("waits at most the deadline, does not wait again on a read past it, and the late answer fills the cache", async () => {
    const session = newSession();
    const dispatch = dispatchAt();
    const hung = deferred<Response>();
    let respond: () => Response | Promise<Response> = () => hung.promise;
    const { calls, fetchImpl } = fakeDispatch(() => respond());
    const options = { session, dispatch, env: envAt(stateDir()), fetchImpl, now: () => NOW };

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
      renderSessionAnswers(recent, {
        now: NOW,
        cached: { readAt: new Date(NOW).toISOString(), reason: "database is down" },
      })
    );
  });

  test("the request's own abort ends its wait at once, and the shared read goes on", async () => {
    const session = newSession();
    const hung = deferred<Response>();
    let respond: () => Response | Promise<Response> = () => hung.promise;
    const { calls, fetchImpl } = fakeDispatch(() => respond());
    const controller = new AbortController();
    const options = {
      session,
      dispatch: dispatchAt(),
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
    };
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
    const options = {
      session,
      dispatch,
      env: envAt(stateDir()),
      fetchImpl,
      now: () => NOW,
      deadlineMs: 2_000,
    };
    const parent = readSessionAnswers(options);
    const subagent = copy.readSessionAnswers(options);
    pending.resolve(listed(recent));
    const [one, two] = await Promise.all([parent, subagent]);
    expect(calls).toHaveLength(1);
    expect(one).toEqual(two);
    expect(one.source).toBe("fresh");
  });

  test("Dispatch's own no-route 404 adds no line, and that Dispatch is not asked again for 5 minutes", async () => {
    const dispatch = dispatchAt();
    const { calls, fetchImpl } = fakeDispatch(noRoute);
    let clock = NOW;
    const options = { dispatch, env: envAt(stateDir()), fetchImpl, now: () => clock };
    const unavailable: SessionAnswersRead = {
      text: undefined,
      source: "unavailable",
      reason: "this Dispatch build has no answers route",
      notice: noRouteNotice(dispatch.url),
    };
    expect(await readSessionAnswers({ ...options, session: newSession() })).toEqual(unavailable);
    // Any session of the process, for that Dispatch URL.
    clock = NOW + 5 * 60_000 - 1;
    expect(await readSessionAnswers({ ...options, session: newSession() })).toEqual(unavailable);
    expect(calls).toHaveLength(1);
    clock = NOW + 5 * 60_000 + 1;
    await readSessionAnswers({ ...options, session: newSession() });
    expect(calls).toHaveLength(2);
  });

  test("Dispatch's own no-route 404 is unavailable within the deadline, from one ask", async () => {
    // On the request path the read's client asks once and lets that answer through: the read's
    // own 5-minute back-off is its answer to a release rolling out, so no request waits out
    // attempts of a route this Dispatch has not got. No fake clock and no fake sleep here.
    const dispatch = dispatchAt();
    const { calls, fetchImpl } = fakeDispatch(noRoute);
    const startedAt = Date.now();
    const read = await readSessionAnswers({
      session: newSession(),
      dispatch,
      env: envAt(stateDir()),
      fetchImpl,
    });
    expect(read).toEqual({
      text: undefined,
      source: "unavailable",
      reason: NO_ANSWERS_ROUTE_REASON,
      notice: noRouteNotice(dispatch.url),
    });
    expect(Date.now() - startedAt).toBeLessThan(SESSION_ANSWERS_DEADLINE_MS);
    expect(calls).toHaveLength(1);
  });

  test("the no-route notice comes once per session and Dispatch URL, and again after the route answered", async () => {
    const session = newSession();
    const a = dispatchAt();
    const b = dispatchAt();
    let routed = false;
    const { calls, fetchImpl } = fakeDispatch((call) => (routed ? listed([]) : noRoute(call)));
    let clock = NOW;
    const options = { env: envAt(stateDir()), fetchImpl, now: () => clock };
    const notices = async (dispatch: ActiveDispatchConfig, of = session) =>
      (await readSessionAnswers({ ...options, session: of, dispatch })).notice;

    // Two reads of A for the session: the 404, then one inside its back-off.
    expect(await notices(a)).toBe(noRouteNotice(a.url));
    expect(await notices(a)).toBeUndefined();
    // After the back-off, A answers 404 again: the session was told already.
    clock += 5 * 60_000 + 1;
    expect(await notices(a)).toBeUndefined();
    expect(calls).toHaveLength(2);
    // Another URL for the same session, and another session for A, are each told once.
    expect(await notices(b)).toBe(noRouteNotice(b.url));
    expect(await notices(b)).toBeUndefined();
    const other = newSession();
    expect(await notices(a, other)).toBe(noRouteNotice(a.url));
    expect(await notices(a, other)).toBeUndefined();
    // A route that answers a list clears it: a later 404 is told again.
    clock += 5 * 60_000 + 1;
    routed = true;
    expect((await readSessionAnswers({ ...options, session, dispatch: a })).source).toBe("fresh");
    routed = false;
    expect(await notices(a)).toBe(noRouteNotice(a.url));
  });

  test("within the no-route back-off, a list read earlier from that URL is still carried, its header naming the missing route", async () => {
    const session = newSession();
    let missing = false;
    const { calls, fetchImpl } = fakeDispatch((call) => (missing ? noRoute(call) : listed(recent)));
    const dispatch = dispatchAt();
    const options = { dispatch, env: envAt(stateDir()), fetchImpl, now: () => NOW };
    expect((await readSessionAnswers({ ...options, session })).source).toBe("fresh");
    missing = true;
    const readAt = new Date(NOW).toISOString();
    const carried: SessionAnswersRead = {
      text: renderSessionAnswers(recent, {
        now: NOW,
        cached: { readAt, reason: NO_ANSWERS_ROUTE_REASON },
        withoutCommand: true,
      }),
      source: "cache",
      readAt,
      reason: NO_ANSWERS_ROUTE_REASON,
    };
    // The read that meets the 404, with the notice; then a request inside the back-off, which
    // asks nothing.
    expect(await readSessionAnswers({ ...options, session })).toEqual({
      ...carried,
      notice: noRouteNotice(dispatch.url),
    });
    expect(await readSessionAnswers({ ...options, session })).toEqual(carried);
    expect(calls).toHaveLength(2);
    expect(carried.text?.split("\n")[1]).toBe(
      `As read at ${readAt}; this read failed: ${NO_ANSWERS_ROUTE_REASON}.`
    );
    expect(carried.text).not.toContain("dispatch answers");
    // A session with no list read from that URL carries nothing.
    expect(await readSessionAnswers({ ...options, session: newSession() })).toEqual({
      text: undefined,
      source: "unavailable",
      reason: NO_ANSWERS_ROUTE_REASON,
      notice: noRouteNotice(dispatch.url),
    });
    expect(calls).toHaveLength(2);
  });

  test("Dispatch off with an error is a failed read with that reason; off with a notice reads nothing", async () => {
    const { calls, fetchImpl } = fakeDispatch(() => listed(recent));
    const notRun =
      "/home/u/envoy.json: dispatch.token's command has not run in this process yet, and this read does not run it";
    const env = envAt(stateDir());
    const failed = await readSessionAnswers({
      session: newSession(),
      dispatch: { enabled: false, notice: null, error: notRun },
      env,
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
      env,
      fetchImpl,
    });
    expect(off).toEqual({ text: undefined, source: "off", reason: "Dispatch is off" });
    expect(calls).toHaveLength(0);
  });

  test("Dispatch off with an error carries only the list read from the configured URL, from memory or answers.json, and none when no URL is named", async () => {
    const root = stateDir();
    const env = envAt(root);
    const session = newSession();
    const a = dispatchAt();
    const b = dispatchAt();
    const { fetchImpl } = fakeDispatch(() => listed(recent));
    await readSessionAnswers({ session, dispatch: a, env, fetchImpl, now: () => NOW });
    const notRun = "dispatch.token's command has not run in this process yet";
    const off = { enabled: false, notice: null, error: notRun } as const;
    const readAt = new Date(NOW).toISOString();
    const fromA: SessionAnswersRead = {
      text: renderSessionAnswers(recent, { now: NOW, cached: { readAt, reason: notRun } }),
      source: "cache",
      readAt,
      reason: notRun,
    };
    const failure: SessionAnswersRead = {
      text: sessionAnswersFailureLine(notRun),
      source: "failure",
      reason: notRun,
    };
    const options = { dispatch: off, env, now: () => NOW };

    // In memory, as in the process that read it.
    expect(await readSessionAnswers({ ...options, session, dispatchUrl: b.url })).toEqual(failure);
    expect(await readSessionAnswers({ ...options, session, dispatchUrl: a.url })).toEqual(fromA);
    expect(await readSessionAnswers({ ...options, session })).toEqual(failure);

    // From A's answers.json, in a process with no list in memory (another session's directory).
    const restarted = newSession();
    mkdirSync(join(root, "sessions", restarted), { recursive: true });
    writeFileSync(cacheFile(root, restarted), readFileSync(cacheFile(root, session)));
    expect(
      await readSessionAnswers({ ...options, session: restarted, dispatchUrl: b.url })
    ).toEqual(failure);
    expect(await readSessionAnswers({ ...options, session: restarted })).toEqual(failure);
    expect(
      await readSessionAnswers({ ...options, session: restarted, dispatchUrl: a.url })
    ).toEqual(fromA);
  });

  test("writes answers.json atomically: a new file renamed over the old, owner-only, no temporary left", async () => {
    const root = stateDir();
    const session = newSession();
    const dispatch = dispatchAt();
    const directory = join(root, "sessions", session);
    mkdirSync(directory, { recursive: true });
    const file = cacheFile(root, session);
    const old = JSON.stringify({
      dispatch_url: dispatch.url,
      read_at: hoursAgo(1),
      total: 0,
      items: [],
    });
    writeFileSync(file, old);
    // A second name for the old file: a write in place would change what it reads too.
    linkSync(file, join(directory, "answers.old"));
    const { fetchImpl } = fakeDispatch(() => listed(recent));
    await readSessionAnswers({ session, dispatch, env: envAt(root), fetchImpl, now: () => NOW });
    expect(readFileSync(join(directory, "answers.old"), "utf-8")).toBe(old);
    expect(JSON.parse(readFileSync(file, "utf-8")).items).toEqual(recent);
    expect(statSync(file).mode & 0o777).toBe(0o600);
    expect(readdirSync(directory).sort()).toEqual(["answers.json", "answers.old"]);
  });
});
