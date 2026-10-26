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

/** The system prompt of a request the gate sends: its own text heads it. */
const isGateRequest = (request: Request): boolean =>
  JSON.stringify(request.body.system ?? "").includes("You are AskGate.");

/** The scripted bash call: a `dispatch ask` by the stand-in's literal path, which the gate reads. */
const ask = (dispatch: string) =>
  `${dispatch} ask --issue DSP-1 --question 'Ship the release today?'`;

/** One tool call of the scripted model turn. */
type Block = { type: "tool_use"; name: string; input: Record<string, unknown> };

/** The real `dispatch` CLI's entry, which a case runs from source through a shim. */
const CLI_ENTRY = path.join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "envoy-client",
  "bin",
  "dispatch.ts"
);
/** Where the fake Dispatch takes a `dispatch ask` for DSP-1. */
const ASKS = "/api/v1/issues/DSP-1/asks";

/**
 * One real session: a `dispatch` on disk, a stand-in model that answers `gateAnswer` to every gate
 * request and `firstTurn(dispatch)` to the session's first turn, then one prompt run to its end.
 * The `dispatch` is a stand-in whose --dry-run prints the arguments and whose send prints that it
 * sent, or, with `cli: "real"`, a shim that runs the real CLI from source against the fake Dispatch
 * the stand-in serves, which records each ask it takes. `envoyJson` is the session's envoy.json and
 * `config` lines are added to the profile's config.yml. `files` are written into the root, beside
 * `bin/`, before the session starts, and `dispatchUrl` names another fake Dispatch. Gives back every
 * request the stand-in took and where the session file landed.
 */
async function runSession(options: {
  gateAnswer: string;
  firstTurn: (dispatch: string) => Block;
  cli?: "standin" | "real";
  envoyJson?: string;
  config?: readonly string[];
  files?: Readonly<Record<string, string>>;
  dispatchUrl?: string;
}): Promise<{ requests: Request[]; sessions: string }> {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  const { root, home, workspace, sessions } = await ompRoot(omp, "askgate-omp-", cleanup);

  // The `dispatch` the session calls by its literal path.
  const dispatch = path.join(root, "bin", "dispatch");
  await mkdir(path.dirname(dispatch));
  await writeFile(
    dispatch,
    options.cli === "real"
      ? `#!/bin/sh\nexec "${process.execPath}" "${CLI_ENTRY}" "$@"\n`
      : [
          "#!/bin/sh",
          'case " $* " in',
          '  *" --dry-run "*) printf \'{"issue":"DSP-1","question":"Ship the release today?"}\\n\' ;;',
          "  *) echo 'stand-in dispatch: sent' ;;",
          "esac",
          "",
        ].join("\n")
  );
  await chmod(dispatch, 0o755);
  await writeFile(path.join(root, "envoy.json"), options.envoyJson ?? "{}\n");
  for (const [name, text] of Object.entries(options.files ?? {}))
    await writeFile(path.join(root, name), text);

  let turns = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
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
    // The fake Dispatch takes an ask.
    if (url.pathname === ASKS)
      return Response.json({
        id: "ask-1",
        issue_key: "DSP-1",
        question: body.question,
        urgency: "med",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "askgate-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base, options.config ?? []);

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
        DISPATCH_URL: options.dispatchUrl ?? base,
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

    expect(await gateEntries(sessions)).toMatchObject([
      {
        tool: "dispatch_ask",
        outcome: "verdict",
        decision: "allow",
        model: "standin/standin-model",
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
        (request) => request.path === "/anthropic/v1/messages" && !isGateRequest(request)
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

// AskGate's enforcement at the CLI, on the real binary with the real CLI and a fake Dispatch: the
// gate reviews a literal call, mints its marker, and the CLI consumes it and posts once. Every
// other form reaches the CLI with no marker and is refused before it sends, which the CLI logs and
// pi-envoy records as an `unmarked` entry. With the switch off nothing asks any form for a marker.
const REFUSED = "dispatch ask was not called: AskGate is on for this session";
/** The ask's argument vector as a JS or Python list literal, the CLI called by its path. */
const askArgv = (dispatch: string) =>
  JSON.stringify([dispatch, "ask", "--issue", "DSP-1", "--question", "Ship the release today?"]);
const asksPosted = (requests: readonly Request[]) =>
  requests.filter((request) => request.path === ASKS).length;
/** The messages of the last turn of the conversation, which hold the call's result. */
const lastTurn = (requests: readonly Request[]) =>
  JSON.stringify(
    requests
      .filter((request) => request.path === "/anthropic/v1/messages" && !isGateRequest(request))
      .at(-1)?.body.messages
  );
const PYTHON_PASSTHROUGH = ["eval:", "  envPassthrough:", "    prefixes: [DISPATCH_, ENVOY_]"];

test.skipIf(omp === undefined && !onActions)(
  "the real CLI: a literal dispatch ask is reviewed, carries its marker and posts once",
  async () => {
    const { requests, sessions } = await runSession({
      cli: "real",
      gateAnswer: '{"decision":"allow"}',
      firstTurn: (dispatch) => ({
        type: "tool_use",
        name: "bash",
        input: { command: ask(dispatch) },
      }),
    });
    expect(asksPosted(requests)).toBe(1);
    expect(lastTurn(requests)).not.toContain(REFUSED);
    expect(await gateEntries(sessions)).toMatchObject([
      { tool: "dispatch_ask", outcome: "verdict", decision: "allow" },
    ]);
  },
  180_000
);

// The review of the marker design at cf3f1e021, C1: the gate bound an upload's marker to its own
// review input, so the CLI refused every reviewed markdown upload by `--path`. The fake Dispatch
// here takes the upload's form and records the file's bytes.
test.skipIf(omp === undefined && !onActions)(
  "the real CLI: a markdown upload by --path is reviewed, carries its marker and posts its bytes once",
  async () => {
    const brief = "# Brief\n\nShip the release today.\n";
    const uploads: string[] = [];
    const fake = Bun.serve({
      port: 0,
      async fetch(request) {
        const route = new URL(request.url).pathname;
        if (request.method === "POST" && route === "/api/v1/issues/DSP-1/artifacts") {
          const file = (await request.formData()).get("file");
          uploads.push(file instanceof Blob ? await file.text() : "");
          return Response.json({
            artifact: { id: "artifact-1", issue_key: "DSP-1", name: "brief.md", slug: "brief-md" },
            version: { number: 1 },
          });
        }
        // An unavailable open-asks read arms no run-end self-check.
        return new Response("unavailable", { status: 503 });
      },
    });
    cleanup.push(() => fake.stop(true));
    const { requests, sessions } = await runSession({
      cli: "real",
      gateAnswer: '{"decision":"allow"}',
      dispatchUrl: `http://127.0.0.1:${fake.port}`,
      files: { "brief.md": brief },
      firstTurn: (dispatch) => ({
        type: "tool_use",
        name: "bash",
        input: {
          command: `${dispatch} artifact --issue DSP-1 --name brief.md --path ${path.join(path.dirname(path.dirname(dispatch)), "brief.md")}`,
        },
      }),
    });
    expect(uploads).toEqual([brief]);
    expect(lastTurn(requests)).not.toContain("was not called");
    expect(await gateEntries(sessions)).toMatchObject([
      { tool: "dispatch_artifact", outcome: "verdict", decision: "allow" },
    ]);
  },
  180_000
);

for (const [form, firstTurn, config] of [
  [
    "python3 -c under bash",
    (dispatch: string): Block => ({
      type: "tool_use",
      name: "bash",
      input: { command: `python3 -c 'import subprocess; subprocess.run(${askArgv(dispatch)})'` },
    }),
    [],
  ],
  [
    "a Python eval cell's subprocess.run",
    (dispatch: string): Block => ({
      type: "tool_use",
      name: "eval",
      input: {
        language: "py",
        code: `import subprocess\nr = subprocess.run(${askArgv(dispatch)}, capture_output=True, text=True)\nprint(r.stdout)`,
      },
    }),
    PYTHON_PASSTHROUGH,
  ],
  [
    "a JS eval cell's Bun.spawnSync",
    (dispatch: string): Block => ({
      type: "tool_use",
      name: "eval",
      input: {
        language: "js",
        code: `const r = Bun.spawnSync(${askArgv(dispatch)});\nreturn new TextDecoder().decode(r.stdout);`,
      },
    }),
    [],
  ],
] as const)
  test.skipIf(omp === undefined && !onActions)(
    `the real CLI refuses a gated write from ${form}, which carries no marker`,
    async () => {
      const { requests, sessions } = await runSession({
        cli: "real",
        gateAnswer: '{"decision":"allow"}',
        firstTurn,
        config,
      });
      expect(asksPosted(requests)).toBe(0);
      expect(lastTurn(requests)).toContain(REFUSED);
      // The gate read no call; the CLI's refusal is recorded from its ledger.
      expect(await gateEntries(sessions)).toMatchObject([
        { tool: "dispatch_ask", outcome: "unmarked", decision: "revise", verdictMode: "block" },
      ]);
    },
    180_000
  );

test.skipIf(omp === undefined && !onActions)(
  "with askgate.enabled false no record is written, and python3 -c under bash posts once",
  async () => {
    const { requests, sessions } = await runSession({
      cli: "real",
      envoyJson: `${JSON.stringify({ askgate: { enabled: false } })}\n`,
      gateAnswer: '{"decision":"allow"}',
      firstTurn: (dispatch) => ({
        type: "tool_use",
        name: "bash",
        input: { command: `python3 -c 'import subprocess; subprocess.run(${askArgv(dispatch)})'` },
      }),
    });
    expect(asksPosted(requests)).toBe(1);
    expect(lastTurn(requests)).not.toContain(REFUSED);
    expect(await gateEntries(sessions)).toEqual([]);
  },
  180_000
);
