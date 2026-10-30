import { afterEach, expect, test } from "bun:test";
import { appendFile, symlink, writeFile } from "node:fs/promises";
import * as path from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
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

// What people wrote to a session, on every request of the real Oh My Pi's main loop
// (src/session-answers-context.ts), and on none of the other requests its
// `context` handler runs for: a side turn, the handoff prompt and a remote compaction's request.
// Oh My Pi keeps nothing a `context` handler returns, so what reaches the model, where the list
// sits and where the binary puts its cache anchors can only be read off the requests it sends. One
// stand-in is the model gateway, the Envoy listener and Dispatch, whose answers route serves a list
// each case controls. LEGION_TEST_OMP names the binary, as in dispatch-first-omp.test.ts. A run
// without one skips, except on GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** What the stand-in's answers route does: list `items` (after `delayMs`, at once when unset),
 *  accept and never answer, or answer as a Dispatch build older than the route does. */
type Route =
  | {
      readonly kind: "list";
      readonly items: readonly SessionAnswer[];
      readonly delayMs?: number;
    }
  | { readonly kind: "hang" }
  | { readonly kind: "missing" };

/** One thing the session is told over RPC, in order, or a change in the answers route before the
 *  next one: a prompt, a manual compaction, a `/btw` side question, a handoff, or an Envoy
 *  delivery (`DELIVERER`) that starts a turn or, quiet, only joins the conversation. */
type Step =
  | { readonly prompt: string }
  | { readonly compact: true }
  | { readonly btw: string }
  | { readonly handoff: true }
  | { readonly deliver: string; readonly quiet?: true }
  | { readonly route: Route };

/**
 * An extension standing in for an Envoy delivery, which the stand-in's Envoy listener cannot
 * send (pi-envoy reads deliveries off NATS): `/deliver <text>` sends an `envoy-message` as
 * `extensions/envoy.ts` sends a delivery, `deliverAs: "steer"` and `triggerTurn: true`, and
 * `/deliver-quiet <text>` the same with no turn. The host queues a send and `sendMessage`
 * returns nothing to await, so the quiet command polls for the condition itself: it returns once
 * the branch holds the message.
 */
const DELIVERER = `export default function deliverer(pi) {
  for (const [name, triggerTurn] of [["deliver", true], ["deliver-quiet", false]]) {
    pi.registerCommand(name, {
      description: "Send an envoy-message",
      handler: async (args, context) => {
        pi.sendMessage(
          { customType: "envoy-message", content: args, display: true },
          { deliverAs: "steer", triggerTurn }
        );
        while (
          !triggerTurn &&
          !context.sessionManager
            .getBranch()
            .some((entry) => entry.type === "custom_message" && JSON.stringify(entry).includes(args))
        ) {
          await Bun.sleep(10);
        }
      },
    });
  }
}
`;

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

/** Whether a Messages request is a `/btw` side turn: one of its messages is the /btw prompt, which
 *  never enters the conversation. Not only the last, so a list put after it fails the case's
 *  assertions rather than its wait for the side turn. */
function isSideTurn(request: Request): boolean {
  return messagesOf(request).some((message) => textOf(message).includes("<btw>"));
}

/** Whether a Messages request is Anthropic remote compaction's: it carries `compaction`. */
function isRemoteCompaction(request: Request): boolean {
  return "compaction" in request.body;
}

/** Whether a Messages request is the handoff's: one of its messages is the host's handoff prompt,
 *  which never enters the conversation. */
function isHandoff(request: Request): boolean {
  return messagesOf(request).some((message) =>
    textOf(message).includes("Write a handoff document for another instance of yourself.")
  );
}

/** Whether a Messages request is a turn of the main loop: a turn that is none of the above. */
function isMainTurn(request: Request): boolean {
  return (
    isTurn(request) && !isSideTurn(request) && !isRemoteCompaction(request) && !isHandoff(request)
  );
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

/** A cached list's read time and the reason its header gives, from the header's second line. */
function cachedOf(text: string): { readonly readAt: string; readonly reason: string } {
  const match = /^As read at (\S+); this read failed: (.+)\.$/m.exec(text);
  if (match?.[1] === undefined || match[2] === undefined) {
    throw new Error(`not a cached list:\n${text}`);
  }
  return { readAt: match[1], reason: match[2] };
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
 * `reply` may change the answers route as the stand-in answers a turn. `methodOrder` is the
 * compaction's (`[soft]` by default) and `model` lines go on the stand-in model's entry in
 * models.yml. With `twoCopies`, omp loads the extension twice: from this checkout and through a
 * symlink to it, which it takes for another extension. With `warm`, the profile's model catalog is
 * built before the session starts. Once the steps have settled, `until` is awaited before omp's
 * stdin closes. Returns every request the stand-in served, in order of arrival, the main loop's
 * turns among them, and when each step was sent (`performance.now()`, route changes left out).
 */
async function runSession(
  binary: string,
  steps: readonly Step[],
  options: {
    readonly reply?: (request: Request, setRoute: (route: Route) => void) => readonly Block[];
    readonly config?: readonly string[];
    readonly methodOrder?: string;
    readonly model?: readonly string[];
    readonly twoCopies?: boolean;
    readonly until?: Promise<unknown>;
    readonly warm?: boolean;
  } = {}
): Promise<{
  readonly seen: Seen[];
  readonly turns: Seen[];
  readonly answers: Seen[];
  readonly sentAt: number[];
  readonly summary: string;
}> {
  const { home, workspace, sessions, root } = await ompRoot(
    binary,
    "session-answers-omp-",
    cleanup
  );
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
  // A /btw step has settled once the stand-in serves its side turn: omp answers the RPC command
  // before it sends the side turn's request.
  let sideTurnServed = Promise.withResolvers<void>();
  const { base } = serveStandin(cleanup, async (url, body) => {
    const request: Seen = { path: url.pathname, body, at: performance.now() };
    seen.push(request);
    if (url.pathname === "/anthropic/v1/messages") {
      answered += 1;
      if (isSideTurn(request)) sideTurnServed.resolve();
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
      // A slow Dispatch answers after a real delay: the case times the request against the read's
      // own deadline, a platform timer inside the binary's process.
      if (current.delayMs !== undefined) await Bun.sleep(current.delayMs);
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
    `  methodOrder: ${options.methodOrder ?? "[soft]"}`,
    ...(options.config ?? []),
  ]);
  if (options.model !== undefined) {
    // The stand-in model is the last entry models.yml lists.
    await appendFile(
      path.join(home, ".omp", "agent", "models.yml"),
      options.model.map((line) => `        ${line}\n`).join("")
    );
  }
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
  const extension = path.join(import.meta.dir, "envoy.ts");
  const extensions = [extension];
  if (options.twoCopies === true) {
    const copy = path.join(root, "envoy-copy.ts");
    await symlink(extension, copy);
    extensions.push(copy);
  }
  if (steps.some((step) => "deliver" in step)) {
    const deliverer = path.join(root, "deliverer.ts");
    await writeFile(deliverer, DELIVERER);
    extensions.push(deliverer);
  }

  // A prompt and a delivery have settled at their terminal agent_end; a compaction, a handoff and
  // a quiet delivery (an RPC prompt that runs a command) at their RPC response; a /btw once its
  // side turn was served.
  let settle: ((frame: object) => boolean) | undefined;
  let settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions,
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
  const sentAt: number[] = [];
  for (const step of steps) {
    if ("route" in step) {
      route = step.route;
      continue;
    }
    if ("btw" in step) {
      sideTurnServed = Promise.withResolvers<void>();
      sentAt.push(performance.now());
      rpc.send({ type: "btw", question: step.btw });
      await Promise.race([sideTurnServed.promise, rpc.closed]);
      continue;
    }
    settled = Promise.withResolvers<void>();
    const command =
      "compact" in step
        ? "compact"
        : "handoff" in step
          ? "handoff"
          : "deliver" in step && step.quiet === true
            ? "prompt"
            : undefined;
    let commandError: unknown;
    settle =
      command !== undefined
        ? (frame) => {
            if (!("type" in frame && frame.type === "response")) return false;
            if (!("command" in frame && frame.command === command)) return false;
            if (!("success" in frame && frame.success === true)) {
              commandError = "error" in frame ? frame.error : "no error given";
            }
            return true;
          }
        : (frame) =>
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false);
    sentAt.push(performance.now());
    rpc.send(
      "prompt" in step
        ? { type: "prompt", message: step.prompt }
        : "deliver" in step
          ? {
              type: "prompt",
              message: `/${step.quiet === true ? "deliver-quiet" : "deliver"} ${step.deliver}`,
            }
          : { type: command }
    );
    await Promise.race([settled.promise, rpc.closed]);
    if (commandError !== undefined) throw new Error(`omp refused the ${command}: ${commandError}`);
  }
  if (options.until !== undefined) await Promise.race([options.until, rpc.closed]);
  await rpc.end();

  return {
    seen,
    turns: seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isMainTurn(request)
    ),
    answers: seen.filter((request) => request.path.endsWith("/answers")),
    sentAt,
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
    expect(list.length).toBeLessThan(8_000);
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
    const earlierCache = cachedOf(earlier);
    expect(earlierCache.reason).toBe(deadline);
    expect(earlier).toBe(renderSessionAnswers([item], { now: Date.now(), cached: earlierCache }));

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
    const carriedCache = cachedOf(carried);
    expect(carriedCache.reason).toBe(deadline);
    expect(carried).toBe(renderSessionAnswers([item], { now: Date.now(), cached: carriedCache }));
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

/** A request's messages as role and text, cache anchors aside. */
function shapeOf(messages: readonly unknown[]): [unknown, string][] {
  return messages.map((message) => [
    typeof message === "object" && message !== null && "role" in message ? message.role : null,
    textOf(message),
  ]);
}

// Oh My Pi runs a `context` handler on every conversion of messages for a model, not only on the
// main loop's requests. A side turn (`AgentSession.runEphemeralTurn`, which a Dispatch BTW, the
// stop-time self-check and `/btw` all reach) converts
// `[...this.messages, {role: "developer", content: [{type: "text", text: ND}], attribution:
// "agent"}, ...history, {role: "user", content: [{type: "text", text: promptText}], attribution:
// "agent"}]` (`#jr`), and must end with its prompt.
test.skipIf(skipped)(
  "(i) a /btw side turn keeps its prompt last, carries no list and starts no read; the turns around it carry the list",
  async () => {
    const item = answer("ask_i", "Side turns stay as they were.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        // A read for the side turn would hang here, and hold its request for the whole deadline.
        { route: { kind: "hang" } },
        { btw: "What is up?" },
        { route: { kind: "list", items: [item] } },
        { prompt: "Second question." },
      ],
      { warm: true }
    );

    const sides = session.seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isSideTurn(request)
    );
    expect(sides).toHaveLength(1);
    const side = sides[0] as Seen;
    const messages = messagesOf(side);
    expect(textOf(messages.at(-1))).toContain("Question:\nWhat is up?\n</btw>");
    expect(textOf(messages.at(-2))).toContain("Ephemeral side-channel turn");
    expect(carriesList(side)).toBe(false);
    // The dispatch-first skill is on it as on every request.
    expect(JSON.stringify(messages)).toContain(DISPATCH_FIRST_MARKER);
    // No answers request between the /btw and its side turn: it read nothing and waited on nothing.
    const btwSent = session.sentAt[1] as number;
    expect(session.answers.filter((read) => read.at >= btwSent && read.at <= side.at)).toEqual([]);
    console.log(
      `(i) the side turn's request came ${(side.at - btwSent).toFixed(0)} ms after the /btw command`
    );
    expect(side.at - btwSent).toBeLessThan(SESSION_ANSWERS_DEADLINE_MS);

    // The main loop's turns on both sides carry the list, read fresh: no read was left hanging.
    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(listOf(first as Seen)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(second as Seen)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

// The handoff converts `[...this.#e.agent.state.messages, {role: "user", content: [{type: "text",
// text: b}], attribution: "agent", ...}]`, `b` the host's handoff prompt, and must end with it.
test.skipIf(skipped)(
  "(j) the handoff prompt stays last and carries no list; the turn after the handoff carries it",
  async () => {
    const item = answer("ask_j", "Hand it over as it is.", 1);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [item] } },
      { prompt: "First question." },
      { handoff: true },
      { prompt: "Second question." },
    ]);

    const handoffs = session.seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isHandoff(request)
    );
    expect(handoffs).toHaveLength(1);
    const handoff = handoffs[0] as Seen;
    expect(textOf(messagesOf(handoff).at(-1))).toStartWith(
      "<critical>\nWrite a handoff document for another instance of yourself."
    );
    expect(carriesList(handoff)).toBe(false);
    const answersBetween = session.answers.filter(
      (read) => read.at >= (session.sentAt[1] as number) && read.at <= handoff.at
    );
    expect(answersBetween).toEqual([]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(listOf(first as Seen)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(second as Seen)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

// Anthropic remote compaction (`buildLiveProviderContext`) converts the conversation up to its cut,
// counts the result, and keeps that many messages of the whole conversation's conversion:
//   const ve = (await this.convertMessagesToLlm(Se, ce, pe)).length;
//   const Re = await this.agent.buildSideRequestContext(await this.convertMessagesToLlm([...Se, ...le], ce, pe));
//   ... return { ...Re, messages: Re.messages.slice(0, Le) };
// A list on the first conversion would make the request keep the first message past the cut. The
// binary runs it for a model with `compat.supportsServerCompaction` that it takes for a
// first-party endpoint (`i4`): `if (e.compat.bedrockMessagesApi === true && (jre(r) || r ===
// _f(e.baseUrl))) return true;`, so the stand-in model sets both, and its compaction request is
// the Messages request that carries `compaction`. The stand-in answers it with no compaction
// block, so the binary goes on to summarize locally, as on a failed remote compaction.
test.skipIf(skipped)(
  "(k) a remote compaction's request ends at its cut with no list, and the turn after it carries the list",
  async () => {
    const item = answer("ask_k", "Compact it remotely.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { prompt: "Second question." },
        { prompt: "Third question." },
        { compact: true },
        { prompt: "Fourth question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [, , third, fourth, ...rest] = session.turns;
    if (third === undefined || fourth === undefined) throw new Error("want four turns");
    expect(rest).toEqual([]);
    // The cut falls between the third question and its answer: the compaction request is the third
    // turn's request without its list, message for message, and carries none.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toBe("Third question.");
    expect(shapeOf(compacted)).toEqual(shapeOf(messagesOf(third).slice(0, -1)));
    expect(carriesList(compaction)).toBe(false);
    expect(listOf(third)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(fourth)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

// The same compaction with its cut right after a delivered message, which is what every Envoy
// delivery with `triggerTurn` is: the branch holds it as a `custom_message` entry, and the
// compaction's conversion makes of it `{role: "custom", ..., timestamp: new Date(s).getTime()}`
// (`yu`). The cut search (`qge`) cuts where the role changes between assistant and not, so with
// the newest turn kept the cut comes between the delivery and its reply.
test.skipIf(skipped)(
  "(m) a remote compaction cut right after a delivered message ends at it with no list, and keeps no message past it",
  async () => {
    const item = answer("ask_m", "Compact after the delivery.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { prompt: "Second question." },
        { deliver: "ENVOY-DELIVERY: CI finished." },
        { compact: true },
        { prompt: "Fourth question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [, , delivered, fourth, ...rest] = session.turns;
    if (delivered === undefined || fourth === undefined) throw new Error("want four turns");
    expect(rest).toEqual([]);
    // The delivery's turn ends at the delivery, then its list; the compaction request is that
    // turn's request without its list, message for message: nothing of the reply after the cut.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toContain("ENVOY-DELIVERY: CI finished.");
    expect(shapeOf(compacted)).toEqual(shapeOf(messagesOf(delivered).slice(0, -1)));
    expect(carriesList(compaction)).toBe(false);
    expect(listOf(delivered)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(fourth)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

// A remote compaction that keeps no tail: two delivered messages with no turn after a turn, the
// newest kept, and no role change from assistant to not past the cut, so `qge` returns the whole
// conversation's length and `buildLiveProviderContext` gets an empty tail. Its two conversions,
// `convert(Se)` and `convert([...Se, ...[]])`, then hold the same messages, both ending at the
// branch's last message, while the session compacts (`isCompacting`, the manual compaction's
// controller set).
test.skipIf(skipped)(
  "(n) a remote compaction that keeps no tail carries no list, and the turn after it carries the list",
  async () => {
    const item = answer("ask_n", "Compact it all.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { deliver: "QUIET-ONE: a notice.", quiet: true },
        { deliver: "QUIET-TWO: another notice.", quiet: true },
        { compact: true },
        { prompt: "Second question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [first, second, ...rest] = session.turns;
    if (first === undefined || second === undefined) throw new Error("want two turns");
    expect(rest).toEqual([]);
    // The whole conversation, ending at the newest delivered message, with no list.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toContain("QUIET-TWO: another notice.");
    expect(JSON.stringify(compacted)).toContain("QUIET-ONE: a notice.");
    expect(JSON.stringify(compacted)).toContain("First question.");
    expect(carriesList(compaction)).toBe(false);
    expect(listOf(first)).toBe(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(second)).toBe(renderSessionAnswers([item], { now: Date.now() }));
  },
  180_000
);

// Two copies of this extension (a checkout's and an installed one, say) each run their own
// `context` handler on one request. When the first reads an empty list it appends nothing, so the
// second must still see that the request was decided, or it reads Dispatch again and the request
// waits on both reads.
test.skipIf(skipped)(
  "(l) two copies and an empty list: one answers request, and the request waits under a second",
  async () => {
    const session = await runSession(
      binary(),
      [{ route: { kind: "list", items: [], delayMs: 600 } }, { prompt: "First question." }],
      { twoCopies: true, warm: true }
    );

    // Both copies ran: each asked for the open asks as its turn started.
    expect(session.seen.filter((request) => request.path === "/api/v1/asks/open")).toHaveLength(2);
    const [first, ...rest] = session.turns;
    if (first === undefined) throw new Error("no turn 1");
    expect(rest).toEqual([]);
    expect(session.answers).toHaveLength(1);
    const ms = first.at - (session.answers[0] as Seen).at;
    console.log(`(l) the Messages request came ${ms.toFixed(0)} ms after the answers request`);
    expect(ms).toBeLessThan(1_000);
    expect(carriesList(first)).toBe(false);
  },
  180_000
);
