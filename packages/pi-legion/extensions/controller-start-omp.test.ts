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
  type Block,
  type Cleanup,
  type Interactive,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnInteractive,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { connect, StringCodec } from "nats";
import { z } from "zod";

// The operator's controller on the real, interactive Oh My Pi, as `legion controller start` runs
// it: both plugins, the controller's environment, and `/legion-controller-start` on the command
// line, which pi-legion's extension command of that name (extensions/legion.ts,
// src/controller-session.ts `startCommand`) answers by claiming the controller and sending the
// start message right before the role claim. Oh My Pi runs a command-line word once its startup is
// over and the TUI listens, so the start turn is the operator's to see, type into and interrupt; a
// send from `session_start`, which Oh My Pi drains at the end of its startup, ran the whole turn
// before the TUI subscribed, with no start message and no busy row on screen, Enter not taking
// typed text and Esc doing nothing; and a role claim made there let a card on the role's lane
// start its turn ahead of the start message. Only a terminal shows that, so the session runs under
// a pseudo-terminal whose screen the cases read (`spawnInteractive`), against one stand-in that is
// the model gateway — each Messages request held until the case answers it — the daemon, Dispatch
// and the Envoy listener, and the real NATS server the Envoy role claim needs, over which the
// cases also send the controller cards and notices. LEGION_TEST_OMP names the binary and
// LEGION_TEST_NATS_URL the NATS server, as in pi-envoy's real-binary tests. A run without both
// skips, except on GitHub Actions, where the pi-legion job supplies both (GITHUB_ACTIONS, not CI:
// agent harnesses on the devbox export CI=true). The file imports the real `nats` module, which
// extensions/legion.test.ts replaces for its whole process, so it runs in a process of its own
// (the package's `test` script).
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

/** The project the cases' controller serves: the daemon state fixture's `legion`. */
const PROJECT = "legion";

/** The TUI's busy row while a turn runs (`⎋ Working…`). */
const BUSY = "Working…";

/**
 * How long after the spawn a wait may last: Oh My Pi's startup, which loads both extensions before
 * `/legion-controller-start` runs, took about 27 s of wall time (9 s of CPU) in a slow but normal
 * pod, so the waits that span it get this much from the spawn.
 */
const STARTUP_DEADLINE_MS = 150_000;

/** How long one wait for something the started session owes a case may last once startup is over. */
const STEP_DEADLINE_MS = 30_000;

/** A case's own timeout: its startup, each of its waits and the cleanup of its processes. */
const CASE_TIMEOUT_MS = 600_000;

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
  /** Answers it with `blocks` (a tool call among them). */
  replyBlocks(blocks: readonly Block[]): void;
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

/** What a case's stand-in does beyond answering the controller's claim. */
interface StandinHooks {
  /**
   * The daemon's answer to the controller's registration (its request body given), in place of
   * the fixture's; a promise holds the registration until it settles.
   */
  readonly register?: (body: Record<string, unknown>) => Response | Promise<Response>;
  /**
   * Called with the claiming session's id once the stand-in has answered the controller role's
   * claim (`/v1/roles/set` for `legion-legion-controller`): the next turn of its event loop, after
   * the answer went out.
   */
  readonly onRoleClaim?: (sid: string) => void;
  /**
   * Called with the session's id as the first listener registration after that role claim
   * arrives (`/v1/interests/subscribe`): the one envoy.ts sends right after it opens the
   * controller topic's NATS subscription (`subscribeNotice`).
   */
  readonly onNoticeInterest?: (sid: string) => void;
  /** Called with the session id each Envoy listener request names, as it arrives. */
  readonly onListener?: (sid: string) => void;
  /** Oh My Pi's `keybindings.yml` for the session, written into its agent directory. */
  readonly keybindings?: string;
}

/**
 * Starts the controller as `legion controller start` does: interactive Oh My Pi with the Envoy and
 * Legion extensions from this checkout, the controller's environment (LEGION_CONTROLLER, its
 * capability file, its start message, the daemon, Envoy and its NATS server) and
 * `CONTROLLER_ARGS`, against one stand-in: the gateway holds each Messages request until the case
 * answers it; the daemon answers its state and the controller's registration from the contract
 * fixtures (or `hooks.register`'s answer) and mints grants; Dispatch is unavailable; and the Envoy
 * listener records the session that sets a role and names it as that role's holder, calling
 * `hooks`' role-claim and notice-interest callbacks as those requests arrive.
 */
async function startController(hooks: StandinHooks = {}): Promise<ControllerSession> {
  const { binary, nats } = setup();
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    "controller-start-omp-",
    cleanup
  );
  const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api");
  // The daemon's golden answers (packages/daemon/internal/api): its state, whose project is
  // `legion`, and the controller's registration, whose claim token is `legion-legion-controller`.
  const daemonState: unknown = JSON.parse(
    await readFile(path.join(fixtures, "state.json"), "utf8")
  );
  const registered = z
    .looseObject({ claimToken: z.string() })
    .parse(JSON.parse(await readFile(path.join(fixtures, "register-controller.json"), "utf8")));
  const state = path.join(root, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });
  const secretFile = path.join(root, "controller-secret");
  await writeFile(secretFile, "controller-start-capability\n", { mode: 0o600 });

  const turns: Turn[] = [];
  let roleHolder = "";
  let grants = 0;
  // Set at the controller role's first claim, until the registration after it arrives.
  let noticeInterestDue = false;
  const { base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        reply: (text) => answer.resolve(messageStream([{ type: "text", text }], `msg_${index}`)),
        replyBlocks: (blocks) => answer.resolve(messageStream(blocks, `msg_${index}`)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/state") return Response.json(daemonState);
    if (url.pathname === "/legion/v1/claims/register") {
      return hooks.register?.(body) ?? Response.json(registered);
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
    hooks.onListener?.(sid);
    if (url.pathname === "/v1/roles/set") {
      if (body.role === registered.claimToken && roleHolder === "") {
        setImmediate(() => hooks.onRoleClaim?.(sid));
        noticeInterestDue = true;
      }
      roleHolder = sid;
    }
    if (url.pathname === "/v1/interests/subscribe" && noticeInterestDue) {
      noticeInterestDue = false;
      hooks.onNoticeInterest?.(sid);
    }
    if (url.pathname === `/v1/roles/${registered.claimToken}`) {
      return Response.json({ role: registered.claimToken, holder: roleHolder, last_seen: 1 });
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
    for (const turn of turns) turn.reply("Case over.");
  });
  await writeStandinProfile(home, base);
  if (hooks.keybindings !== undefined) {
    await writeFile(path.join(home, ".omp", "agent", "keybindings.yml"), hooks.keybindings);
  }

  const spawnedAt = Date.now();
  const terminal = spawnInteractive(
    binary,
    {
      extensions: [
        // The Envoy entry is the sibling plugin's: the controller loads both, and the Legion
        // entry refuses to run without it.
        path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
        path.resolve(import.meta.dir, "legion.ts"),
      ],
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
    // A wait gets its own step, and one that starts before Oh My Pi's startup could be over gets
    // until the startup deadline: the first wait of every case spans that startup.
    const deadline = Math.max(Date.now() + STEP_DEADLINE_MS, spawnedAt + STARTUP_DEADLINE_MS);
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

/** A Messages request's conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** The text of a Messages request's last message: what the turn it answers was started on. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

/**
 * Whether the session has settled on `reply`: the screen shows the reply and no busy row, and the
 * transcript's last message is the assistant's, ended normally.
 */
async function isSettledOn(session: ControllerSession, reply: string): Promise<boolean> {
  const screen = session.terminal.screen();
  if (!screen.includes(reply) || screen.includes(BUSY)) return false;
  const last = (await session.messages()).at(-1)?.message;
  return last?.role === "assistant" && last.stopReason === "stop";
}

/** Resolves once the session has settled on `reply` (`isSettledOn`). */
async function settledOn(session: ControllerSession, reply: string): Promise<void> {
  await session.until("the session to settle on the reply", async () =>
    (await isSettledOn(session, reply)) ? true : undefined
  );
}

/** A card the case published on the controller role's lane: its text, and the frame it went in. */
interface Card {
  readonly summary: string;
  readonly frame: Uint8Array;
}

/** A card in flight: the card, and its lane receipt once the session acknowledges it. */
interface SentCard {
  readonly card: Card;
  readonly receipt: Promise<unknown>;
}

/**
 * What reaches a controller session over the real NATS server: a card on the controller role's
 * lane, as the Envoy listener forwards it to the session's agent subject, and a daemon notice on
 * the controller topic, as the listener publishes what the daemon's controller-notice executor
 * posts (packages/daemon/internal/daemon/outbox.go `controllerNotice`, the listener's
 * `messageEnvelope` in packages/envoy/cmd/listener/api.go).
 */
interface ControllerInbound {
  /**
   * Publishes one card to session `sid` and returns it with its lane receipt: the forwarded
   * envelope keeps the role topic, so the session acknowledges it on the request's reply inbox
   * (`expectsLaneReceipt`, packages/envoy-client/src/delivery.ts) before it delivers the card.
   */
  card(sid: string, label: string): SentCard;
  /**
   * Sends `card`'s frame to `sid` again, as the listener re-sends a card whose receipt it missed,
   * and resolves on its receipt. The session acknowledges a frame before it checks for a repeat
   * (pi-envoy's envoy.ts `deliver`) and delivers a repeat nowhere, so this adds no message: it only
   * shows that the session's NATS connection has taken everything published before it.
   */
  repeat(sid: string, card: Card): Promise<unknown>;
  /**
   * Publishes one triage notice on the controller topic and returns its summary (`triage on
   * <issue>`, the daemon's `noticeSummary`), whose issue key no other notice shares.
   */
  notice(): string;
}

async function controllerInbound(nats: string): Promise<ControllerInbound> {
  const connection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  const noticeSubject = legionControllerNoticeSubject(PROJECT);
  const receipt = (sid: string, frame: Uint8Array): Promise<unknown> =>
    connection.request(`notifications.agent.${sid}`, frame, { timeout: 5_000 });
  return {
    card(sid, label) {
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
      return { card: { summary, frame }, receipt: receipt(sid, frame) };
    },
    repeat: (sid, card) => receipt(sid, card.frame),
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
  CASE_TIMEOUT_MS
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
    const held = conversation(next.request);
    const startAt = held.indexOf(START_MESSAGE_JSON);
    expect(startAt).toBeGreaterThanOrEqual(0);
    expect(held.indexOf(typed)).toBeGreaterThan(startAt);
    expect(lastMessage(next.request)).toContain(typed);
    next.reply("Typed message answered.");
    await settledOn(session, "Typed message answered.");
  },
  CASE_TIMEOUT_MS
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
  CASE_TIMEOUT_MS
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
  CASE_TIMEOUT_MS
);

// Main's pi-envoy delivers every card as a steer, which starts a turn of its own when the session
// is idle, so whatever reaches the session on the controller role's lane or the controller topic
// first takes the first turn. The start command's claim sends the start message once the
// registration is in, right before the role claim, so a card the listener forwards the moment it
// takes the role (which can reach the session before the role claim's answer does), and a notice
// published the moment the controller topic is registered, both find the start turn already
// queued: the first request carries the start message, and each of them reaches the model after
// it, as a steer into the start turn or in a later turn. The card's lane receipt shows the session
// took it, and the repeat's receipt that its connection took the notice published before it.
test.skipIf(skip)(
  "a card published as soon as the role is claimed comes after the start turn",
  async () => {
    const inbound = await controllerInbound(setup().nats);
    let sid: string | undefined;
    let sent: SentCard | undefined;
    let notice: string | undefined;
    const session = await startController({
      onRoleClaim: (claimed) => {
        sid = claimed;
        sent = inbound.card(claimed, "role-claim");
      },
      onNoticeInterest: () => {
        notice = inbound.notice();
      },
    });
    const { card, receipt } = await session.until("the controller role's claim", () => sent);
    await receipt;
    const published = await session.until("the controller topic's registration", () => notice);
    await inbound.repeat(sid ?? "", card);

    // Every request is answered, in order, until the session settles with both having reached
    // the model.
    const reached = (): boolean =>
      session.turns.some(({ request }) => conversation(request).includes(card.summary)) &&
      session.turns.some(({ request }) => conversation(request).includes(published));
    for (let answered = 0; ; ) {
      const turn = await session.nextTurn(`model request ${answered + 1}`);
      answered += 1;
      const reply = `Reply ${answered}.`;
      turn.reply(reply);
      const next = await session.until(
        "the next model request, or the session settled with the card and the notice read",
        async () => {
          if (session.turns.length > answered) return "request";
          return reached() && (await isSettledOn(session, reply)) ? "settled" : undefined;
        }
      );
      if (next === "settled") break;
    }

    const requests = session.turns.map(({ request }) => request);
    const firstStart = requests.findIndex((request) =>
      conversation(request).includes(START_MESSAGE_JSON)
    );
    expect(firstStart).toBeGreaterThanOrEqual(0);
    for (const request of requests.slice(0, firstStart)) {
      expect(lastMessage(request)).not.toContain(card.summary);
      expect(lastMessage(request)).not.toContain(published);
    }
    // From the first request carrying it on, the start message comes before the card and the
    // notice wherever they are.
    for (const request of requests.slice(firstStart)) {
      const held = conversation(request);
      for (const text of [card.summary, published]) {
        if (held.includes(text)) {
          expect(held.indexOf(text)).toBeGreaterThan(held.indexOf(START_MESSAGE_JSON));
        }
      }
    }
    const userMessages = (await session.messages())
      .filter(({ message }) => message.role === "user")
      .map((entry) => JSON.stringify(entry));
    expect(userMessages[0]).toContain(START_MESSAGE_JSON);
    expect(userMessages.filter((entry) => entry.includes(START_MESSAGE_JSON))).toHaveLength(1);
  },
  CASE_TIMEOUT_MS
);

// The claim runs from the start command, after startup, in the terminal the operator reads: a
// refusal there is shown as one warning naming the daemon's answer and the command that claims
// again, and no start message is sent.
test.skipIf(skip)(
  "the operator sees a claim refusal",
  async () => {
    const session = await startController({
      register: () => Response.json({ error: "invalid controller capability" }, { status: 403 }),
    });
    await session.shows(
      "the claim refusal's warning on screen",
      "the controller claim failed",
      "invalid controller capability",
      "/legion-claim-controller"
    );
    // A real clock, deliberately: the proposition is that the refused claim sends no start
    // message, and a send that never happens emits no signal to await, so only a quiet window
    // with no Messages request shows the absence.
    await Bun.sleep(3_000);
    expect(session.turns).toHaveLength(0);
  },
  CASE_TIMEOUT_MS
);

// A `/new` replaces the controller session, and pi-legion's session_switch handler claims the new
// session again. When the daemon refuses that re-claim, the operator is told in a warning naming
// the refusal (`reclaimAfterSessionChange`). Oh My Pi's `/new` awaits the session_switch handlers,
// then resets the transcript and clears the scrollback
// (command-controller.ts `#runNewSessionFlow` in the pinned build), so a notice the handler shows
// while it runs is wiped before the operator can read it. The new session's `legion` tool then
// refuses every call, and the screen does not say why.
test.skipIf(skip)(
  "the operator sees a refused re-claim after /new",
  async () => {
    const fixture: unknown = JSON.parse(
      await readFile(
        path.resolve(
          import.meta.dir,
          "../../contracts/fixtures/daemon-api/register-controller.json"
        ),
        "utf8"
      )
    );
    let registrations = 0;
    const session = await startController({
      register: () => {
        registrations += 1;
        return registrations === 1
          ? Response.json(fixture)
          : Response.json({ error: "capability revoked" }, { status: 403 });
      },
    });
    const start = await session.nextTurn("the start turn's model request");
    start.reply("Start procedure done.");
    await settledOn(session, "Start procedure done.");

    session.terminal.write("/new");
    await session.shows("/new in the composer", "/new");
    session.terminal.write("\r");
    await session.until("the re-claim's registration", () =>
      registrations >= 2 ? true : undefined
    );
    await session.shows(
      "the re-claim's refusal on screen after /new",
      "re-claiming the controller for this session failed",
      "capability revoked"
    );
  },
  CASE_TIMEOUT_MS
);

// A `/new` typed while the start command's claim is in flight stops that claim at its
// registration, and the session change's re-claim registers the new session and sends the start
// message there, saying so in an info notice (`reclaimAfterSessionChange`). That claim ends while
// the `/new`'s session_switch handler still waits behind it, before Oh My Pi resets the transcript
// and clears the scrollback (command-controller.ts `#runNewSessionFlow` in the pinned build), so a
// notice it showed was wiped; the re-claim shows it once the `/new` has redrawn the screen. The
// first registration is held until the new session exists: Envoy's own session_switch handler,
// which runs before pi-legion's, names it to the listener.
test.skipIf(skip)(
  "the operator sees the hand-over notice after a /new during the start claim",
  async () => {
    const fixture: unknown = JSON.parse(
      await readFile(
        path.resolve(
          import.meta.dir,
          "../../contracts/fixtures/daemon-api/register-controller.json"
        ),
        "utf8"
      )
    );
    const registrations: Record<string, unknown>[] = [];
    const held = Promise.withResolvers<void>();
    let firstSession: string | undefined;
    let newSession: string | undefined;
    const session = await startController({
      register: async (body) => {
        registrations.push(body);
        if (registrations.length === 1) await held.promise;
        return Response.json(fixture);
      },
      onListener: (sid) => {
        if (firstSession !== undefined && sid !== "" && sid !== firstSession) newSession ??= sid;
      },
    });
    const first = await session.until("the start claim's registration", () => registrations[0]);
    firstSession = String(first.sessionId);

    session.terminal.write("/new");
    await session.shows("/new in the composer", "/new");
    session.terminal.write("\r");
    const switched = await session.until("the new session's listener request", () => newSession);
    held.resolve();
    await session.shows(
      "the hand-over notice on screen after the /new's reset",
      "New session started",
      "the session changed while the controller claim ran"
    );

    const start = await session.nextTurn("the start turn's model request");
    const reclaimed = await session.until("the re-claim's registration", () => registrations[1]);
    expect(reclaimed.sessionId).toBe(switched);
    const messages = Array.isArray(start.request.body.messages) ? start.request.body.messages : [];
    expect(messages).toHaveLength(1);
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    start.reply("Start procedure done.");
    await session.shows("the start turn's reply", "Start procedure done.");

    // The start message is the new session's first user message, and the replaced session has none.
    const [replacedUsers, newUsers] = await session.until(
      "the new session's transcript to hold the start message",
      async () => {
        const users = await Promise.all(
          [first.ompSessionFile, reclaimed.ompSessionFile].map(async (file) =>
            (await readFile(String(file), "utf8"))
              .split("\n")
              .filter(Boolean)
              .flatMap((line) => {
                const entry = MessageEntrySchema.safeParse(JSON.parse(line));
                return entry.success && entry.data.message.role === "user"
                  ? [JSON.stringify(entry.data)]
                  : [];
              })
          )
        );
        return (users[1]?.length ?? 0) > 0 ? users : undefined;
      }
    );
    expect(newUsers?.[0]).toContain(START_MESSAGE_JSON);
    expect(newUsers?.filter((entry) => entry.includes(START_MESSAGE_JSON))).toHaveLength(1);
    expect(replacedUsers).toEqual([]);
    expect(session.turns).toHaveLength(1);
  },
  CASE_TIMEOUT_MS
);

// The same refused re-claim, with a `task` subagent's view focused when the `/new` starts. Oh My
// Pi's `/new` then returns to the main session before it resets the transcript (command-
// controller.ts `#runNewSessionFlow` in the pinned build), and that return's replay can yield to
// the event loop (session-focus-controller.ts `#attach`), so a notice from a timer could land
// before the reset. The re-claim's outcome goes to the extension widget, which neither the return
// nor the reset takes down. A focused view refuses a typed `/new` (input-controller.ts: only
// /btw, /export, /jobs and /usage run there), so the case binds the host's `app.session.new`,
// which has no default key, to alt+n, whose handler runs the `/new` flow from any view. The
// subagent's own request is held, so it is still running when its view is focused (alt+a opens
// the agent hub, Enter focuses the selected agent).
test.skipIf(skip)(
  "the operator sees a refused re-claim after /new from a focused subagent view",
  async () => {
    const fixture: unknown = JSON.parse(
      await readFile(
        path.resolve(
          import.meta.dir,
          "../../contracts/fixtures/daemon-api/register-controller.json"
        ),
        "utf8"
      )
    );
    let registrations = 0;
    const session = await startController({
      register: () => {
        registrations += 1;
        return registrations === 1
          ? Response.json(fixture)
          : Response.json({ error: "capability revoked" }, { status: 403 });
      },
      keybindings: "app.session.new: alt+n\n",
    });
    const start = await session.nextTurn("the start turn's model request");
    start.replyBlocks([
      {
        type: "tool_use",
        name: "task",
        input: { agent: "task", task: "Reply with done.", solutionSpace: "Nothing to explore." },
      },
    ]);
    // The start turn's request after the task tool's result carries the start message; the
    // subagent's carries only its assignment, and stays unanswered.
    for (;;) {
      const turn = await session.nextTurn("the start turn's request after the task call");
      if (JSON.stringify(turn.request.body.messages ?? []).includes(START_MESSAGE_JSON)) {
        turn.reply("Start procedure done.");
        break;
      }
    }
    await session.shows("the start turn's reply", "Start procedure done.");
    // Oh My Pi focuses only a subagent that has started its run: Enter in the hub before its
    // first model request reaches a subagent with nothing to show ("cannot be revived"). Its
    // request carries its assignment and not the start message, and stays held.
    await session.until("the subagent's first model request", () =>
      session.turns.some((turn) => {
        const messages = JSON.stringify(turn.request.body.messages ?? []);
        return messages.includes("Reply with done.") && !messages.includes(START_MESSAGE_JSON);
      })
        ? true
        : undefined
    );

    session.terminal.write("\x1ba");
    await session.shows("the agent hub with the running subagent", "Agent Hub", "1 running");
    session.terminal.write("\r");
    await session.shows("the subagent's view focused", "Viewing agent", "Reply with done.");

    session.terminal.write("\x1bn");
    await session.until("the re-claim's registration", () =>
      registrations >= 2 ? true : undefined
    );
    await session.shows(
      "the re-claim's refusal on screen after /new from the focused view",
      "New session started",
      "re-claiming the controller for this session failed",
      "capability revoked"
    );
  },
  CASE_TIMEOUT_MS
);
