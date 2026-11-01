import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import * as path from "node:path";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import {
  type Block,
  type Cleanup,
  call,
  deliveryFor,
  messageStream,
  ompRoot,
  type Request,
  STANDIN_STREAM_ID,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// On the real Oh My Pi: a Legion role pane dispatches `deep-worker`, gets its
// `task` call back while the subagent is still running, reads a message that arrives in the
// meantime, and is woken by the job's own result. Legion writes no Oh My Pi settings overlay,
// so `async.enabled` and `bash.autoBackground.enabled` are the pinned binary's own defaults,
// and no agent the plugin ships declares `blocking`; nothing in the plugin decides any of this,
// which is exactly why only the real binary can say it. A pin whose defaults turned background
// work off, or a `blocking: true` put back into agents/deep-worker.md, would hold the pane in the
// foreground for the whole subagent — deaf to its messages — with every unit test still green.
// The pane is a role session as a pod runs one: pi-envoy's extension and this package's root
// (the root, not extensions/legion.ts, so Oh My Pi discovers the `agents/` beside it), the role's
// LEGION_* environment against a stand-in daemon, real NATS, and no `--config` overlay and no
// `async` or `bash` settings in the profile, so the pin's own `--mode rpc` defaults decide.
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server, as in
// controller-start-omp.test.ts. A run without both skips, except on GitHub Actions, where the
// pi-legion job supplies both (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export
// CI=true). The message is published over the core NATS client protocol on a socket rather than
// through the `nats` package: extensions/legion.test.ts and extensions/legion-role-claim.test.ts
// replace `nats` for their whole process, and a socket keeps this case green in a run that loads
// them.
const omp = process.env.LEGION_TEST_OMP;
const natsUrl = process.env.LEGION_TEST_NATS_URL;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = (omp === undefined || natsUrl === undefined) && !onActions;

/** The binary and NATS server the case runs against; throws on GitHub Actions without them. */
function setup(): { readonly binary: string; readonly nats: string } {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  if (natsUrl === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
  return { binary: omp, nats: natsUrl };
}

/** The pane's role, tree and issue; the daemon's state below moves the issue past this phase. */
const ROLE = "implementer";
const TREE = "BGSUB-1";
const ISSUE = "BGSUB-2";

/**
 * The daemon's assignment, sent as the RPC prompt. Every turn of the role's own conversation
 * carries it, and nothing else the pane sends the gateway does, so it is what tells a role turn
 * from the subagent's turn and from a side request.
 */
const ASSIGNMENT = `Implement ${ISSUE}. BGSUB-ROLE-ASSIGNMENT.`;

/**
 * The first line of agents/deep-worker.md's body, which Oh My Pi puts in the subagent's system
 * prompt. The subagent's own Messages request is told by this, never by its place in the order:
 * the pane also sends side requests the host answers at once (the task label, the run-end
 * self-check, a skill's routing-hint compression), and those arrive among the turns.
 */
const DEEP_WORKER_SYSTEM = "You are an autonomous coding worker";

/** What the subagent reports, which the finished job's result must carry back to the role. */
const SUBAGENT_REPORT = "BGSUB-SUBAGENT-REPORT: the scratch check passed.";

/** What the message published while the subagent runs says. */
const MESSAGE_SUMMARY = "BGSUB-MESSAGE: answer the reviewer's question about the retry bound.";

/** How long the case waits for one thing the pane owes it before failing on it. */
const DEADLINE_MS = 60_000;

/** The bound on the message: in the role's context at its next model turn within 30 seconds. */
const MESSAGE_BOUND_MS = 30_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A Messages request's whole conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** A Messages request's last message, which is what started the turn it answers. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

/** Whether a Messages request is the subagent's own: only its system prompt is deep-worker's. */
function isSubagentRequest(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(DEEP_WORKER_SYSTEM);
}

/**
 * Whether a Messages request is a turn of the role's own conversation: it carries the daemon's
 * assignment and is not a side turn. A side turn (pi-envoy's run-end self-check) is an ordinary
 * Messages request over a snapshot of that same conversation whose last message is the `<btw>`
 * block, so the assignment alone would take it for a turn.
 */
function isRoleTurn(request: Request): boolean {
  if (isSubagentRequest(request)) return false;
  if (lastMessage(request).includes("<btw>")) return false;
  return conversation(request).includes("BGSUB-ROLE-ASSIGNMENT");
}

/**
 * One moment the case observed: `at` is the wall clock, which the message's bound and the report
 * are measured in, and `order` is a counter minted with it, which is what the orderings are
 * asserted on. Date.now() has a one-millisecond floor, and the moments this case orders are
 * separated by the host's own work rather than by any wait: a request the stand-in takes and the
 * reply the case sends next land inside one millisecond often enough on a loaded runner that a
 * `<` between their stamps fails on an order that is in fact right. Every moment here is minted
 * by this process, in the order the process saw them, so the counter cannot tie.
 */
interface Moment {
  readonly at: number;
  readonly order: number;
}

let moments = 0;

/** Stamps the moment it is called; the case's only source of a `Moment`. */
function moment(): Moment {
  moments += 1;
  return { at: Date.now(), order: moments };
}

/** One Messages request the stand-in holds until the case answers it, and when it arrived. */
interface Held {
  readonly request: Request;
  /** When the stand-in took it, which for the subagent's is when the subagent really started. */
  readonly arrived: Moment;
  /** Answers it, returning the moment the stand-in let it go. */
  reply(blocks: readonly Block[]): Moment;
}

/**
 * A role-lane message as the Envoy listener forwards it to the role holder's agent subject: the
 * envelope keeps the role topic, so the session would acknowledge it on the request's reply inbox
 * (`expectsLaneReceipt`, packages/envoy-client/src/delivery.ts). This one is published with no
 * reply inbox — nothing here reads the receipt, and the case's stamp is the publish — under a
 * dedupe key minted per label as pi-envoy's card-delivery-omp.test.ts builds it.
 */
function forwardedRoleEnvelope(summary: string, label: string): string {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "envoy",
    source_event_id: `source-${label}`,
    topic: `${"notifications.role"}.legion-bgsub-${ROLE}`,
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: Date.now(),
    payload_summary: summary,
    trace_id: `trace-${label}`,
  });
}

/**
 * A core NATS client on a socket, which relays one frame of the session's consumer as the
 * listener does: CONNECT with headers on, then one HPUB per frame on the session's deliver
 * subject, carrying the stored subject, sequence, stream and time, with a reply inbox of its own.
 * A session takes its messages from that relay alone now (INFRA-771), so nothing reaches it on
 * its agent subject. The `nats` package is deliberately not imported (see the file header), so
 * the frame, its header block and the reply are written and read here.
 *
 * The frame goes again every 100 ms until the session answers it: until the pane has subscribed
 * its deliver subject the server drops the send, and nothing but the next send says so. The
 * stamp returned is the moment the answered send went out, which is the message's arrival: it is
 * earlier than the session's own reply, so measuring the role's next turn against it is the
 * stronger of the two readings.
 */
async function natsRelay(
  url: string
): Promise<(sessionID: string, payload: string, seq: number) => Promise<Moment>> {
  const address = new URL(url);
  const inbox = `_INBOX.bgsub.${randomUUID()}`;
  let answered = 0;
  let buffered = Buffer.alloc(0);
  const socket = await Bun.connect({
    hostname: address.hostname,
    port: Number(address.port),
    socket: {
      data(connection, chunk) {
        buffered = Buffer.concat([buffered, chunk]);
        for (;;) {
          const end = buffered.indexOf("\r\n");
          if (end === -1) return;
          const line = buffered.subarray(0, end).toString();
          // A MSG's payload follows its line and can hold anything, so its byte count — the
          // line's last field — is what says where the next line starts.
          if (line.startsWith("MSG ") || line.startsWith("HMSG ")) {
            const fields = line.split(" ");
            const bytes = Number(fields[fields.length - 1]);
            if (buffered.length < end + 2 + bytes + 2) return;
            buffered = buffered.subarray(end + 2 + bytes + 2);
            answered += 1;
            continue;
          }
          buffered = buffered.subarray(end + 2);
          if (line.startsWith("PING")) connection.write("PONG\r\n");
        }
      },
    },
  });
  cleanup.push(async () => {
    socket.end();
  });
  socket.write('CONNECT {"verbose":false,"pedantic":false,"protocol":1,"headers":true}\r\n');
  socket.write(`SUB ${inbox} 1\r\n`);
  return async (sessionID, payload, seq) => {
    const headers =
      "NATS/1.0\r\n" +
      `${RelayHeader.subject}: notifications.agent.${sessionID}\r\n` +
      `${RelayHeader.streamSeq}: ${seq}\r\n` +
      `${RelayHeader.streamID}: ${STANDIN_STREAM_ID}\r\n` +
      `${RelayHeader.delivered}: 1\r\n` +
      `${RelayHeader.storedAt}: ${new Date().toISOString()}\r\n` +
      "\r\n";
    const headerBytes = Buffer.byteLength(headers);
    const total = headerBytes + Buffer.byteLength(payload);
    const taken = answered;
    const deadline = Date.now() + DEADLINE_MS;
    for (;;) {
      const sent = moment();
      socket.write(
        `HPUB ${deliverSubject(sessionID)} ${inbox} ${headerBytes} ${total}\r\n${headers}${payload}\r\n`
      );
      // A real clock, deliberately, as the shared sendRelayFrame uses one: a session that has
      // not subscribed its deliver subject yet emits no signal at all here, and the server only
      // drops the send, so the next send is the one thing that can tell this rig it is up.
      for (let waited = 0; waited < 100; waited += 10) {
        if (answered > taken) return sent;
        await Bun.sleep(10);
      }
      if (Date.now() > deadline) {
        throw new Error(`${sessionID} took no relay frame carrying ${payload}`);
      }
    }
  };
}

/** Polls `probe` until it returns a value, failing on `what` at the deadline or when omp exits. */
async function until<T>(
  what: string,
  probe: () => T | undefined,
  closed: { readonly error?: unknown }
): Promise<T> {
  const deadline = Date.now() + DEADLINE_MS;
  for (;;) {
    if (closed.error !== undefined) throw closed.error;
    const value = probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await Bun.sleep(10);
  }
}

/** The role pane the case runs, and what it lets the case do to it. */
interface RolePane {
  /** Every Messages request the stand-in holds, in arrival order. */
  readonly held: readonly Held[];
  /** The session id the pane registered with the listener, whose consumer the relay frames feed. */
  readonly sessionId: string;
  /** When the host printed its first `agent_end` frame, once it has. */
  firstAgentEnd(): Moment | undefined;
  /** Waits for a held Messages request `match` accepts that the case has not taken yet. */
  next(what: string, match: (request: Request) => boolean): Promise<Held>;
  /** Relays `payload` to the pane as its consumer's frame, returning the frame's stamp. */
  publish(payload: string): Promise<Moment>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined): Promise<T>;
}

/**
 * Starts one Legion role pane on the real Oh My Pi: pi-envoy's extension and this package's root,
 * the role's LEGION_* environment against a stand-in that is the model gateway, the daemon's
 * claim, grant and state routes and the Envoy listener, and the real NATS server. The daemon's
 * state reports the issue past this role's phase, so the phase-stall follow-up (src/phase-stall.ts)
 * puts no reminder in front of the pane and adds no turn of its own. Every Messages request is
 * held for the case except a side request — the task label, a self-check, a skill's routing-hint
 * compression — which is answered at once with one word, since none of them is a turn and a held
 * one would stall the host inside the work the case is timing.
 */
async function startRolePane(): Promise<RolePane> {
  const { binary, nats } = setup();
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { home, workspace, sessions } = await ompRoot(binary, "legion-bgsub-", cleanup);

  const held: Held[] = [];
  let sides = 0;
  let grants = 0;
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      if (!isSubagentRequest(request) && !isRoleTurn(request)) {
        sides += 1;
        return new Response(
          messageStream([{ type: "text", text: "PROCEEDING" }], `side_${sides}`),
          {
            headers: { "content-type": "text/event-stream" },
          }
        );
      }
      const answer = Promise.withResolvers<string>();
      const index = held.length;
      held.push({
        request,
        arrived: moment(),
        reply(blocks) {
          const released = moment();
          answer.resolve(messageStream(blocks, `msg_${index}`));
          return released;
        },
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: `legion-bgsub-${ISSUE.toLowerCase()}-${ROLE}`,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: "bgsub-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `bgsub-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname === "/legion/v1/state") return Response.json(MOVED_ON);
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest. A
    // registration is answered a consumer on the session's own relay subject, the one path a
    // message reaches a session by (INFRA-771); an answer without one leaves the pane deaf and
    // its claim boot fails.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "bgsub-machine",
      dir: workspace,
      topics: [],
      ...deliveryFor(url.pathname, body, "ses_bgsub"),
    });
  });
  // A request still held when the case ends is answered, so the stand-in stops at once.
  cleanup.push(async () => {
    for (const request of held) request.reply([{ type: "text", text: "Case over." }]);
  });
  // No `--config` overlay and no `async` or `bash` line: the profile only maps the model roles,
  // `deep` among them, which agents/deep-worker.md asks for as `@deep` and which no role pane's
  // own profile would otherwise carry.
  await writeStandinProfile(home, base, ["  deep: standin/standin-model"]);

  const frames: object[] = [];
  const agentEnds: Moment[] = [];
  const closed: { error?: unknown } = {};
  const rpc = spawnRpc(
    binary,
    {
      extensions: [
        path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
        // The package root, not its extension file: Oh My Pi discovers an extension package's
        // `agents/` from its root, and `deep-worker` lives there.
        path.resolve(import.meta.dir, ".."),
      ],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: ROLE,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_PROJECT: "bgsub",
        LEGION_BOOT_TOKEN: "bgsub-boot",
        LEGION_STATE_DIR: path.join(workspace, ".state"),
        LEGION_WORKSPACE: workspace,
      },
      onFrame: (frame) => {
        frames.push(frame);
        // The first `agent_end` is recorded whatever its `isTerminal` says, and it says false:
        // the run that ends with an agent-owned job still pending is not the session's last, so a
        // wait on the terminal flag (the harness's `runLegionPane`) would never fire here. That
        // frame is the moment the `task` call's turn ended, which is what the case times.
        if ("type" in frame && frame.type === "agent_end") agentEnds.push(moment());
      },
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });
  rpc.send({ type: "prompt", message: ASSIGNMENT });

  const sessionId = await until(
    "the pane's registration with the Envoy listener",
    () => {
      const subscribe = requests.find((request) => request.path === "/v1/interests/subscribe");
      const id = subscribe?.body.session_id;
      return typeof id === "string" && id !== "" ? id : undefined;
    },
    closed
  );
  const relay = await natsRelay(nats);
  // The stand-in stream's sequence: one case is one stream, so its frames are ordered by this.
  let seq = 1;

  const taken = new Set<Held>();
  return {
    held,
    sessionId,
    firstAgentEnd: () => agentEnds[0],
    async next(what, match) {
      const request = await until(
        what,
        () => held.find((candidate) => !taken.has(candidate) && match(candidate.request)),
        closed
      );
      taken.add(request);
      return request;
    },
    publish: (payload) => relay(sessionId, payload, seq++),
    until: (what, probe) => until(what, probe, closed),
  };
}

/**
 * The daemon's state with the issue past this pane's phase: the reviewer works it now, so the
 * phase-stall follow-up has no reminder for this role and the case's turns are its own.
 */
const MOVED_ON = {
  daemon: {
    project: "BGSUB",
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-10-30T14:03:27Z",
    startedAt: "2026-10-31T09:15:02Z",
  },
  admission: { cap: 1, active: [TREE], waiting: [], blocked: [] },
  issues: {
    [ISSUE]: {
      key: ISSUE,
      generation: 1,
      phase: "reviewing",
      status: "in_progress",
      repo: "acme/widgets",
      defaultBranch: "main",
      workers: {},
    },
  },
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  capabilities: [],
  repositories: {
    owners: ["acme"],
    mapped: [{ repo: "acme/widgets", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { max_fix_attempts: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
};

test.skipIf(skip)(
  "a role's deep-worker runs in the background: the task call returns before it ends, a message sent meanwhile is read, and the job's result wakes the role",
  async () => {
    const pane = await startRolePane();

    // The assignment's turn dispatches the worker. `solutionSpace` rides the child's first prompt
    // into the thinking classifier; the batch shape's `context` is required shared background.
    const dispatch = await pane.next("the role's first turn", isRoleTurn);
    dispatch.reply([
      call("task", {
        context: `The scratch check of ${ISSUE}.`,
        tasks: [
          {
            name: "DeepWorker",
            agent: "deep-worker",
            task: "Run the scratch check in the workspace and report what it printed.",
            solutionSpace: "one check: the command is given",
          },
        ],
      }),
    ]);

    // The subagent's own first Messages request, held from here until after the message's turn:
    // nothing in this case is gated on a sleep, and while it is held the subagent cannot end.
    const subagent = await pane.next("the subagent's own model request", isSubagentRequest);

    // The `task` call came back at once: its tool result started a second turn, which ends — the
    // host's first `agent_end` — while the subagent's request is still held.
    const started = await pane.next("the role's turn after the task call", isRoleTurn);
    expect(conversation(started.request)).toContain("Spawned");
    started.reply([
      { type: "text", text: "The worker is running; I will be woken by its result." },
    ]);
    const ended = await pane.until("the role's turn to end", () => pane.firstAgentEnd());

    // Ordering, by the session's own facts rather than by a clock the case set: the subagent had
    // started before the role's turn ended, and the role's turn ended before the stand-in let the
    // subagent's request go. Each is asserted on the moments' order, not on their clock stamps,
    // which are a millisecond apart at best and tie whenever the host does this work fast.
    expect(subagent.arrived.order).toBeLessThan(ended.order);

    const published = await pane.publish(
      forwardedRoleEnvelope(MESSAGE_SUMMARY, `bgsub-${randomUUID()}`)
    );
    const read = await pane.next("the role's turn carrying the message", (request) =>
      isRoleTurn(request) ? conversation(request).includes(MESSAGE_SUMMARY) : false
    );
    expect(read.arrived.at - published.at).toBeLessThan(MESSAGE_BOUND_MS);
    read.reply([{ type: "text", text: "Noted; the retry bound is three." }]);

    const released = subagent.reply([{ type: "text", text: SUBAGENT_REPORT }]);
    expect(ended.order).toBeLessThan(released.order);
    // The message's own turn ran while the subagent's request was still held: the role read it
    // while the worker was running, not once the worker had ended.
    expect(read.arrived.order).toBeLessThan(released.order);

    // The finished job wakes the role into a turn nothing prompted, carrying the worker's report.
    const woken = await pane.next("the role's turn woken by the finished job", (request) =>
      isRoleTurn(request) ? conversation(request).includes(SUBAGENT_REPORT) : false
    );
    expect(lastMessage(woken.request)).toMatch(/[Bb]ackground job/);
    expect(woken.arrived.order).toBeGreaterThan(released.order);
    woken.reply([{ type: "text", text: "The worker is done." }]);

    console.error(
      `the subagent started ${ended.at - subagent.arrived.at} ms before the role's turn ended; ` +
        `the role's turn ended ${released.at - ended.at} ms before the subagent's request was released; ` +
        `the message was read ${read.arrived.at - published.at} ms after it was published; ` +
        `the job woke the role ${woken.arrived.at - released.at} ms after the release`
    );
  },
  300_000
);
