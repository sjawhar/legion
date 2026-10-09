import { afterEach, expect, test } from "bun:test";
import {
  type Cleanup,
  type LegionPane,
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

/** The implementer pane the phase-stall cases run, with its transcript's phase-stall entries. */
interface StallPane extends LegionPane {
  /** The persisted transcript's phase-stall entries, in order. */
  readonly phaseEntries: () => Promise<unknown[]>;
}

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

/** How one pane differs from the implementer pane the phase-stall cases run. */
interface PaneOptions {
  /**
   * False drops every `LEGION_*` variable, so the pane is an ordinary session: the Legion
   * extension stays inert and the Envoy extension's run-end ask nudge is not excluded.
   */
  readonly legion?: boolean;
  /** Configures Dispatch against the stand-in, whose open-ask snapshot answers with this count. */
  readonly openAsks?: number;
  /** The held ask questions the stand-in returns in the session's open-ask snapshot. */
  readonly openAskQuestions?: readonly string[];
  /**
   * Settle when the gateway has answered nothing for this long, instead of at the host's
   * terminal `agent_end`. A `triggerTurn` steer sent from `agent_end` starts its continuation
   * after that frame, so the terminal frame is not the end of the run's provider traffic.
   */
  readonly quietMs?: number;
  /** The one word the gateway answers the run-end self-check with. */
  readonly selfCheck?: string;
}

/**
 * Runs one implementer pane on the real Oh My Pi (`runLegionPane`), the stand-in answering
 * Dispatch's open-ask snapshot when a case configures Dispatch, until its run settles.
 */
async function stallPane(
  binary: string,
  replies: readonly Reply[],
  options: PaneOptions = {}
): Promise<StallPane> {
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
      answer: (url) => {
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
  return { ...pane, phaseEntries: () => pane.transcriptEntries("legion-phase-stall") };
}

test.skipIf(omp === undefined && !onActions)(
  "a turn that ends on a legion tool call written as text gets the follow-up, and the next turn's real legion tool call runs legion handoff complete",
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

    // The worker registered through the daemon's routes, and its handoff_complete minted a grant.
    expect(
      pane.requests.map((request) => request.path).filter((p) => p.startsWith("/legion/"))
    ).toEqual(["/legion/v1/claims/register", "/legion/v1/claims/ready", "/legion/v1/grants"]);
    const turns = pane.turns();
    // Three turns in one run: the text-only one, the follow-up's, and the reply to the tool result.
    // None after: the handoff closed the phase, so the last settle sent nothing.
    expect(turns).toHaveLength(3);
    // A Legion pane, both entries loaded: the host offers the model the `legion` tool.
    expect(toolNames(turns[0] as Request)).toContain("legion");
    expect(userText(turns[0] as Request)).not.toContain("handoff_complete");
    expect(userText(turns[1] as Request)).toContain("written as text");
    expect(userText(turns[1] as Request)).toContain("WAITING");
    expect(await pane.legionLog()).toEqual([
      "handoff complete --summary Stall proof done.",
      "grant stall-grant-1",
    ]);
    // The transcript holds every change, which a worker relaunched with --resume restores.
    expect(await pane.phaseEntries()).toEqual([
      { state: "open" },
      { state: "quiet" },
      { state: "closed" },
    ]);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a WAITING reply to the follow-up settles the run with no further follow-up",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(omp, [
      [{ type: "text", text: "I pushed the change." }],
      [{ type: "text", text: "WAITING: CI on the pull request." }],
    ]);

    const turns = pane.turns();
    expect(turns).toHaveLength(2);
    expect(userText(turns[1] as Request)).toContain("handoff_complete");
    expect(userText(turns[1] as Request)).not.toContain("written as text");
    expect(await pane.legionLog()).toEqual([]);
    expect(await pane.phaseEntries()).toEqual([{ state: "open" }, { state: "quiet" }]);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a shell `legion handoff complete` runs and completes nothing the extension can see: the settle still gets the follow-up",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await stallPane(omp, [
      [
        {
          type: "tool_use",
          name: "bash",
          input: {
            i: "Completing from the shell",
            command: "legion handoff complete --summary 'Shell proof done.'",
          },
        },
      ],
      [{ type: "text", text: "Completed from the shell." }],
      [{ type: "text", text: "WAITING: the phase was completed from the shell." }],
    ]);

    // The bash call minted a grant like any other credentialed command: nothing refused it.
    expect(
      pane.requests.map((request) => request.path).filter((p) => p.startsWith("/legion/"))
    ).toEqual(["/legion/v1/claims/register", "/legion/v1/claims/ready", "/legion/v1/grants"]);
    // The CLI ran with that grant (the stand-in logs `$*`: the quotes were the shell's).
    expect(await pane.legionLog()).toEqual([
      "handoff complete --summary Shell proof done.",
      "grant stall-grant-1",
    ]);
    const turns = pane.turns();
    // Three turns: the bash one, the reply to its result, and the follow-up's.
    expect(turns).toHaveLength(3);
    // The extension saw no completion and asked for the tool call; the reply was not
    // tool-call-shaped text.
    expect(userText(turns[2] as Request)).toContain("handoff_complete");
    expect(userText(turns[2] as Request)).not.toContain("written as text");
    // Never closed: only the `legion` tool's own handoff_complete closes the stall
    // (src/handoff-actions.ts calls onPhaseCompleted from the tool alone); WAITING quieted it.
    expect(await pane.phaseEntries()).toEqual([{ state: "open" }, { state: "quiet" }]);
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
    expect(userText(turns[1] as Request)).toContain("dispatch_ask");
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
