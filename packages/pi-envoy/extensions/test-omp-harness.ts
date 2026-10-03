// The harness shared by the tests that run the real Oh My Pi binary (legion-phase-stall-omp.test.ts,
// dispatch-first-omp.test.ts): a stand-in server that is the model gateway and whatever else a test
// answers, a profile whose every model role is that stand-in, and one `omp --mode rpc` child with
// only this checkout's extensions loaded. The profile format, the flags and the RPC stream are the
// Oh My Pi pin's (the repository's .omp-pin), so a pin bump that changes one is fixed
// here once.
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { linkOmpNatives } from "./test-omp-natives";

export interface Request {
  readonly path: string;
  readonly body: Record<string, unknown>;
}

export type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> };

/** What a test undoes after each case, run last first. */
export type Cleanup = (() => Promise<void>)[];

/**
 * A scratch root with the profile's home, the session's workspace and its transcript directory.
 * The home's Oh My Pi natives are hardlinks to `binary`'s one cached copy (test-omp-natives.ts), so
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
 * Starts `omp --mode rpc` in `workspace` with only `extensions` (files beside this one) loaded, no
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
      ...options.extensions.flatMap((file) => ["-e", path.join(import.meta.dir, file)]),
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
