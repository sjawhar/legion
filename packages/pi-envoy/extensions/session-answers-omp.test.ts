import { afterEach, expect, test } from "bun:test";
import * as path from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import {
  isSessionAnswersText,
  NO_ANSWERS_ROUTE_REASON,
  renderSessionAnswers,
  SESSION_ANSWERS_DEADLINE_MS,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import {
  type Block,
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// What people wrote to a session, on every model request of the real Oh My Pi
// (src/session-answers-context.ts, dispatch://INFRA-148): Oh My Pi keeps nothing a `context`
// handler returns, so what reaches the model, where the list sits and where the binary puts its
// cache anchors can only be read off the requests it sends. One stand-in is the model gateway, the
// Envoy listener and Dispatch, whose answers route serves a list each case controls. LEGION_TEST_OMP
// names the binary, as in dispatch-first-omp.test.ts. A run without one skips, except on GitHub
// Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** What the stand-in's answers route does: list `items`, accept and never answer, or answer as a
 *  Dispatch build older than the route does. */
type Route =
  | { readonly kind: "list"; readonly items: readonly SessionAnswer[] }
  | { readonly kind: "hang" }
  | { readonly kind: "missing" };

/** One thing the session is told over RPC, in order, or a change in the answers route before the
 *  next one. */
type Step = { readonly prompt: string } | { readonly compact: true } | { readonly route: Route };

/** A request the stand-in served, with when it arrived (`performance.now()`). */
interface Seen extends Request {
  readonly at: number;
}

/** The assignment a case's `task` call hands its subagent, which tells its requests apart. */
const ASSIGNMENT = "SUBAGENT-ASSIGNMENT: report what you were told.";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Whether a Messages request is one of a compaction's summary calls, sent under their own system prompt. */
function isSummarization(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(
    "Summarize user–AI coding-assistant conversations"
  );
}

/** Whether a Messages request is a turn of a conversation, the parent's or a subagent's: one that
 *  offers the agent's tools. The binary's side calls (a compaction summary, a subagent's label, a
 *  skill's routing hint) offer none. */
function isTurn(request: Request): boolean {
  return (
    Array.isArray(request.body.tools) && request.body.tools.length > 0 && !isSummarization(request)
  );
}

/** Whether a turn is the subagent's: it carries the assignment and none of the parent's conversation. */
function isSubagentTurn(request: Request): boolean {
  const messages = JSON.stringify(request.body.messages);
  return messages.includes(ASSIGNMENT) && !messages.includes("First question.");
}

function messagesOf(request: Request): unknown[] {
  return Array.isArray(request.body.messages) ? request.body.messages : [];
}

/** The text of a message as the binary sent it: string content, or its text blocks joined. */
function textOf(message: unknown): string {
  if (typeof message !== "object" || message === null || !("content" in message)) return "";
  const { content } = message;
  if (typeof content === "string") return content;
  return (Array.isArray(content) ? content : [])
    .map((block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "text" in block &&
      typeof block.text === "string"
        ? block.text
        : ""
    )
    .join("");
}

/** The request's last message, which must be the list as a user message of its own with no cache
 *  anchor, and its text. */
function listOf(request: Request): string {
  const last = messagesOf(request).at(-1);
  expect(last).toMatchObject({ role: "user" });
  expect(JSON.stringify(last)).not.toContain("cache_control");
  const text = textOf(last);
  expect(isSessionAnswersText(text)).toBe(true);
  return text;
}

/** Whether any message of the request is a list or the failure line. */
function carriesList(request: Request): boolean {
  return messagesOf(request).some((message) => isSessionAnswersText(textOf(message)));
}

/** A cached list's read time, from its header's second line. */
function cachedAt(text: string): string {
  const match = /^As read at (\S+); Dispatch did not answer now\.$/m.exec(text);
  if (match?.[1] === undefined) throw new Error(`not a cached list:\n${text}`);
  return match[1];
}

const MINUTE = 60_000;

/** A person's answer to the session's ask, written `minutesAgo` minutes ago. */
function answer(id: string, text: string, minutesAgo: number): SessionAnswer {
  return {
    kind: "answer",
    relation: "answered_your_ask",
    id,
    at: new Date(Date.now() - minutesAgo * MINUTE).toISOString(),
    author: { kind: "user", id: "alice" },
    text,
    selected: ["The second way"],
    ask: { id, question: "Which way should the fix go?" },
    owner: { issue: { key: "LEGION-1", title: "The work" } },
    ref: `dispatch://LEGION-1/ask/${id}`,
    href: `/issues/LEGION-1#ask-${id}`,
  };
}

/** The answers request whose read a Messages request waited on: the last one before it. */
function answersBefore(seen: readonly Seen[], request: Seen): Seen {
  const before = seen.filter((other) => other.path.endsWith("/answers") && other.at <= request.at);
  const last = before.at(-1);
  if (last === undefined) throw new Error("no answers request came before the Messages request");
  return last;
}

/**
 * Runs an ordinary session (the Envoy extension from this checkout, Dispatch configured, no Legion
 * environment) through `steps` on the real Oh My Pi. Each turn is answered by `reply`, or `Answer
 * N.`; each compaction summary call with a fixed summary; every other side call with a label.
 * `reply` may change the answers route as the stand-in answers a turn. With `warm`, the profile's
 * model catalog is built before the session starts. Once the steps have settled, `until` is
 * awaited before omp's stdin closes. Returns every request the stand-in served, in order of
 * arrival.
 */
async function runSession(
  binary: string,
  steps: readonly Step[],
  options: {
    readonly reply?: (request: Request, setRoute: (route: Route) => void) => readonly Block[];
    readonly config?: readonly string[];
    readonly until?: Promise<unknown>;
    readonly warm?: boolean;
  } = {}
): Promise<{
  readonly seen: Seen[];
  readonly turns: Seen[];
  readonly answers: Seen[];
  readonly summary: string;
}> {
  const { home, workspace, sessions } = await ompRoot(binary, "session-answers-omp-", cleanup);
  const summary = "SUMMARY-OF-THE-EARLIER-TURNS";
  let route: Route = { kind: "list", items: [] };
  const setRoute = (next: Route) => {
    route = next;
  };
  // A route that never answers holds its request until the case ends, and lets go of it before
  // the stand-in stops (cleanup runs last first), which waits on every open request.
  const release = Promise.withResolvers<void>();

  const seen: Seen[] = [];
  let answered = 0;
  const { base } = serveStandin(cleanup, (url, body) => {
    const request: Seen = { path: url.pathname, body, at: performance.now() };
    seen.push(request);
    if (url.pathname === "/anthropic/v1/messages") {
      answered += 1;
      const blocks: readonly Block[] = isSummarization(request)
        ? [{ type: "text", text: summary }]
        : !isTurn(request)
          ? [{ type: "text", text: "Label." }]
          : (options.reply?.(request, setRoute) ?? [{ type: "text", text: `Answer ${answered}.` }]);
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation or one of the binary's side calls.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    const answers = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
    if (answers !== null) {
      const current = route;
      if (current.kind === "hang") {
        return release.promise.then(() => new Response("released", { status: 503 }));
      }
      if (current.kind === "missing") {
        // What a Dispatch build without the route answers (routes/router.go's fallback).
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for GET ${url.pathname}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        );
      }
      return Response.json({
        session_id: decodeURIComponent(answers[1] ?? ""),
        since: new Date(Date.now() - 72 * 60 * MINUTE).toISOString(),
        total: current.items.length,
        items: current.items,
      });
    }
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "session-answers-machine",
      dir: workspace,
      topics: [],
    });
  });
  cleanup.push(async () => release.resolve());
  await writeStandinProfile(home, base, [
    "compaction:",
    // Keep only the newest turn, so a short conversation still has something to summarize.
    "  keepRecentTokens: 1",
    "  methodOrder: [soft]",
    ...(options.config ?? []),
  ]);
  if (options.warm === true) {
    // A fresh profile has no model catalog, so the binary builds one beside its first turn, and
    // that work holds its event loop for one to two seconds (measured with --cpu-prof: the model
    // registry's refresh), whatever any extension does: a request waits on it as long with an
    // answers route that answers at once. A pane's profile has the catalog already. Building it
    // first keeps that host cost out of a case that times the answers read.
    const built = Bun.spawn([binary, "models", "refresh"], {
      cwd: workspace,
      env: { HOME: home, PATH: "/usr/local/bin:/usr/bin:/bin" },
      stdout: "ignore",
      stderr: "pipe",
    });
    if ((await built.exited) !== 0) {
      throw new Error(`omp models refresh failed:\n${await new Response(built.stderr).text()}`);
    }
  }

  // A prompt has settled at its terminal agent_end, a compaction at its RPC response.
  let settle: ((frame: object) => boolean) | undefined;
  let settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions: [path.join(import.meta.dir, "envoy.ts")],
      home,
      workspace,
      sessions,
      env: { ENVOY_URL: base, DISPATCH_URL: base, DISPATCH_TOKEN: "session-answers-token" },
      onFrame: (frame) => {
        if (settle?.(frame)) {
          settle = undefined;
          settled.resolve();
        }
      },
    },
    cleanup
  );
  for (const step of steps) {
    if ("route" in step) {
      route = step.route;
      continue;
    }
    settled = Promise.withResolvers<void>();
    let compactError: unknown;
    settle =
      "compact" in step
        ? (frame) => {
            if (!("type" in frame && frame.type === "response")) return false;
            if (!("command" in frame && frame.command === "compact")) return false;
            if (!("success" in frame && frame.success === true)) {
              compactError = "error" in frame ? frame.error : "no error given";
            }
            return true;
          }
        : (frame) =>
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false);
    rpc.send("compact" in step ? { type: "compact" } : { type: "prompt", message: step.prompt });
    await Promise.race([settled.promise, rpc.closed]);
    if (compactError !== undefined) throw new Error(`omp refused the compaction: ${compactError}`);
  }
  if (options.until !== undefined) await Promise.race([options.until, rpc.closed]);
  await rpc.end();

  return {
    seen,
    turns: seen.filter((request) => request.path === "/anthropic/v1/messages" && isTurn(request)),
    answers: seen.filter((request) => request.path.endsWith("/answers")),
    summary,
  };
}

/** A reply that answers the turn after `prompt` with a `task` call handing `ASSIGNMENT` to one
 *  subagent, calling `onTask` as it does; resolves `subagent` when the subagent's first turn
 *  arrives. Every other turn is answered `Done.`. */
function delegating(prompt: string, onTask: (setRoute: (route: Route) => void) => void = () => {}) {
  const subagent = Promise.withResolvers<void>();
  let delegated = false;
  const reply = (request: Request, setRoute: (route: Route) => void): readonly Block[] => {
    if (isSubagentTurn(request)) {
      subagent.resolve();
      return [{ type: "text", text: "Done." }];
    }
    if (!delegated && JSON.stringify(messagesOf(request)).includes(prompt)) {
      delegated = true;
      onTask(setRoute);
      return [
        {
          type: "tool_use",
          name: "task",
          input: {
            i: "Delegating the report",
            context: "A one-step check.",
            tasks: [{ name: "Reporter", task: ASSIGNMENT, solutionSpace: "Reply in one line." }],
          },
        },
      ];
    }
    return [{ type: "text", text: "Done." }];
  };
  return { reply, subagent: subagent.promise };
}

const skipped = omp === undefined && !onActions;
const binary = (): string => {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
};

test.skipIf(skipped)(
  "(a) a person's answer that appears after turn 1 is in turn 2, word for word",
  async () => {
    const item = answer("ask_a", 'Go the *second* way.\nKeep `retry` at 3, and say "done".', 2);
    const session = await runSession(binary(), [
      { prompt: "First question." },
      { route: { kind: "list", items: [item] } },
      { prompt: "Second question." },
    ]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(carriesList(first as Seen)).toBe(false);
    // Compared after JSON parsing: the text the model reads, not its escaped bytes.
    const list = listOf(second as Seen);
    expect(list).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(list).toContain(item.text);
  },
  180_000
);

test.skipIf(skipped)(
  "(b) an answer that appears after a manual compaction is in the next turn",
  async () => {
    const item = answer("ask_b", "Compaction or not, ship it.", 1);
    const session = await runSession(binary(), [
      { prompt: "First question." },
      { prompt: "Second question." },
      { compact: true },
      { route: { kind: "list", items: [item] } },
      { prompt: "Third question." },
    ]);

    const [first, second, third, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(carriesList(first as Seen)).toBe(false);
    expect(carriesList(second as Seen)).toBe(false);
    // The third turn is sent over the compacted history, with the list as its last message.
    const third_ = JSON.stringify(messagesOf(third as Seen));
    expect(third_).not.toContain("First question.");
    expect(third_).toContain(session.summary);
    expect(listOf(third as Seen)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

test.skipIf(skipped)(
  "(c) a subagent started after the answer carries it in its first request, read under its parent's id",
  async () => {
    const item = answer("ask_c", "The subagent should hear this too.", 1);
    const { reply, subagent } = delegating("Delegate it.");
    const session = await runSession(
      binary(),
      [
        { prompt: "First question." },
        { route: { kind: "list", items: [item] } },
        { prompt: "Delegate it." },
      ],
      { reply, until: subagent }
    );

    const parentID = session.answers[0]?.path.split("/")[4];
    expect(parentID).toBeString();
    const first = session.turns.find(isSubagentTurn);
    if (first === undefined) throw new Error("the subagent sent no Messages request");
    expect(listOf(first)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    // Every read, the subagent's included, asked for the parent's list.
    expect(new Set(session.answers.map((request) => request.path))).toEqual(
      new Set([`/api/v1/agents/${parentID}/answers`])
    );
    expect(answersBefore(session.seen, first).path).toBe(`/api/v1/agents/${parentID}/answers`);
  },
  180_000
);

test.skipIf(skipped)(
  "(d) an answer, a reply to its comment, a comment on its document and a message to it are each in the list word for word",
  async () => {
    const items: SessionAnswer[] = [
      answer("ask_d", "Answer: use the **new** schema.", 1),
      {
        kind: "comment",
        relation: "replied_in_a_comment_thread",
        id: "comment_reply",
        at: new Date(Date.now() - 2 * MINUTE).toISOString(),
        author: { kind: "user", id: "bob" },
        text: "Reply: that paragraph is wrong;\n- fix the date\n- keep the link",
        owner: { issue: { key: "LEGION-1", title: "The work" } },
        ref: "dispatch://LEGION-1/comment/comment_reply",
        href: "/issues/LEGION-1#comment-comment_reply",
      },
      {
        kind: "comment",
        relation: "commented_on_a_document",
        id: "comment_doc",
        at: new Date(Date.now() - 3 * MINUTE).toISOString(),
        author: { kind: "user", id: "carol" },
        text: "Document comment: the table needs a total row.",
        owner: { document: { project: "LEGION", slug: "runbook", name: "Runbook" } },
        ref: "dispatch://LEGION/artifact/runbook/comment/comment_doc",
        href: "/projects/LEGION/documents/runbook#comment-comment_doc",
      },
      {
        kind: "message",
        relation: "sent_you_a_message",
        id: "message_1",
        at: new Date(Date.now() - 4 * MINUTE).toISOString(),
        author: { kind: "user", id: "dave" },
        text: "Message: pause after the migration — I'll check the numbers.",
        owner: null,
        ref: null,
        href: "/agents/session/live",
      },
    ];
    const session = await runSession(binary(), [
      { route: { kind: "list", items } },
      { prompt: "First question." },
    ]);

    const [first, ...rest] = session.turns;
    expect(rest).toEqual([]);
    const list = listOf(first as Seen);
    expect(list).toBe(renderSessionAnswers(items, { now: Date.now() }));
    for (const item of items) expect(list).toContain(item.text);
  },
  180_000
);

test.skipIf(skipped)(
  "(e) twelve items past 8,000 characters: the newest prefix and the left-out line with its count",
  async () => {
    const items = Array.from({ length: 12 }, (_, index) =>
      answer(
        `ask_e${index}`,
        `Item ${index}: ${"word ".repeat(180)}end of item ${index}.`,
        index + 1
      )
    );
    expect(items.reduce((sum, item) => sum + item.text.length, 0)).toBeGreaterThan(8_000);
    const session = await runSession(binary(), [
      { route: { kind: "list", items } },
      { prompt: "First question." },
    ]);

    const list = listOf(session.turns[0] as Seen);
    expect(list.length).toBeLessThanOrEqual(8_000);
    // Shown items are the newest, a strict prefix: the first left out ends the list.
    const shown = items.map((item) => list.includes(item.text));
    const count = shown.indexOf(false);
    expect(count).toBeGreaterThan(0);
    expect(shown.slice(count).every((included) => !included)).toBe(true);
    const leftOut = items.length - count;
    expect(
      list.endsWith(
        `\n\n${leftOut} older items left out to stay under 8,000 characters: run \`dispatch answers\` to read every one in full.`
      )
    ).toBe(true);
    expect(list).toBe(renderSessionAnswers(items, { now: Date.now() }));
  },
  180_000
);

test.skipIf(skipped)(
  "(f) an answers route that never answers holds no request for a second: turn 1, turn 2 after a good read, and a subagent's first request",
  async () => {
    const deadline = `Dispatch did not answer within ${SESSION_ANSWERS_DEADLINE_MS} ms`;
    const waited = (session: { readonly seen: Seen[] }, request: Seen, label: string): void => {
      const ms = request.at - answersBefore(session.seen, request).at;
      console.log(
        `(f) ${label}: the Messages request came ${ms.toFixed(0)} ms after the answers request`
      );
      expect(ms).toBeLessThan(1_000);
    };

    // Turn 1, with no list ever read: the failure line.
    const cold = await runSession(
      binary(),
      [{ route: { kind: "hang" } }, { prompt: "First question." }],
      { warm: true }
    );
    const [coldFirst] = cold.turns;
    if (coldFirst === undefined) throw new Error("no turn 1");
    waited(cold, coldFirst, "turn 1 with no cache");
    expect(listOf(coldFirst)).toBe(sessionAnswersFailureLine(deadline));

    // Turn 2 after a good read: the list read in turn 1, with its read time.
    const item = answer("ask_f", "Read before Dispatch hung.", 1);
    const cached = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { route: { kind: "hang" } },
        { prompt: "Second question." },
      ],
      { warm: true }
    );
    const [cachedFirst, cachedSecond] = cached.turns;
    if (cachedFirst === undefined || cachedSecond === undefined) throw new Error("no turn 2");
    expect(listOf(cachedFirst)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    waited(cached, cachedSecond, "turn 2 after a good read");
    const earlier = listOf(cachedSecond);
    expect(earlier).toBe(
      renderSessionAnswers([item], { now: Date.now(), cachedAt: cachedAt(earlier) })
    );

    // The subagent's first request: Dispatch hangs from the parent's delegating turn on, so the
    // subagent's read is the first to hang. `task` runs in the foreground here, so no read of the
    // parent's is in flight when the subagent's starts.
    const { reply, subagent } = delegating("Delegate it.", (setRoute) =>
      setRoute({ kind: "hang" })
    );
    const sub = await runSession(
      binary(),
      [{ route: { kind: "list", items: [item] } }, { prompt: "Delegate it." }],
      { reply, until: subagent, config: ["async:", "  enabled: false"], warm: true }
    );
    const subFirst = sub.turns.find(isSubagentTurn);
    if (subFirst === undefined) throw new Error("the subagent sent no Messages request");
    expect(sub.answers.length).toBeGreaterThanOrEqual(2);
    expect(answersBefore(sub.seen, subFirst)).toBe(sub.answers[1] as Seen);
    waited(sub, subFirst, "the subagent's first request");
    const carried = listOf(subFirst);
    expect(carried).toBe(
      renderSessionAnswers([item], { now: Date.now(), cachedAt: cachedAt(carried) })
    );
  },
  300_000
);

test.skipIf(skipped)(
  "(g) a Dispatch with no answers route: the failure line, and no answers request on the next turn",
  async () => {
    const session = await runSession(binary(), [
      { route: { kind: "missing" } },
      { prompt: "First question." },
      { prompt: "Second question." },
    ]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    const failure = sessionAnswersFailureLine(NO_ANSWERS_ROUTE_REASON);
    expect(listOf(first as Seen)).toBe(failure);
    expect(listOf(second as Seen)).toBe(failure);
    expect(session.answers).toHaveLength(1);
    expect((session.answers[0] as Seen).at).toBeLessThan((first as Seen).at);
  },
  180_000
);

test.skipIf(skipped)(
  "(h) a change in the list leaves the cached prefix byte for byte, and the list carries no cache anchor",
  async () => {
    const older = answer("ask_h1", "The first answer.", 3);
    const newer = answer("ask_h2", "A second answer, after turn 1.", 1);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [older] } },
      { prompt: "First question." },
      { route: { kind: "list", items: [newer, older] } },
      { prompt: "Second question." },
    ]);

    const [n, next, ...rest] = session.turns;
    if (n === undefined || next === undefined) throw new Error("want two turns");
    expect(rest).toEqual([]);
    // Each list is the last message, with no cache_control (listOf), and they differ.
    expect(listOf(n)).toBe(renderSessionAnswers([older], { now: Date.now() }));
    expect(listOf(next)).toBe(renderSessionAnswers([newer, older], { now: Date.now() }));
    // N's messages through its last cache_control-bearing one are N+1's first, byte for byte.
    const messages = messagesOf(n);
    const anchor = messages.findLastIndex((message) =>
      JSON.stringify(message).includes("cache_control")
    );
    expect(anchor).toBeGreaterThanOrEqual(0);
    expect(anchor).toBeLessThan(messages.length - 1);
    expect(JSON.stringify(messagesOf(next).slice(0, anchor + 1))).toBe(
      JSON.stringify(messages.slice(0, anchor + 1))
    );
    console.log(
      `(h) request N+1's last two messages: ${JSON.stringify(messagesOf(next).slice(-2))}`
    );
  },
  180_000
);
