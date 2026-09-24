import { afterEach, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

// The phase-stall follow-up on the real Oh My Pi (src/legion/phase-stall.ts): only the real binary
// shows when the host fires `session_stop`, how it turns the returned follow-up into the next turn,
// what the model is sent, and that the transcript keeps the state a resumed worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in packages/daemon/src/daemon/omp-pin.ts, which
// CI's pi-envoy job installs; on the devbox, `mise where <pin>`/bin/omp. A run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
const extensions = path.join(import.meta.dir);

type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> };

interface Request {
  readonly path: string;
  readonly body: Record<string, unknown>;
}

interface Pane {
  /** Every request the stand-in served: the model gateway's, the daemon's, and the listener's. */
  readonly requests: Request[];
  /** The Messages requests the model gateway answered, in order. */
  readonly turns: () => Request[];
  /** One line per invocation of the stand-in `legion`: its arguments, then the grant it read. */
  readonly legionLog: () => Promise<string[]>;
  /** The persisted transcript's phase-stall entries, in order. */
  readonly phaseEntries: () => Promise<unknown[]>;
}

const cleanup: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The Anthropic Messages stream for one scripted reply. */
function messageStream(blocks: readonly Block[], id: string): string {
  const events: [string, unknown][] = [
    [
      "message_start",
      {
        type: "message_start",
        message: {
          id,
          type: "message",
          role: "assistant",
          model: "standin-model",
          content: [],
          stop_reason: null,
          stop_sequence: null,
          usage: { input_tokens: 5, output_tokens: 1 },
        },
      },
    ],
  ];
  blocks.forEach((block, index) => {
    if (block.type === "text") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
      ]);
      events.push([
        "content_block_delta",
        { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } },
      ]);
    } else {
      events.push([
        "content_block_start",
        {
          type: "content_block_start",
          index,
          content_block: {
            type: "tool_use",
            id: `toolu_${id}_${index}`,
            name: block.name,
            input: {},
          },
        },
      ]);
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        },
      ]);
    }
    events.push(["content_block_stop", { type: "content_block_stop", index }]);
  });
  const stopReason = blocks.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn";
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: { stop_reason: stopReason, stop_sequence: null },
      usage: { output_tokens: 1 },
    },
  ]);
  events.push(["message_stop", { type: "message_stop" }]);
  return events.map(([name, data]) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}

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

/**
 * Runs one implementer pane on the real Oh My Pi until its run settles: the Legion and Envoy
 * extensions from this checkout, booted against a stand-in for the TypeScript daemon's worker
 * routes and the Envoy listener (no NATS: the Envoy extension then skips inbound delivery, and
 * the role claim is two listener calls), with a stand-in model gateway that answers the pane's
 * turns from `replies`, and a stand-in `legion` on PATH that records what it was run with. The
 * daemon's assignment arrives as the RPC `prompt`, as both daemons deliver it.
 */
async function runPane(binary: string, replies: readonly (readonly Block[])[]): Promise<Pane> {
  const root = await mkdtemp(path.join(os.tmpdir(), "legion-phase-stall-"));
  cleanup.push(() => rm(root, { recursive: true, force: true }));
  const home = path.join(root, "home");
  const state = path.join(root, "state");
  const workspace = path.join(root, "workspace");
  const bin = path.join(root, "bin");
  const sessions = path.join(root, "sessions");
  const legionLog = path.join(root, "legion.log");
  for (const directory of [path.join(home, ".omp", "agent"), workspace, bin, sessions]) {
    await mkdir(directory, { recursive: true });
  }
  await mkdir(path.join(state, "secrets"), { recursive: true, mode: 0o700 });

  const requests: Request[] = [];
  let answered = 0;
  let grants = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url);
      const text = request.method === "POST" ? await request.text() : "";
      const body = text === "" ? {} : (JSON.parse(text) as Record<string, unknown>);
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/anthropic/v1/messages") {
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
    },
  });
  cleanup.push(async () => {
    await server.stop(true);
  });
  const base = `http://127.0.0.1:${server.port}`;

  // The stand-in gateway as the profile's one provider, keyed by a literal: a plain API-key caller.
  await writeFile(
    path.join(home, ".omp", "agent", "models.yml"),
    [
      "providers:",
      "  standin:",
      `    baseUrl: ${base}/anthropic`,
      "    auth: apiKey",
      "    api: anthropic-messages",
      "    apiKey: standin-key",
      "    models:",
      "      - id: standin-model",
      "        name: Stand-in",
      "        reasoning: false",
      "        input: [text]",
      "        contextWindow: 200000",
      "        maxTokens: 8000",
      "        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}",
      "",
    ].join("\n")
  );
  // Every role on the stand-in, and the providers a devbox or runner could answer from without it
  // disabled, so no turn reaches a real model.
  await writeFile(
    path.join(home, ".omp", "agent", "config.yml"),
    [
      "enabledModels:",
      "  - standin/*",
      "disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]",
      "modelRoles:",
      ...["default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"].map(
        (role) => `  ${role}: standin/standin-model`
      ),
      "",
    ].join("\n")
  );
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

  const child = Bun.spawn(
    [
      binary,
      "--mode",
      "rpc",
      "--no-extensions",
      "-e",
      path.join(extensions, "envoy.ts"),
      "-e",
      path.join(extensions, "legion.ts"),
      "--no-skills",
      "--no-rules",
      "--no-lsp",
      "--no-title",
      "--session-dir",
      sessions,
      "--cwd",
      workspace,
    ],
    {
      cwd: workspace,
      env: {
        HOME: home,
        PATH: `${bin}:/usr/local/bin:/usr/bin:/bin`,
        ENVOY_URL: base,
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: "implementer",
        LEGION_TREE: "STALL-1",
        LEGION_ISSUE: "STALL-2",
        LEGION_GENERATION: "1",
        LEGION_BOOT_TOKEN: "stall-boot",
        LEGION_STATE_DIR: state,
        LEGION_WORKSPACE: workspace,
        LEGION_GRANT_FILE: path.join(state, "secrets", "legion-stall-stall-2-implementer-grant"),
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  cleanup.push(async () => {
    child.kill("SIGKILL");
    await child.exited;
  });

  // The run has settled when the RPC stream reports its terminal agent_end: a continuation the
  // host scheduled (the follow-up) starts its turn before that, under the same run. Both streams
  // are read to the end, so a full pipe never blocks omp.
  const stderr = new Response(child.stderr).text();
  const settled = Promise.withResolvers<void>();
  void (async () => {
    let buffered = "";
    for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk;
      let newline = buffered.indexOf("\n");
      while (newline !== -1) {
        const frame: unknown = JSON.parse(buffered.slice(0, newline));
        buffered = buffered.slice(newline + 1);
        newline = buffered.indexOf("\n");
        if (
          typeof frame === "object" &&
          frame !== null &&
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        ) {
          settled.resolve();
        }
      }
    }
    settled.reject(new Error(`omp closed its RPC stream before its run settled:\n${await stderr}`));
  })();
  child.stdin.write(`${JSON.stringify({ type: "prompt", message: "Implement STALL-2." })}\n`);
  child.stdin.flush();
  await settled.promise;
  child.stdin.end();
  await child.exited;

  return {
    requests,
    turns: () => requests.filter((request) => request.path === "/anthropic/v1/messages"),
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
