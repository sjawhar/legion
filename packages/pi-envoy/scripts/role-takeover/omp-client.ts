#!/usr/bin/env bun
/**
 * omp-client.ts — one Oh My Pi Envoy extension instance in an OS process of its own, for
 * `scripts/smoke-role-takeover.sh` only.
 *
 * It loads the real `extensions/envoy.ts` from this checkout and hands it the stand-in host the
 * package's own suite uses (`createPi` in `extensions/envoy.test.ts`): one session manager whose
 * id and branch the driver moves, `pi.appendEntry` writing into that branch, and
 * `context.setInterval` collecting the heartbeat rather than arming a timer, so the smoke ticks
 * the heartbeat when it wants one. Nothing else is faked: the extension dials the smoke's natsd,
 * and every listener call goes over HTTP to the smoke's recording proxy (`envoy.url` in the
 * `ENVOY_CONFIG_FILE` the smoke writes). One process is one client instance, which is what the
 * listener's rule turns on (R5, R6 of dispatch://INFRA-404/spec).
 *
 * Run it as `bun --preload <dir>/host-stub.ts <dir>/omp-client.ts`: the preload supplies the host
 * package the extension imports.
 *
 * Protocol: one JSON object per line on stdin, one answer per line on stdout, matched by `id`.
 *
 *   {"id":"1","op":"start","session":"ses_a","roles":["reviewer"]}
 *   {"id":"2","op":"tool","name":"envoy_role_set","args":{"role":"reviewer"}}
 *   {"id":"3","op":"switch","session":"ses_b","reason":"fork","detach":true}
 *   {"id":"4","op":"join"}
 *
 * `roles` replaces the session's branch with one `envoy-role-claim` entry per element (a string
 * is a claim, null a release), which is what a resumed or forked transcript carries; omitted, the
 * branch carries on as it is. `detach` answers before the handler settles, for a step the smoke
 * holds at the proxy; `join` then waits for every detached handler.
 */
import { appendFileSync } from "node:fs";
import type { PiApi } from "@legion/pi-shared/pi-types";
import { z } from "zod";
import envoyExtension from "../../extensions/envoy";

const ROLE_CLAIM_ENTRY = "envoy-role-claim";

interface BranchEntry {
  readonly type: "custom";
  readonly customType: string;
  readonly data: unknown;
}

interface Command {
  readonly id?: string;
  readonly op?: string;
  readonly session?: string;
  readonly reason?: string;
  readonly roles?: readonly (string | null)[];
  readonly name?: string;
  readonly args?: Record<string, unknown>;
  readonly detach?: boolean;
}

interface RegisteredTool {
  readonly name: string;
  readonly execute: (
    id: string,
    parameters: Record<string, unknown>,
    signal: AbortSignal | undefined,
    onUpdate: unknown,
    context: unknown
  ) => Promise<unknown>;
}

type EventHandler = (event: unknown, context: unknown) => Promise<unknown>;

const { ROLE_TAKEOVER_CWD, ROLE_TAKEOVER_CLIENT_LOG } = process.env;
const cwd = ROLE_TAKEOVER_CWD ?? process.cwd();

let sessionID = "";
const entries: BranchEntry[] = [];
const tools: RegisteredTool[] = [];
const handlers = new Map<string, EventHandler>();
const notices: string[] = [];
const intervals: (() => void)[] = [];
/** Handlers a `detach` command left running, awaited by `join`. */
const detached: Promise<unknown>[] = [];

const sessionManager = {
  getSessionId: () => sessionID,
  getSessionName: () => undefined,
  getBranch: (): readonly unknown[] => entries,
  getEntries: (): readonly unknown[] => entries,
  getSessionFile: () => undefined,
  ensureOnDisk: async (): Promise<void> => undefined,
};

// One context, as the host has one session manager: every handler reads the id and branch as
// they stand when it runs, which is what makes a move mid-pass observable at all.
const context = {
  cwd,
  hasUI: true,
  agent: { kind: "main" as const },
  sessionManager,
  isIdle: () => true,
  setInterval: (callback: () => void) => {
    intervals.push(callback);
  },
  setTimeout: (callback: () => void) => {
    setTimeout(callback, 0);
  },
  ui: {
    notify: (message: string, level: string) => {
      notices.push(`${level}: ${message}`);
    },
    onTerminalInput: () => () => undefined,
    getEditorText: () => "",
  },
};

const pi = {
  zod: z,
  registerTool: (tool: RegisteredTool) => {
    tools.push(tool);
  },
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
  getCommands: () => [],
  on: (event: string, handler: EventHandler) => {
    handlers.set(event, handler);
  },
  sendMessage: () => undefined,
  sendUserMessage: () => undefined,
  sendUserInput: () => undefined,
  appendEntry: (customType: string, data: unknown) => {
    entries.push({ type: "custom", customType, data });
  },
};

// The stand-in host is a subset of the real `PiApi`: the extension reads only what `createPi`
// in the package's own suite supplies, and the cast is that same narrowing.
envoyExtension(pi as unknown as PiApi);

/** Replaces the branch with the transcript a resumed, forked or fresh session carries. */
function setBranch(roles: readonly (string | null)[] | undefined): void {
  if (roles === undefined) return;
  entries.length = 0;
  for (const role of roles) {
    entries.push({ type: "custom", customType: ROLE_CLAIM_ENTRY, data: { role } });
  }
}

function handlerFor(event: string): EventHandler {
  const handler = handlers.get(event);
  if (handler === undefined) throw new Error(`the extension registered no ${event} handler`);
  return handler;
}

async function run(command: Command): Promise<Record<string, unknown>> {
  switch (command.op) {
    case "start": {
      sessionID = command.session ?? "";
      setBranch(command.roles ?? []);
      await handlerFor("session_start")({}, context);
      return { session: sessionID };
    }
    case "switch": {
      await handlerFor("session_before_switch")({ reason: command.reason }, context);
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      await handlerFor("session_switch")({ reason: command.reason }, context);
      return { session: sessionID };
    }
    case "branch": {
      await handlerFor("session_before_branch")({}, context);
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      await handlerFor("session_branch")({}, context);
      return { session: sessionID };
    }
    case "tree": {
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      await handlerFor("session_tree")({}, context);
      return { session: sessionID };
    }
    case "move": {
      // A move the host made with no session event, which only the next pass sees.
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      return { session: sessionID };
    }
    case "tick": {
      for (const callback of intervals) callback();
      return { ticked: intervals.length };
    }
    case "tool": {
      const tool = tools.find((candidate) => candidate.name === command.name);
      if (tool === undefined) throw new Error(`no tool named ${command.name}`);
      return {
        result: await tool.execute("smoke", command.args ?? {}, undefined, undefined, context),
      };
    }
    case "shutdown":
      await handlerFor("session_shutdown")({}, context);
      return { session: sessionID };
    case "join": {
      const waiting = detached.splice(0);
      await Promise.allSettled(waiting);
      return { joined: waiting.length };
    }
    case "notices":
      return { notices: [...notices] };
    case "exit":
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

if (ROLE_TAKEOVER_CLIENT_LOG !== undefined) {
  // Every notice the pane would show, for a failed run's evidence.
  process.on("exit", () => {
    appendFileSync(ROLE_TAKEOVER_CLIENT_LOG, `${notices.join("\n")}\n`);
  });
}

answer({ ready: true, pid: process.pid });
for await (const line of console) {
  const text = line.trim();
  if (text === "") continue;
  const command = JSON.parse(text) as Command;
  const id = command.id ?? "";
  if (command.detach === true) {
    answer({ id, ok: true, started: true });
    detached.push(
      run(command).catch((error: unknown) => {
        notices.push(`detached ${command.op} failed: ${String(error)}`);
      })
    );
    continue;
  }
  try {
    answer({ id, ok: true, ...(await run(command)) });
  } catch (error) {
    answer({ id, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
}
