import { afterEach, expect, test } from "bun:test";
import { chmod, mkdir, readdir, readFile, writeFile } from "node:fs/promises";
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
} from "./test-omp-harness";

// The phase-stall follow-up on the real Oh My Pi (src/legion/phase-stall.ts): only the real binary
// shows when the host fires `session_stop`, how it turns the returned follow-up into the next turn,
// what the model is sent, and that the transcript keeps the state a resumed worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in packages/daemon/src/daemon/omp-pin.ts, which
// CI's pi-envoy job installs; on the devbox, `mise where <pin>`/bin/omp. A run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
// It is also the only check that the run-end nudge's hidden self-check starts no run: the nudge
// treats any `agent_start` after a settle as a newer run and withholds its steer, so a host that
// counted the side turn as a run would silence the nudge with every unit test still green.
// The WAITING self-check case below fails if that ever changes.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

interface Pane {
  /** Every request the stand-in served: the model gateway's, the daemon's, and the listener's. */
  readonly requests: Request[];
  /** The Messages requests that were turns of the conversation, in order. */
  readonly turns: () => Request[];
  /** The Messages requests that were side turns (the self-check), in order. */
  readonly selfChecks: () => Request[];
  /** One line per invocation of the stand-in `legion`: its arguments, then the grant it read. */
  readonly legionLog: () => Promise<string[]>;
  /** The persisted transcript's phase-stall entries, in order. */
  readonly phaseEntries: () => Promise<unknown[]>;
}

/**
 * Whether a Messages request is a side turn rather than a turn. The host sends one as an
 * ordinary Messages request over a snapshot of the conversation whose last message is the `<btw>`
 * block around the question — the host adds it for `pi.askEphemeral` on the pin (measured there,
 * which is the only thing that can say), and pi-envoy adds it for `ctx.runEphemeralTurn` on 18.3 —
 * so the stand-in answers it distinctly and nothing about it reaches the transcript.
 */
function isSelfCheck(request: Request): boolean {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null).includes("<btw>");
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
 * Runs one implementer pane on the real Oh My Pi until its run settles: the Legion and Envoy
 * extensions from this checkout, booted against a stand-in for the TypeScript daemon's worker
 * routes and the Envoy listener (no NATS: the Envoy extension then skips inbound delivery, and
 * the role claim is two listener calls), with a stand-in model gateway that answers the pane's
 * turns from `replies`, and a stand-in `legion` on PATH that records what it was run with. The
 * daemon's assignment arrives as the RPC `prompt`, as both daemons deliver it.
 */
async function runPane(
  binary: string,
  replies: readonly (readonly Block[])[],
  options: PaneOptions = {}
): Promise<Pane> {
  const legionPane = options.legion ?? true;
  const { root, home, workspace, sessions } = await ompRoot(binary, "legion-phase-stall-", cleanup);
  const state = path.join(root, "state");
  const bin = path.join(root, "bin");
  const legionLog = path.join(root, "legion.log");
  await mkdir(bin, { recursive: true });
  await mkdir(path.join(state, "secrets"), { recursive: true, mode: 0o700 });

  let answered = 0;
  let selfChecks = 0;
  let grants = 0;
  let lastAnsweredAt = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      lastAnsweredAt = Date.now();
      // The self-check is not a turn: it consumes no scripted reply, and the conversation's
      // next turn is answered as if it had never happened — which is what the host's snapshot
      // makes true.
      if (isSelfCheck({ path: url.pathname, body })) {
        selfChecks += 1;
        const verdict = options.selfCheck ?? "PROCEEDING";
        return new Response(messageStream([{ type: "text", text: verdict }], `btw_${selfChecks}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      const reply = replies[answered];
      answered += 1;
      if (reply === undefined) {
        return Response.json(
          {
            type: "error",
            error: { type: "invalid_request_error", message: "no reply scripted" },
          },
          { status: 400 }
        );
      }
      return new Response(messageStream(reply, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/api/v1/asks/open") {
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
    }
    if (url.pathname === "/legion/v1/worker/started") {
      return Response.json({
        roleToken: "legion-stall-stall-2-implementer",
        secret: "stall-secret",
        gitName: "Legion Worker",
        gitEmail: "worker@example.test",
      });
    }
    if (url.pathname === "/legion/v1/worker/ready") return Response.json({});
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `stall-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "stall-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  const legion = path.join(bin, "legion");
  await writeFile(
    legion,
    [
      "#!/bin/sh",
      `printf '%s\\n' "$*" >> '${legionLog}'`,
      `printf 'grant %s\\n' "$(cat "$LEGION_GRANT_FILE")" >> '${legionLog}'`,
      "",
    ].join("\n")
  );
  await chmod(legion, 0o755);

  // The run has settled when the RPC stream reports its terminal agent_end: a continuation the
  // host scheduled (the follow-up) starts its turn before that, under the same run. A steer the
  // extension sends from `agent_end` instead starts its continuation after that frame, so
  // `quietMs` waits for the gateway to fall silent rather than for the frame.
  const settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions: ["envoy.ts", "legion.ts"],
      home,
      workspace,
      sessions,
      bin,
      env: {
        ENVOY_URL: base,
        ...(options.openAsks === undefined && options.openAskQuestions === undefined
          ? {}
          : { DISPATCH_URL: base, DISPATCH_TOKEN: "stall-dispatch-token" }),
        ...(legionPane
          ? {
              LEGION_DAEMON_URL: base,
              LEGION_ROLE: "implementer",
              LEGION_TREE: "STALL-1",
              LEGION_ISSUE: "STALL-2",
              LEGION_GENERATION: "1",
              LEGION_BOOT_TOKEN: "stall-boot",
              LEGION_STATE_DIR: state,
              LEGION_WORKSPACE: workspace,
              LEGION_GRANT_FILE: path.join(
                state,
                "secrets",
                "legion-stall-stall-2-implementer-grant"
              ),
            }
          : {}),
      },
      onFrame: (frame) => {
        if (
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        ) {
          settled.resolve();
        }
      },
    },
    cleanup
  );
  rpc.send({ type: "prompt", message: "Implement STALL-2." });
  if (options.quietMs === undefined) {
    await Promise.race([settled.promise, rpc.closed]);
  } else {
    const quietMs = options.quietMs;
    const deadline = Date.now() + 90_000;
    await Promise.race([
      (async () => {
        // A real clock, deliberately: the proposition is that the real host started no further
        // turn, and a host that does nothing emits no signal to await. The whole point is to
        // see the absence, and only elapsed time shows it.
        while (Date.now() < deadline) {
          await Bun.sleep(200);
          if (answered > 0 && Date.now() - lastAnsweredAt >= quietMs) return;
        }
        throw new Error(`the stand-in gateway never went quiet for ${quietMs} ms`);
      })(),
      rpc.closed,
    ]);
  }
  await rpc.end();

  return {
    requests,
    turns: () =>
      requests.filter(
        (request) => request.path === "/anthropic/v1/messages" && !isSelfCheck(request)
      ),
    selfChecks: () =>
      requests.filter(
        (request) => request.path === "/anthropic/v1/messages" && isSelfCheck(request)
      ),
    legionLog: async () => {
      // No log file: the stand-in never ran.
      const text = await readFile(legionLog, "utf8").catch((error: NodeJS.ErrnoException) => {
        if (error.code === "ENOENT") return "";
        throw error;
      });
      return text.split("\n").filter(Boolean);
    },
    phaseEntries: async () => {
      const files = (await readdir(sessions, { recursive: true })).filter((file) =>
        file.endsWith(".jsonl")
      );
      if (files.length !== 1)
        throw new Error(`want one transcript under ${sessions}, found ${files}`);
      return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
        .split("\n")
        .filter(Boolean)
        .map((line): unknown => JSON.parse(line))
        .filter(
          (entry) =>
            typeof entry === "object" &&
            entry !== null &&
            "customType" in entry &&
            entry.customType === "legion-phase-stall"
        )
        .map((entry) => (entry as { readonly data: unknown }).data);
    },
  };
}

test.skipIf(omp === undefined && !onActions)(
  "a turn that ends on a legion tool call written as text gets the follow-up, and the next turn's real legion tool call runs legion handoff complete",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await runPane(omp, [
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
    ).toEqual(["/legion/v1/worker/started", "/legion/v1/worker/ready", "/legion/v1/grants"]);
    const turns = pane.turns();
    // Three turns in one run: the text-only one, the follow-up's, and the reply to the tool result.
    // None after: the handoff closed the phase, so the last settle sent nothing.
    expect(turns).toHaveLength(3);
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
    const pane = await runPane(omp, [
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
    const pane = await runPane(
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
    const pane = await runPane(
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
