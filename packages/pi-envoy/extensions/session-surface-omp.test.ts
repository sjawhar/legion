import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { realpathSync } from "node:fs";
import { readFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  isSummarization,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { connect, ErrorCode, type NatsConnection, NatsError, StringCodec } from "nats";
import { z } from "zod";
import { SHELL_OUTPUT_MAX } from "../src/typed-shell";

// What the live view's two session-side surfaces do on the real Oh My Pi (dispatch://INFRA-812,
// R3 and R4): the `context` frame the session publishes about how full its context window is
// (extensions/envoy.ts's `refreshAgentStreamContext` over src/agent-stream.ts's `setContext`), and
// a person's `!command`, which the session runs itself before the host is handed the line
// (src/typed-shell.ts, read in `runTypedInput`). Both are read off the wire: the frames over a real
// NATS server, as the Dispatch relay reads them, and the reply over the stand-in Dispatch. What
// reached the model is read off the requests the binary sends the stand-in gateway, each held
// until the test answers it.
// LEGION_TEST_OMP names the binary, as in card-delivery-omp.test.ts, and LEGION_TEST_NATS_URL the
// NATS server. A run without both skips, except on GitHub Actions, where the pi-envoy job
// supplies both.
// This file runs in a Bun process of its own (the package's `test` script): envoy.test.ts's
// `mock.module("nats", …)` replaces `nats` for the whole process, and a file loaded after it gets
// the mock's `connect`, which the check below turns into one clear failure.
const omp = process.env.LEGION_TEST_OMP;
const natsUrl = process.env.LEGION_TEST_NATS_URL;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = (omp === undefined || natsUrl === undefined) && !onActions;

/** The binary and NATS server a case runs against; throws on GitHub Actions without them. */
function setup(): { readonly binary: string; readonly nats: string } {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  if (natsUrl === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
  return { binary: omp, nats: natsUrl };
}

// The real `connect`'s connection reports the server it reached (`getServer`); the mock's has no
// such method. Checked once, before any case spends a binary's startup on a connection that
// reaches no server.
beforeAll(async () => {
  if (skip) return;
  const probe = await connect({ servers: setup().nats });
  try {
    if (typeof probe.getServer !== "function") {
      throw new Error(
        "`nats` here is envoy.test.ts's mock: run this file in its own process with `bun run test`"
      );
    }
  } finally {
    await probe.close();
  }
});

/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 30_000;

/** How often one session may put its context usage on the wire in these cases, the interval the
 *  check names (`ENVOY_CONTEXT_FRAME_INTERVAL_MS`). */
const CONTEXT_INTERVAL_MS = 2_000;

/** How long a `!command` runs in the shell session before it is stopped
 *  (`ENVOY_SHELL_COMMAND_LIMIT_MS`), the limit the check names. */
const SHELL_LIMIT_MS = 2_000;

/** The sessions these cases share are torn down once, at the end: each boot costs the real
 *  binary's startup, and every case of a session leaves it settled for the next one. */
const cleanup: Cleanup = [];
afterAll(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

function text(reply: string): Block {
  return { type: "text", text: reply };
}

/** Every message of a Messages request as JSON, in conversation order. */
function messagesOf(request: Request): string[] {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages.map((message) => JSON.stringify(message));
}

/** A Messages request's conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** One Messages request the stand-in holds until the test answers it. */
interface Turn {
  readonly request: Request;
  /** Its position among every Messages request the session sent, from 0. */
  readonly index: number;
  /** Answers it with `blocks`, reporting `inputTokens` of input usage. */
  reply(blocks: readonly Block[], inputTokens?: number): void;
}

/** The fields of `get_state`'s answer (the pin's `RpcSessionState`) the cases read. */
const RpcStateSchema = z.object({
  isSettled: z.boolean(),
  sessionFile: z.string().optional(),
});

/** One frame the session published on its stream, and when this test read it. */
interface SeenFrame {
  readonly at: number;
  readonly frame: Record<string, unknown>;
}

/** What a person sent the session from Dispatch's Agents page: the message Dispatch stored, and
 *  the frame the listener hands the session. */
interface Sent {
  readonly messageID: string;
  readonly eventID: string;
}

/** A running session with pi-envoy, a real NATS connection to its subjects, and what it sent the
 *  stand-in. */
interface SurfaceSession {
  /** The directory the session runs in, which its `!command` runs in too. */
  readonly workspace: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** Every frame the session published on its agent stream, in arrival order. */
  readonly frames: readonly SeenFrame[];
  /** Every request the stand-in served, in arrival order. */
  readonly requests: readonly Request[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** Sends a person's direct message and resolves once the session has taken the frame: a Send or
   *  an Aside once Dispatch was asked to accept it, any other mode once the session answered or
   *  acted on it. */
  send(
    body: string,
    options?: {
      readonly mode?: "aside" | "btw" | "steer";
      readonly broadcastID?: string;
      readonly accepted?: boolean;
    }
  ): Promise<Sent>;
  /** The reply the session posted on Dispatch for `message`, once it is there. */
  reply(message: Sent, what: string): Promise<string>;
  /** Whether the session has posted a reply for `message` yet. */
  replied(message: Sent): boolean;
  /** Arms this session's publisher as the Dispatch relay does, and keeps it armed. */
  arm(): Promise<void>;
  /** What the session answers a replay request with, as the relay asks for it. */
  replay(): Promise<{ readonly frames: readonly Record<string, unknown>[] }>;
  /** Sends one RPC command and resolves with its response frame. */
  command(command: Record<string, unknown>): Promise<Record<string, unknown>>;
  /** Resolves once `get_state` reports the session settled. */
  settled(): Promise<void>;
  /** The session file's lines, as raw JSON. */
  entries(): Promise<string[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/** Polls `probe` until it returns a value, failing on `what` at the deadline or when omp exits. */
async function until<T>(
  what: string,
  probe: () => T | undefined | Promise<T | undefined>,
  closed: { readonly error?: unknown }
): Promise<T> {
  const deadline = Date.now() + DEADLINE_MS;
  for (;;) {
    if (closed.error !== undefined) throw closed.error;
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // What the cases wait on (a request reaching the stand-in, a frame, the host's state) has no
    // event the test can await, so it is polled, and a missing one fails at the deadline.
    await Bun.sleep(20);
  }
}

/**
 * Starts the real Oh My Pi in RPC mode with only envoy.ts loaded, against one stand-in that is the
 * model gateway, the Envoy listener and Dispatch, and the NATS server `setup` names. `config` lines
 * go into the profile's config.yml and `env` beside the plugin's own variables. Resolves once the
 * session has registered its id with the listener, the test holds a NATS connection, and this
 * test's subscription to the session's stream is live.
 */
async function startSession(
  options: { readonly config?: readonly string[]; readonly env?: Record<string, string> } = {}
): Promise<SurfaceSession> {
  const { config = [], env = {} } = options;
  const { binary, nats } = setup();
  const { home, workspace, sessions } = await ompRoot(binary, "session-surface-omp-", cleanup);

  const turns: Turn[] = [];
  /** The body and mode each published message id carries, which the accept answers with. */
  const stored = new Map<string, { readonly body: string; readonly mode: "aside" | "steer" }>();
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        index,
        reply: (blocks, inputTokens) =>
          answer.resolve(messageStream(blocks, `msg_${index}`, inputTokens)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation, a side turn or a compaction's summary call.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    // The answers route, as a Dispatch answers it with nothing for the session.
    const answers = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
    if (answers !== null) {
      return Response.json({
        session_id: decodeURIComponent(answers[1] ?? ""),
        since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
        total: 0,
        items: [],
      });
    }
    const accept = url.pathname.match(/^\/api\/v1\/messages\/([^/]+)\/deliveries\/(\d+)\/accept$/);
    if (accept !== null) {
      const messageID = accept[1] as string;
      const message = stored.get(messageID);
      if (message === undefined) return Response.json({ code: "NOT_FOUND" }, { status: 404 });
      return Response.json({
        message_id: messageID,
        body: message.body,
        delivery: message.mode,
        accepted_as: "user_turn",
        attempt: Number(accept[2]),
      });
    }
    if (/^\/api\/v1\/messages\/[^/]+\/reply$/.test(url.pathname)) {
      return Response.json({}, { status: 201 });
    }
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "session-surface-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end (an aborted one, a command's own turn) is answered, so the
  // server stops at once.
  cleanup.push(async () => {
    for (const turn of turns) turn.reply([text("Case over.")]);
  });
  await writeStandinProfile(home, base, config);

  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  const closed: { error?: unknown } = {};
  const rpc = spawnRpc(
    binary,
    {
      extensions: [path.join(import.meta.dir, "envoy.ts")],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
        DISPATCH_URL: base,
        DISPATCH_TOKEN: "session-surface-token",
        ...env,
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "response" && "id" in frame) {
          responses.get(String(frame.id))?.resolve(frame as Record<string, unknown>);
        }
      },
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });

  const sid = await until(
    "the session's registration with the listener",
    () => {
      const subscribe = requests.find((request) => request.path === "/v1/interests/subscribe");
      const id = subscribe?.body.session_id;
      return typeof id === "string" && id !== "" ? id : undefined;
    },
    closed
  );
  const connection: NatsConnection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  const subject = `notifications.agent.${sid}`;
  const control = `agentstream.${sid}.control`;

  const frames: SeenFrame[] = [];
  const stream = connection.subscribe(`agentstream.${sid}.frames`);
  void (async () => {
    for await (const message of stream) {
      frames.push({ at: performance.now(), frame: JSON.parse(codec.decode(message.data)) });
    }
  })();

  /** The replay request the relay sends, which arms the publisher and answers what it holds. */
  const askReplay = async (timeout: number): Promise<Record<string, unknown> | undefined> => {
    const asked = JSON.stringify({ type: "replay", v: 1 });
    try {
      const answer = await connection.request(control, codec.encode(asked), { timeout });
      return JSON.parse(codec.decode(answer.data)) as Record<string, unknown>;
    } catch (error) {
      if (error instanceof NatsError && error.code === ErrorCode.NoResponders) return undefined;
      if (error instanceof NatsError && error.code === ErrorCode.Timeout) return undefined;
      throw error;
    }
  };

  let armed = false;
  let taken = 0;
  const session: SurfaceSession = {
    workspace,
    turns,
    frames,
    requests,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken], closed);
      taken += 1;
      return turn;
    },
    async send(body, sendOptions = {}) {
      const { mode = "steer", broadcastID, accepted = mode !== "btw" } = sendOptions;
      const messageID = randomUUID();
      const label = `send-${messageID.slice(0, 8)}`;
      const eventID = `dispatch-${label}`;
      if (mode !== "btw") stored.set(messageID, { body, mode });
      const actor = { id: "alice", kind: "user" };
      const envelope = JSON.stringify({
        dedupe_key: label,
        event_id: eventID,
        issued_at: Date.now(),
        payload: JSON.stringify({
          event: {
            actor,
            issue_key: null,
            payload: {
              author: actor,
              body,
              ...(broadcastID === undefined ? {} : { broadcast_id: broadcastID }),
              created_at: "2026-11-07T00:00:00Z",
              deliveries: [],
              id: messageID,
              in_reply_to: null,
              issue_key: null,
              target: `session:${sid}`,
            },
            type: "message.created",
          },
          delivery: { attempt: 1, mode },
        }),
        payload_summary: body,
        source: "dispatch",
        source_event_id: "1",
        topic: subject,
        trace_id: label,
      });
      // The session opens its agent subject after it registers. A request finds no responder until
      // then, and nothing took the frame, so it is sent again; the frame owes no receipt, so a
      // request that reaches the session times out, and that timeout is the delivery.
      if (armed) {
        connection.publish(subject, codec.encode(envelope));
      } else {
        await until(
          `the session's agent subject for ${label}`,
          async () => {
            try {
              await connection.request(subject, codec.encode(envelope), { timeout: 500 });
              return true;
            } catch (error) {
              if (error instanceof NatsError && error.code === ErrorCode.Timeout) return true;
              if (error instanceof NatsError && error.code === ErrorCode.NoResponders) {
                return undefined;
              }
              throw error;
            }
          },
          closed
        );
        armed = true;
      }
      const sent = { eventID, messageID };
      if (accepted) {
        await until(
          `Dispatch's accept of ${label}`,
          () =>
            requests.some((request) =>
              request.path.startsWith(`/api/v1/messages/${messageID}/deliveries/`)
            )
              ? true
              : undefined,
          closed
        );
      }
      return sent;
    },
    async reply(message, what) {
      const body = await until(
        what,
        () =>
          requests.find((request) => request.path === `/api/v1/messages/${message.messageID}/reply`)
            ?.body,
        closed
      );
      return typeof body.body === "string" ? body.body : JSON.stringify(body);
    },
    replied(message) {
      return requests.some(
        (request) => request.path === `/api/v1/messages/${message.messageID}/reply`
      );
    },
    async arm() {
      // A replay arms the publisher for `AGENT_STREAM_WATCH_TTL_MS`, as the relay's first pair of
      // control messages does, and the first one answers before it arms, so a case reads its
      // frames from a later replay. The keep-alive is the relay's watch ping, well inside the
      // window, so no case loses the stream by running long.
      await until(
        "the session's agent stream to answer a replay",
        async () => ((await askReplay(1_000)) === undefined ? undefined : true),
        closed
      );
      const ping = setInterval(() => {
        connection.publish(control, codec.encode(JSON.stringify({ type: "watch", v: 1 })));
      }, 5_000);
      cleanup.push(async () => clearInterval(ping));
    },
    async replay() {
      const answer = await until(
        "the session's replay",
        async () => await askReplay(5_000),
        closed
      );
      const replayed = answer as { readonly frames?: readonly Record<string, unknown>[] };
      return { frames: replayed.frames ?? [] };
    },
    async command(command) {
      const id = randomUUID();
      const waiting = Promise.withResolvers<Record<string, unknown>>();
      responses.set(id, waiting);
      rpc.send({ ...command, id });
      const response = await Promise.race([waiting.promise, rpc.closed]);
      if (response.success !== true) {
        throw new Error(`omp refused ${String(command.type)}: ${JSON.stringify(response)}`);
      }
      return response;
    },
    async settled() {
      await until(
        "the session to settle",
        async () => {
          const response = await session.command({ type: "get_state" });
          return RpcStateSchema.parse(response.data).isSettled ? true : undefined;
        },
        closed
      );
    },
    async entries() {
      const response = await session.command({ type: "get_state" });
      const file = RpcStateSchema.parse(response.data).sessionFile;
      if (file === undefined) throw new Error("get_state named no session file");
      return (await readFile(file, "utf8")).split("\n");
    },
    until: (what, probe) => until(what, probe, closed),
  };
  return session;
}

/**
 * Resolves as `promise` does, or fails on `what` once `ms` have passed without it settling. A
 * real timer, as what it bounds is the real binary's answer time; a passing case never waits on it.
 */
async function within<T>(what: string, ms: number, promise: Promise<T>): Promise<T> {
  const late = Promise.withResolvers<never>();
  const timer = setTimeout(() => late.reject(new Error(`${what}: no answer within ${ms} ms`)), ms);
  try {
    return await Promise.race([promise, late.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** The `context` frames among `seen`, which carry the two figures. */
function contextFrames(
  seen: readonly SeenFrame[]
): { readonly at: number; readonly tokens: number; readonly contextWindow: number }[] {
  return seen
    .filter((each) => each.frame.kind === "context")
    .map((each) => ({
      at: each.at,
      contextWindow: Number(each.frame.contextWindow),
      tokens: Number(each.frame.tokens),
    }));
}

/** The `message` frames among `seen`: what the session published about its conversation. */
function messageFrames(
  seen: readonly SeenFrame[]
): { readonly at: number; readonly message: Record<string, unknown> }[] {
  return seen
    .filter((each) => each.frame.kind === "message")
    .map((each) => ({ at: each.at, message: each.frame.message as Record<string, unknown> }));
}

/** The text every part of a stream message carries. */
function frameText(message: Record<string, unknown>): string {
  const parts = Array.isArray(message.parts) ? message.parts : [];
  return parts.map((part: { readonly text?: string }) => part.text ?? "").join("");
}

/** The context session, booted once: the `ENVOY_CONTEXT_FRAME_INTERVAL_MS` the check names, and a
 *  compaction threshold its last case reaches with one reply's reported usage. */
let contextSessionPromise: Promise<SurfaceSession> | undefined;
async function contextSession(): Promise<SurfaceSession> {
  contextSessionPromise ??= (async () => {
    const session = await startSession({
      config: [
        "compaction:",
        "  thresholdTokens: 100000",
        "  asyncEnabled: false",
        "  autoContinue: false",
        "  keepRecentTokens: 1",
        "  methodOrder: [soft]",
        "  midTurnEnabled: false",
      ],
      env: { ENVOY_CONTEXT_FRAME_INTERVAL_MS: String(CONTEXT_INTERVAL_MS) },
    });
    await session.arm();
    return session;
  })();
  return contextSessionPromise;
}

/** Waits until the interval the publisher throttles by has passed since `at`. */
async function pastTheInterval(at: number): Promise<void> {
  const left = CONTEXT_INTERVAL_MS + 100 - (performance.now() - at);
  if (left > 0) await Bun.sleep(left);
}

// The figures the frame carries are the host's own (`getContextUsage`), and the window is the
// model's: the harness's models.yml gives the stand-in model a 200000-token window. One turn of a
// real conversation ends several messages - the person's own and the assistant's - and the
// publisher reads the figures at the turn's start and after each of them, so a turn inside one
// interval is one frame, not one per message.
test.skipIf(skip)(
  "a turn's messages inside one interval publish one frame, carrying the host's figures",
  async () => {
    const session = await contextSession();
    const from = session.frames.length;
    await session.command({ type: "prompt", message: "What is the first case?" });
    const turn = await session.nextTurn("the first model request");
    turn.reply([text("The first case.")]);
    await session.settled();
    const seen = session.frames.slice(from);

    const published = contextFrames(seen);
    expect(published).toHaveLength(1);
    const reading = published[0] as { readonly at: number; readonly tokens: number };
    // Two messages of the turn, the person's own and the assistant's, and the premise of the row:
    // both ended inside the interval the first reading opened.
    const messages = messageFrames(seen);
    expect(new Set(messages.map((each) => each.message.id)).size).toBeGreaterThanOrEqual(2);
    expect((messages.at(-1)?.at ?? 0) - reading.at).toBeLessThan(CONTEXT_INTERVAL_MS);
    expect(published[0]?.contextWindow).toBe(200_000);
    expect(reading.tokens).toBeGreaterThan(0);
    expect(reading.tokens).toBeLessThan(200_000);
  },
  120_000
);

// The next message after the interval publishes the next frame, and the figures reach the viewer
// in the same breath as that message: the publisher reads them in the host's `message_end`, right
// after the message's own frame, so the gap between the two frames is what the read cost. The
// model request is held past the interval, so the turn's start is one interval and the assistant's
// message the next.
test.skipIf(skip)(
  "a message after the interval publishes the next frame, within 200 ms of that message",
  async () => {
    const session = await contextSession();
    const before = contextFrames(session.frames).at(-1);
    if (before !== undefined) await pastTheInterval(before.at);

    const from = session.frames.length;
    await session.command({ type: "prompt", message: "What is the second case?" });
    const turn = await session.nextTurn("the second model request");
    // The turn has started and its user message is written: that is the interval this case opens.
    const atStart = await session.until("the turn's own context frame", () => {
      const published = contextFrames(session.frames.slice(from));
      return published.length > 0 ? published[0] : undefined;
    });
    await pastTheInterval(atStart.at);
    const held = session.frames.length;
    turn.reply([text("The second case.")]);
    await session.settled();

    const seen = session.frames.slice(held);
    const published = contextFrames(seen);
    expect(published).toHaveLength(1);
    const frame = published[0] as { readonly at: number; readonly tokens: number };
    expect(frame.tokens).toBeGreaterThan(atStart.tokens);
    const message = messageFrames(seen)
      .filter((each) => each.at <= frame.at)
      .at(-1);
    if (message === undefined) throw new Error("no message frame came before the context frame");
    expect(frame.at - message.at).toBeLessThan(200);
  },
  120_000
);

// A view that opens or wakes draws its ring from the replay, so the newest figures ride in its
// tail beside the settings and the command list, in that order, with no new message since.
test.skipIf(skip)(
  "a replay after the turn carries the context frame in its tail, beside the settings and the commands",
  async () => {
    const session = await contextSession();
    const live = contextFrames(session.frames).at(-1);
    if (live === undefined) throw new Error("no context frame was published before this case");
    const turnsBefore = session.turns.length;

    const replayed = await session.replay();
    expect(session.turns.length).toBe(turnsBefore);
    const tail = replayed.frames.slice(-3).map((frame) => frame.kind);
    expect(tail).toEqual(["settings", "context", "commands"]);
    const frame = replayed.frames.at(-2) as { readonly tokens: number; contextWindow: number };
    expect(frame.contextWindow).toBe(200_000);
    expect(frame.tokens).toBe(live.tokens);
  },
  120_000
);

// A compaction is the one moment the figures fall, and a ring left at the old reading would tell a
// person the session is still as full as it was. The publisher reads them after a compaction too,
// with no message of a turn to carry them: the reply below reports usage past the threshold, the
// host compacts, and the summary call is held past the publisher's interval so the frame that
// follows it is the compaction's own.
test.skipIf(skip)(
  "a compaction publishes a frame of its own, with no further message",
  async () => {
    const session = await contextSession();
    const last = contextFrames(session.frames).at(-1);
    if (last !== undefined) await pastTheInterval(last.at);

    const opened = session.frames.length;
    await session.command({ type: "prompt", message: "What is the third case?" });
    const turn = await session.nextTurn("the third model request");
    turn.reply([text("The third case.")], 150_000);
    const summary = await session.nextTurn("the automatic compaction's summary request");
    expect(isSummarization(summary.request)).toBe(true);
    const atSummary = await session.until("the turn's own context frame", () =>
      contextFrames(session.frames.slice(opened)).at(-1)
    );
    await pastTheInterval(atSummary.at);

    const from = session.frames.length;
    summary.reply([text("The conversation so far.")]);
    // A compaction can make more than one summary call (a split turn's prefix is summarised on
    // its own), and the frame comes once the compaction has ended, so each is answered as it
    // arrives.
    const after = await session.until("the compaction's context frame", async () => {
      for (const call of session.turns.slice(summary.index + 1)) {
        if (isSummarization(call.request)) call.reply([text("The conversation so far.")]);
      }
      const published = contextFrames(session.frames.slice(from));
      return published.length > 0 ? published[0] : undefined;
    });
    await session.settled();

    // Nothing of the conversation drove it: no message was written after the compaction's summary
    // call, no turn of the conversation ran, and the frame carries figures of its own.
    expect(messageFrames(session.frames.slice(from))).toEqual([]);
    expect(
      session.turns.slice(summary.index + 1).filter((call) => !isSummarization(call.request))
    ).toEqual([]);
    expect(after.contextWindow).toBe(200_000);
    expect(after.tokens).not.toBe(atSummary.tokens);
  },
  120_000
);

/** The shell session, booted once: the `ENVOY_SHELL_COMMAND_LIMIT_MS` the check names, and a
 *  viewer attached, since a `!command`'s own frame is the live view's copy of it. */
let shellSessionPromise: Promise<SurfaceSession> | undefined;
async function shellSession(): Promise<SurfaceSession> {
  shellSessionPromise ??= (async () => {
    const session = await startSession({
      env: { ENVOY_SHELL_COMMAND_LIMIT_MS: String(SHELL_LIMIT_MS) },
    });
    await session.arm();
    return session;
  })();
  return shellSessionPromise;
}

/** What the agent is told above a command and its output (`src/typed-shell.ts`). */
const SHELL_HEADING = "A person ran this shell command in this session from Dispatch.";

/**
 * Ends a `!command` case: the session has settled with no model request of its own since the
 * command, and a probe prompt is the next request, which carries what the model was told. Answers
 * the probe and waits for the session to settle again.
 */
async function probe(session: SurfaceSession): Promise<Request> {
  const expected = session.turns.length;
  await session.until("the session to settle with no turn of the command's own", async () => {
    const extra = session.turns[expected];
    if (extra !== undefined) {
      throw new Error(`a turn ran for the command, on ${conversation(extra.request)}`);
    }
    const response = await session.command({ type: "get_state" });
    return RpcStateSchema.parse(response.data).isSettled ? true : undefined;
  });
  const prompt = `PROBE-${randomUUID().slice(0, 8)}`;
  await session.command({ type: "prompt", message: prompt });
  const turn = await session.nextTurn("the probe's model request");
  expect(conversation(turn.request)).toContain(prompt);
  turn.reply([text("Probe answered.")]);
  await session.settled();
  return turn.request;
}

// The negative control: the same line without the mark is a prompt, and the session runs nothing.
// It is the shell session's first case, so no `!command` of an earlier case has put the heading
// below into this conversation and its absence is the line's own.
test.skipIf(skip)(
  "a Send of `printf hi` with no mark runs no command and reaches the model as a prompt",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("printf hi");
    const turn = await session.nextTurn("the turn the person's prompt started");
    expect(conversation(turn.request)).toContain("printf hi");
    turn.reply([text("Nothing was run.")]);
    await session.settled();

    expect(session.replied(sent)).toBe(false);
    const published = messageFrames(session.frames.slice(from)).map((each) =>
      frameText(each.message)
    );
    expect(published).toContain("printf hi");
    expect(published).not.toContain("printf hi\n\nhi");
    expect(conversation(turn.request)).not.toContain(SHELL_HEADING);
  },
  120_000
);

// A person's `!command` is the session's own to run: the host is never handed the line, the live
// view gets the person's message with what it printed, Dispatch gets the reply, and the model
// reads it at its next turn with no turn started for it.
test.skipIf(skip)(
  "a Send of `!printf hi` is run by the session, and all three answers carry it",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const turnsBefore = session.turns.length;

    const sent = await session.send("!printf hi");
    const reply = await session.reply(sent, "the reply to the person's command");
    expect(reply).toBe("!printf hi\n\nhi");

    // The live view: one user message, keyed on the Dispatch message it answers.
    const published = messageFrames(session.frames.slice(from));
    expect(published).toHaveLength(1);
    const message = published[0]?.message as Record<string, unknown>;
    expect(message.role).toBe("user");
    expect(message.dispatchMessageId).toBe(sent.messageID);
    expect(frameText(message)).toBe("!printf hi\n\nhi");

    // No turn ran for it, and the model reads the command with the next turn.
    expect(session.turns.length).toBe(turnsBefore);
    const request = await probe(session);
    const messages = messagesOf(request);
    expect(messages.filter((each) => each.includes(SHELL_HEADING))).toHaveLength(1);
    expect(conversation(request)).toContain("!printf hi\\n\\nhi");
    // No prompt carries the person's line: the host was handed nothing for it, so the only copy
    // of the line in the conversation is the one inside the command's own report.
    expect(conversation(request).split("!printf hi").length - 1).toBe(1);

    // The ledger holds the message as taken, though the host wrote nothing for the line.
    const entries = await session.entries();
    expect(
      entries.filter(
        (entry) => entry.includes("envoy-replay-delivered") && entry.includes(sent.eventID)
      )
    ).toHaveLength(1);
    expect(
      entries.filter(
        (entry) =>
          entry.includes("envoy-dispatch-handled-attempt") && entry.includes(sent.messageID)
      )
    ).toHaveLength(1);
  },
  120_000
);

// The command runs where the session runs: `SessionContext.cwd`, not the test's directory.
test.skipIf(skip)(
  "a Send of `!pwd` answers the session's own directory",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!pwd -P");
    expect(await session.reply(sent, "the reply to `!pwd -P`")).toBe(
      `!pwd -P\n\n${realpathSync(session.workspace)}`
    );
  },
  120_000
);

// An Aside is the person's turn too, so it takes the same path and is answered the same way.
test.skipIf(skip)(
  "an Aside of `!printf hi` behaves as the Send does",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const turnsBefore = session.turns.length;

    const sent = await session.send("!printf aside", { mode: "aside" });
    expect(await session.reply(sent, "the reply to the person's Aside")).toBe(
      "!printf aside\n\naside"
    );
    const published = messageFrames(session.frames.slice(from));
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe("!printf aside\n\naside");
    expect(session.turns.length).toBe(turnsBefore);
  },
  120_000
);

// Every `!` message is answered, one whose command printed nothing included: silence is an answer
// a person must be given, or the message looks lost.
test.skipIf(skip)(
  "a Send of `!true` is answered, saying there was no output",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!true");
    expect(await session.reply(sent, "the reply to `!true`")).toBe(
      "!true\n\nThe command printed nothing."
    );
  },
  120_000
);

test.skipIf(skip)(
  "a Send of `!exit 3` names the status in the frame and in the reply",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("!exit 3");
    const answer =
      "!exit 3\n\nThe command printed nothing.\n\nThe command ended with exit status 3.";
    expect(await session.reply(sent, "the reply to `!exit 3`")).toBe(answer);
    const published = messageFrames(session.frames.slice(from));
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe(answer);
  },
  120_000
);

// A command that would run for ten minutes is stopped at the session's limit, and the answer says
// so rather than leaving the person waiting.
test.skipIf(skip)(
  "a Send of `!sleep 600` is stopped at the limit and says when",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!sleep 600");
    const reply = await within(
      "the reply to `!sleep 600`",
      5_000,
      session.reply(sent, "the reply to `!sleep 600`")
    );
    expect(reply).toBe(
      `!sleep 600\n\nThe command printed nothing.\n\nThe command was stopped after ${SHELL_LIMIT_MS} ms.`
    );
  },
  120_000
);

// One command runs for a session at a time: a second is answered naming the first rather than
// queued behind it.
test.skipIf(skip)(
  "a second `!` sent while one runs is answered naming the command still running",
  async () => {
    const session = await shellSession();
    const first = await session.send("!sleep 600");
    const second = await session.send("!printf hi");
    expect(await session.reply(second, "the reply to the second command")).toBe(
      "!printf hi\n\nThe command was not run: this session is still running !sleep 600."
    );
    expect(await session.reply(first, "the reply to the command still running")).toBe(
      `!sleep 600\n\nThe command printed nothing.\n\nThe command was stopped after ${SHELL_LIMIT_MS} ms.`
    );
  },
  120_000
);

// A command that prints without end must not fill the bus, the person's page or the model's
// context: all three copies are cut at the same bound and say so.
test.skipIf(skip)(
  "a Send whose command prints 20000 characters is cut at 8000 in the frame and the model's copy",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const command = "!printf 'a%.0s' $(seq 1 20000)";
    const sent = await session.send(command);
    const cut = `${command}\n\n${"a".repeat(SHELL_OUTPUT_MAX)}\n\nThe output was cut at ${SHELL_OUTPUT_MAX} characters.`;
    expect(await session.reply(sent, "the reply to the long command")).toBe(cut);
    const published = messageFrames(session.frames.slice(from));
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe(cut);

    const request = await probe(session);
    expect(conversation(request)).toContain("a".repeat(SHELL_OUTPUT_MAX));
    expect(conversation(request)).not.toContain("a".repeat(SHELL_OUTPUT_MAX + 1));
  },
  120_000
);

// The mark is one `!`, and the rest is the command whatever it starts with.
test.skipIf(skip)(
  "a Send of `!!printf hi` runs `!printf hi` and answers with that shell's failure",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!!printf hi");
    const reply = await session.reply(sent, "the reply to `!!printf hi`");
    expect(reply).toStartWith("!!printf hi\n\n");
    expect(reply).toContain("!printf");
    expect(reply).toEndWith("The command ended with exit status 127.");

    // Nothing is hidden from the model: it reads the same report.
    const request = await probe(session);
    expect(conversation(request)).toContain("The command ended with exit status 127.");
  },
  120_000
);

// A BTW is a side question the model answers, never a line the session runs.
test.skipIf(skip)(
  "a BTW runs no command",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("!printf hi", { mode: "btw" });
    const side = await session.nextTurn("the BTW's side turn");
    expect(conversation(side.request)).toContain("!printf hi");
    side.reply([text("The model's answer.")]);
    expect(await session.reply(sent, "the BTW's reply")).toBe("The model's answer.");
    expect(
      messageFrames(session.frames.slice(from)).map((each) => frameText(each.message))
    ).not.toContain("!printf hi\n\nhi");
    await session.settled();
  },
  120_000
);

// A broadcast is no person's turn at all: it keeps its card, and the session runs nothing.
test.skipIf(skip)(
  "a broadcast runs no command",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("!printf hi", {
      accepted: false,
      broadcastID: "66666666-6666-4666-8666-666666666666",
    });
    // The card starts a turn of its own, which carries the text the broadcast sent.
    const turn = await session.nextTurn("the turn the broadcast's card started");
    expect(conversation(turn.request)).toContain("!printf hi");
    turn.reply([text("Read the broadcast.")]);
    await session.settled();

    expect(session.replied(sent)).toBe(false);
    expect(
      messageFrames(session.frames.slice(from)).map((each) => frameText(each.message))
    ).not.toContain("!printf hi\n\nhi");
    expect(
      session.requests.some((request) =>
        request.path.startsWith(`/api/v1/messages/${sent.messageID}/deliveries/`)
      )
    ).toBe(false);
  },
  120_000
);

// A `!command` runs on its own promise, never inside the queued delivery task (D11): a command
// that runs for minutes must not hold the session's other Dispatch messages behind it. This
// session's limit is the default two minutes, and the command ends when the case says so, so the
// second message is answered with the first command demonstrably still running.
test.skipIf(skip)(
  "a `!sleep` holds none of the session's other Dispatch messages",
  async () => {
    const session = await startSession();
    const release = path.join(session.workspace, "release");
    const waiting = await session.send(
      `!while [ ! -f ${release} ]; do sleep 0.05; done; printf released`
    );

    const prompt = await session.send("Are you still taking messages?");
    const turn = await session.nextTurn("the turn the second message started");
    expect(conversation(turn.request)).toContain("Are you still taking messages?");
    // The first command is still running: its own answer is not on Dispatch yet.
    expect(session.replied(waiting)).toBe(false);
    turn.reply([text("Still taking them.")]);
    await session.settled();
    expect(session.replied(waiting)).toBe(false);
    expect(session.replied(prompt)).toBe(false);

    await Bun.write(release, "");
    expect(await session.reply(waiting, "the reply once the command ended")).toEndWith("released");
  },
  120_000
);
