import { afterEach, expect, test } from "bun:test";
import { readFile } from "node:fs/promises";
import * as path from "node:path";
import {
  LegionActivityRequest,
  type LegionSessionActivityBody,
} from "@legion/contracts/legion-api";
import { ASYNC_JOB_KIND, SUBAGENT_KIND } from "@legion/pi-shared/session-activity";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  ompRoot,
  type Request,
  refusingNats,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// What the pinned Oh My Pi reports of a role session's own background work (INFRA-945), which is
// the whole input to the daemon's idle rule: a claim that reports background work keeps its pod,
// and one that reports none is released once no turn is in progress and none is due. The report
// is built from `ctx.getAsyncJobSnapshot()` (`asyncJobKinds` in packages/pi-shared/src/
// session-activity.ts) and posted to `claims/activity`, so what reaches the daemon is decided by
// the host's job manager and by nothing this repository owns — which is why only the real binary
// can say it. The three kinds of background work a role can start are each run here against the
// pin, and each is asserted either reported or not reported:
//
//   - an `async: true` `bash` job — the host's own job manager runs it, and it is reported as
//     `async-job`;
//   - a `task` subagent — the same manager runs it as a job of type `task`, and it is reported as
//     `subagent`;
//   - a named `bash` service — it is *not* reported, and this case pins that it is not. A service
//     runs outside the session's job manager, so `getAsyncJobSnapshot()` never names it and the
//     daemon cannot know it is there. That is what skills/legion-worker/SKILL.md tells a worker
//     ("A named Oh My Pi `bash` service goes with the pod too … your session reports nothing about
//     it"), and the case fails if a pin starts reporting one, because the skill would then be
//     telling a worker to throw away work the daemon could have kept.
//
// The pane is a role session as a pod runs one: pi-envoy's extension and this package's root (the
// root, not extensions/legion.ts, so Oh My Pi discovers the `agents/` beside it), the role's
// LEGION_* environment against a stand-in daemon, and no `--config` overlay and no `async` or
// `bash` settings in the profile, so the pin's own `--mode rpc` defaults decide. Nothing here
// takes a NATS delivery, so the Envoy extension is given an address that refuses every connection
// (`refusingNats`) rather than a server. LEGION_TEST_OMP names the binary, as in
// legion-background-subagent-omp.test.ts; a run without it skips, except on GitHub Actions, where
// the pi-legion job supplies it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export
// CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = omp === undefined && !onActions;

/** The binary the case runs against; throws on GitHub Actions without it. */
function setup(): string {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
}

/** The pane's role, tree and issue; the daemon's state below moves the issue past this phase. */
const ROLE = "implementer";
const TREE = "ACT-1";
const ISSUE = "ACT-2";

/**
 * The daemon's assignment, sent as the RPC prompt. Every turn of the role's own conversation
 * carries it, and nothing else the pane sends the gateway does, so it is what tells a role turn
 * from the subagent's turn and from a side request.
 */
const ASSIGNMENT = `Implement ${ISSUE}. ACT-ROLE-ASSIGNMENT.`;

/** The first line of agents/deep-worker.md's body, which Oh My Pi puts in the subagent's system
 *  prompt: the one thing that tells the subagent's own Messages request from a role turn. */
const DEEP_WORKER_SYSTEM = "You are an autonomous coding worker";

/** The name of the service the case starts, which the host requires to be unique in the session. */
const SERVICE_NAME = "activity-service";

/** How long the backgrounded `bash` job sleeps: longer than the case, so it is running at every
 *  reading the case makes of what the session reports. */
const ASYNC_JOB_SECONDS = 600;

/** How long the case waits for one thing the pane owes it before failing on it. */
const DEADLINE_MS = 90_000;

/**
 * The reporting period the pane's own interval runs on (`SESSION_ACTIVITY_PERIOD_MS` in
 * extensions/legion.ts), and how long the case watches for a report that must not come. Three
 * periods and a margin: a report of the service would have to arrive inside one of them, since
 * the pane reads the host's snapshot once a period and sends whenever the reading changed.
 */
const REPORT_PERIOD_MS = 5_000;
const QUIET_WATCH_MS = 3 * REPORT_PERIOD_MS + 2_000;

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
  return conversation(request).includes("ACT-ROLE-ASSIGNMENT");
}

/** One Messages request the stand-in holds until the case answers it. */
interface Held {
  readonly request: Request;
  /** Answers it, which lets the host go on with the turn. */
  reply(blocks: readonly Block[]): void;
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
    await Bun.sleep(50);
  }
}

/** The role pane the case runs, and what it lets the case do to it. */
interface RolePane {
  /** Every `claims/activity` report the pane has sent, in order. */
  readonly reports: LegionSessionActivityBody[];
  /** Every `claims/activity` body that was not the contract's shape, which must stay empty. */
  readonly malformed: string[];
  /** The pane's issue workspace, where the service writes its heartbeat. */
  readonly workspace: string;
  /** Waits for a held Messages request `match` accepts that the case has not taken yet. */
  next(what: string, match: (request: Request) => boolean): Promise<Held>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/**
 * Starts one Legion role pane on the real Oh My Pi: pi-envoy's extension and this package's root,
 * the role's LEGION_* environment against a stand-in that is the model gateway, the daemon's
 * claim, grant, state and activity routes and the Envoy listener. The daemon's state reports the
 * issue past this role's phase, so the phase-stall follow-up (src/phase-stall.ts) puts no reminder
 * in front of the pane, adds no turn of its own, and leaves the report's `phase` half closed and
 * unchanging — so a report the pane sends between turns is one its background work changed. Every
 * Messages request is held for the case except a side request — the task label, a self-check, a
 * skill's routing-hint compression — which is answered at once with one word, since none of them
 * is a turn and a held one would stall the host inside the work the case is reading.
 */
async function startRolePane(): Promise<RolePane> {
  const binary = setup();
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { home, workspace, sessions } = await ompRoot(binary, "legion-activity-", cleanup);

  const held: Held[] = [];
  const reports: LegionSessionActivityBody[] = [];
  const malformed: string[] = [];
  let sides = 0;
  let grants = 0;
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      if (!isSubagentRequest(request) && !isRoleTurn(request)) {
        sides += 1;
        return new Response(
          messageStream([{ type: "text", text: "PROCEEDING" }], `side_${sides}`),
          { headers: { "content-type": "text/event-stream" } }
        );
      }
      const answer = Promise.withResolvers<string>();
      const index = held.length;
      held.push({
        request,
        reply(blocks) {
          answer.resolve(messageStream(blocks, `msg_${index}`));
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
        claimToken: `legion-act-${ISSUE.toLowerCase()}-${ROLE}`,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: "act-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/claims/activity") {
      // The body is read through the contract's own schema: a report that is not what
      // `POST /legion/v1/claims/activity` takes would be refused by the daemon, so one here is a
      // failure of the case rather than a reading of the host.
      const parsed = LegionActivityRequest.safeParse(body);
      if (parsed.success) reports.push(parsed.data.activity);
      else malformed.push(JSON.stringify(body));
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({ grantId: `act-grant-${grants}`, expiresAt: "2099-01-01T00:00:00Z" });
    }
    if (url.pathname === "/legion/v1/state") return Response.json(MOVED_ON);
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "act-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held when the case ends is answered, so the stand-in stops at once.
  cleanup.push(async () => {
    for (const request of held) request.reply([{ type: "text", text: "Case over." }]);
  });
  // No `--config` overlay and no `async` or `bash` line: the profile only maps the model roles,
  // `deep` among them, which agents/deep-worker.md asks for as `@deep`.
  await writeStandinProfile(home, base, ["  deep: standin/standin-model"]);

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
        ENVOY_NATS_URL: refusingNats(cleanup),
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: ROLE,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_PROJECT: "act",
        LEGION_BOOT_TOKEN: "act-boot",
        LEGION_STATE_DIR: path.join(workspace, ".state"),
        LEGION_WORKSPACE: workspace,
      },
      onFrame: () => undefined,
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });
  rpc.send({ type: "prompt", message: ASSIGNMENT });

  await until(
    "the pane's registration with the Envoy listener",
    () => requests.find((request) => request.path === "/v1/interests/subscribe"),
    closed
  );

  const taken = new Set<Held>();
  return {
    reports,
    malformed,
    workspace,
    async next(what, match) {
      const request = await until(
        what,
        () => held.find((candidate) => !taken.has(candidate) && match(candidate.request)),
        closed
      );
      taken.add(request);
      return request;
    },
    until: (what, probe) => until(what, probe, closed),
  };
}

/**
 * The daemon's state with the issue past this pane's phase: the reviewer works it now, so the
 * phase-stall follow-up has no reminder for this role and the case's turns are its own.
 */
const MOVED_ON = {
  daemon: {
    project: "ACT",
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
  "what a role session reports of its background work on the real Oh My Pi: a backgrounded bash job and a task subagent reach the daemon, a named bash service does not",
  async () => {
    const pane = await startRolePane();
    const heartbeat = path.join(pane.workspace, "service-heartbeat.log");
    /** How many ticks the service has written, which is how the case knows it is running. */
    const ticks = async (): Promise<number> => {
      try {
        return (await readFile(heartbeat, "utf8")).split("\n").filter(Boolean).length;
      } catch {
        return 0;
      }
    };

    // 1. The named service. It is a `bash` call with a `name`, which is how the host is asked for
    // a long-lived service rather than a job, and it outlives the turn that started it.
    const first = await pane.next("the role's first turn", isRoleTurn);
    first.reply([
      call("bash", {
        command: `while true; do printf 'tick\\n' >> ${JSON.stringify(heartbeat)}; sleep 0.2; done`,
        name: SERVICE_NAME,
      }),
    ]);
    const serviceTurn = await pane.next("the role's turn after the service started", isRoleTurn);
    expect(conversation(serviceTurn.request)).toContain(SERVICE_NAME);
    const started = await pane.until("the service's first heartbeat", async () =>
      (await ticks()) > 0 ? await ticks() : undefined
    );
    const beforeTheService = pane.reports.length;

    // Nothing else of the session is running — the role's turn is held by the stand-in, which is
    // the host waiting on the model, not background work of the session's own. The pane reads the
    // host's snapshot once a period and sends only what changed, so a service the host reported
    // would show here as a new report naming a kind. A real clock is what shows it: the
    // proposition is that the host reported nothing new over three of its own periods, and a
    // host that reports nothing emits no signal to await.
    await Bun.sleep(QUIET_WATCH_MS);
    const serviceTicks = await ticks();
    expect(serviceTicks).toBeGreaterThan(started);
    expect(pane.reports.length).toBeGreaterThan(0);
    for (const report of pane.reports.slice(beforeTheService)) {
      expect(report.backgroundWork).toEqual({ running: false, kinds: [] });
    }
    // What the daemon holds while the service runs: a session running no background work, which
    // is a pod it may release. The service is invisible to `getAsyncJobSnapshot()`, so it is not
    // the host's to report.
    expect(pane.reports.at(-1)?.backgroundWork).toEqual({ running: false, kinds: [] });

    // 2. The backgrounded job, which the host's own job manager runs and the pane reports.
    serviceTurn.reply([call("bash", { command: `sleep ${ASYNC_JOB_SECONDS}`, async: true })]);
    const jobTurn = await pane.next("the role's turn after the job was backgrounded", isRoleTurn);
    const jobReport = await pane.until("a report naming the backgrounded job", () =>
      pane.reports.find((report) => report.backgroundWork.kinds.includes(ASYNC_JOB_KIND))
    );
    // Exactly the one kind: the service is still running and still adds nothing.
    expect(jobReport.backgroundWork).toEqual({ running: true, kinds: [ASYNC_JOB_KIND] });

    // 3. The subagent, which the same manager runs as a job of type `task`. Its own Messages
    // request is held, so the job is still running at every reading below.
    jobTurn.reply([
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
    const subagent = await pane.next("the subagent's own model request", isSubagentRequest);
    const dispatched = await pane.next("the role's turn after the task call", isRoleTurn);
    const bothReport = await pane.until("a report naming the subagent", () =>
      pane.reports.find((report) => report.backgroundWork.kinds.includes(SUBAGENT_KIND))
    );
    // Both the host's own kinds, and still nothing of the service, which is running throughout.
    expect(bothReport.backgroundWork).toEqual({
      running: true,
      kinds: [ASYNC_JOB_KIND, SUBAGENT_KIND],
    });
    expect(await ticks()).toBeGreaterThan(serviceTicks);

    // Nothing the pane sent named a kind the host did not run: the service never appears under a
    // name of its own either.
    const kinds = new Set(pane.reports.flatMap((report) => report.backgroundWork.kinds));
    expect([...kinds].sort()).toEqual([ASYNC_JOB_KIND, SUBAGENT_KIND]);
    expect(pane.malformed).toEqual([]);

    dispatched.reply([
      { type: "text", text: "The worker is running; I will wait for its result." },
    ]);
    subagent.reply([{ type: "text", text: "ACT-SUBAGENT-REPORT: the scratch check passed." }]);

    console.error(
      `the backgrounded bash job reported ${JSON.stringify(jobReport.backgroundWork.kinds)}; ` +
        `the task subagent brought the report to ${JSON.stringify(bothReport.backgroundWork.kinds)}; ` +
        `the named bash service wrote ${await ticks()} heartbeats and was reported by nothing, ` +
        `across ${pane.reports.length} reports`
    );
  },
  300_000
);
