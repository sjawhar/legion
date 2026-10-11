// The harness shared by the tests that run the real Oh My Pi binary (each plugin's `*-omp.test.ts`):
// a stand-in server that is the model gateway and whatever else a test answers, a profile whose
// every model role is that stand-in, one `omp --mode rpc` child with only the caller's extensions
// loaded, and the Legion pane runner (`runLegionPane`) that puts those together as one Legion pane
// against a stand-in daemon and listener. The profile format, the flags and the RPC stream are the
// Oh My Pi pin's (the repository's .omp-pin), so a pin bump that changes one is fixed here once.
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { linkOmpNatives } from "./omp-natives";

export interface Request {
  readonly path: string;
  readonly body: Record<string, unknown>;
}

export type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> };

/**
 * One scripted reply: the blocks, or a function of the Messages request it answers, so a reply can
 * be built from the previous turn's tool result (`toolResultsIn(request).at(-1)`).
 */
export type Reply = readonly Block[] | ((request: Request) => readonly Block[]);

/** What a test undoes after each case, run last first. */
export type Cleanup = (() => Promise<void>)[];

/**
 * A scratch root with the profile's home, the session's workspace and its transcript directory.
 * The home's Oh My Pi natives are hardlinks to `binary`'s one cached copy (omp-natives.ts), so
 * a case writes none of them.
 */
export async function ompRoot(
  binary: string,
  prefix: string,
  cleanup: Cleanup
): Promise<{
  readonly home: string;
  readonly workspace: string;
  readonly sessions: string;
  readonly root: string;
}> {
  const root = await mkdtemp(path.join(os.tmpdir(), prefix));
  cleanup.push(() => rm(root, { recursive: true, force: true }));
  const home = path.join(root, "home");
  const workspace = path.join(root, "workspace");
  const sessions = path.join(root, "sessions");
  for (const directory of [path.join(home, ".omp", "agent"), workspace, sessions]) {
    await mkdir(directory, { recursive: true });
  }
  await linkOmpNatives(binary, home);
  return { root, home, workspace, sessions };
}

/**
 * Serves every request with `answer`, recording each first. `answer` gets the request's parsed JSON
 * body, empty for a GET. Returns the recorded requests and the server's base URL.
 */
export function serveStandin(
  cleanup: Cleanup,
  answer: (url: URL, body: Record<string, unknown>) => Response | Promise<Response>
): { readonly requests: Request[]; readonly base: string } {
  const requests: Request[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url);
      const text = request.method === "POST" ? await request.text() : "";
      const body = text === "" ? {} : (JSON.parse(text) as Record<string, unknown>);
      requests.push({ path: url.pathname, body });
      return answer(url, body);
    },
  });
  cleanup.push(async () => {
    await server.stop(true);
  });
  return { requests, base: `http://127.0.0.1:${server.port}` };
}

/**
 * Writes the profile under `home`: the stand-in at `base` as its one provider, keyed by a literal
 * like a plain API-key caller, every model role on it, and every provider a devbox or runner could
 * answer from without it disabled, so no turn reaches a real model. `config` lines are appended to
 * config.yml.
 */
export async function writeStandinProfile(
  home: string,
  base: string,
  config: readonly string[] = []
): Promise<void> {
  const agent = path.join(home, ".omp", "agent");
  await writeFile(
    path.join(agent, "models.yml"),
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
  await writeFile(
    path.join(agent, "config.yml"),
    [
      "enabledModels:",
      "  - standin/*",
      "disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]",
      "modelRoles:",
      ...["default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"].map(
        (role) => `  ${role}: standin/standin-model`
      ),
      ...config,
      "",
    ].join("\n")
  );
}

export interface Rpc {
  /** Writes one RPC command to omp's stdin. */
  send(command: Record<string, unknown>): void;
  /** Closes omp's stdin and waits for it to exit. */
  end(): Promise<void>;
  /**
   * Rejects, naming omp's stderr, once omp closes its RPC stream, as it does when it exits. Race it
   * against a step's own settle; it carries a handler, so a case that has settled never sees it.
   */
  readonly closed: Promise<never>;
}

/**
 * Starts `omp --mode rpc` in `workspace` with only `extensions` loaded — absolute paths, since this
 * module lives in another package than its callers (pass `path.join(import.meta.dir, …)`) — no
 * skills, rules, LSP or title call, its transcripts under `sessions`, `home` as HOME and `bin`, when
 * given, first on PATH. `onFrame` gets each RPC frame omp prints, in order. Both of omp's streams
 * are read to the end, so a full pipe never blocks it, and the child is killed at cleanup.
 */
export function spawnRpc(
  binary: string,
  options: {
    readonly extensions: readonly string[];
    readonly home: string;
    readonly workspace: string;
    readonly sessions: string;
    readonly bin?: string;
    readonly env: Record<string, string>;
    readonly onFrame: (frame: object) => void;
  },
  cleanup: Cleanup
): Rpc {
  const child = Bun.spawn(
    [
      binary,
      "--mode",
      "rpc",
      "--no-extensions",
      ...options.extensions.flatMap((file) => ["-e", file]),
      "--no-skills",
      "--no-rules",
      "--no-lsp",
      "--no-title",
      "--session-dir",
      options.sessions,
      "--cwd",
      options.workspace,
    ],
    {
      cwd: options.workspace,
      env: {
        HOME: options.home,
        PATH: [options.bin, "/usr/local/bin:/usr/bin:/bin"].filter(Boolean).join(":"),
        ...options.env,
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

  const stderr = new Response(child.stderr).text();
  const closed = (async (): Promise<never> => {
    let buffered = "";
    for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk;
      let newline = buffered.indexOf("\n");
      while (newline !== -1) {
        const frame: unknown = JSON.parse(buffered.slice(0, newline));
        buffered = buffered.slice(newline + 1);
        newline = buffered.indexOf("\n");
        if (typeof frame === "object" && frame !== null) options.onFrame(frame);
      }
    }
    throw new Error(`omp closed its RPC stream before the step settled:\n${await stderr}`);
  })();
  closed.catch(() => undefined);

  return {
    send(command) {
      child.stdin.write(`${JSON.stringify(command)}\n`);
      child.stdin.flush();
    },
    async end() {
      child.stdin.end();
      await child.exited;
    },
    closed,
  };
}

/** The Anthropic Messages stream for one scripted reply. */
export function messageStream(blocks: readonly Block[], id: string): string {
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

/**
 * Whether a Messages request is a side turn rather than a turn. The host sends one as an
 * ordinary Messages request over a snapshot of the conversation whose last message is the `<btw>`
 * block around the question, which pi-envoy adds for `ctx.runEphemeralTurn` — so the stand-in
 * answers it distinctly and nothing about it reaches the transcript.
 */
function isSelfCheck(request: Request): boolean {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null).includes("<btw>");
}

/** A `tool_result` block as the host sent it back to the gateway. */
export interface ToolResult {
  readonly tool_use_id: string;
  readonly is_error: boolean;
  /** The result's string content, or its text blocks joined. */
  readonly text: string;
}

/** The `tool_result` blocks of a Messages request's user messages, in conversation order. */
export function toolResultsIn(request: Request): ToolResult[] {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages.flatMap((message) => {
    if (typeof message !== "object" || message === null) return [];
    if (!("role" in message) || message.role !== "user") return [];
    if (!("content" in message) || !Array.isArray(message.content)) return [];
    return message.content.flatMap((block: unknown) => {
      if (typeof block !== "object" || block === null) return [];
      if (!("type" in block) || block.type !== "tool_result") return [];
      if (!("tool_use_id" in block) || typeof block.tool_use_id !== "string") return [];
      // The result's content is a string or content blocks, of which the text ones are its text.
      const content = "content" in block ? block.content : undefined;
      const text =
        typeof content === "string"
          ? content
          : (Array.isArray(content) ? content : [])
              .map((part: unknown) =>
                typeof part === "object" &&
                part !== null &&
                "type" in part &&
                part.type === "text" &&
                "text" in part &&
                typeof part.text === "string"
                  ? part.text
                  : ""
              )
              .join("");
      return [
        {
          tool_use_id: block.tool_use_id,
          is_error: "is_error" in block && block.is_error === true,
          text,
        },
      ];
    });
  });
}

/** Which Legion pane `runLegionPane` runs, and what its stand-in answers beyond the runner's routes. */
export interface LegionPaneOptions {
  /**
   * Names every fixture string a case's assertions may meet: the scratch root's `legion-<name>-`
   * prefix, the grant ids `<name>-grant-<n>` the `legion` tool's own operations mint, the claim's
   * `<name>-secret`, the listener's `<name>-machine`, the boot token `<name>-boot`, the Dispatch
   * token `<name>-dispatch-token` and the claim token `legion-<name>-<issue>-<role>`, the issue
   * lower-cased.
   */
  readonly name: string;
  /** `LEGION_ROLE`. */
  readonly role: string;
  /** `LEGION_TREE`. */
  readonly tree: string;
  /** `LEGION_ISSUE`; the tree itself makes the pane the tree's root: a root architect's. */
  readonly issue: string;
  /** The daemon's assignment, sent as the RPC `prompt`. */
  readonly prompt: string;
  /**
   * False drops every `LEGION_*` variable, so the pane is an ordinary session: the Legion
   * extension stays inert and the Envoy extension's run-end ask nudge is not excluded.
   */
  readonly legion?: boolean;
  /** Configures Dispatch against the stand-in: `DISPATCH_URL` is its base, `DISPATCH_TOKEN` the
   * name's token. The case answers Dispatch's routes through `answer`. */
  readonly dispatch?: boolean;
  /** Static extras spread into the pane's environment, last. */
  readonly env?: Readonly<Record<string, string>>;
  /** Tried first for every request the stand-in serves; `undefined` falls through to the runner's
   * own routes (the gateway, the daemon's claim and grant routes, the Envoy listener). */
  readonly answer?: (url: URL, body: Record<string, unknown>) => Response | undefined;
  /** Runs once the scratch root, `bin` (first on the pane's PATH) and `state` exist, before the
   * profile is written and omp is spawned: what the pane's workspace must hold. */
  readonly prepare?: (paths: {
    readonly workspace: string;
    readonly bin: string;
    readonly state: string;
  }) => Promise<void>;
  /**
   * Settle when the gateway has answered nothing for this long, instead of at the host's
   * terminal `agent_end`. A `triggerTurn` steer sent from `agent_end` starts its continuation
   * after that frame, so the terminal frame is not the end of the run's provider traffic.
   */
  readonly quietMs?: number;
  /** The one word the gateway answers the run-end self-check with; `PROCEEDING` by default. */
  readonly selfCheck?: string;
}

/** What a settled Legion pane left behind. */
export interface LegionPane {
  /** Every request the stand-in served: the model gateway's, the daemon's, and the listener's. */
  readonly requests: Request[];
  /** The Messages requests that were turns of the conversation, in order. */
  readonly turns: () => Request[];
  /** The Messages requests that were side turns (the self-check), in order. */
  readonly selfChecks: () => Request[];
  /** Every `tool_result` the host sent back to the gateway, in conversation order. */
  readonly toolResults: () => ToolResult[];
  /** The persisted transcript's entries of `customType`, in order, each its `data`. */
  readonly transcriptEntries: (customType: string) => Promise<unknown[]>;
  /** The pane's issue workspace. */
  readonly workspace: string;
}

/**
 * Runs one Legion pane on the real Oh My Pi until its run settles: the Legion and Envoy
 * extensions from this checkout, booted against a stand-in for the daemon's claim and grant
 * routes and the Envoy listener (no NATS: the Envoy extension then skips inbound delivery, and
 * the role claim is two listener calls), with a stand-in model gateway that answers the pane's
 * turns from `replies`. The daemon's assignment arrives as the RPC `prompt`. Nothing in a pane
 * runs `legion` from bash (LEGION-631), so no stand-in `legion` is on its PATH: `bin` holds what
 * `prepare` put there.
 */
export async function runLegionPane(
  binary: string,
  replies: readonly Reply[],
  options: LegionPaneOptions,
  cleanup: Cleanup
): Promise<LegionPane> {
  const { name, role, tree, issue } = options;
  const claimToken = `legion-${name}-${issue.toLowerCase()}-${role}`;
  // The daemon's golden registration answer (`packages/daemon/internal/api`), so a field the
  // daemon adds to it reaches the stand-in's `claims/register`.
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { root, home, workspace, sessions } = await ompRoot(binary, `legion-${name}-`, cleanup);
  const state = path.join(root, "state");
  const bin = path.join(root, "bin");
  await mkdir(bin, { recursive: true });
  await mkdir(state, { recursive: true, mode: 0o700 });
  await options.prepare?.({ workspace, bin, state });

  let answered = 0;
  let selfChecks = 0;
  let grants = 0;
  let lastAnsweredAt = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    const own = options.answer?.(url, body);
    if (own !== undefined) return own;
    if (url.pathname === "/anthropic/v1/messages") {
      lastAnsweredAt = Date.now();
      const request: Request = { path: url.pathname, body };
      // The self-check is not a turn: it consumes no scripted reply, and the conversation's
      // next turn is answered as if it had never happened — which is what the host's snapshot
      // makes true.
      if (isSelfCheck(request)) {
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
      const blocks = typeof reply === "function" ? reply(request) : reply;
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken,
        tree,
        issue,
        role,
        generation: 1,
        secret: `${name}-secret`,
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `${name}-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: `${name}-machine`,
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  // The run has settled when the RPC stream reports its terminal agent_end: a continuation the
  // host scheduled (the follow-up) starts its turn before that, under the same run. A steer the
  // extension sends from `agent_end` instead starts its continuation after that frame, so
  // `quietMs` waits for the gateway to fall silent rather than for the frame.
  const settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions: [
        // The Envoy entry is the sibling plugin's: a Legion pane loads both, and the Legion entry
        // refuses to run without it.
        path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
        path.resolve(import.meta.dir, "../../pi-legion/extensions/legion.ts"),
      ],
      home,
      workspace,
      sessions,
      bin,
      env: {
        ENVOY_URL: base,
        ...(options.dispatch
          ? { DISPATCH_URL: base, DISPATCH_TOKEN: `${name}-dispatch-token` }
          : {}),
        ...((options.legion ?? true)
          ? {
              LEGION_DAEMON_URL: base,
              LEGION_ROLE: role,
              LEGION_TREE: tree,
              LEGION_ISSUE: issue,
              LEGION_GENERATION: "1",
              LEGION_BOOT_TOKEN: `${name}-boot`,
              LEGION_STATE_DIR: state,
              LEGION_WORKSPACE: workspace,
            }
          : {}),
        ...options.env,
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
  rpc.send({ type: "prompt", message: options.prompt });
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

  const turns = (): Request[] =>
    requests.filter(
      (request) => request.path === "/anthropic/v1/messages" && !isSelfCheck(request)
    );
  return {
    requests,
    turns,
    selfChecks: () =>
      requests.filter(
        (request) => request.path === "/anthropic/v1/messages" && isSelfCheck(request)
      ),
    toolResults: () => {
      // Every turn carries the whole conversation so far, so the first sighting of each id is
      // its place in the conversation.
      const seen = new Set<string>();
      const results: ToolResult[] = [];
      for (const turn of turns()) {
        for (const result of toolResultsIn(turn)) {
          if (seen.has(result.tool_use_id)) continue;
          seen.add(result.tool_use_id);
          results.push(result);
        }
      }
      return results;
    },
    transcriptEntries: async (customType) => {
      const files = (await readdir(sessions, { recursive: true })).filter((file) =>
        file.endsWith(".jsonl")
      );
      if (files.length !== 1)
        throw new Error(`want one transcript under ${sessions}, found ${files}`);
      return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
        .split("\n")
        .filter(Boolean)
        .map((line): unknown => JSON.parse(line))
        .flatMap((entry) => {
          if (typeof entry !== "object" || entry === null) return [];
          if (!("customType" in entry) || entry.customType !== customType) return [];
          return ["data" in entry ? entry.data : undefined];
        });
    },
    workspace,
  };
}
