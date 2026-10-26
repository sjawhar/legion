import { afterEach, expect, test } from "bun:test";
import { chmod, mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// AskGate on the real Oh My Pi: the loader resolves the gate's string-literal imports of the host's
// own modules (pi-ai, the session context, the settings, the fallback chains) only inside the
// binary, so only a real session shows that the gate binds, reviews a `dispatch ask` with the
// session's model and records one `advisor-gate` entry. LEGION_TEST_OMP names the binary, as in
// dispatch-first-omp.test.ts. A run without one skips, except on GitHub Actions.
//
// What these tests prove, and what they do not: the gate binds inside the binary, reads the call,
// asks the session's model, records its entry, and hands its note back where the agent reads it.
// The model is a stand-in that answers a scripted line, so nothing here is evidence about a real
// model's judgement; one `bash` form and one eval cell in each language are driven, not the
// grammar's range, which `askgate.test.ts` covers.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The system prompt of a request the charter check sends: its own frame heads it. */
const isGateRequest = (request: Request): boolean =>
  JSON.stringify(request.body.system ?? "").includes("You are AskGate.");
/** The rules check's own frame, which a `dispatch ask` brings beside the charter call. */
const isRuleRequest = (request: Request): boolean =>
  JSON.stringify(request.body.system ?? "").includes("You are AskGate's rule check.");

/** The scripted bash call: a `dispatch ask` by the stand-in's literal path, which the gate reads. */
const ask = (dispatch: string) =>
  `${dispatch} ask --issue DSP-1 --question 'Ship the release today?'`;

/** One tool call of the scripted model turn. */
type Block = { type: "tool_use"; name: string; input: Record<string, unknown> };

/**
 * One real session: a stand-in `dispatch` on disk, a stand-in model that answers `gateAnswer` to
 * every charter request, no match to every rules request, and `firstTurn(dispatch)` to the
 * session's first turn, then one prompt run to its end. Gives back every request the stand-in took
 * and where the session file landed.
 */
async function runSession(options: {
  gateAnswer: string;
  firstTurn: (dispatch: string) => Block;
}): Promise<{ requests: Request[]; sessions: string }> {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  const { root, home, workspace, sessions } = await ompRoot(omp, "askgate-omp-", cleanup);

  // A stand-in `dispatch`, called by its literal path: its --dry-run prints the arguments, as the
  // real CLI does, and a send prints that it sent, so the test needs no built CLI and no Dispatch.
  const dispatch = path.join(root, "bin", "dispatch");
  await mkdir(path.dirname(dispatch));
  await writeFile(
    dispatch,
    [
      "#!/bin/sh",
      'case " $* " in',
      '  *" --dry-run "*) printf \'{"issue":"DSP-1","question":"Ship the release today?"}\\n\' ;;',
      "  *) echo 'stand-in dispatch: sent' ;;",
      "esac",
      "",
    ].join("\n")
  );
  await chmod(dispatch, 0o755);

  let turns = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      if (isRuleRequest({ path: url.pathname, body }))
        return new Response(
          messageStream([{ type: "text", text: '{"matches": [], "reason": ""}' }], "rules"),
          { headers: { "content-type": "text/event-stream" } }
        );
      if (isGateRequest({ path: url.pathname, body }))
        return new Response(messageStream([{ type: "text", text: options.gateAnswer }], "gate"), {
          headers: { "content-type": "text/event-stream" },
        });
      turns += 1;
      const blocks =
        turns === 1 ? [options.firstTurn(dispatch)] : [{ type: "text" as const, text: "Asked." }];
      return new Response(messageStream(blocks, `turn_${turns}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "askgate-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  let settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    omp,
    {
      extensions: [path.join(import.meta.dir, "..", "envoy.ts")],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        DISPATCH_URL: base,
        DISPATCH_TOKEN: "askgate-token",
        ENVOY_CONFIG_FILE: path.join(root, "envoy.json"),
      },
      onFrame: (frame) => {
        if (
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        )
          settled.resolve();
      },
    },
    cleanup
  );
  settled = Promise.withResolvers<void>();
  rpc.send({ type: "prompt", message: "Ask whether to ship." });
  await Promise.race([settled.promise, rpc.closed]);
  await rpc.end();
  return { requests, sessions };
}

/** Every `advisor-gate` entry the session recorded, in order. */
async function gateEntries(sessions: string): Promise<unknown[]> {
  const files = (await readdir(sessions, { recursive: true })).filter((file) =>
    file.endsWith(".jsonl")
  );
  expect(files).toHaveLength(1);
  return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
    .split("\n")
    .filter(Boolean)
    .map((line): unknown => JSON.parse(line))
    .flatMap((entry) =>
      typeof entry === "object" &&
      entry !== null &&
      "customType" in entry &&
      entry.customType === "advisor-gate" &&
      "data" in entry
        ? [entry.data]
        : []
    );
}

test.skipIf(omp === undefined && !onActions)(
  "a dispatch ask in a session with Dispatch on is reviewed once by the session's model, and one advisor-gate entry is recorded",
  async () => {
    const { requests, sessions } = await runSession({
      gateAnswer: '{"decision":"allow"}',
      firstTurn: (dispatch) => ({
        type: "tool_use",
        name: "bash",
        input: { command: ask(dispatch) },
      }),
    });

    const gate = requests.filter(
      (request) => request.path === "/anthropic/v1/messages" && isGateRequest(request)
    );
    expect(gate).toHaveLength(1);
    expect(JSON.stringify(gate[0]?.body.messages)).toContain("Ship the release today?");
    // The `dispatch ask` opens a new ask, so the rules check asks the model beside the charter check.
    const rules = requests.filter(
      (request) => request.path === "/anthropic/v1/messages" && isRuleRequest(request)
    );
    expect(rules).toHaveLength(1);
    // The request body is JSON, so the listed ask is looked for as JSON escapes it.
    expect(JSON.stringify(rules[0]?.body.messages)).toContain(
      JSON.stringify('<ask n="1" kind="ask">\nShip the release today?\n</ask>').slice(1, -1)
    );

    expect(await gateEntries(sessions)).toMatchObject([
      {
        tool: "dispatch_ask",
        outcome: "verdict",
        decision: "allow",
        model: "standin/standin-model",
        ruleOutcome: "none",
        ruleModel: "standin/standin-model",
        target: "dispatch_ask issue=DSP-1",
      },
    ]);
  },
  180_000
);

// A write an eval cell makes reaches the gate through the tool bridge, which calls the tool with no
// loop context, so the note of a write that went out unreviewed was dropped: the agent was told
// nothing. The note rides on the call's own result now, so the next model request holds it. A JS
// cell runs in a worker of the Oh My Pi process; a Python cell runs in a Python process of its own
// and reaches the same bridge over HTTP, so each language gets a case. The Python runner needs only
// the standard library and a `python3` on PATH.
for (const [language, code] of [
  ["js", (command: string) => `return await tool.bash({ command: ${JSON.stringify(command)} })`],
  ["py", (command: string) => `print(await tool.bash({"command": ${JSON.stringify(command)}}))`],
] as const)
  test.skipIf(omp === undefined && !onActions)(
    `a write a ${language} eval cell makes, whose review gives no verdict, tells the next model request it went out unreviewed`,
    async () => {
      const { requests, sessions } = await runSession({
        // No JSON verdict on the last line: the gate reaches no verdict and the write posts
        // unreviewed.
        gateAnswer: "The call looks fine to me.",
        firstTurn: (dispatch) => ({
          type: "tool_use",
          name: "eval",
          input: { language, code: code(ask(dispatch)) },
        }),
      });

      const main = requests.filter(
        (request) =>
          request.path === "/anthropic/v1/messages" &&
          !isGateRequest(request) &&
          !isRuleRequest(request)
      );
      expect(main.length).toBeGreaterThanOrEqual(2);
      const next = JSON.stringify(main.at(-1)?.body.messages);
      expect(next).toContain("AskGate did not review this Dispatch write");
      expect(next).toContain("The call ran unreviewed.");
      expect(next).toContain("stand-in dispatch: sent");

      expect(await gateEntries(sessions)).toMatchObject([
        { tool: "dispatch_ask", outcome: "no-verdict", decision: "allow" },
      ]);
    },
    180_000
  );
