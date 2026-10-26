import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
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
import { connect, ErrorCode, type NatsConnection, NatsError, StringCodec } from "nats";
import { z } from "zod";

// How an Envoy card and a person's Send reach the real Oh My Pi (extensions/envoy.ts's card send
// and its wake for a card the host folds): cards come over a real NATS server as the listener
// forwards a role lane to the session's agent subject, the Send as Dispatch's delivery frame
// accepted through the stand-in. What reached the model is read off the requests the binary sends
// the stand-in gateway, each held until the test answers it. LEGION_TEST_OMP names the binary, as
// in dispatch-first-omp.test.ts, and LEGION_TEST_NATS_URL the NATS server (the name the daemon's
// tests read, packages/daemon/internal/testnats/nats.go). A run without both skips, except on
// GitHub Actions, where the pi-envoy job supplies both.
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

/** What the wake (extensions/envoy.ts, `foldedCards`) says, in its one- and many-card forms. */
const WAKE_TEXT = /Envoy messages? above arrived with no turn to read (it|them)/;

/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 30_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Whether a Messages request is one of a compaction's summary calls (dispatch-first-omp.test.ts). */
function isSummarization(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(
    "Summarize user–AI coding-assistant conversations"
  );
}

/** A Messages request's conversation, as one string to find texts and their order in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** The text of a Messages request's last message: what the turn it answers was started on. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

/** A tool call with the one-line intent every Oh My Pi tool requires. */
function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Running the case's step", ...input } };
}

function text(reply: string): Block {
  return { type: "text", text: reply };
}

/**
 * A role-lane card as the listener forwards it to the role holder's agent subject: the envelope
 * keeps the role topic, so the session acknowledges it on the request's reply inbox
 * (`expectsLaneReceipt`, packages/envoy-client/src/delivery.ts), under a key minted per label as
 * envoy.test.ts's `forwardedRoleEnvelope` builds it.
 */
function forwardedRoleEnvelope(summary: string, label: string): string {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "envoy",
    source_event_id: `source-${label}`,
    topic: "notifications.role.card-delivery",
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: Date.now(),
    payload_summary: summary,
    trace_id: `trace-${label}`,
  });
}

/**
 * A person's direct Send to session `sid` from Dispatch's Agents page, as the listener hands the
 * session its delivery frame (envoy.test.ts's `directDispatchEnvelope`): no issue, the person as
 * the event's actor, attempt 1 in steer.
 */
function directDispatchEnvelope(
  sid: string,
  messageID: string,
  body: string,
  label: string
): string {
  const actor = { id: "alice", kind: "user" };
  return JSON.stringify({
    dedupe_key: `send-${label}`,
    event_id: `dispatch-send-${label}`,
    issued_at: Date.now(),
    payload: JSON.stringify({
      event: {
        actor,
        issue_key: null,
        payload: {
          author: actor,
          body,
          created_at: "2026-10-26T00:00:00Z",
          deliveries: [],
          id: messageID,
          in_reply_to: null,
          issue_key: null,
          target: `session:${sid}`,
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode: "steer" },
    }),
    payload_summary: body,
    source: "dispatch",
    source_event_id: "1",
    topic: `notifications.agent.${sid}`,
    trace_id: `send-${label}`,
  });
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
  queuedMessages: z.object({ steering: z.array(z.string()) }),
});

/** A running session with pi-envoy, a real NATS connection to it, and what it sent the stand-in. */
interface CardSession {
  readonly root: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** Every RPC frame so far, in order. */
  readonly frames: readonly object[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** Publishes a card and resolves once the session has acknowledged it (its lane receipt). */
  card(label: string): Promise<string>;
  /** Publishes a person's Send and resolves once the session asked Dispatch to accept it. */
  send(label: string): Promise<string>;
  /** Sends one RPC command and resolves with its response frame. */
  command(command: Record<string, unknown>): Promise<Record<string, unknown>>;
  state(): Promise<z.infer<typeof RpcStateSchema>>;
  /** Resolves once `get_state` reports the session settled. */
  settled(): Promise<void>;
  /** The session file's `envoy-message` custom messages, as raw JSON lines. */
  cardEntries(): Promise<string[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
  /** Answers the registration a session started with `holdStartup` holds; a no-op otherwise. */
  releaseStartup(): void;
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
    // What the cases wait on (a request reaching the stand-in, a file, the host's state) has no
    // event the test can await, so it is polled, and a missing one fails at the deadline.
    await Bun.sleep(20);
  }
}

/**
 * Starts the real Oh My Pi in RPC mode with only envoy.ts loaded, against one stand-in that is the
 * model gateway, the Envoy listener and Dispatch, and the NATS server `setup` names. `config` lines
 * go into the profile's config.yml. Resolves once the session has registered its id with the
 * listener and the test holds a NATS connection. With `holdStartup` the stand-in holds its answer
 * to that first registration until `releaseStartup`: envoy.ts's `session_start` awaits it
 * (`reconcileNow` registers once the agent subject is open), so the host is still inside its
 * startup, before it starts any extension send or reads any RPC command.
 */
async function startSession(
  options: { readonly config?: readonly string[]; readonly holdStartup?: boolean } = {}
): Promise<CardSession> {
  const { config = [], holdStartup = false } = options;
  const { binary, nats } = setup();
  const { root, home, workspace, sessions } = await ompRoot(binary, "card-delivery-omp-", cleanup);

  let startupHold = holdStartup ? Promise.withResolvers<void>() : undefined;
  const releaseStartup = startupHold?.resolve ?? (() => undefined);
  const turns: Turn[] = [];
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
    // turn of the conversation or a compaction's summary call.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    const accept = url.pathname.match(/^\/api\/v1\/messages\/([^/]+)\/deliveries\/(\d+)\/accept$/);
    if (accept !== null) {
      const messageID = accept[1] as string;
      return Response.json({
        message_id: messageID,
        body: sends.get(messageID) ?? "",
        delivery: "steer",
        accepted_as: "user_turn",
        attempt: Number(accept[2]),
      });
    }
    if (/^\/api\/v1\/messages\/[^/]+\/deliveries\/\d+\/reply$/.test(url.pathname)) {
      return Response.json({}, { status: 201 });
    }
    // The Envoy listener: registration and any read answer with an interest. A held startup
    // holds the first registration.
    if (url.pathname === "/v1/interests/subscribe" && startupHold !== undefined) {
      const held = startupHold.promise;
      startupHold = undefined;
      await held;
    }
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "card-delivery-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end (an aborted one, a held registration) is answered, so the
  // server stops at once.
  cleanup.push(async () => {
    releaseStartup();
    for (const turn of turns) turn.reply([text("Case over.")]);
  });
  /** The body each published Send's message id carries, which the accept answers with. */
  const sends = new Map<string, string>();
  await writeStandinProfile(home, base, config);

  const frames: object[] = [];
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
        DISPATCH_TOKEN: "card-delivery-token",
      },
      onFrame: (frame) => {
        frames.push(frame);
        if ("type" in frame && frame.type === "response" && "id" in frame) {
          const waiting = responses.get(String(frame.id));
          waiting?.resolve(frame as Record<string, unknown>);
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

  let taken = 0;
  const session: CardSession = {
    root,
    turns,
    frames,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken], closed);
      taken += 1;
      return turn;
    },
    async card(label) {
      const summary = `CARD-${label}-${randomUUID().slice(0, 8)}`;
      const envelope = codec.encode(forwardedRoleEnvelope(summary, `${label}-${randomUUID()}`));
      // The session opens its agent subject after it registers: until then a request finds no
      // responder, and nothing took it, so it is sent again.
      await until(
        `the session's receipt for card ${label}`,
        async () => {
          try {
            await connection.request(subject, envelope, { timeout: 5_000 });
            return true;
          } catch (error) {
            if (error instanceof NatsError && error.code === ErrorCode.NoResponders) {
              return undefined;
            }
            throw error;
          }
        },
        closed
      );
      return summary;
    },
    async send(label) {
      const body = `SEND-${label}-${randomUUID().slice(0, 8)}`;
      const messageID = randomUUID();
      sends.set(messageID, body);
      connection.publish(
        subject,
        codec.encode(directDispatchEnvelope(sid, messageID, body, label))
      );
      await until(
        `Dispatch's accept of Send ${label}`,
        () =>
          requests.some((request) => request.path.includes(`/messages/${messageID}/deliveries/`))
            ? true
            : undefined,
        closed
      );
      return body;
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
    async state() {
      const response = await session.command({ type: "get_state" });
      return RpcStateSchema.parse(response.data);
    },
    async settled() {
      await until(
        "the session to settle",
        async () => ((await session.state()).isSettled ? true : undefined),
        closed
      );
    },
    async cardEntries() {
      const file = (await session.state()).sessionFile;
      if (file === undefined) throw new Error("get_state named no session file");
      return (await readFile(file, "utf8"))
        .split("\n")
        .filter((line) => line.includes('"custom_message"') && line.includes('"envoy-message"'));
    },
    until: (what, probe) => until(what, probe, closed),
    releaseStartup,
  };
  return session;
}

/**
 * Ends a negative check: the session has settled with no Messages request after the `expected`
 * ones, and a probe prompt is the next request, on that prompt. Answers the probe and waits for
 * the session to settle again. Returns the probe's request.
 */
async function probe(session: CardSession, expected: number): Promise<Request> {
  await session.settled();
  expect(session.turns.length).toBe(expected);
  const prompt = `PROBE-${randomUUID().slice(0, 8)}`;
  await session.command({ type: "prompt", message: prompt });
  const turn = await session.nextTurn("the probe's model request");
  expect(turn.index).toBe(expected);
  expect(lastMessage(turn.request)).toContain(prompt);
  turn.reply([text("Probe answered.")]);
  await session.settled();
  expect(session.turns.length).toBe(expected + 1);
  return turn.request;
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

// A card queued while a turn runs used to be a steer, and the host drains its steering queue one
// message per step, first in first out: five cards ahead of a person's Send held the Send back
// five steps. As asides, the cards wait behind the Send, which reaches the model at the first step
// after it was sent.
test.skipIf(skip)(
  "a person's Send sent after five cards reaches the model at the next step, ahead of all five",
  async () => {
    const session = await startSession();
    await session.command({ type: "prompt", message: "Start case a." });
    const held = await session.nextTurn("the first model request");

    const cards: string[] = [];
    for (let n = 1; n <= 5; n += 1) cards.push(await session.card(`a${n}`));
    const send = await session.send("a");
    await session.until("the Send in the host's steering queue", async () =>
      (await session.state()).queuedMessages.steering.some((chip) => chip.includes(send))
        ? true
        : undefined
    );
    // A tool the host runs to its end: a `bash` call is moved to the background when a message
    // is queued, and its result starts a run of its own.
    held.reply([call("read", { path: "." })]);

    const next = await session.nextTurn("the model request after the held one");
    const firstWithSend = session.turns.find((turn) => conversation(turn.request).includes(send));
    expect(firstWithSend?.index).toBe(held.index + 1);
    const messages = conversation(next.request);
    for (const card of cards) {
      expect(messages).toContain(card);
      expect(messages.indexOf(send)).toBeLessThan(messages.indexOf(card));
    }
    next.reply([text("Read the Send and the cards.")]);

    // Every card was read in a run: none gets a wake.
    const request = await probe(session, next.index + 1);
    expect(conversation(request)).not.toMatch(WAKE_TEXT);
  },
  120_000
);

/** How an interrupt case holds the run when it interrupts it. */
type Hold = "model request" | "tool call";

/**
 * Interrupts a run with `abort` (the RPC `abort` a Legion daemon sends, or `abort_and_restore_queue`,
 * the TUI's Esc) while `hold` holds it and three cards are queued: the cards are folded into the
 * context with no turn, and the next prompt's request carries them.
 */
async function interruptKeepsCards(
  abort: "abort" | "abort_and_restore_queue",
  hold: Hold
): Promise<void> {
  const session = await startSession();
  await session.command({ type: "prompt", message: `Start case b, ${abort} during a ${hold}.` });
  const first = await session.nextTurn("the first model request");
  if (hold === "tool call") {
    // A real tool that runs until it is killed or released: the started marker says the shell
    // is in its loop, and the release at cleanup ends a shell an abort left behind.
    const started = path.join(session.root, "tool-started");
    const release = path.join(session.root, "tool-release");
    cleanup.push(() => writeFile(release, ""));
    first.reply([
      call("bash", {
        command: `touch '${started}'; while [ ! -e '${release}' ]; do sleep 0.1; done; echo released`,
      }),
    ]);
    await session.until("the blocking tool call to start", async () =>
      (await Bun.file(started).exists()) ? true : undefined
    );
  }
  const requestsBefore = session.turns.length;

  const cards = [await session.card("b1"), await session.card("b2"), await session.card("b3")];
  await session.command({ type: abort });

  // No turn after the interrupt: the cards are folded, and no wake follows them.
  const request = await probe(session, requestsBefore);
  const entries = await session.cardEntries();
  for (const card of cards) {
    expect(entries.filter((entry) => entry.includes(card))).toHaveLength(1);
    expect(conversation(request)).toContain(card);
  }
  expect(conversation(request)).not.toMatch(WAKE_TEXT);
}

for (const abort of ["abort", "abort_and_restore_queue"] as const) {
  for (const hold of ["model request", "tool call"] as const) {
    test.skipIf(skip)(
      `an RPC ${abort} during a held ${hold} keeps the three queued cards, folded with no turn`,
      () => interruptKeepsCards(abort, hold),
      120_000
    );
  }
}

// The host folds an aside that reaches it after the run's last step (here: during the run's
// automatic compaction) into the context with no turn while an interrupt since the last user
// prompt is still in effect, which a card-started run does not end. Under every-card-a-steer the
// card started a turn there; pi-envoy's wake gives it one.
test.skipIf(skip)(
  "a card that lands during a run's automatic compaction, in a session interrupted earlier, gets a turn",
  async () => {
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
    });
    await session.command({ type: "prompt", message: "Start case c." });
    await session.nextTurn("the first model request");
    // The interrupt the host keeps in effect until the next user prompt.
    await session.command({ type: "abort" });
    await session.settled();

    // An idle card gets a turn under the interrupt; its reply reports usage past the threshold.
    // The session got no card while it started, so this one is still in the startup phase
    // (extensions/envoy.ts, `startupPhase`): it goes into the context with no turn, and its wake
    // starts the turn, as its steer did before.
    const first = await session.card("c1");
    const cardTurn = await session.nextTurn("the turn the idle card's wake started");
    expect(conversation(cardTurn.request)).toContain(first);
    expect(lastMessage(cardTurn.request)).toMatch(WAKE_TEXT);
    cardTurn.reply([text("Read the first card.")], 150_000);

    const summary = await session.nextTurn("the automatic compaction's summary request");
    expect(isSummarization(summary.request)).toBe(true);
    const framesBefore = session.frames.length;
    const late = await session.card("c2");
    summary.reply([text("The conversation so far.")]);

    // A compaction can make more than one summary call (a split turn's prefix is summarised on
    // its own); each is answered as it comes, and the first other request is the wake's turn.
    let woken = await session.nextTurn("the turn the folded card's wake started");
    while (isSummarization(woken.request)) {
      woken.reply([text("The conversation so far.")]);
      woken = await session.nextTurn("the turn the folded card's wake started");
    }
    expect(conversation(woken.request)).toContain(late);
    expect(lastMessage(woken.request)).toMatch(WAKE_TEXT);
    // The card-started run ended with a continuation still owed: the late aside.
    const agentEndsSince = (from: number): object[] =>
      session.frames.slice(from).filter((frame) => "type" in frame && frame.type === "agent_end");
    const continuing = (frame: object): boolean =>
      "isTerminal" in frame && frame.isTerminal === false;
    expect(agentEndsSince(framesBefore).some(continuing)).toBe(true);
    const framesAtWake = session.frames.length;
    woken.reply([text("Read the late card.")]);
    await session.settled();
    expect(agentEndsSince(framesAtWake).some((frame) => !continuing(frame))).toBe(true);
    expect(session.turns.length).toBe(woken.index + 1);
  },
  120_000
);

// The first idle card of a session that got none while it started is still in the startup phase
// (extensions/envoy.ts, `startupPhase`): it goes into the context with no turn, and its wake
// starts the turn and ends the phase. An idle card after that starts its turn itself.
test.skipIf(skip)(
  "a card that reaches an idle session starts a turn",
  async () => {
    const session = await startSession();
    const first = await session.card("d1");
    const woken = await session.nextTurn("the turn the first idle card's wake started");
    expect(conversation(woken.request)).toContain(first);
    expect(lastMessage(woken.request)).toMatch(WAKE_TEXT);
    woken.reply([text("Read the first card.")]);
    await session.settled();

    const card = await session.card("d2");
    const turn = await session.nextTurn("the turn the idle card started");
    expect(lastMessage(turn.request)).toContain(card);
    turn.reply([text("Read the card.")]);
    await probe(session, turn.index + 1);
  },
  120_000
);

// Oh My Pi reads no RPC command until every send an extension started during its startup has
// settled (runtime-init.ts `initializeExtensions`: `runner.sends.drain()`), and an idle card sent
// with `triggerTurn` lasts as long as the turn it starts: a card that reached pi-envoy while the
// session started ran its whole turn inside that drain, and `get_state` and `prompt` waited for
// it. The stand-in holds the registration envoy.ts's `session_start` awaits, with the agent
// subject already open, so the cards reach pi-envoy inside the host's startup: the RPC command
// written meanwhile is not even read until startup is over.
test.skipIf(skip)(
  "a card that arrives while the session starts holds no RPC input",
  async () => {
    const session = await startSession({ holdStartup: true });
    const early = session.command({ type: "get_state" });
    let answeredEarly = false;
    void early.then(
      () => {
        answeredEarly = true;
      },
      () => undefined
    );
    const cards = [await session.card("s1"), await session.card("s2")];
    expect(answeredEarly).toBe(false);
    session.releaseStartup();

    // The cards' turn is held at its model request, and the host still answers RPC commands:
    // the one written during startup, and one written now.
    const woken = await session.nextTurn("the turn the startup cards' wake started");
    await within("get_state written during startup", 5_000, early);
    await within("get_state written while the turn is held", 5_000, session.state());

    const messages = conversation(woken.request);
    for (const card of cards) expect(messages).toContain(card);
    expect(lastMessage(woken.request)).toMatch(WAKE_TEXT);
    woken.reply([text("Read the startup cards.")]);
    await probe(session, woken.index + 1);
  },
  120_000
);

// Plan mode is the host's other fold with no turn. The pinned build's `/plan` is a TUI-only
// built-in (a `handleTui` and no `handle` in slash-commands/builtin-modes.ts), which RPC's builtin
// dispatch (`executeAcpBuiltinSlashCommand`) skips, so over RPC the text is an ordinary prompt and
// no RPC surface reaches plan mode: the wake there runs the handler the compaction case proves.
test.skipIf(skip)(
  "an RPC prompt of /plan reaches the model as a prompt, entering no plan mode",
  async () => {
    const session = await startSession();
    await session.command({ type: "prompt", message: "/plan" });
    const turn = await session.nextTurn("the model request /plan started");
    expect(lastMessage(turn.request)).toContain("/plan");
    turn.reply([text("Answered /plan as a prompt.")]);
    await session.settled();
  },
  120_000
);
