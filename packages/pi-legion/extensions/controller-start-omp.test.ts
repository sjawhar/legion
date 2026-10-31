import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  controllerToken,
  legionControllerNoticeSubject,
  ROLE_TOPIC_PREFIX,
  slackMentionTopic,
} from "@legion/contracts";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import {
  type Block,
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
import { connect, headers as natsHeaders, StringCodec } from "nats";
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
// cases also send the controller cards and notices. The controller the daemon launches, which runs
// Oh My Pi in RPC mode and gets its start message as the daemon's prompt, runs against the same
// stand-in (`startDaemonController`). LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the
// NATS server, as in pi-envoy's real-binary tests. A run without both skips, except on GitHub
// Actions, where the pi-legion job supplies both (GITHUB_ACTIONS, not CI: agent harnesses on the
// devbox export CI=true). The file imports the real `nats` module, which extensions/legion.test.ts
// replaces for its whole process, so it runs in a process of its own (the package's `test`
// script).
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

/** The project the cases' controller serves: the daemon state fixtures' `legion`. */
const PROJECT = "legion";
/**
 * A Slack channel of the project, as the daemon names the controller it launches its channels
 * (LEGION_SLACK_CHANNELS), and the team whose webhook publishes a mention in it. The controller
 * registers the channel's mention topic (`slackMentionTopic`,
 * `notifications.slack.*.<channel>.mention`); the stream stores a mention on the team's own
 * subject, which that topic covers.
 */
const SLACK_CHANNEL = "C0CONTROLLER";
const SLACK_TEAM = "T0LEGION";
/**
 * The stream the stand-in listener names in its registration answers and in the relay frames it
 * sends: one stream for the whole case, so a frame never reads as a stream the session's resume
 * point does not name (`ConsumerDelivery.answered`).
 */
const STAND_IN_STREAM_ID = "controller-start-stream";

/**
 * The extensions every controller loads. The Envoy entry is the sibling plugin's: the controller
 * loads both, and the Legion entry refuses to run without it.
 */
const CONTROLLER_EXTENSIONS = [
  path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
  path.resolve(import.meta.dir, "legion.ts"),
];

/**
 * What pi-envoy's wake (src/card-wake.ts, `CardWake`) says, in any count, and for the one card the
 * startup cases send.
 */
const WAKE_TEXT = /Envoy messages? above arrived with no turn to read (it|them)/;
const WAKE_ONE = "1 Envoy message above arrived with no turn to read it";

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

/** The one transcript Oh My Pi wrote under `sessions`, line by line; none until the file exists. */
async function transcriptLines(sessions: string): Promise<string[]> {
  const files = (await readdir(sessions, { recursive: true })).filter((file) =>
    file.endsWith(".jsonl")
  );
  if (files.length === 0) return [];
  if (files.length !== 1) throw new Error(`want one transcript under ${sessions}, found ${files}`);
  return (await readFile(path.join(sessions, files[0] ?? ""), "utf8")).split("\n").filter(Boolean);
}

/** Its message entries, in order. */
async function transcriptMessages(sessions: string): Promise<MessageEntry[]> {
  return (await transcriptLines(sessions)).flatMap((line) => {
    const entry = MessageEntrySchema.safeParse(JSON.parse(line));
    return entry.success ? [entry.data] : [];
  });
}

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
  /**
   * Polls `probe` until it returns a value, failing at the deadline (`withinMs` from now, when a
   * case's own bound is the property under test) or when omp exits.
   */
  until<T>(
    what: string,
    probe: () => T | undefined | Promise<T | undefined>,
    withinMs?: number
  ): Promise<T>;
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

/** The stand-in route `STARTUP_HOLD_EXTENSION` requests. */
const STARTUP_HOLD_ROUTE = "/test/startup-hold";

/**
 * A stand-in for whatever of Oh My Pi's startup runs after the Legion entry's `session_start`,
 * loaded after both plugins: its own `session_start` awaits the stand-in's `STARTUP_HOLD_ROUTE`,
 * which a case holds (`StandinHooks.startupHold`). Oh My Pi runs each extension's `session_start`
 * in load order, awaiting each, so while it is held the Envoy entry has opened the session's agent
 * subject and the Legion entry has marked the start turn, and Oh My Pi is still inside its startup:
 * `/legion-controller-start` has not run.
 */
const STARTUP_HOLD_EXTENSION = `export default function startupHold(pi) {
  pi.on("session_start", async () => {
    await fetch(process.env.LEGION_TEST_STARTUP_HOLD_URL);
  });
}
`;

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
   * controller topic's NATS subscription (`subscribeNotice`), which the claim awaits. A promise
   * holds that registration until it settles.
   */
  readonly onNoticeInterest?: (sid: string) => void | Promise<void>;
  /** Called with the session id each Envoy listener request names, as it arrives. */
  readonly onListener?: (sid: string) => void;
  /** Oh My Pi's `keybindings.yml` for the session, written into its agent directory. */
  readonly keybindings?: string;
  /**
   * Loads `STARTUP_HOLD_EXTENSION` after both plugins and holds its request until the case
   * releases this hold.
   */
  readonly startupHold?: Hold;
  /**
   * Holds the daemon-launched controller's `POST /legion/v1/claims/ready`, the last step of its
   * claim in `session_start`, sent once it holds the role and the controller topic, with the
   * start-turn mark set sent right before it.
   */
  readonly ready?: Hold;
  /**
   * Called with the session's id and the topics of each listener registration
   * (`/v1/interests/subscribe`) as it arrives. The Response it answers with, when it gives one,
   * is the listener's own in place of the interest the stand-in would give: a refusal there is
   * the listener unable to give the session a consumer, which leaves the session receiving
   * nothing at all until a later registration is accepted (`sendRegistration` in
   * packages/pi-envoy/extensions/envoy.ts).
   */
  readonly onRegistration?: (sid: string, topics: readonly string[]) => Response | undefined;
  /**
   * Called with the topics of each removal from the registry (`/v1/interests/unsubscribe`): what
   * a role's end takes out of the session's registration, and so out of its consumer's filter.
   */
  readonly onUnsubscribe?: (topics: readonly string[]) => void;
  /**
   * The session the listener names as the controller role's live holder from now on: while it
   * answers one, the role read names that session and a claim of the role is refused 409 with
   * it, as the listener refuses a claim of a role another live session holds. The heartbeat's
   * re-assertion then ends the role here (`reassertRole` in
   * packages/pi-envoy/src/registration-state.ts).
   */
  readonly roleHeldBy?: () => string | undefined;
}

/** A controller's scratch root, and the stand-in it runs against. */
interface ControllerStandin {
  readonly binary: string;
  readonly nats: string;
  readonly root: string;
  readonly home: string;
  readonly workspace: string;
  readonly sessions: string;
  /** The controller's LEGION_STATE_DIR. */
  readonly state: string;
  readonly base: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: Turn[];
}

/**
 * Serves the controller's stand-in and writes the profile that points Oh My Pi at it: the gateway
 * holds each Messages request until the case answers it; the daemon answers its state from
 * `stateFixture` and the controller's registration from the contract fixtures (or
 * `hooks.register`'s answer), acknowledges a ready and mints grants; Dispatch is unavailable; and
 * the Envoy listener records the session that sets a role and names it as that role's holder,
 * calling `hooks`' role-claim and notice-interest callbacks as those requests arrive. `hooks`'
 * holds hold the requests they name.
 */
async function serveController(
  stateFixture: string,
  hooks: StandinHooks
): Promise<ControllerStandin> {
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
    if (url.pathname === STARTUP_HOLD_ROUTE) {
      await hooks.startupHold?.wait();
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/state") return Response.json(daemonState);
    if (url.pathname === "/legion/v1/claims/register") {
      return hooks.register?.(body) ?? Response.json(registered);
    }
    if (url.pathname === "/legion/v1/claims/ready") {
      await hooks.ready?.wait();
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
    hooks.onListener?.(sid);
    // The role the controller holds is another live session's from the moment a case names one:
    // the read below answers that session, and a claim of the role is refused with it, as the
    // listener refuses a claim of a role a live session holds.
    const otherHolder = hooks.roleHeldBy?.();
    if (url.pathname === "/v1/roles/set") {
      if (otherHolder !== undefined && body.role === registered.claimToken) {
        return Response.json(
          {
            error: `role ${registered.claimToken} is held by ${otherHolder}`,
            role: body.role,
            holder: otherHolder,
          },
          { status: 409 }
        );
      }
      if (body.role === registered.claimToken && roleHolder === "") {
        setImmediate(() => hooks.onRoleClaim?.(sid));
        noticeInterestDue = true;
      }
      roleHolder = sid;
    }
    const topics = Array.isArray(body.topics)
      ? body.topics.filter((topic): topic is string => typeof topic === "string")
      : [];
    if (url.pathname === "/v1/interests/unsubscribe") hooks.onUnsubscribe?.(topics);
    if (url.pathname === "/v1/interests/subscribe") {
      if (noticeInterestDue) {
        noticeInterestDue = false;
        await hooks.onNoticeInterest?.(sid);
      }
      const answer = hooks.onRegistration?.(sid, topics);
      if (answer !== undefined) return answer;
    }
    if (url.pathname === `/v1/roles/${registered.claimToken}`) {
      return Response.json({
        role: registered.claimToken,
        holder: otherHolder ?? roleHolder,
        last_seen: 1,
      });
    }
    return Response.json({
      session_id: sid === "" ? roleHolder : sid,
      machine_id: "controller-start-machine",
      dir: workspace,
      topics: [],
      // A session takes its messages from its own consumer, which this listener relays on the
      // session's deliver subject (`controllerInbound`): a registration answered without this
      // leaves the plugin with no consumer and the session deaf, which is what it refuses.
      ...(url.pathname === "/v1/interests/subscribe"
        ? {
            delivery: {
              mode: "consumer",
              subject: deliverSubject(sid),
              stream_id: STAND_IN_STREAM_ID,
              gap: false,
            },
          }
        : {}),
    });
  });
  // A request still held at the end (an interrupted one among them) is answered, so the server
  // stops at once.
  cleanup.push(async () => {
    hooks.startupHold?.release();
    hooks.ready?.release();
    for (const turn of turns) turn.reply("Case over.");
  });
  await writeStandinProfile(home, base);
  if (hooks.keybindings !== undefined) {
    await writeFile(path.join(home, ".omp", "agent", "keybindings.yml"), hooks.keybindings);
  }
  return { binary, nats, root, home, workspace, sessions, state, base, turns };
}

/**
 * Starts the controller as `legion controller start` does: interactive Oh My Pi with the Envoy and
 * Legion extensions from this checkout (and `STARTUP_HOLD_EXTENSION` after them, for a case that
 * holds the startup), the controller's environment (LEGION_CONTROLLER, its capability file, its
 * start message, the daemon, Envoy and its NATS server) and `args` (`CONTROLLER_ARGS` unless a
 * case drops the start word), against the controller's stand-in (`serveController`) with `hooks`.
 */
async function startController(
  hooks: StandinHooks = {},
  args: readonly string[] = CONTROLLER_ARGS
): Promise<ControllerSession> {
  const { binary, nats, root, home, workspace, sessions, state, base, turns } =
    await serveController("state.json", hooks);
  const secretFile = path.join(root, "controller-secret");
  await writeFile(secretFile, "controller-start-capability\n", { mode: 0o600 });
  const startupHold = path.join(root, "startup-hold.ts");
  if (hooks.startupHold !== undefined) await writeFile(startupHold, STARTUP_HOLD_EXTENSION);

  const spawnedAt = Date.now();
  const terminal = spawnInteractive(
    binary,
    {
      extensions: [
        ...CONTROLLER_EXTENSIONS,
        ...(hooks.startupHold === undefined ? [] : [startupHold]),
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
        LEGION_TEST_STARTUP_HOLD_URL: `${base}${STARTUP_HOLD_ROUTE}`,
      },
      args,
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
    probe: () => T | undefined | Promise<T | undefined>,
    withinMs?: number
  ): Promise<T> => {
    // A wait gets its own step, and one that starts before Oh My Pi's startup could be over gets
    // until the startup deadline: the first wait of every case spans that startup. A case's own
    // bound replaces both.
    const deadline =
      withinMs === undefined
        ? Math.max(Date.now() + STEP_DEADLINE_MS, spawnedAt + STARTUP_DEADLINE_MS)
        : Date.now() + withinMs;
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

  let taken = 0;
  return {
    terminal,
    turns,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken]);
      taken += 1;
      return turn;
    },
    transcriptLines: () => transcriptLines(sessions),
    messages: () => transcriptMessages(sessions),
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
const RpcStateSchema = z.looseObject({ isStreaming: z.boolean(), isSettled: z.boolean() });

/** The controller the daemon launched, over its RPC stream, and what it sent the stand-in. */
interface DaemonController {
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
  /** Answers one RPC `get_state`, which Oh My Pi reads only once its startup is over. */
  state(): Promise<z.infer<typeof RpcStateSchema>>;
  /** Sends one RPC `abort`, as a Legion daemon interrupts a turn, and resolves on its answer. */
  abort(): Promise<void>;
  /** The one transcript's lines, raw; none until Oh My Pi writes the file. */
  transcriptLines(): Promise<string[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/**
 * Starts the controller as the daemon launches it (`controller: daemon`): `omp --mode rpc` with
 * both extensions and the pod's environment — its boot token's file in place of a capability, the
 * project's Slack channels (none unless `launch` names them), and no
 * LEGION_CONTROLLER_START_MESSAGE, since the daemon delivers the start message itself once the
 * controller reports ready — against the controller's stand-in (`serveController`), which answers
 * the daemon's state while the daemon holds the controller's claim (internal/projection's
 * golden), with `hooks`. `launch.heartbeatMs` is Envoy's registration heartbeat
 * (ENVOY_HEARTBEAT_MS, 90 s by default), which a case that watches a pass shortens.
 */
async function startDaemonController(
  hooks: StandinHooks,
  launch: { readonly slackChannels?: readonly string[]; readonly heartbeatMs?: number } = {}
): Promise<DaemonController> {
  const { binary, nats, root, home, workspace, sessions, state, base, turns } =
    await serveController("state-controller-claim.json", hooks);
  const bootFile = path.join(root, "LEGION_BOOT_TOKEN");
  await writeFile(bootFile, "launch-boot-token\n", { mode: 0o600 });

  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  const closed: { error?: unknown } = {};
  const spawnedAt = Date.now();
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
        LEGION_SLACK_CHANNELS: (launch.slackChannels ?? []).join(","),
        ...(launch.heartbeatMs === undefined
          ? {}
          : { ENVOY_HEARTBEAT_MS: String(launch.heartbeatMs) }),
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
    // As in `startController`: the first wait spans Oh My Pi's startup.
    const deadline = Math.max(Date.now() + STEP_DEADLINE_MS, spawnedAt + STARTUP_DEADLINE_MS);
    for (;;) {
      if (closed.error !== undefined) throw closed.error;
      const value = await probe();
      if (value !== undefined) return value;
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
      // As in `startController`: what the cases wait on has no event the test can await.
      await Bun.sleep(25);
    }
  };

  // One RPC command, answered by its response frame; a refusal throws.
  const command = async (body: Record<string, unknown>): Promise<Record<string, unknown>> => {
    const id = randomUUID();
    const waiting = Promise.withResolvers<Record<string, unknown>>();
    responses.set(id, waiting);
    rpc.send({ ...body, id });
    const response = await Promise.race([waiting.promise, rpc.closed]);
    if (response.success !== true) {
      throw new Error(`omp refused ${String(body.type)}: ${JSON.stringify(response)}`);
    }
    return response;
  };

  let taken = 0;
  return {
    turns,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken]);
      taken += 1;
      return turn;
    },
    deliver(message) {
      rpc.send({ type: "prompt", id: randomUUID(), message });
    },
    async state() {
      return RpcStateSchema.parse((await command({ type: "get_state" })).data);
    },
    async abort() {
      await command({ type: "abort" });
    },
    transcriptLines: () => transcriptLines(sessions),
    until,
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

/** A card in flight: the card, and the server's acknowledgement that it took the frame. */
interface SentCard {
  readonly card: Card;
  readonly taken: Promise<unknown>;
}

/**
 * One message the stream stored for a session: its summary, and the relay of that one frame.
 * `deliver` sends it on the session's deliver subject under the sequence and the stored time the
 * stream holds it at, and resolves once the session has taken it; a second call is the listener's
 * redelivery of that same stored frame, which `delivered` counts (the ack floor a restarted relay
 * resumes from). A message is stored where it is minted: one stored while nothing relays to the
 * session carries that moment, which is what the session weighs its frame against
 * (`ConsumerDelivery.followedBefore`).
 */
interface StoredMessage {
  readonly summary: string;
  deliver(sid: string, delivered?: number): Promise<unknown>;
}

/**
 * What reaches a controller session over the real NATS server. A session takes its messages from
 * its own consumer and from nothing else (INFRA-771), so every frame here goes where the Envoy
 * listener's relay would send it: the session's deliver subject
 * (`notifications.envoy.deliver.<session>`), under the headers that name the subject the stream
 * stored it on. A card is the listener's forward of a role-lane message to the session's agent
 * subject; a notice is what the daemon's controller-notice executor posts on the controller topic
 * (packages/daemon/internal/daemon/outbox.go `controllerNotice`, the listener's
 * `messageEnvelope` in packages/envoy/cmd/listener/api.go), which only a session holding that
 * project's controller role registers, and so only its consumer carries; a mention is what
 * Envoy's Slack webhook publishes for an app mention in a channel, which only that controller's
 * launch names.
 */
interface ControllerInbound {
  /**
   * Relays one card to session `sid` and returns it with the session's answer that it took the
   * frame: the forwarded envelope keeps the role topic, and the frame travels the session's
   * deliver subject, which it answers `received` on as it decodes it.
   */
  card(sid: string, label: string): SentCard;
  /**
   * Relays `card`'s frame to `sid` again, as the listener redelivers a frame, and resolves once
   * the session has taken the repeat. The session delivers a repeat nowhere (its dedupe), so this
   * adds no message: it only puts another frame behind the ones relayed before it, which the
   * session takes in that order.
   */
  repeat(sid: string, card: Card): Promise<unknown>;
  /**
   * Relays one triage notice on the controller topic to session `sid` and returns its summary
   * (`triage on <issue>`, the daemon's `noticeSummary`), whose issue key no other notice shares.
   * The session receives it only while its registration carries that lane, which its controller
   * role is what allows.
   */
  notice(sid: string): string;
  /** The same notice, stored and not relayed: the stream holds it until `deliver`. */
  storeNotice(): StoredMessage;
  /**
   * One app mention of `channel`, stored and not relayed, on the subject Envoy's Slack webhook
   * publishes it on (`notifications.slack.<team>.<channel>.mention`) — the concrete subject the
   * controller's registered topic (`slackMentionTopic`) covers.
   */
  storeMention(channel: string): StoredMessage;
}

async function controllerInbound(nats: string): Promise<ControllerInbound> {
  const connection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  const noticeSubject = legionControllerNoticeSubject(PROJECT);
  // The stream's sequence: one stream for the whole case, so every frame of it is ordered by the
  // sequence it was stored at, and a frame the session has written already is recognised by it.
  let sequence = 0;
  /** The sequence the stream stores the next message at. */
  const nextSequence = (): number => {
    sequence += 1;
    return sequence;
  };
  /**
   * One relay frame, as the listener's relay of the session's consumer sends it: the stored
   * subject, sequence, stream and time in its headers (`decodeRelayFrame`), on the session's
   * deliver subject. The request's answer is the session's own `received`, which it sends as it
   * decodes the frame; until the session has subscribed that subject the server reports no
   * responder, so the frame is sent again, as the one frame it is. A frame with no stamp of its
   * own is stored as it is relayed, at the sequence after the last.
   */
  const relay = async (
    sid: string,
    subject: string,
    frame: Uint8Array,
    stamp: { readonly seq: number; readonly storedAt: string; readonly delivered: number } = {
      seq: nextSequence(),
      storedAt: new Date().toISOString(),
      delivered: 1,
    }
  ): Promise<unknown> => {
    const deadline = Date.now() + 30_000;
    const header = natsHeaders();
    header.set(RelayHeader.subject, subject);
    header.set(RelayHeader.streamSeq, String(stamp.seq));
    header.set(RelayHeader.streamID, STAND_IN_STREAM_ID);
    header.set(RelayHeader.delivered, String(stamp.delivered));
    header.set(RelayHeader.storedAt, stamp.storedAt);
    for (;;) {
      try {
        return await connection.request(deliverSubject(sid), frame, {
          headers: header,
          timeout: 2_000,
        });
      } catch (error) {
        if (Date.now() > deadline) {
          throw new Error(`${sid} took no relay frame on ${subject}: ${String(error)}`);
        }
        // A real clock, deliberately: a session that has not yet subscribed its deliver subject
        // emits no signal here — the server only answers the next send with no responder.
        await Bun.sleep(100);
      }
    }
  };
  /** One message the stream stores now on `subject`, its envelope's summary and topic set. */
  const store = (
    subject: string,
    summary: string,
    envelope: Record<string, unknown>
  ): StoredMessage => {
    const stored = { seq: nextSequence(), storedAt: new Date().toISOString() };
    const frame = codec.encode(
      JSON.stringify({ ...envelope, topic: subject, payload_summary: summary })
    );
    return {
      summary,
      deliver: (sid, delivered = 1) => relay(sid, subject, frame, { ...stored, delivered }),
    };
  };
  const storeNotice = (): StoredMessage =>
    store(noticeSubject, `triage on LEGION-${100_000 + Math.floor(Math.random() * 900_000)}`, {
      event_id: randomUUID(),
      // The daemon names no source, so the listener's default.
      source: "agent",
      source_event_id: randomUUID(),
      dedupe_key: `legion-outbox:${randomUUID()}`,
      issued_at: Date.now(),
      payload: JSON.stringify({ kind: "triage" }),
      trace_id: randomUUID(),
    });
  const storeMention = (channel: string): StoredMessage =>
    store(
      `notifications.slack.${SLACK_TEAM}.${channel}.mention`,
      `@controller MENTION-${randomUUID().slice(0, 8)}`,
      {
        event_id: randomUUID(),
        source: "slack",
        source_event_id: `slack-${randomUUID()}`,
        dedupe_key: `slack:${randomUUID()}`,
        issued_at: Date.now(),
        trace_id: randomUUID(),
      }
    );
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
      return {
        card: { summary, frame },
        taken: relay(sid, `notifications.agent.${sid}`, frame),
      };
    },
    repeat: (sid, card) => relay(sid, `notifications.agent.${sid}`, card.frame),
    notice(sid) {
      const message = storeNotice();
      void message.deliver(sid);
      return message.summary;
    },
    storeNotice,
    storeMention,
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
// it, as a steer into the start turn or in a later turn. The card's frame and the repeat behind it
// are taken by the session's one connection in the order they were published.
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
      onNoticeInterest: (claimed) => {
        notice = inbound.notice(claimed);
      },
    });
    const { card, taken } = await session.until("the controller role's claim", () => sent);
    await taken;
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
    // Oh My Pi focuses only a subagent whose session is attached: Enter in the hub before that
    // reaches a running entry with no session ("cannot be revived"). The subagent's own first
    // model request comes from that session, so the case waits for it. It carries the assignment
    // and not the start message, and stays held. Oh My Pi's task-label request also carries the
    // assignment, but it is sent at spawn, before the session is attached (task/executor.ts,
    // task/label.ts), and its system prompt is the label prompt, so it does not count.
    await session.until("the subagent's own first model request", () =>
      session.turns.some((turn) => {
        const messages = JSON.stringify(turn.request.body.messages ?? []);
        const system = JSON.stringify(turn.request.body.system ?? null);
        return (
          messages.includes("Reply with done.") &&
          !messages.includes(START_MESSAGE_JSON) &&
          !system.includes("Label the delegated work")
        );
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

// The start command's claim sends the start message, marking the start turn sent, then claims the
// role and subscribes to the controller topic, so a card the listener forwards on the role's lane
// and a daemon notice on the topic reach the session while the start turn runs. The stand-in holds
// the registration that follows the topic's subscription, which the claim awaits, and the start
// turn's model request, until both have reached the session: the card's frame and the repeat
// behind it are taken by the session's one connection in the order they were published. The
// start turn's first request carries the start message alone; the card and the notice wait for it
// and each reaches the model in a request after it, behind the start message.
test.skipIf(skip)(
  "a card and a Legion notice that arrive while the start command's claim runs wait for the start turn, and each still gets a turn after it",
  async () => {
    const noticeRegistration = newHold();
    let sid: string | undefined;
    const session = await startController({
      onRoleClaim: (claimed) => {
        sid = claimed;
      },
      onNoticeInterest: () => noticeRegistration.wait(),
    });
    await session.until("the registration after the controller topic's subscription", () =>
      noticeRegistration.reached() ? true : undefined
    );
    const claimed = await session.until("the controller role's claim", () => sid);
    const start = await session.nextTurn("the start turn's model request");
    const inbound = await controllerInbound(setup().nats);
    const { card, taken } = inbound.card(claimed, "claim");
    await taken;
    const notice = inbound.notice(claimed);
    await inbound.repeat(claimed, card);
    expect(session.turns).toHaveLength(1);
    noticeRegistration.release();

    const startMessages = Array.isArray(start.request.body.messages)
      ? start.request.body.messages
      : [];
    expect(startMessages).toHaveLength(1);
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    let reply = "Start procedure done.";
    start.reply(reply);

    // Every later request is answered, in order, until the session settles on the last reply with
    // both having reached the model.
    const reached = (): boolean =>
      session.turns.some(({ request }) => conversation(request).includes(card.summary)) &&
      session.turns.some(({ request }) => conversation(request).includes(notice));
    for (let answered = 1; ; answered += 1) {
      const next = await session.until(
        "the next model request, or the session settled with the card and the notice read",
        async () => {
          if (session.turns.length > answered) return "request";
          return reached() && (await isSettledOn(session, reply)) ? "settled" : undefined;
        }
      );
      if (next === "settled") break;
      reply = `Reply ${answered + 1}.`;
      (await session.nextTurn(`model request ${answered + 1}`)).reply(reply);
    }

    for (const { request } of session.turns.slice(1)) {
      const held = conversation(request);
      const startAt = held.indexOf(START_MESSAGE_JSON);
      expect(startAt).toBeGreaterThanOrEqual(0);
      for (const text of [card.summary, notice]) {
        if (held.includes(text)) expect(held.indexOf(text)).toBeGreaterThan(startAt);
      }
    }
    expect(reached()).toBe(true);
  },
  CASE_TIMEOUT_MS
);

// The daemon's controller reports ready once it holds the role and the controller topic, and the
// daemon writes its start prompt then, which RPC mode reads only once Oh My Pi's startup is over. A
// card that reached the session during that startup is a follow-up that starts no turn, and the
// wake for it fired once the host was idle after the startup, which can be before the prompt
// arrives, so the wake's turn was the first. The claim marks the start turn sent right before its
// ready (src/controller-session.ts), which holds the wake behind the start turn. The stand-in holds
// that ready, inside the claim `session_start` awaits, while the card arrives. RPC mode answers
// its first command only once its startup drain has returned, when the card's follow-up is in the
// context and the check its `message_end` armed is on the host's 0 ms timer; the second
// `get_state`, written after that answer, is read on a later turn of omp's event loop, after that
// timer ran, so a wake that check sent has started its turn by then. Only then does the case
// deliver the start prompt.
test.skipIf(skip)(
  "the daemon-launched controller's start prompt is its first turn, and a startup card's wake runs after it",
  async () => {
    const ready = newHold();
    let sid: string | undefined;
    const session = await startDaemonController({
      ready,
      onRoleClaim: (claimed) => {
        sid = claimed;
      },
    });
    await session.until("the controller's ready", () => (ready.reached() ? true : undefined));
    const claimed = await session.until("the controller role's claim", () => sid);
    const inbound = await controllerInbound(setup().nats);
    const { card, taken } = inbound.card(claimed, "daemon-startup");
    await taken;
    await inbound.repeat(claimed, card);
    expect(session.turns).toHaveLength(0);
    ready.release();

    await session.state();
    expect((await session.state()).isStreaming).toBe(false);
    expect(session.turns).toHaveLength(0);
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
    await session.until("the session to settle", async () =>
      (await session.state()).isSettled ? true : undefined
    );
    expect(session.turns).toHaveLength(2);
  },
  CASE_TIMEOUT_MS
);

// An interrupt of the start turn: RPC `abort`, as the daemon interrupts a turn (the terminal's Esc
// ends the run with the same "Interrupted by user" reply), while the start turn's model request is
// held. The startup card that reached the session while its ready was held is owed a wake after
// the start turn, and that wake, a next-turn message with `triggerTurn`, would start the turn the
// interrupt stopped: the card stays folded for the next turn instead. So the session settles with
// no model request after the start turn's, and a probe prompt is the next request, carrying the
// card and no wake. A `get_state` after the settle is read once the check the start turn's
// `agent_end` armed has run (as in the case above), so a wake that check sent is under way before
// the probe goes.
test.skipIf(skip)(
  "an RPC abort of the daemon-launched controller's start turn leaves its startup card folded, with no wake",
  async () => {
    const ready = newHold();
    let sid: string | undefined;
    const session = await startDaemonController({
      ready,
      onRoleClaim: (claimed) => {
        sid = claimed;
      },
    });
    await session.until("the controller's ready", () => (ready.reached() ? true : undefined));
    const claimed = await session.until("the controller role's claim", () => sid);
    const inbound = await controllerInbound(setup().nats);
    const { card, taken } = inbound.card(claimed, "interrupted-start");
    await taken;
    await inbound.repeat(claimed, card);
    ready.release();

    await session.state();
    expect((await session.state()).isStreaming).toBe(false);
    expect(session.turns).toHaveLength(0);
    session.deliver(START_MESSAGE);
    const start = await session.nextTurn("the start turn's model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    expect(conversation(start.request)).toContain(card.summary);
    await session.abort();

    const noRequestAfterStart = (): undefined => {
      const extra = session.turns[1];
      if (extra !== undefined) {
        throw new Error(`a model request came before the probe, on ${lastMessage(extra.request)}`);
      }
      return undefined;
    };
    await session.until("the interrupted session to settle", async () => {
      noRequestAfterStart();
      return (await session.state()).isSettled ? true : undefined;
    });
    await session.state();
    noRequestAfterStart();

    const prompt = `PROBE-${randomUUID().slice(0, 8)}`;
    session.deliver(prompt);
    const probe = await session.nextTurn("the probe's model request");
    expect(lastMessage(probe.request)).toContain(prompt);
    expect(conversation(probe.request)).toContain(card.summary);
    expect(conversation(probe.request)).not.toMatch(WAKE_TEXT);
    probe.reply("Probe answered.");
    await session.until("the session to settle", async () =>
      (await session.state()).isSettled ? true : undefined
    );
    expect(session.turns).toHaveLength(2);
  },
  CASE_TIMEOUT_MS
);

// The operator's controller claims and sends its start message only from
// `/legion-controller-start`, after Oh My Pi's startup, while the Envoy entry opens the session's
// agent subject at its own `session_start`. A card that reaches that subject in between found an
// idle terminal and started its own turn, inside the startup drain, ahead of the start message.
// The Legion entry's `session_start` marks the start turn unsent (src/controller-session.ts
// `handleSessionStart`), so the card goes into the context with no turn and its wake waits for the
// start turn. The stand-in holds `STARTUP_HOLD_EXTENSION`'s request, after both plugins'
// `session_start`, while the card arrives on the session's agent subject, forwarded as the listener
// forwards a card on the role's lane, and the repeat behind it shows that its delivery, one frame
// at a time, has handed the card to the host.
test.skipIf(skip)(
  "a card that reaches the operator's controller while Oh My Pi starts, before /legion-controller-start runs, waits for the start turn",
  async () => {
    const startupHold = newHold();
    let sid: string | undefined;
    const session = await startController({
      startupHold,
      onListener: (listened) => {
        if (listened !== "") sid ??= listened;
      },
    });
    await session.until("the startup hold", () => (startupHold.reached() ? true : undefined));
    const opened = await session.until("the session's listener registration", () => sid);
    const inbound = await controllerInbound(setup().nats);
    const { card, taken } = inbound.card(opened, "startup");
    await taken;
    await inbound.repeat(opened, card);
    expect(session.turns).toHaveLength(0);
    startupHold.release();

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
    await settledOn(session, "Read the startup card.");
    expect(session.turns).toHaveLength(2);
    const delivered = (await session.transcriptLines()).filter(
      (line) => line.includes('"custom_message"') && line.includes(card.summary)
    );
    expect(delivered).toHaveLength(1);
    expect(delivered[0]).toContain('"wake":"startup"');
  },
  CASE_TIMEOUT_MS
);

// The start-turn mark owes every startup card one wake after the start turn, since the start
// turn's reply ran the start procedure rather than reading the card. A turn after the start turn
// that has the card in its context is that one turn: here, the one a card published while the
// start turn runs gets. A wake after it would start a further turn telling the controller that
// cards it already read "arrived with no turn to read them".
test.skipIf(skip)(
  "a startup card that a turn after the start turn reads gets no second wake",
  async () => {
    const startupHold = newHold();
    let sid: string | undefined;
    const session = await startController({
      startupHold,
      onListener: (listened) => {
        if (listened !== "") sid ??= listened;
      },
    });
    await session.until("the startup hold", () => (startupHold.reached() ? true : undefined));
    const opened = await session.until("the session's listener registration", () => sid);
    const inbound = await controllerInbound(setup().nats);
    const early = inbound.card(opened, "startup");
    await early.taken;
    await inbound.repeat(opened, early.card);
    startupHold.release();

    const start = await session.nextTurn("the start turn's model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    const during = inbound.card(opened, "during-start");
    await during.taken;
    await inbound.repeat(opened, during.card);
    start.reply("Start procedure done.");

    const next = await session.nextTurn("the model request that reads the card sent meanwhile");
    const read = conversation(next.request);
    expect(read).toContain(early.card.summary);
    expect(read).toContain(during.card.summary);
    next.reply("Read both cards.");
    await settledOn(session, "Read both cards.");
    // A real clock, as in "the operator sees a claim refusal": a wake that is not sent emits no
    // signal to await, so only a quiet window with no further Messages request shows its absence.
    await Bun.sleep(3_000);
    expect(session.turns.slice(2).map(({ request }) => lastMessage(request))).toEqual([]);
  },
  CASE_TIMEOUT_MS
);

// The operator's controller marks its start unsent at `session_start`, and while the mark holds,
// a startup card waits for the start turn. Two ways leave that start unsent for good: the start
// claim's registration that never answers (the daemon unreachable mid-request), and a session
// launched without the start word on its command line. A card that reached the idle session must
// still get a turn of its own, without waiting for the operator to type; the bound here is three
// times pi-envoy's STARTUP_TURN_CAP_MS (30 s).
for (const variant of ["the start claim never answers", "the start command never runs"] as const) {
  test.skipIf(skip)(
    `a startup card held by an unsent start mark gets its turn when ${variant}`,
    async () => {
      const startupHold = newHold();
      const registration = newHold();
      let sid: string | undefined;
      const session = await startController(
        {
          startupHold,
          onListener: (listened) => {
            if (listened !== "") sid ??= listened;
          },
          register: async () => {
            await registration.wait();
            return Response.json({ error: "case over" }, { status: 503 });
          },
        },
        variant === "the start command never runs" ? CONTROLLER_ARGS.slice(0, -1) : CONTROLLER_ARGS
      );
      // Pushed after the stand-in's own cleanup, so it runs first: the stand-in's `server.stop`
      // waits for the held register handler, which ends only on this release.
      cleanup.push(async () => registration.release());
      await session.until("the startup hold", () => (startupHold.reached() ? true : undefined));
      const opened = await session.until("the session's listener registration", () => sid);
      const inbound = await controllerInbound(setup().nats);
      const { card, taken } = inbound.card(opened, "held");
      await taken;
      await inbound.repeat(opened, card);
      startupHold.release();

      // A real clock: the bound is the property under test.
      const first = await session.until(
        "the startup card's model request, within 90 s",
        () => session.turns[0],
        90_000
      );
      expect(conversation(first.request)).toContain(card.summary);
      expect(lastMessage(first.request)).toContain(WAKE_ONE);
    },
    CASE_TIMEOUT_MS
  );
}

// The real pair's proof of the controller's own lanes (INFRA-771): a session receives what its
// own consumer carries and nothing else, and the controller role is what puts its project's
// notice lane (`notifications.legion.<project>.controller`) and its Slack channels' mention
// topics into that consumer's filter. The controller the daemon launches is the one whose launch
// names channels (LEGION_SLACK_CHANNELS), so it is the one here, with a 2 s registration
// heartbeat so that the case watches its passes. Three things, in one session's life:
//   - a notice stored while the session's registration had lapsed — the listener able to give it
//     no consumer, so nothing at all reached it — is read once when the registration is back,
//     the relay's redelivery of that same stored frame included;
//   - a mention of the channel its launch names reaches it;
//   - once the role ends (the heartbeat's re-assertion refused by a listener that names another
//     live holder), the registration that follows carries neither topic, and the session's own
//     consumer drops a frame stored on either, while a card on its agent subject still reaches
//     it, so each of the two had its turn at the session first.
// Stopping the session and starting it again is the one thing this rig cannot do — Oh My Pi
// mints a new session id, and the daemon's controller is not launched with `--resume` — so the
// lapse stands in for it: what the listener holds for the session is gone and comes back, which
// is the state a stopped session leaves behind, and the frame that waited carries the time it
// was stored at, which is what the session weighs it against. A mocked bus covers the same three
// things at the plugin's own level in packages/pi-envoy/extensions/envoy.test.ts ("a controller
// session registers its project's notice lane and its Slack mention topics, and takes a notice
// on the lane").
test.skipIf(skip)(
  "the controller's notice lane and its channel's mentions reach the session through its consumer, and stop once its role ends",
  async () => {
    const lane = legionControllerNoticeSubject(PROJECT);
    const mentions = slackMentionTopic(SLACK_CHANNEL);
    const inbound = await controllerInbound(setup().nats);
    /** The topics of every registration the stand-in accepted, in order. */
    const registered: string[][] = [];
    /** Every topic a removal from the registry named (`/v1/interests/unsubscribe`). */
    const unregistered: string[] = [];
    /** How many registrations the listener refused while the session had no consumer. */
    let refusals = 0;
    let lapsed = false;
    let otherHolder: string | undefined;
    let sid: string | undefined;
    const session = await startDaemonController(
      {
        onRoleClaim: (claimed) => {
          sid = claimed;
        },
        onRegistration: (_claimed, topics) => {
          if (!lapsed) {
            registered.push([...topics]);
            return undefined;
          }
          refusals += 1;
          return Response.json(
            { error: "consumer_error: this session has no consumer" },
            { status: 503 }
          );
        },
        onUnsubscribe: (topics) => unregistered.push(...topics),
        roleHeldBy: () => otherHolder,
      },
      { slackChannels: [SLACK_CHANNEL], heartbeatMs: 2_000 }
    );
    const claimed = await session.until("the controller role's claim", () => sid);
    await session.until("the registration that carries the lane and the channel", () =>
      registered.some((topics) => topics.includes(lane) && topics.includes(mentions))
        ? true
        : undefined
    );

    // The daemon's start prompt and the turn it runs: the session's startup phase ends there, so
    // what arrives after it is an idle session's to read.
    session.deliver(START_MESSAGE);
    const start = await session.nextTurn("the start turn's model request");
    expect(lastMessage(start.request)).toContain(START_MESSAGE_JSON);
    start.reply("Start procedure done.");
    let answered = 1;
    /**
     * Answers every model request in order until the session has settled with `done` true: a
     * turn the session starts on its own (an inbound message, a wake, the notice a refused
     * registration shows) is answered like any other.
     */
    const settled = async (what: string, done: () => boolean = () => true): Promise<void> => {
      for (;;) {
        const next = await session.until(`the next model request, or ${what}`, async () => {
          if (session.turns.length > answered) return "request";
          return done() && (await session.state()).isSettled ? "settled" : undefined;
        });
        if (next === "settled") return;
        answered += 1;
        (await session.nextTurn(`model request ${answered}`)).reply(`Reply ${answered}.`);
      }
    };
    /** The same, until a request the session sent carries `text`: what it read. */
    const read = (what: string, text: string): Promise<void> =>
      settled(`the session settled having read ${what}`, () =>
        session.turns.some(({ request }) => conversation(request).includes(text))
      );
    await settled("the session settled after the start turn");

    // The listener can give this session no consumer: every registration from here is refused,
    // and nothing is relayed to it. The notice is stored inside that window, as the stream holds
    // what it stores while nothing reads the session's consumer.
    lapsed = true;
    await session.until("a registration refused while the session has no consumer", () =>
      refusals > 0 ? true : undefined
    );
    const notice = inbound.storeNotice();
    await session.until("a second refusal, with the notice stored behind it", () =>
      refusals > 1 ? true : undefined
    );
    lapsed = false;
    const lapsedUntil = registered.length;
    await session.until("the registration the listener accepts again", () =>
      registered.length > lapsedUntil ? true : undefined
    );

    // The relay restarts at its ack floor: the frame the stream held goes out, and the same
    // frame again behind it, which the session has written by then.
    await notice.deliver(claimed);
    await notice.deliver(claimed, 2);
    await read("the notice stored while its registration had lapsed", notice.summary);
    const mention = inbound.storeMention(SLACK_CHANNEL);
    await mention.deliver(claimed);
    await read("the mention of its Slack channel", mention.summary);

    /** Every transcript line that holds `text`: what a reader of the session sees of it. */
    const written = async (text: string): Promise<string[]> =>
      (await session.transcriptLines()).filter((line) => line.includes(text));
    // Each was handed to the session once, as the delivery the host records (a card's entry).
    const noticeWritten = await written(notice.summary);
    expect(noticeWritten).toHaveLength(1);
    expect(noticeWritten[0]).toContain('"custom_message"');
    const mentionWritten = await written(mention.summary);
    expect(mentionWritten).toHaveLength(1);
    expect(mentionWritten[0]).toContain('"custom_message"');

    // The role ends: the listener names another live session as its holder, the heartbeat's
    // re-assertion is refused, and the subjects the controller opened for the role are closed
    // and taken out of the registry, so the registration after that carries neither.
    otherHolder = "ses-controller-start-other";
    await session.until("the lane and the mention topic taken out of the registry", () =>
      unregistered.includes(lane) && unregistered.includes(mentions) ? true : undefined
    );
    const ended = registered.length;
    const after = await session.until("a registration that carries neither topic", () =>
      registered.slice(ended).find((topics) => !topics.includes(lane) && !topics.includes(mentions))
    );
    expect(after).toContain(`notifications.agent.${claimed}`);

    // What the stream stores on either subject still reaches the session's deliver subject, as a
    // listener that went on relaying them would send it, and the session drops both itself: it
    // follows neither topic now. The card behind them, on the agent subject it does still follow,
    // is read, so each of the two reached the session's consumer before it.
    const afterNotice = inbound.storeNotice();
    await afterNotice.deliver(claimed);
    const afterMention = inbound.storeMention(SLACK_CHANNEL);
    await afterMention.deliver(claimed);
    const { card, taken } = inbound.card(claimed, "after-role");
    await taken;
    await read("the card sent after the role ended", card.summary);
    expect(await written(afterNotice.summary)).toEqual([]);
    expect(await written(afterMention.summary)).toEqual([]);
    for (const { request } of session.turns) {
      expect(conversation(request)).not.toContain(afterNotice.summary);
      expect(conversation(request)).not.toContain(afterMention.summary);
    }
  },
  CASE_TIMEOUT_MS
);
