#!/usr/bin/env bun
/**
 * claude-client.ts — the `claude` process of one Claude Code session, for
 * `scripts/smoke-role-takeover.sh` only.
 *
 * It spawns the real channel server (`packages/claude-envoy/bin/envoy-channel.ts`) as its child,
 * exactly as Claude Code spawns its stdio MCP server, and speaks MCP to it over that pipe. Being
 * the server's parent is the point: the server watches
 * `${CLAUDE_PLUGIN_DATA}/sessions/<its parent's pid>/session-id` for the id Claude Code mints on
 * `/clear` (`sessionHandoffFile`), and this process is that parent, so `{"op":"clear"}` writes
 * that file the way the SessionStart hook does and the server adopts the handoff on its own.
 *
 * Nothing of the server is faked: it registers, heartbeats, claims and deregisters over HTTP to
 * the smoke's recording proxy, and over NATS to the smoke's natsd. One process is one client
 * instance, which is what the listener's rule turns on (R5, R6 of dispatch://INFRA-404/spec).
 *
 * Usage:
 *   bun claude-client.ts --server <bin/envoy-channel.ts> --session <id> --state-dir <dir>
 *                        --config-dir <dir> --heartbeat-ms <n> [--stderr <file>]
 *
 * Protocol: one JSON object per line on stdin, one answer per line on stdout, matched by `id`.
 *
 *   {"id":"1","op":"tool","name":"envoy_role_set","args":{"role":"reviewer"}}
 *   {"id":"2","op":"clear","session":"<the id Claude Code minted>"}
 *   {"id":"3","op":"kill"}          a crash: the registration outlives the process
 *   {"id":"4","op":"stop"}          a clean exit: the server deregisters itself
 */
import { mkdirSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { Subprocess } from "bun";

interface Options {
  readonly server: string;
  readonly session: string;
  readonly stateDirectory: string;
  readonly configDirectory: string;
  readonly heartbeatMs: string;
  readonly stderr: string | undefined;
}

interface Command {
  readonly id?: string;
  readonly op?: string;
  readonly name?: string;
  readonly args?: Record<string, unknown>;
  readonly session?: string;
}

function parseOptions(argv: readonly string[]): Options {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (name === undefined || !name.startsWith("--") || value === undefined) {
      throw new Error(`claude-client: expected --name value pairs, got ${argv.join(" ")}`);
    }
    values.set(name.slice(2), value);
  }
  const server = values.get("server");
  const session = values.get("session");
  const stateDirectory = values.get("state-dir");
  const configDirectory = values.get("config-dir");
  const heartbeatMs = values.get("heartbeat-ms");
  if (server === undefined) throw new Error("claude-client: --server is required");
  if (session === undefined) throw new Error("claude-client: --session is required");
  if (stateDirectory === undefined) throw new Error("claude-client: --state-dir is required");
  if (configDirectory === undefined) throw new Error("claude-client: --config-dir is required");
  if (heartbeatMs === undefined) throw new Error("claude-client: --heartbeat-ms is required");
  return {
    server,
    session,
    stateDirectory,
    configDirectory,
    heartbeatMs,
    stderr: values.get("stderr"),
  };
}

const options = parseOptions(process.argv.slice(2));
const { PATH } = process.env;

const child: Subprocess<"pipe", "pipe", "pipe"> = Bun.spawn(["bun", options.server], {
  cwd: options.stateDirectory,
  env: {
    PATH: PATH ?? "",
    HOME: options.stateDirectory,
    CLAUDE_CONFIG_DIR: options.configDirectory,
    CLAUDE_CODE_SESSION_ID: options.session,
    CLAUDE_PLUGIN_DATA: options.stateDirectory,
    ENVOY_HEARTBEAT_MS: options.heartbeatMs,
  },
  stdin: "pipe",
  stdout: "pipe",
  stderr: "pipe",
});

const answers = new Map<number, unknown>();
const waiters = new Map<number, () => void>();
let serverStderr = "";

void (async () => {
  let buffered = "";
  for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
    buffered += chunk;
    for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
      const message = JSON.parse(buffered.slice(0, newline)) as { readonly id?: unknown };
      buffered = buffered.slice(newline + 1);
      const { id } = message;
      if (typeof id !== "number") continue;
      answers.set(id, message);
      waiters.get(id)?.();
    }
  }
})();

void (async () => {
  for await (const chunk of child.stderr.pipeThrough(new TextDecoderStream())) {
    serverStderr += chunk;
    if (options.stderr !== undefined) await Bun.write(options.stderr, serverStderr);
  }
})();

let nextRequestID = 0;

/** Sends one MCP request and resolves with the server's answer to it. */
async function request(method: string, params: unknown): Promise<unknown> {
  nextRequestID += 1;
  const id = nextRequestID;
  const settled = Promise.withResolvers<void>();
  waiters.set(id, settled.resolve);
  child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
  child.stdin.flush();
  const deadline = setTimeout(() => {
    settled.reject(
      new Error(`the channel server did not answer ${method} within 60s: ${serverStderr}`)
    );
  }, 60_000);
  try {
    await settled.promise;
  } finally {
    clearTimeout(deadline);
    waiters.delete(id);
  }
  return answers.get(id);
}

/** The handoff file Claude Code's SessionStart hook writes for this process's children. */
function writeHandoff(sessionID: string): string {
  const directory = join(options.stateDirectory, "sessions", String(process.pid));
  mkdirSync(directory, { recursive: true });
  const file = join(directory, "session-id");
  const temporary = `${file}.tmp`;
  writeFileSync(temporary, `${sessionID}\n`);
  renameSync(temporary, file);
  return file;
}

async function stopChild(signal: "SIGTERM" | "SIGKILL"): Promise<number | null> {
  child.kill(signal);
  const killer = setTimeout(() => child.kill("SIGKILL"), 10_000);
  const code = await child.exited;
  clearTimeout(killer);
  return code;
}

async function run(command: Command): Promise<Record<string, unknown>> {
  switch (command.op) {
    case "tool":
      return {
        result: await request("tools/call", { name: command.name, arguments: command.args ?? {} }),
      };
    case "clear": {
      if (command.session === undefined) throw new Error("clear needs the id Claude Code minted");
      return { handoff: writeHandoff(command.session) };
    }
    case "kill":
      return { exit: await stopChild("SIGKILL") };
    case "stop":
      return { exit: await stopChild("SIGTERM") };
    case "stderr":
      return { stderr: serverStderr };
    case "exit":
      await stopChild("SIGTERM");
      process.exit(0);
      break;
    default:
      throw new Error(`unknown op ${command.op}`);
  }
  return {};
}

function answer(line: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

await request("initialize", {
  protocolVersion: "2025-06-18",
  capabilities: {},
  clientInfo: { name: "smoke-role-takeover", version: "0" },
});
child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`);
child.stdin.flush();
answer({ ready: true, pid: process.pid, serverPid: child.pid, session: options.session });

for await (const line of console) {
  const text = line.trim();
  if (text === "") continue;
  const command = JSON.parse(text) as Command;
  const id = command.id ?? "";
  try {
    answer({ id, ok: true, ...(await run(command)) });
  } catch (error) {
    answer({ id, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
}
