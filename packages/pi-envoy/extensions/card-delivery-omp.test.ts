import { afterEach, beforeAll, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
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

// How an Envoy card and a person's Send reach the real Oh My Pi (extensions/envoy.ts's `handCard`):
// cards come over a real NATS server as the listener forwards a role lane to the session's agent
// subject, the Send as Dispatch's delivery frame accepted through the stand-in. What reached the
// model is read off the requests the binary sends the stand-in gateway, each held until the test
// answers it. LEGION_TEST_OMP names the binary, as in dispatch-first-omp.test.ts, and
// LEGION_TEST_NATS_URL the NATS server (the name the daemon's tests read,
// packages/daemon/internal/testnats/nats.go). A run without both skips, except on GitHub Actions,
// where the pi-envoy job supplies both.
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

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A Messages request's conversation, as one string to find texts and their order in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** The text of a Messages request's last message: what the turn it answers was started on. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

function text(reply: string): Block {
  return { type: "text", text: reply };
}

/** A `tool_use` block calling tool `name` with `input`, and the one-line intent (`i`) every Oh My
 *  Pi tool requires, which no case reads. */
function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Running the case's step", ...input } };
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

/** The field of `get_state`'s answer (the pin's `RpcSessionState`) this case reads. */
const RpcStateSchema = z.object({
  queuedMessages: z.object({ steering: z.array(z.string()) }),
});

/** One Messages request the stand-in holds until the test answers it. */
interface Turn {
  readonly request: Request;
  /** Its position among every Messages request the session sent, from 0. */
  readonly index: number;
  /** Answers it with `blocks`. */
  reply(blocks: readonly Block[]): void;
}

/** A running session with pi-envoy, a real NATS connection to it, and what it sent the stand-in. */
interface CardSession {
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** Publishes a card and resolves once the session has acknowledged it (its lane receipt). */
  card(label: string): Promise<string>;
  /** Publishes a person's Send and resolves once the session asked Dispatch to accept it. */
  send(label: string): Promise<string>;
  /** Sends one RPC command and resolves with its response frame. */
  command(command: Record<string, unknown>): Promise<Record<string, unknown>>;
  /** The host's steering queue, as `get_state` reports it. */
  steering(): Promise<readonly string[]>;
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
    // What the cases wait on (a request reaching the stand-in, the host's state) has no event the
    // test can await, so it is polled, and a missing one fails at the deadline.
    await Bun.sleep(20);
  }
}

/**
 * Starts the real Oh My Pi in RPC mode with only envoy.ts loaded, against one stand-in that is the
 * model gateway, the Envoy listener and Dispatch, and the NATS server `setup` names. Resolves once
 * the session has registered its id with the listener and the test holds a NATS connection.
 */
async function startSession(): Promise<CardSession> {
  const { binary, nats } = setup();
  const { home, workspace, sessions } = await ompRoot(binary, "card-delivery-omp-", cleanup);

  const turns: Turn[] = [];
  /** The body each published Send's message id carries, which the accept answers with. */
  const sends = new Map<string, string>();
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        index,
        reply: (blocks) => answer.resolve(messageStream(blocks, `msg_${index}`)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation.
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
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "card-delivery-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end (an aborted one) is answered, so the server stops at once.
  cleanup.push(async () => {
    for (const turn of turns) turn.reply([text("Case over.")]);
  });
  await writeStandinProfile(home, base);

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
    turns,
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
    async steering() {
      const response = await session.command({ type: "get_state" });
      return RpcStateSchema.parse(response.data).queuedMessages.steering;
    },
    until: (what, probe) => until(what, probe, closed),
  };
  return session;
}

// The measurement this release exists for. A card queued while a turn runs used to be a steer, and
// the host drains its steering queue one message per turn, first in first out: of five cards and a
// person's Send queued during one step, the step after it took one. As asides the five cards are
// folded in whole at that step, behind the Send, which is still a steer. The Send is published
// last and waited for in the host's steering queue: the session reads one subject in order, so a
// Send the host holds means every card before it was handed to the host too.
test.skipIf(skip)(
  "five cards sent while a turn runs all reach the model at the next step, behind a person's Send",
  async () => {
    const session = await startSession();
    await session.command({ type: "prompt", message: "Start the case." });
    const held = await session.nextTurn("the first model request");

    const cards: string[] = [];
    for (let n = 1; n <= 5; n += 1) cards.push(await session.card(`c${n}`));
    const send = await session.send("c");
    await session.until("the Send in the host's steering queue", async () =>
      (await session.steering()).some((chip) => chip.includes(send)) ? true : undefined
    );
    // A tool the host runs to its end: a `bash` call is moved to the background when a message is
    // queued, and its result starts a run of its own.
    held.reply([call("read", { path: "." })]);

    const next = await session.nextTurn("the model request after the held one");
    expect(next.index).toBe(held.index + 1);
    const messages = conversation(next.request);
    const missing = cards.filter((card) => !messages.includes(card));
    expect({ missing, startedOn: lastMessage(next.request) }).toEqual({
      missing: [],
      startedOn: lastMessage(next.request),
    });
    expect(messages).toContain(send);
    for (const card of cards) {
      expect(messages.indexOf(send)).toBeLessThan(messages.indexOf(card));
    }
    next.reply([text("Read the Send and the cards.")]);
  },
  120_000
);
