import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  controllerToken,
  legionControllerNoticeSubject,
  ROLE_TOPIC_PREFIX,
} from "@legion/contracts";
import {
  type Cleanup,
  type Interactive,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnInteractive,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { connect, StringCodec } from "nats";
import { z } from "zod";

// The operator's controller on the real, interactive Oh My Pi, as `legion controller start` runs
// it: both plugins, the controller's environment, and `/legion-controller-start` on the command
// line, which pi-legion's extension command of that name (extensions/legion.ts,
// src/controller-session.ts `sendStartMessage`) answers by sending the start message. Oh My Pi
// runs a command-line word once its startup is over and the TUI listens, so the start turn is the
// operator's to see, type into and interrupt; a send from `session_start`, which Oh My Pi drains
// at the end of its startup, ran the whole turn before the TUI subscribed, with no start message
// and no busy row on screen, Enter not taking typed text and Esc doing nothing. Only a terminal
// shows that, so the session runs under a pseudo-terminal whose screen the cases read
// (`spawnInteractive`), against one stand-in that is the model gateway — each Messages request held
// until the case answers it — the daemon, Dispatch and the Envoy listener, and the real NATS server
// the Envoy role claim needs, over which the cases also send the controller its startup cards and
// notices. The controller the daemon launches, which runs Oh My Pi in RPC mode and gets its start
// message as the daemon's prompt, runs against the same stand-in (`startDaemonController`).
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server, as in pi-envoy's
// real-binary tests. A run without both skips, except on GitHub Actions, where the pi-legion job
// supplies both (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
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

/**
 * The start message `legion controller start` hands the controller as
 * LEGION_CONTROLLER_START_MESSAGE: daemon.ControllerStartMessage in
 * packages/daemon/internal/daemon/controller.go, copied literally.
 */
const START_MESSAGE =
  'Legion controller start: follow skill://legion-controller\'s start procedure now ("What happened before you started"), then end the turn.';

/** The start message inside JSON (a request body, a transcript line), its quotes escaped. */
const START_MESSAGE_JSON = JSON.stringify(START_MESSAGE).slice(1, -1);

/** The command-line word `legion controller start` passes (daemon.ControllerStartCommand). */
const START_COMMAND = "/legion-controller-start";

/**
 * What `legion controller start` puts on Oh My Pi's command line after its own flags: the
 * controller's instructions as an appended system prompt (a stand-in here) and the start word.
 * A run at the commit before the start word existed drops its last element.
 */
const CONTROLLER_ARGS = [
  "--append-system-prompt",
  "You are the stand-in Legion controller.",
  START_COMMAND,
] as const;

/**
 * The extensions every controller loads. The Envoy entry is the sibling plugin's: the controller
 * loads both, and the Legion entry refuses to run without it.
 */
const CONTROLLER_EXTENSIONS = [
  path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
  path.resolve(import.meta.dir, "legion.ts"),
];

/** The project the cases' controller serves: the daemon state fixtures' `legion`. */
const PROJECT = "legion";

/**
 * What pi-envoy's wake (src/card-wake.ts, `CardWake`) says, in any count, and for the one and
 * two startup messages the cases send.
 */
const WAKE_TEXT = /Envoy messages? above arrived with no turn to read (it|them)/;
const WAKE_ONE = "1 Envoy message above arrived with no turn to read it";
const WAKE_TWO = "2 Envoy messages above arrived with no turn to read them";

/** The TUI's busy row while a turn runs (`⎋ Working…`). */
const BUSY = "Working…";

/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 30_000;

/**
 * Wide enough that the start message, as the TUI renders the user message, fits on one row, so
 * the screen holds it verbatim.
 */
const COLUMNS = 200;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** One Messages request the stand-in holds until the case answers it. */
interface Turn {
  readonly request: Request;
  /** Answers it with one text block. */
  reply(text: string): void;
}

/** The fields of a transcript message entry the cases read; the rest is passed through. */
const MessageEntrySchema = z.object({
  type: z.literal("message"),
  message: z.looseObject({
    role: z.string(),
    stopReason: z.string().optional(),
    errorMessage: z.string().optional(),
  }),
});
type MessageEntry = z.infer<typeof MessageEntrySchema>;

/** A running controller session, its terminal, and what it sent the stand-in. */
interface ControllerSession {
  readonly terminal: Interactive;
  /** The session that claimed the controller role; empty until one has. */
  sessionID(): string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** The one transcript's lines, raw; none until Oh My Pi writes the file. */
  transcriptLines(): Promise<string[]>;
  /** The transcript's message entries, in order. */
  messages(): Promise<MessageEntry[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
  /** Resolves once the screen shows all of `texts` at once; fails on `what` at the deadline. */
  shows(what: string, ...texts: readonly string[]): Promise<void>;
}

/** A request the stand-in holds, the first of its kind, until the case releases it. */
interface Hold {
  /** Whether the request has reached the stand-in, which holds it now. */
  reached(): boolean;
  /** Lets the stand-in answer it. */
  release(): void;
  /** The stand-in's side: marks the request reached and resolves once the case releases it. */
  wait(): Promise<void>;
}

function newHold(): Hold {
  let reached = false;
  const released = Promise.withResolvers<void>();
  return {
    reached: () => reached,
    release: () => released.resolve(),
    wait() {
      reached = true;
      return released.promise;
    },
  };
}

/** What the controller's stand-in holds until the case releases it. */
interface ControllerHolds {
  /**
   * The first listener registration after the role claim (`/v1/roles/set`): the one envoy.ts
   * sends right after it opens the controller topic's NATS subscription (`subscribeNotice`), which
   * the controller's claim in `session_start` awaits. The agent subject is open since envoy.ts's
   * own `session_start`, so while it is held the session takes a card and a controller notice
   * inside Oh My Pi's startup.
   */
  readonly noticeRegistration?: Hold;
  /**
   * The daemon-launched controller's `POST /legion/v1/claims/ready`, the last step of its claim in
   * `session_start`, sent once it holds the role and the controller topic, with the start-turn
   * mark stamped sent.
   */
  readonly ready?: Hold;
}

/** A controller's scratch root, and the stand-in it runs against. */
interface ControllerStandin {
  readonly root: string;
  readonly home: string;
  readonly workspace: string;
  readonly sessions: string;
  /** The controller's LEGION_STATE_DIR. */
  readonly state: string;
  readonly base: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: Turn[];
  /** The session that claimed the controller role; empty until one has. */
  sessionID(): string;
}

/**
 * Serves the controller's stand-in and writes the profile that points Oh My Pi at it: the gateway
 * holds each Messages request until the case answers it; the daemon answers its state from
 * `stateFixture` and the controller's registration from the contract fixtures, acknowledges a
 * ready and mints grants; Dispatch is unavailable; and the Envoy listener records the session that
 * sets a role and names it as that role's holder. `holds` names the requests it holds.
 */
async function serveController(
  binary: string,
  stateFixture: string,
  holds: ControllerHolds
): Promise<ControllerStandin> {
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    "controller-start-omp-",
    cleanup
  );
  const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api");
  // The daemon's golden answers (packages/daemon/internal/api): its state, whose project is
  // `legion`, and the controller's registration, whose claim token is `legion-legion-controller`.
  const daemonState: unknown = JSON.parse(
    await readFile(path.join(fixtures, stateFixture), "utf8")
  );
  const registered = z
    .looseObject({ claimToken: z.string() })
    .parse(JSON.parse(await readFile(path.join(fixtures, "register-controller.json"), "utf8")));
  const state = path.join(root, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });

  const turns: Turn[] = [];
  let roleHolder = "";
  let grants = 0;
  let { noticeRegistration, ready } = holds;
  const { base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        reply: (text) => answer.resolve(messageStream([{ type: "text", text }], `msg_${index}`)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/state") return Response.json(daemonState);
    if (url.pathname === "/legion/v1/claims/register") return Response.json(registered);
    if (url.pathname === "/legion/v1/claims/ready") {
      const held = ready;
      ready = undefined;
      await held?.wait();
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `controller-start-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // An unavailable Dispatch arms no run-end self-check, so every Messages request is a turn.
    if (url.pathname.startsWith("/api/")) return new Response("unavailable", { status: 503 });
    // The Envoy listener: the role claim records its holder, the role read names it, and
    // registration and any other read answer with an interest.
    const sid = typeof body.session_id === "string" ? body.session_id : "";
    if (url.pathname === "/v1/roles/set") roleHolder = sid;
    if (url.pathname === `/v1/roles/${registered.claimToken}`) {
      return Response.json({ role: registered.claimToken, holder: roleHolder, last_seen: 1 });
    }
    if (url.pathname === "/v1/interests/subscribe" && roleHolder !== "") {
      const held = noticeRegistration;
      noticeRegistration = undefined;
      await held?.wait();
    }
    return Response.json({
      session_id: sid === "" ? roleHolder : sid,
      machine_id: "controller-start-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end (an interrupted one among them) is answered, so the server
  // stops at once.
  cleanup.push(async () => {
    holds.noticeRegistration?.release();
    holds.ready?.release();
    for (const turn of turns) turn.reply("Case over.");
  });
  await writeStandinProfile(home, base);
  return { root, home, workspace, sessions, state, base, turns, sessionID: () => roleHolder };
}

/**
 * Starts the controller as `legion controller start` does: interactive Oh My Pi with the Envoy and
 * Legion extensions from this checkout, the controller's environment (LEGION_CONTROLLER, its
 * capability file, its start message, the daemon, Envoy and its NATS server) and
 * `CONTROLLER_ARGS`, against the controller's stand-in (`serveController`), which holds `holds`.
 */
async function startController(holds: ControllerHolds = {}): Promise<ControllerSession> {
  const { binary, nats } = setup();
  const { root, home, workspace, sessions, state, base, turns, sessionID } = await serveController(
    binary,
    "state.json",
    holds
  );
  const secretFile = path.join(root, "controller-secret");
  await writeFile(secretFile, "controller-start-capability\n", { mode: 0o600 });

  const terminal = spawnInteractive(
    binary,
    {
      extensions: CONTROLLER_EXTENSIONS,
      home,
      workspace,
      sessions,
      env: {
        LEGION_CONTROLLER: "1",
        LEGION_ROLE: "controller",
        LEGION_DAEMON_URL: base,
        LEGION_PROJECT: PROJECT,
        LEGION_STATE_DIR: state,
        LEGION_CONTROLLER_SECRET_FILE: secretFile,
        LEGION_CONTROLLER_START_MESSAGE: START_MESSAGE,
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
      },
      args: CONTROLLER_ARGS,
      cols: COLUMNS,
    },
    cleanup
  );
  let exitCode: number | undefined;
  void terminal.exited.then((code) => {
    exitCode = code;
  });

  const until = async <T>(
    what: string,
    probe: () => T | undefined | Promise<T | undefined>
  ): Promise<T> => {
    const deadline = Date.now() + DEADLINE_MS;
    for (;;) {
      if (exitCode !== undefined) {
        throw new Error(
          `omp exited (${exitCode}) while waiting for ${what}; its screen:\n${terminal.screen()}`
        );
      }
      const value = await probe();
      if (value !== undefined) return value;
      if (Date.now() > deadline) {
        throw new Error(`timed out waiting for ${what}; the screen:\n${terminal.screen()}`);
      }
      // What the cases wait on (a request reaching the stand-in, the screen, the transcript) has
      // no event the test can await, so it is polled, and a missing one fails at the deadline.
      await Bun.sleep(25);
    }
  };

  const transcriptLines = async (): Promise<string[]> => {
    const files = (await readdir(sessions, { recursive: true })).filter((file) =>
      file.endsWith(".jsonl")
    );
    if (files.length === 0) return [];
    if (files.length !== 1)
      throw new Error(`want one transcript under ${sessions}, found ${files}`);
    return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
      .split("\n")
      .filter(Boolean);
  };

  let taken = 0;
  return {
    terminal,
    sessionID,
    turns,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken]);
      taken += 1;
      return turn;
    },
    transcriptLines,
    async messages() {
      return (await transcriptLines()).flatMap((line) => {
        const entry = MessageEntrySchema.safeParse(JSON.parse(line));
        return entry.success ? [entry.data] : [];
      });
    },
    until,
    async shows(what, ...texts) {
      await until(what, () => {
        const screen = terminal.screen();
        return texts.every((text) => screen.includes(text)) ? true : undefined;
      });
    },
  };
}

/** The fields of `get_state`'s answer (the pin's `RpcSessionState`) the cases read. */
const RpcStateSchema = z.looseObject({ isSettled: z.boolean() });

/** The controller the daemon launched, over its RPC stream, and what it sent the stand-in. */
interface DaemonController {
  /** The session that claimed the controller role; empty until one has. */
  sessionID(): string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /**
   * Writes the daemon's delivery of `message` as the shim writes it to Oh My Pi's stdin: one RPC
   * `prompt` (packages/daemon/internal/shimwire `Prompt`). Its response is not awaited, as the
   * daemon's delivery reads only the turn it starts.
   */
  deliver(message: string): void;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
  /** Resolves once `get_state` reports the session settled. */
  settled(): Promise<void>;
}

/**
 * Starts the controller as the daemon launches it (`controller: daemon`): `omp --mode rpc` with
 * both extensions and the pod's environment — its boot token's file in place of a capability, the
 * project's Slack channels (none), and no LEGION_CONTROLLER_START_MESSAGE, since the daemon
 * delivers the start message itself once the controller reports ready — against the controller's
 * stand-in (`serveController`), which answers the daemon's state while the daemon holds the
 * controller's claim (internal/projection's golden) and holds `holds`.
 */
async function startDaemonController(holds: ControllerHolds): Promise<DaemonController> {
  const { binary, nats } = setup();
  const { root, home, workspace, sessions, state, base, turns, sessionID } = await serveController(
    binary,
    "state-controller-claim.json",
    holds
  );
  const bootFile = path.join(root, "LEGION_BOOT_TOKEN");
  await writeFile(bootFile, "launch-boot-token\n", { mode: 0o600 });

  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  const closed: { error?: unknown } = {};
  const rpc = spawnRpc(
    binary,
    {
      extensions: CONTROLLER_EXTENSIONS,
      home,
      workspace,
      sessions,
      env: {
        LEGION_CONTROLLER: "1",
        LEGION_ROLE: "controller",
        LEGION_BOOT_TOKEN_FILE: bootFile,
        LEGION_SLACK_CHANNELS: "",
        LEGION_DAEMON_URL: base,
        LEGION_PROJECT: PROJECT,
        LEGION_STATE_DIR: state,
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
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

  const until = async <T>(
    what: string,
    probe: () => T | undefined | Promise<T | undefined>
  ): Promise<T> => {
    const deadline = Date.now() + DEADLINE_MS;
    for (;;) {
      if (closed.error !== undefined) throw closed.error;
      const value = await probe();
      if (value !== undefined) return value;
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
      // As in `startController`: what the cases wait on has no event the test can await.
      await Bun.sleep(25);
    }
  };

  const rpcState = async (): Promise<z.infer<typeof RpcStateSchema>> => {
    const id = randomUUID();
    const waiting = Promise.withResolvers<Record<string, unknown>>();
    responses.set(id, waiting);
    rpc.send({ type: "get_state", id });
    const response = await Promise.race([waiting.promise, rpc.closed]);
    if (response.success !== true) {
      throw new Error(`omp refused get_state: ${JSON.stringify(response)}`);
    }
    return RpcStateSchema.parse(response.data);
  };

  let taken = 0;
  return {
    sessionID,
    turns,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken]);
      taken += 1;
      return turn;
    },
    deliver(message) {
      rpc.send({ type: "prompt", id: randomUUID(), message });
    },
    until,
    async settled() {
      await until("the session to settle", async () =>
        (await rpcState()).isSettled ? true : undefined
      );
    },
  };
}

/** A card the case published on the controller role's lane: its text, and the frame it went in. */
interface Card {
  readonly summary: string;
  readonly frame: Uint8Array;
}

/**
 * What reaches the controller session `sid` over the real NATS server: a card on the controller
 * role's lane, as the Envoy listener forwards it to the session's agent subject, and a daemon
 * notice on the controller topic, as the listener publishes what the daemon's controller-notice
 * executor posts (packages/daemon/internal/daemon/outbox.go `controllerNotice`, its notify
 * `Publish`, the listener's `messageEnvelope` in packages/envoy/cmd/listener/api.go).
 */
interface ControllerInbound {
  /**
   * Publishes one card and resolves once the session acknowledged it: the forwarded envelope keeps
   * the role topic, so the session sends a lane receipt on the request's reply inbox
   * (`expectsLaneReceipt`, packages/envoy-client/src/delivery.ts), as pi-envoy's
   * card-delivery-omp.test.ts `forwardedRoleEnvelope` builds it.
   */
  card(label: string): Promise<Card>;
  /**
   * Sends `card`'s frame again, as the listener re-sends a card whose receipt it missed, and
   * resolves on its receipt. The session acknowledges a frame before it checks for a repeat
   * (pi-envoy's envoy.ts `deliver`) and delivers a repeat nowhere, so this adds no message: it only
   * shows that the session's NATS connection has taken everything published before it.
   */
  repeat(card: Card): Promise<void>;
  /**
   * Publishes one triage notice on the controller topic and returns its summary (`triage on
   * <issue>`, the daemon's `noticeSummary`), whose issue key no other notice shares.
   */
  notice(): string;
}

async function controllerInbound(nats: string, sid: string): Promise<ControllerInbound> {
  const connection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  const agentSubject = `notifications.agent.${sid}`;
  const noticeSubject = legionControllerNoticeSubject(PROJECT);
  const receipt = async (frame: Uint8Array): Promise<void> => {
    await connection.request(agentSubject, frame, { timeout: 5_000 });
  };
  return {
    async card(label) {
      const summary = `CARD-${label}-${randomUUID().slice(0, 8)}`;
      const key = `${label}-${randomUUID()}`;
      const minted = createHash("sha256").update(key).digest("hex").slice(0, 32);
      const frame = codec.encode(
        JSON.stringify({
          event_id: `evt-${key}`,
          source: "envoy",
          source_event_id: `source-${key}`,
          topic: `${ROLE_TOPIC_PREFIX}${controllerToken(PROJECT)}`,
          dedupe_key: `envoy.role.forward.publish.${minted}`,
          issued_at: Date.now(),
          payload_summary: summary,
          trace_id: `trace-${key}`,
        })
      );
      await receipt(frame);
      return { summary, frame };
    },
    repeat: (card) => receipt(card.frame),
    notice() {
      const text = `triage on LEGION-${100_000 + Math.floor(Math.random() * 900_000)}`;
      connection.publish(
        noticeSubject,
        codec.encode(
          JSON.stringify({
            event_id: randomUUID(),
            // The daemon names no source, so the listener's default.
            source: "agent",
            source_event_id: randomUUID(),
            topic: noticeSubject,
            dedupe_key: `legion-outbox:${randomUUID()}`,
            issued_at: Date.now(),
            payload_summary: text,
            payload: JSON.stringify({ kind: "triage" }),
            trace_id: randomUUID(),
          })
        )
      );
      return text;
    },
  };
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

/**
 * Resolves once the session has settled on `reply`: the screen shows the reply and no busy row,
 * and the transcript's last message is the assistant's, ended normally.
 */
async function settledOn(session: ControllerSession, reply: string): Promise<void> {
  await session.until("the session to settle on the reply", async () => {
    const screen = session.terminal.screen();
    if (!screen.includes(reply) || screen.includes(BUSY)) return undefined;
    const last = (await session.messages()).at(-1)?.message;
    return last?.role === "assistant" && last.stopReason === "stop" ? true : undefined;
  });
}

test.skipIf(skip)(
  "the start turn runs after startup and the TUI shows it",
  async () => {
    const session = await startController();
    const start = await session.nextTurn("the start turn's model request");
    await session.shows(
      "the start message and the busy row on screen while the start turn's request is held",
      START_MESSAGE,
      BUSY
    );
    start.reply("Start procedure done.");
    await settledOn(session, "Start procedure done.");
    expect(session.turns.length).toBe(1);
  },
  120_000
);

test.skipIf(skip)(
  "the operator can type during the start turn",
  async () => {
    const session = await startController();
    const start = await session.nextTurn("the start turn's model request");
    await session.shows("the busy row while the start turn's request is held", START_MESSAGE, BUSY);
    const typed = `TYPED-${randomUUID().slice(0, 8)}`;
    session.terminal.write(typed);
    // Enter only once the composer holds the text, so the keystrokes reach the TUI as typing.
    await session.shows("the typed text in the composer", typed);
    session.terminal.write("\r");
    await session.shows("the typed text queued as a steer", "Steering", `1. ${typed}`);
    expect(session.turns.length).toBe(1);

    start.reply("Start procedure done.");
    const next = await session.nextTurn("the typed message's model request");
    const conversation = JSON.stringify(next.request.body.messages ?? null);
    const startAt = conversation.indexOf(START_MESSAGE_JSON);
    expect(startAt).toBeGreaterThanOrEqual(0);
    expect(conversation.indexOf(typed)).toBeGreaterThan(startAt);
    expect(lastMessage(next.request)).toContain(typed);
    next.reply("Typed message answered.");
    await settledOn(session, "Typed message answered.");
  },
  120_000
);

test.skipIf(skip)(
  "Esc interrupts the start turn",
  async () => {
    const session = await startController();
    await session.nextTurn("the start turn's model request");
    await session.shows("the busy row while the start turn's request is held", START_MESSAGE, BUSY);
    // A lone ESC byte is the Esc key. Oh My Pi aborts the held request's connection itself; the
    // stand-in answers it at cleanup.
    session.terminal.write("\x1b");
    await session.until("the interrupted start turn's assistant entry", async () =>
      (await session.messages()).find(
        ({ message }) =>
          message.role === "assistant" &&
          message.stopReason === "aborted" &&
          message.errorMessage === "Interrupted by user"
      )
    );
    await session.until("the busy row to leave the screen", () =>
      session.terminal.screen().includes(BUSY) ? undefined : true
    );
    // A real clock, deliberately: the proposition is that the interrupted session starts no
    // further turn, and a host that does nothing emits no signal to await, so only a quiet
    // window with no Messages request shows the absence.
    await Bun.sleep(3_000);
    expect(session.turns.length).toBe(1);
  },
  120_000
);

test.skipIf(skip)(
  "the start message starts the first turn, once",
  async () => {
    const session = await startController();
    const start = await session.nextTurn("the start turn's model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    start.reply("Start procedure done.");
    await settledOn(session, "Start procedure done.");

    for (const turn of session.turns) {
      expect(JSON.stringify(turn.request.body)).not.toContain(START_COMMAND);
    }
    const lines = await session.transcriptLines();
    expect(lines.filter((line) => line.includes(START_COMMAND))).toEqual([]);
    const userMessages = (await session.messages())
      .filter(({ message }) => message.role === "user")
      .map((entry) => JSON.stringify(entry));
    expect(userMessages[0]).toContain(START_MESSAGE_JSON);
    expect(userMessages.filter((entry) => entry.includes(START_MESSAGE_JSON))).toHaveLength(1);
  },
  120_000
);

// A card the listener forwards on the controller role's lane as soon as it records the role, and a
// daemon notice on the controller topic once the claim has subscribed to it, both reach the session
// inside Oh My Pi's startup, where an idle card was a steer whose turn ran inside the startup
// drain, ahead of `/legion-controller-start`. The start-turn mark the claim sets before its role
// claim makes each a follow-up that starts no turn (pi-envoy's src/card-wake.ts, "The start
// turn"): the start turn is the first and has both in its context, and one wake after it gives
// them their turn, owed although the start turn's reply follows them. The stand-in holds the
// registration that follows the controller topic's subscription, inside the claim `session_start`
// awaits, until both have reached the session: the card's receipt shows the session took it, and
// the repeat's receipt that its connection took the notice published before it.
test.skipIf(skip)(
  "a card and a Legion notice that arrive during startup do not take the first turn, and each still gets a turn after it",
  async () => {
    const noticeRegistration = newHold();
    const session = await startController({ noticeRegistration });
    await session.until("the registration after the controller topic's subscription", () =>
      noticeRegistration.reached() ? true : undefined
    );
    const inbound = await controllerInbound(setup().nats, session.sessionID());
    const card = await inbound.card("startup");
    const notice = inbound.notice();
    await inbound.repeat(card);
    expect(session.turns).toHaveLength(0);
    noticeRegistration.release();

    const start = await session.nextTurn("the first model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    const first = conversation(start.request);
    expect(first).not.toMatch(WAKE_TEXT);
    const startAt = first.indexOf(START_MESSAGE_JSON);
    for (const text of [card.summary, notice]) {
      expect(first.indexOf(text)).toBeGreaterThanOrEqual(0);
      expect(first.indexOf(text)).toBeLessThan(startAt);
    }
    start.reply("Start procedure done.");

    const wake = await session.nextTurn("the startup wake's model request");
    expect(lastMessage(wake.request)).toContain(WAKE_TWO);
    wake.reply("Read the startup messages.");
    await settledOn(session, "Read the startup messages.");
    expect(session.turns).toHaveLength(2);

    // The cards' entries: each carries its card marker (`envoyCard`); the wake is an envoy-message
    // too, marked as the session's own notice instead.
    const delivered = (await session.transcriptLines()).filter(
      (line) =>
        line.includes('"custom_message"') &&
        line.includes('"envoy-message"') &&
        line.includes('"envoyCard"')
    );
    expect(delivered).toHaveLength(2);
    for (const entry of delivered) expect(entry).toContain('"wake":"startup"');
    expect(delivered.some((entry) => entry.includes(card.summary))).toBe(true);
    expect(delivered.some((entry) => entry.includes(notice))).toBe(true);
  },
  120_000
);

// The daemon's controller reports ready once it holds the role and the controller topic, and the
// daemon writes its start prompt then, which RPC mode reads only once Oh My Pi's startup is over.
// The wake for a card that reached the session during that startup fired once the host was idle
// after it, which can be before the prompt arrives, so the wake's turn was the first. The claim
// stamps the start-turn mark sent right before its ready (src/controller-session.ts), which holds
// the wake behind the start turn. The stand-in holds that ready while the card arrives.
test.skipIf(skip)(
  "the daemon-launched controller's start prompt is its first turn, and a startup card's wake runs after it",
  async () => {
    const ready = newHold();
    const session = await startDaemonController({ ready });
    await session.until("the controller's ready", () => (ready.reached() ? true : undefined));
    const inbound = await controllerInbound(setup().nats, session.sessionID());
    const card = await inbound.card("daemon-startup");
    expect(session.turns).toHaveLength(0);
    ready.release();
    // A real clock, deliberately: the daemon's delivery reaches Oh My Pi some time after the ready
    // it answers, and the wake the mark holds back fired in that gap, once the host was idle after
    // its startup, which the host signals to no extension and no test.
    await Bun.sleep(300);
    session.deliver(START_MESSAGE);

    const start = await session.nextTurn("the first model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    const first = conversation(start.request);
    expect(first).not.toMatch(WAKE_TEXT);
    expect(first.indexOf(card.summary)).toBeGreaterThanOrEqual(0);
    expect(first.indexOf(card.summary)).toBeLessThan(first.indexOf(START_MESSAGE_JSON));
    start.reply("Start procedure done.");

    const wake = await session.nextTurn("the startup wake's model request");
    expect(lastMessage(wake.request)).toContain(WAKE_ONE);
    wake.reply("Read the startup card.");
    await session.settled();
    expect(session.turns).toHaveLength(2);
  },
  120_000
);
