import { afterEach, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Cleanup,
  type Interactive,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnInteractive,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
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
// the Envoy role claim needs. LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS
// server, as in pi-envoy's real-binary tests. A run without both skips, except on GitHub Actions,
// where the pi-legion job supplies both (GITHUB_ACTIONS, not CI: agent harnesses on the devbox
// export CI=true).
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

/**
 * Starts the controller as `legion controller start` does: interactive Oh My Pi with the Envoy and
 * Legion extensions from this checkout, the controller's environment (LEGION_CONTROLLER, its
 * capability file, its start message, the daemon, Envoy and its NATS server) and
 * `CONTROLLER_ARGS`, against one stand-in: the gateway holds each Messages request until the case
 * answers it; the daemon answers its state and the controller's registration from the contract
 * fixtures and mints grants; Dispatch is unavailable; and the Envoy listener records the session
 * that sets a role and names it as that role's holder.
 */
async function startController(): Promise<ControllerSession> {
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
        LEGION_PROJECT: "legion",
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
