import { afterEach, expect, test } from "bun:test";
import { mkdir, symlink, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { PHASE_STALL_ENTRY } from "@legion/pi-shared/phase-stall";
import {
  type Cleanup,
  type LegionPane,
  type LegionPaneOptions,
  type Reply,
  type Request,
  runLegionPane,
} from "@legion/pi-shared/test/omp-harness";

// The phase-stall follow-up on the real Oh My Pi (src/phase-stall.ts): only the real binary
// shows when the host fires `session_stop`, how it turns the returned follow-up into the next turn,
// what the model is sent, and that the transcript keeps the state a resumed worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which
// CI's pi-envoy job installs; on the devbox, `mise where <pin>`/bin/omp. A run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
// It is also the only check that the run-end nudge's hidden self-check starts no run: the nudge
// treats any `agent_start` after a settle as a newer run and withholds its steer, so a host that
// counted the side turn as a run would silence the nudge with every unit test still green.
// The WAITING self-check case below fails if that ever changes.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** The implementer pane the phase-stall cases run, with what its stand-in daemon recorded and
 * its transcript's phase-stall entries. */
interface StallPane extends LegionPane {
  /** The body of each `POST /legion/v1/handoff/complete` the pane made, in order. */
  readonly completions: () => unknown[];
  /** The body of each `POST /v1/messages/publish` the pane made, in order: the architect notice
   * a run of unanswered reminders earns, and every retry of one the listener refused. */
  readonly publishes: () => Record<string, unknown>[];
  /** The persisted transcript's phase-stall entries, in order. */
  readonly phaseEntries: () => Promise<unknown[]>;
  /** The pushed commit carrying `.legion/STALL-2/implement.json` in the pane's workspace: what
   * its `handoff_complete` finds and reports. */
  readonly handoffCommit: string;
}

/**
 * The reminder count each phase-stall entry recorded, with a repeat of the value before it
 * dropped: what is left is the count's own climb, one step per reminder the host put in front of
 * the worker. The repeats are the writes that carry no count of their own — the architect notice
 * minting its key and then marking itself sent — and the closing entry, which follows a
 * `message_end` that already put the count back to zero, in whichever order the host delivers
 * the two.
 */
function stallCounts(entries: readonly unknown[]): number[] {
  const counts = entries.map((entry) =>
    typeof entry === "object" && entry !== null && "count" in entry
      ? Number(entry.count)
      : Number.NaN
  );
  return counts.filter((count, index) => index === 0 || count !== counts[index - 1]);
}

/** The listener's answer to a publish it accepted: an envelope, which the client parses before
 * it answers its caller (`EnvelopeSchema`, packages/contracts/src/envelope.ts). */
function published(body: Record<string, unknown>): Response {
  const key = String(body.idempotency_key ?? "");
  return Response.json({
    event_id: `evt-${key}`,
    source: "agent",
    source_event_id: key,
    topic: String(body.topic ?? ""),
    dedupe_key: `publish.${key}`,
    issued_at: 1,
    payload_summary: "a worker has let its phase reminders go unanswered",
    trace_id: `trace-${key}`,
  });
}

/** The identity the daemon puts on every pane (runtime.GitIdentity), which the pane's jj commits
 * as and `handoff_complete` holds the carrying commit's author to. */
const PANE_IDENTITY = { JJ_USER: "Legion Test", JJ_EMAIL: "legion-test@example.invalid" };

/** Runs `jj` in `cwd` as the pane's identity, failing loudly on a non-zero exit, and answers its
 * stdout. */
async function runJj(jj: string, cwd: string, args: readonly string[]): Promise<string> {
  const child = Bun.spawn([jj, ...args], {
    cwd,
    env: { ...process.env, ...PANE_IDENTITY },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

/** The daemon's state as the pane's `handoff_complete` reads it: STALL-2 at implementing, the
 * phase whose handoff is `.legion/STALL-2/implement.json`. */
const STALL_STATE = {
  daemon: {
    project: "STALL",
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-09-18T14:03:27Z",
    startedAt: "2026-09-22T09:15:02Z",
  },
  admission: { cap: 1, active: ["STALL-1"], waiting: [] },
  issues: {
    "STALL-2": {
      key: "STALL-2",
      generation: 1,
      phase: "implementing",
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
    caches: [],
  },
  settings: { max_fix_attempts: 3, worker_boot_timeout_seconds: 120 },
};

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The text of every user message in a Messages request, joined. */
function userText(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(
    messages.filter(
      (message: unknown) =>
        typeof message === "object" &&
        message !== null &&
        "role" in message &&
        message.role === "user"
    )
  );
}

/** The names of the tools a Messages request offers the model. */
function toolNames(request: Request): string[] {
  const tools = Array.isArray(request.body.tools) ? request.body.tools : [];
  return tools.map((tool: unknown) =>
    typeof tool === "object" && tool !== null && "name" in tool ? String(tool.name) : ""
  );
}

/** The description of the `legion` tool a Messages request offers the model. */
function legionToolDescription(request: Request): string {
  const tools = Array.isArray(request.body.tools) ? request.body.tools : [];
  const tool = tools.find(
    (candidate: unknown) =>
      typeof candidate === "object" &&
      candidate !== null &&
      "name" in candidate &&
      candidate.name === "legion"
  );
  return String(tool?.description ?? "");
}

/** How one pane differs from the implementer pane the phase-stall cases run: the runner's
 * `legion`, `quietMs`, `selfCheck` and environment, what the stand-in answers ahead of this
 * file's own routes, and the stand-in's open-ask snapshot. */
interface PaneOptions
  extends Pick<LegionPaneOptions, "legion" | "quietMs" | "selfCheck" | "env" | "answer"> {
  /** Configures Dispatch against the stand-in, whose open-ask snapshot answers with this count. */
  readonly openAsks?: number;
  /** The held ask questions the stand-in returns in the session's open-ask snapshot. */
  readonly openAskQuestions?: readonly string[];
}

/**
 * Runs one implementer pane on the real Oh My Pi (`runLegionPane`), the stand-in answering the
 * daemon's state and completion routes and Dispatch's open-ask snapshot when a case configures
 * Dispatch, until its run settles. The pane's workspace is a jj repository with
 * `.legion/STALL-2/implement.json` committed and pushed to a bare origin's `legion/STALL-2`, as a
 * worker leaves it before completing, so the pane's `handoff_complete` finds the carrying commit.
 */
async function stallPane(
  binary: string,
  replies: readonly Reply[],
  options: PaneOptions = {}
): Promise<StallPane> {
  let handoffCommit = "";
  const pane = await runLegionPane(
    binary,
    replies,
    {
      name: "stall",
      role: "implementer",
      tree: "STALL-1",
      issue: "STALL-2",
      prompt: "Implement STALL-2.",
      dispatch: options.openAsks !== undefined || options.openAskQuestions !== undefined,
      legion: options.legion,
      quietMs: options.quietMs,
      selfCheck: options.selfCheck,
      env: {
        ...PANE_IDENTITY,
        // The daemon puts the project token on every pane, and the architect notice's role topic
        // is built from it (`roleToken`).
        ...(options.legion === false ? {} : { LEGION_PROJECT: "stall" }),
        ...options.env,
      },
      prepare: async ({ workspace, bin }) => {
        // The pane's PATH is the harness's fixed one plus `bin`: the jj this test process finds is
        // linked into `bin`, so the pane's `handoff_complete` runs the one that made its workspace.
        const jj = Bun.which("jj");
        if (jj === null) throw new Error("jj is not on PATH");
        await symlink(jj, path.join(bin, "jj"));
        const origin = path.join(path.dirname(workspace), "origin.git");
        if ((await Bun.spawn(["git", "init", "--bare", "--quiet", origin]).exited) !== 0) {
          throw new Error(`git init --bare ${origin} failed`);
        }
        await runJj(jj, path.dirname(workspace), ["git", "init", workspace]);
        // From here on, every command runs in the workspace, as the pane's own shell does.
        const jjIn = (...args: string[]) => runJj(jj, workspace, args);
        await jjIn("git", "remote", "add", "origin", origin);
        await writeFile(path.join(workspace, "main.py"), 'print("stall")\n');
        await jjIn("describe", "-m", "implement: the change");
        await jjIn("new");
        await mkdir(path.join(workspace, ".legion", "STALL-2"), { recursive: true });
        await writeFile(
          path.join(workspace, ".legion", "STALL-2", "implement.json"),
          '{"schemaVersion":1,"phase":"implement","issue":"STALL-2","completed":"2026-10-09T08:00:00Z"}\n'
        );
        await jjIn("split", "-m", "implement: record handoff", ".legion/STALL-2/implement.json");
        await jjIn("bookmark", "set", "legion/STALL-2", "-r", "@-");
        await jjIn("git", "push", "--bookmark", "legion/STALL-2");
        handoffCommit = await jjIn("log", "-r", "@-", "--no-graph", "-T", "commit_id");
      },
      answer: (url, body) => {
        const own = options.answer?.(url, body);
        if (own !== undefined) return own;
        if (url.pathname === "/legion/v1/state") return Response.json(STALL_STATE);
        if (url.pathname === "/legion/v1/handoff/complete") return Response.json({});
        // The Envoy listener takes the architect notice unless a case refuses it.
        if (url.pathname === "/v1/messages/publish") return published(body);
        if (url.pathname !== "/api/v1/asks/open") return undefined;
        const questions = options.openAskQuestions ?? [];
        return Response.json({
          session_id: "",
          as_of: new Date().toISOString(),
          opened_since: false,
          count: options.openAsks ?? questions.length,
          waiting_on_human: 0,
          waiting_on_agent: 0,
          asks: questions.map((question, index) => ({
            id: `ask-${index}`,
            ref: `/issues/LEGION-${index}#ask-${index}`,
            question,
            kind: "question",
            urgency: "normal",
            created_at: "2026-09-13T00:00:00Z",
            age_seconds: 0,
            priority: null,
            owner: { issue: { key: `LEGION-${index}`, title: "Test" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          })),
        });
      },
    },
    cleanup
  );
  return {
    ...pane,
    completions: () =>
      pane.requests
        .filter((request) => request.path === "/legion/v1/handoff/complete")
        .map((request) => request.body),
    publishes: () =>
      pane.requests
        .filter((request) => request.path === "/v1/messages/publish")
        .map((request) => request.body),
    phaseEntries: () => pane.transcriptEntries(PHASE_STALL_ENTRY),
    handoffCommit,
  };
}

test.skipIf(omp === undefined && !onActions)(
  "a turn that ends on a legion tool call written as text is reminded that it did not run, and the next turn's real legion tool call posts the completion",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(omp, [
      [
        {
          type: "text",
          text: 'court\n<invoke name="legion">\n<parameter name="op">handoff_complete</parameter>\n<parameter name="summary">Stall proof done.</parameter>\n</invoke>',
        },
      ],
      [
        {
          type: "tool_use",
          name: "legion",
          input: { op: "handoff_complete", summary: "Stall proof done." },
        },
      ],
      [{ type: "text", text: "Reported." }],
    ]);

    // The worker registered through the daemon's routes; the reminder's record check read the
    // issue's record before it was sent, and the handoff_complete read it again, found the pushed
    // commit carrying .legion/STALL-2/implement.json in its workspace with the pane's jj, minted
    // a grant in-process and posted the completion with both: no grant file, no `legion` command,
    // and no handoff read by the daemon.
    expect(
      pane.requests.map((request) => request.path).filter((p) => p.startsWith("/legion/"))
    ).toEqual([
      "/legion/v1/claims/register",
      "/legion/v1/claims/ready",
      "/legion/v1/state",
      "/legion/v1/state",
      "/legion/v1/grants",
      "/legion/v1/handoff/complete",
    ]);
    expect(pane.handoffCommit).toMatch(/^[0-9a-f]{40}$/);
    expect(pane.completions()).toEqual([
      {
        grantId: "stall-grant-1",
        summary: "Stall proof done.",
        verdict: "",
        ready: false,
        commit: pane.handoffCommit,
      },
    ]);
    const turns = pane.turns();
    // Three turns in one run: the text-only one, the reminder's continuation, and the reply to
    // the tool result. None after: the handoff closed the phase, so the last settle sent nothing.
    expect(turns).toHaveLength(3);
    // A Legion pane, both entries loaded: the host offers the model the `legion` tool.
    expect(toolNames(turns[0] as Request)).toContain("legion");
    // Every role is offered the same tool text, and it gives a verdict to the tester alone.
    expect(legionToolDescription(turns[0] as Request)).toContain(
      "`verdict` pass|fail from the tester alone"
    );
    expect(userText(turns[0] as Request)).not.toContain("handoff_complete");
    // The sentence the settling message earned still leads the reminder it earned.
    expect(userText(turns[1] as Request)).toContain("written as text");
    expect(userText(turns[1] as Request)).toContain("WAITING");
    expect(userText(turns[1] as Request)).toContain("repeats at the end of every turn");
    // The transcript holds every change, which a worker relaunched with --resume restores: the
    // phase opened, one reminder was counted, and the completion closed it.
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "closed", count: 0 });
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a worker that keeps settling is reminded at the end of every turn, past the architect threshold, until its handoff_complete closes the phase",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(
      omp,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "Nearly there." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "Stall proof done." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500", LEGION_STALL_ARCHITECT_AFTER: "2" },
        quietMs: 6_000,
      }
    );

    const turns = pane.turns();
    // One turn per reminder: the prompt's own, three the reminders started — the first the
    // `session_stop` continuation, the two after it the host's own timer, each of which lets the
    // run settle first — and the reply to the tool result.
    expect(turns).toHaveLength(5);
    for (const index of [1, 2, 3]) {
      expect(userText(turns[index] as Request)).toContain("handoff_complete");
      expect(userText(turns[index] as Request)).toContain("repeats at the end of every turn");
    }
    // One entry per delivered reminder is what proves each landed at a turn boundary of its own,
    // which the continuation alone does not show: the count climbs 1, 2, 3, and the completion
    // puts it back to 0.
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 2, 3, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "closed", count: 0 });
    // The threshold was crossed at the second reminder; the notice the architect took there
    // stopped nothing, and the third reminder earned no second notice.
    expect(pane.publishes()).toHaveLength(1);
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a WAITING reply buys its quiet wait, and the reminder comes again when the wait is over",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    // Every request the stand-in served, with the moment it was served: the proposition is that
    // nothing reached the session until the wait was over, and only elapsed time shows it.
    const seen: { readonly path: string; readonly at: number }[] = [];
    const pane = await stallPane(
      omp,
      [
        [{ type: "text", text: "Pushed the change.\nWAITING: CI on the pull request." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "CI passed." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_WAITING_MS: "1500" },
        quietMs: 4_000,
        answer: (url) => {
          seen.push({ path: url.pathname, at: Date.now() });
          return undefined;
        },
      }
    );

    const turns = pane.turns();
    expect(turns).toHaveLength(3);
    const [waitingTurn = 0, reminderTurn = 0] = seen
      .filter((request) => request.path === "/anthropic/v1/messages")
      .map((request) => request.at);
    // The WAITING line bought the wait: no turn started while it ran, and the reminder's did
    // only once it was over.
    expect(reminderTurn - waitingTurn).toBeGreaterThanOrEqual(1_400);
    // Nothing arrived from outside in between: in that gap the pane spoke only to the daemon,
    // for the reminder's own record check, and to the listener for its session registration.
    const between = seen
      .filter((request) => request.at > waitingTurn && request.at < reminderTurn)
      .map((request) => request.path);
    expect(
      between.filter((path) => path !== "/legion/v1/state" && path !== "/v1/interests/subscribe")
    ).toEqual([]);
    // What the worker is told once its wait is over: the reminding repeats, rather than this
    // being the last it hears until something arrives.
    expect(userText(turns[1] as Request)).toContain("handoff_complete");
    expect(userText(turns[1] as Request)).toContain("repeats at the end of every turn");
    expect(userText(turns[1] as Request)).not.toContain(
      "nothing more will be sent until a new event arrives"
    );
    expect(userText(turns[1] as Request)).not.toContain("written as text");
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "closed", count: 0 });
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a run of unanswered reminders tells the issue's architect over Envoy, retried under its own key until the listener takes it, and never again",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    let publishes = 0;
    const pane = await stallPane(
      omp,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "The architect has not answered my question yet." }],
        [{ type: "tool_use", name: "legion", input: { op: "read_record", issue: "STALL-2" } }],
        [{ type: "text", text: "The record still says implementing." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "Stall proof done." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500", LEGION_STALL_ARCHITECT_AFTER: "2" },
        quietMs: 6_000,
        // No live session holds the architect's role the first time, as the listener answers
        // while that architect is between sessions; the second is taken.
        answer: (url) => {
          if (url.pathname !== "/v1/messages/publish") return undefined;
          publishes += 1;
          return publishes === 1
            ? Response.json({ reason: "unclaimed" }, { status: 404 })
            : undefined;
        },
      }
    );

    const notices = pane.publishes();
    expect(notices).toHaveLength(2);
    // The refused notice is sent again under the key it was minted with, so a listener that took
    // one whose answer was lost does not deliver it twice.
    expect(notices[1]?.idempotency_key).toBe(String(notices[0]?.idempotency_key));
    // The issue has no architect claim of its own, so the tree's architect holds this worker.
    expect(notices[1]?.topic).toBe("notifications.role.legion-stall-stall-1-architect");
    expect(String(notices[1]?.message)).toContain("STALL-2");
    expect(String(notices[1]?.message)).toContain("implementing");
    expect(String(notices[1]?.message)).toContain(
      "The architect has not answered my question yet."
    );
    expect(JSON.parse(String(notices[1]?.payload))).toMatchObject({
      issue: "STALL-2",
      role: "implementer",
      phase: "implementing",
      reminders: 3,
    });
    // The worker then called a tool, which put the count back to zero: the reminder after it is
    // a run's first again, so it told the architect nothing.
    expect(pane.turns()).toHaveLength(7);
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

/** The daemon's state with STALL-2 moved past implementing: the reviewer works it now, so no
 * reminder is the implementer's to get. */
const MOVED_ON = {
  ...STALL_STATE,
  issues: { "STALL-2": { ...STALL_STATE.issues["STALL-2"], phase: "reviewing" } },
};

test.skipIf(omp === undefined && !onActions)(
  "a worker whose record has already moved past its phase is reminded of nothing: its run settles where it is",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(
      omp,
      // A second reply is scripted so a turn that should not happen shows up as a turn rather
      // than as the gateway's "no reply scripted" refusal.
      [
        [{ type: "text", text: "I completed this from the shell." }],
        [{ type: "text", text: "Understood." }],
      ],
      {
        quietMs: 4_000,
        answer: (url) =>
          url.pathname === "/legion/v1/state" ? Response.json(MOVED_ON) : undefined,
      }
    );

    // One turn, the prompt's own, and the wait proves no second: the record check read the
    // phase as the reviewer's, so `session_stop` ran no continuation at all.
    expect(pane.turns()).toHaveLength(1);
    expect(pane.publishes()).toEqual([]);
    expect(pane.completions()).toEqual([]);
    // Nothing was put in front of the worker, so nothing was counted.
    expect(stallCounts(await pane.phaseEntries())).toEqual([0]);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a record that moves on between reminders stops them: the reminder its timer carried is never sent",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    let reads = 0;
    const pane = await stallPane(
      omp,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "A turn that should never start." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500" },
        quietMs: 4_000,
        answer: (url) => {
          if (url.pathname !== "/legion/v1/state") return undefined;
          reads += 1;
          // The first read is the continuation reminder's own check; by the timer's read the
          // daemon has moved the issue on.
          return Response.json(reads === 1 ? STALL_STATE : MOVED_ON);
        },
      }
    );

    // Two turns: the prompt's and the continuation reminder's. The timer the second settle armed
    // found the phase no longer this role's and sent the reminder it carried to nobody.
    expect(pane.turns()).toHaveLength(2);
    expect(pane.publishes()).toEqual([]);
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1]);
    expect(entries.at(-1)).toMatchObject({ state: "open", count: 1 });
  },
  120_000
);

// The run-end ask nudge (extensions/envoy.ts) is a hidden side-turn self-check whose
// WAITING verdict — and nothing else — buys one steered turn. Two host behaviours carry it, and
// only the real binary can say either: an ephemeral call is served as a Messages request over a
// snapshot of the conversation that the transcript never keeps, and a `triggerTurn` continuation
// of an agent-attributed custom message re-enters no `before_agent_start`, so it arms no period.
// The check the period owes is re-armed by the agent's own work, and a continuation that only
// replies calls no tool, so it owes none: those two together are why the nudge cannot nudge
// itself. If either changed, an ordinary session would nudge itself to the per-period cap after
// every settle. This is what a pin bump is re-run against.
test.skipIf(omp === undefined && !onActions)(
  "a PROCEEDING self-check leaves an ask-free stop with no visible turn at all",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(
      omp,
      // A second reply is scripted so a turn that should not happen shows up as a turn rather
      // than as the gateway's "no reply scripted" refusal.
      [[{ type: "text", text: "Done." }], [{ type: "text", text: "Understood." }]],
      // An ordinary session, not a Legion pane: a Legion-driven one is excluded from the nudge.
      { legion: false, openAsks: 0, selfCheck: "PROCEEDING", quietMs: 8_000 }
    );

    // One turn, and the wait proves no second: the user's own. The agent said it is not waiting
    // on anyone, so the whole stop cost one hidden call the user never saw.
    expect(pane.turns()).toHaveLength(1);
    // Both entries loaded but no LEGION_* in the environment: a person's own session gets no
    // `legion` tool, and the Legion entry never speaks to the daemon.
    expect(toolNames(pane.turns()[0] as Request)).not.toContain("legion");
    expect(
      pane.requests.map((request) => request.path).filter((p) => p.startsWith("/legion/"))
    ).toEqual([]);
    const selfChecks = pane.selfChecks();
    expect(selfChecks).toHaveLength(1);
    expect(userText(selfChecks[0] as Request)).toContain("WAITING or PROCEEDING");
    const asks = pane.requests.filter((request) => request.path === "/api/v1/asks/open");
    expect(asks).toHaveLength(2);
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "a WAITING self-check with a held ask runs exactly one nudge and lists the ask",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const heldQuestion = "Which deployment window should I use?";
    const pane = await stallPane(
      omp,
      [[{ type: "text", text: "Done." }], [{ type: "text", text: "Understood." }]],
      {
        legion: false,
        openAskQuestions: [heldQuestion],
        selfCheck: "WAITING",
        quietMs: 8_000,
      }
    );

    const turns = pane.turns();
    // Two, and the wait proves no third: the user's turn, and the one nudge continuation.
    expect(turns).toHaveLength(2);
    expect(userText(turns[0] as Request)).not.toContain("no open ask in Dispatch");
    expect(userText(turns[1] as Request)).toContain(
      "You just said you are waiting on a human for something no open ask in Dispatch covers."
    );
    expect(userText(turns[1] as Request)).toContain("dispatch ask");
    const selfChecks = pane.selfChecks();
    expect(selfChecks).toHaveLength(1);
    expect(userText(selfChecks[0] as Request)).toContain(heldQuestion);
    // The continuation's own stop found the period already fired, so it ran no second
    // self-check and read no third open-ask snapshot: one nudge per period, and no loop.
    const asks = pane.requests.filter((request) => request.path === "/api/v1/asks/open");
    expect(asks).toHaveLength(2);
  },
  180_000
);
