import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { controllerToken, type LegionRole } from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { messageFor } from "@legion/envoy-client/errors";
import { logger } from "@oh-my-pi/pi-utils";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import {
  classifySession,
  generation,
  requiredControllerCapability,
  requiredEnvironment,
  requiredSecret,
} from "../src/legion/classify";
import {
  handleLegionControlDirective,
  type LegionControlDirective,
  parseControlDirective,
} from "../src/legion/control";
import {
  createLegionDaemonClient,
  LegionDaemonApiError,
  type LegionDaemonClient,
} from "../src/legion/daemon-client";
import { writeGrantFile } from "../src/legion/grant-file";
import { exportJjSessionAttribution } from "../src/legion/jj-attribution";
import { createLegionTool } from "../src/legion/tools";
import { setJjIdentity } from "../src/legion/workspace-helpers";
import type {
  CommandContext,
  PiApi,
  SessionContext,
  ToolCallEvent,
  ToolCallEventResult,
} from "../src/pi-types";
import { claimEnvoyRole, onEnvoyRoleRegained, type RoleRegainReason } from "./envoy";

interface LegionCapability {
  readonly kind: "root-architect" | "phase-worker";
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly roleToken: string;
  readonly secret: string;
}

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

// Bounds retries of the transient `/process/ready` and `/worker/ready` bootstrap requests.
const READY_RETRY_ATTEMPTS = 3;
const READY_RETRY_DELAY_MS = 1_000;

function isNetworkOrTimeoutError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  if (error.name === "NetworkError" || error.name === "TimeoutError") return true;

  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  return (
    code === "ConnectionRefused" ||
    code === "ConnectionTimeout" ||
    code === "EAI_AGAIN" ||
    code === "ECONNREFUSED" ||
    code === "ECONNRESET" ||
    code === "EHOSTUNREACH" ||
    code === "ENETUNREACH" ||
    code === "ENOTFOUND" ||
    code === "ETIMEDOUT" ||
    (error instanceof TypeError &&
      (error.message === "Failed to fetch" || error.message === "fetch failed"))
  );
}

/**
 * Calls a role's `/process/ready` or `/worker/ready` daemon request. The daemon acknowledges
 * this request before dialing back into this process's shim socket, so retry only errors that can
 * resolve on their own: daemon 5xx responses and network or timeout failures, up to a bounded
 * number of attempts. A definitive 4xx (401/403 or any other) propagates immediately, and an
 * exhausted retry budget propagates too -- both reach the enclosing bootstrap catch, which exits
 * the process so the daemon respawns a fresh attempt.
 */
const callReadyWithRetry = async (label: string, call: () => Promise<void>): Promise<void> => {
  for (let attempt = 1; attempt <= READY_RETRY_ATTEMPTS; attempt++) {
    try {
      await call();
      return;
    } catch (error) {
      const retryable =
        error instanceof LegionDaemonApiError
          ? error.status >= 500 && error.status < 600
          : isNetworkOrTimeoutError(error);
      if (!retryable) throw error;
      if (attempt === READY_RETRY_ATTEMPTS) {
        console.error(`[legion] ${label} failed after ${attempt} attempts: ${messageFor(error)}`);
        throw error;
      }
      console.error(
        `[legion] ${label} failed (attempt ${attempt}/${READY_RETRY_ATTEMPTS}), retrying: ${messageFor(error)}`
      );
      const retryDelay = Promise.withResolvers<void>();
      setTimeout(retryDelay.resolve, READY_RETRY_DELAY_MS);
      await retryDelay.promise;
    }
  }
};

async function persistedTranscript(
  context: SessionContext
): Promise<{ readonly sessionFile: string; readonly agentId: string }> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion session transcript has no agent id");
  return { sessionFile, agentId };
}

/**
 * OMP identifies a subagent session by its transcript path, not by environment: a `task`-spawned
 * subagent's transcript file lives inside a directory named after its parent's transcript file
 * (minus the `.jsonl` extension), so `fs.existsSync(path.dirname(sessionFile) + ".jsonl")` finds
 * the parent (oh-my-pi `packages/coding-agent/src/session/session-manager.ts:143-154`). A
 * subagent session still loads a fresh instance of this extension module and inherits the
 * parent's LEGION_* environment, so without this guard `classifySession` would still see
 * root-architect or phase-worker markers and try to bootstrap a second time: `/process/started`
 * or `/worker/started` would be called with the already-consumed `LEGION_BOOT_TOKEN`, the daemon
 * would refuse it, and the bootstrap catch's `exitProcess(1)` would kill the whole OS process --
 * including the parent that is still waiting on the subagent. A subagent session must therefore
 * claim no role, call no daemon route, install no tool gate of its own (the parent's gate, live
 * in the parent process, still applies to it), and never call `exitProcess`.
 */
async function isSubagentSession(context: SessionContext): Promise<boolean> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  return sessionFile !== undefined && fs.existsSync(`${path.dirname(sessionFile)}.jsonl`);
}

// An architect delegates code work, but its prompt requires `legion handoff
// write/complete` and `legion gh --` to report its own phase and touch GitHub.
// Allow bash only for a single `legion ...` invocation: no chaining outside a
// quoted argument. This is a conservative character scan, not a shell parser --
// it rejects some legitimate quoting it can't reason about (nested quotes,
// escapes) rather than risk letting a chained command through.
function isSingleLegionCommand(command: unknown): boolean {
  if (typeof command !== "string") return false;
  const trimmed = command.trim();
  if (trimmed.length === 0) return false;
  let quote: '"' | "'" | undefined;
  for (const char of trimmed) {
    if (quote !== undefined) {
      if (char === quote) quote = undefined;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      continue;
    }
    if (char === "\n" || char === ";" || char === "&" || char === "|") return false;
  }
  if (quote !== undefined) return false;
  return trimmed.split(/\s+/, 1)[0] === "legion";
}

// Every Legion issue workspace is a `jj workspace` of one shared clone, so they all share one
// operation log: `jj undo`, `jj abandon`, and `jj op restore|revert|abandon|undo` rewrite it for
// every tree at once (LEGION-45: one worker's `jj undo` rewrote nine of another tree's commits).
// The tool_call hook refuses them in every phase-worker pane. `restore`/`revert` are operation-log
// commands only under `op`/`operation`; `jj restore <paths>` is file-level and stays allowed.
const JJ_LOG_REWRITE_WORDS = ["undo", "abandon"];
const JJ_OP_WORDS = ["op", "operation"];
const JJ_OP_LOG_REWRITE_WORDS = ["restore", "revert"];
const JJ_MENTION = /\bjj\b/;
// Any non-word run between `op` and `restore`, so `"op", "restore"` in an argv literal counts.
const JJ_LOG_REWRITE_MENTION = /\b(?:undo|abandon)\b|\b(?:op|operation)\b\W+(?:restore|revert)\b/;

/** The plain-text rule for text the extension does not tokenise as a shell command -- `eval`
 * code, a `hub` process start, each word of a tokenised `bash` command (`sh -c "jj undo"`), and
 * a `bash` command with unbalanced quoting: the blocked words `text` mentions together with `jj`
 * (e.g. `undo`, `op restore`), or undefined. */
function jjLogRewriteMention(text: string): string | undefined {
  if (!JJ_MENTION.test(text)) return undefined;
  const match = JJ_LOG_REWRITE_MENTION.exec(text);
  return match === null ? undefined : match[0].replace(/\W+/g, " ");
}

/** Splits a shell command into simple commands (at `;`, `&`, `|`, newline, `(`, `)`, and
 * backtick) of words, honouring single quotes, double quotes, backslash escapes, backslash-newline
 * continuation, and redirection operators (`<`, `>`, `>&`, `<&`, `&>` end a word, never the simple
 * command). A word is its unquoted text -- the argv bash would build -- so quoting never changes a
 * verdict. Undefined on an unterminated quote: the caller then applies the plain-text rule to the
 * whole command, never allows it. Not a shell parser -- no expansions, no heredoc awareness -- and
 * every gap errs toward refusing (a heredoc body is read as commands). */
function splitShellCommands(command: string): string[][] | undefined {
  const commands: string[][] = [];
  let words: string[] = [];
  let text = "";
  let inWord = false;
  let quote: '"' | "'" | undefined;
  const endWord = (): void => {
    if (inWord) words.push(text);
    text = "";
    inWord = false;
  };
  const endCommand = (): void => {
    endWord();
    if (words.length > 0) commands.push(words);
    words = [];
  };
  for (let i = 0; i < command.length; i += 1) {
    const char = command.charAt(i);
    if (quote !== undefined) {
      if (char === quote) quote = undefined;
      else if (quote === '"' && char === "\\" && command.charAt(i + 1) === "\n") i += 1;
      else if (quote === '"' && char === "\\" && i + 1 < command.length) {
        i += 1;
        text += command.charAt(i);
      } else text += char;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      inWord = true;
    } else if (char === "\\") {
      // Backslash-newline is line continuation: both characters vanish, the word continues
      // (inside double quotes too, above; single quotes keep both, as bash does).
      if (command.charAt(i + 1) === "\n") i += 1;
      else if (i + 1 < command.length) {
        i += 1;
        text += command.charAt(i);
        inWord = true;
      }
    } else if (char === " " || char === "\t" || char === "<" || char === ">") {
      // A redirection operator ends the word before it and belongs to the same simple command:
      // `jj undo>/dev/null` is `jj undo`, and `2>&1`'s operands are harmless extra words.
      endWord();
    } else if (
      char === "&" &&
      (command.charAt(i - 1) === ">" || command.charAt(i - 1) === "<" || command.charAt(i + 1) === ">")
    ) {
      // The `&` of `>&`, `<&`, and `&>` is part of the redirection, not a command terminator.
      endWord();
    } else if (";&|\n()`".includes(char)) {
      endCommand();
    } else {
      text += char;
      inWord = true;
    }
  }
  if (quote !== undefined) return undefined;
  endCommand();
  return commands;
}

/** The first thing in a `bash` command that would rewrite the shared jj operation log, named for
 * the refusal, or undefined. Each simple command is judged on the whole argument list after its
 * first `jj` (or `.../jj`) word -- never only the first word after it -- so `jj -R <path> undo`,
 * `jj --at-op <id> op restore <id>`, and `jj operation restore` count, in any position of a
 * pipeline or `&&` chain. A word is judged by its text whatever its quoting, since bash hands jj
 * the same argv either way: `jj "undo"`, `"jj" undo`, and `jj \u\n\d\o` are `jj undo`, and a
 * one-word `-m "undo"` is refused with them (the spec's tradeoff: one rephrase), while
 * `-m "undo this"` is a different word and stays allowed. `undo`/`abandon` count anywhere;
 * `restore`/`revert` only beside `op`/`operation`. Every word is also held to the plain-text
 * rule (`sh -c "jj undo"`), and so is the whole command when it does not tokenise. */
function jjLogRewriteInvocation(command: string): string | undefined {
  const commands = splitShellCommands(command);
  if (commands === undefined) {
    return jjLogRewriteMention(command) === undefined ? undefined : command.trim();
  }
  for (const words of commands) {
    const mentioned = words.find((word) => jjLogRewriteMention(word) !== undefined);
    if (mentioned !== undefined) return mentioned;
    const jj = words.findIndex((word) => word === "jj" || word.endsWith("/jj"));
    if (jj === -1) continue;
    const args = words.slice(jj + 1);
    const rewritesLog =
      args.some((arg) => JJ_LOG_REWRITE_WORDS.includes(arg)) ||
      (args.some((arg) => JJ_OP_WORDS.includes(arg)) &&
        args.some((arg) => JJ_OP_LOG_REWRITE_WORDS.includes(arg)));
    if (rewritesLog) return words.slice(jj).join(" ");
  }
  return undefined;
}

/** What a phase worker's tool call would run against the shared operation log, or undefined. A
 * `bash` command is tokenised; `eval` code and a `hub` call's `application`, `args`, and `text`
 * (a process start's program and arguments, and stdin sent to a supervised process) are held to
 * the plain-text rule, since each runs a shell from the pane exactly as `bash` does. */
function jjLogRewriteAttempt(toolCall: ToolCallEvent): string | undefined {
  const { toolName, input } = toolCall;
  if (toolName === "bash") {
    return typeof input.command === "string" ? jjLogRewriteInvocation(input.command) : undefined;
  }
  let text: string;
  if (toolName === "eval" && typeof input.code === "string") text = input.code;
  else if (toolName === "hub") {
    text = [input.application, ...(Array.isArray(input.args) ? input.args : []), input.text]
      .filter((part): part is string => typeof part === "string")
      .join(" ");
  } else return undefined;
  const mention = jjLogRewriteMention(text);
  return mention === undefined ? undefined : `${toolName}: jj ${mention}`;
}

// Read by the daemon's startup probe (packages/daemon/src/daemon/index.ts,
// verifyLegionPluginLoaded) to prove this extension actually loaded from an
// ambient installed-plugin discovery -- not just that a manifest file exists,
// which stays true even when the plugin is disabled or unregistered in OMP's
// own plugin registry.
const LEGION_LOADED_MARKER = Symbol.for("legion.pi-envoy.legion-loaded");

/** Code-mutation tools blocked for an architect session (root or sub-architect) and a reviewer
 * (whose only sanctioned mutation is the final `.legion/` cleanup commit, made via `bash`).
 * `write` here means a real filesystem write; see `isToolDeviceInvocation` for the `xd://`
 * tool-device carve-out. */
const CODE_MUTATION_TOOLS = ["edit", "write", "apply_patch"];
/** The merger verifies and reports only: no code mutation, and no further Legion spawns. */
const MERGER_BLOCKED_TOOLS = [...CODE_MUTATION_TOOLS, "task"];

/** OMP's "tool device" convention invokes extension-registered tools (e.g. the nine Dispatch
 * tools) as a `write` whose `path` is an `xd://<tool>` URI carrying the tool's JSON args as
 * `content`. That `write` is a tool invocation, not a file mutation -- it must never trip the
 * `CODE_MUTATION_TOOLS` gate below for any role. */
function isToolDeviceInvocation(toolCall: ToolCallEvent): boolean {
  return (
    toolCall.toolName === "write" &&
    typeof toolCall.input.path === "string" &&
    toolCall.input.path.startsWith("xd://")
  );
}

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own). The id ties every hook log line
  // below to the instance that emitted it, so a per-call grant count can be attributed.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  (globalThis as Record<symbol, unknown>)[LEGION_LOADED_MARKER] = import.meta.url;
  const defaults = envoyDefaultsFromEnvironment(process.env);
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controlConnection: NatsConnection | undefined;
  let controlSubscription: Subscription | undefined;
  const controlCodec = StringCodec();

  // A Legion root or phase-worker session boots as its own OMP process and
  // holds exactly one role for its whole lifetime, so its identity lives in
  // plain closure state.
  let capability: LegionCapability | undefined;
  let bootstrap: Promise<void> | undefined;

  // A subagent session's transcript path never changes over its lifetime, so the check that
  // gates both session_start and tool_call below needs to run at most once per session instead
  // of once per tool call.
  let subagentSession: Promise<boolean> | undefined;
  const checkSubagentSession = (context: SessionContext): Promise<boolean> => {
    subagentSession ??= isSubagentSession(context);
    return subagentSession;
  };
  // Whether this pane was launched for a phase worker, judged from the environment on the first
  // tool_call (see the LEGION-45 guard there). A subagent's own instance inherits the pane's
  // environment, so the guard binds it exactly as it binds the worker that spawned it.
  let phaseWorkerPane: boolean | undefined;

  // One client for the session's whole life: its recovery record (the newest recovered secret
  // and the recovery in flight) is what lets two requests refused together share one
  // /worker-session round trip instead of racing each other's secret (LEGION-73). Every
  // capability-bearing call — the legion tool, the bash grant hook, process/ready re-runs, the
  // exit report — goes through this same instance.
  let daemonClient: LegionDaemonClient | undefined;
  const roleDaemon = (): LegionDaemonClient => {
    daemonClient ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL"),
      fetch,
      {
        recoveryToken: (sessionId) => {
          // A worker's boot token (read again from `LEGION_BOOT_TOKEN_FILE` here — the daemon keeps
          // that file for as long as the pane's locator lives) is its recovery token exactly like
          // the root's: it is single-use to redeem the initial capability, but the daemon accepts it
          // again on /worker-session to reissue a secret it has since forgotten (e.g. after a daemon
          // restart).
          if (capability !== undefined && sessionId === capability.sessionID) {
            return requiredSecret(process.env, "LEGION_BOOT_TOKEN");
          }
          throw new Error(`Legion session ${sessionId} has no persisted recovery token`);
        },
        onRecovered: (sessionId, recovered) => {
          if (capability === undefined || sessionId !== capability.sessionID) {
            throw new Error(`Legion session ${sessionId} has no persisted recovery token`);
          }
          if (
            recovered.tree !== capability.tree ||
            recovered.issue !== capability.issue ||
            recovered.role !== capability.role
          ) {
            throw new Error("Daemon recovered a capability for a different Legion role");
          }
          capability = { ...capability, secret: recovered.secret };
        },
      }
    );
    return daemonClient;
  };

  /**
   * Re-runs a role's daemon ready call after the Envoy heartbeat re-established this session as
   * the role's live holder (`reassertRole` in envoy.ts). Whatever the daemon published to the
   * role meanwhile got a 404 "no holder", and recovery differs by kind:
   *  - controller: the daemon queued each notice in `controllerPendingNotices` and only
   *    `/controller/ready` drains them (and forces a resync) -- the same call the boot handshake
   *    and `/legion-claim-controller` make, so re-run it (index.ts `onControllerReady`).
   *  - root architect: the daemon's no-holder recovery (`onUndeliverable` -> `resumeWorker`) is
   *    a no-op for the root's claim (no worker locator to resume), so nothing replays the missed
   *    wake; `/process/ready` re-emits the overseer catch-up (`onTreeReady`), so re-run it. A
   *    stale generation 409s, which `callReadyWithRetry` propagates without retrying.
   *  - phase worker (sub-architect included): `resumeWorker` -> `spawnWorker` already prompts or
   *    queues a state-derived catch-up on the live worker's own socket, and `/worker/ready` is a
   *    no-op once the boot is confirmed. No listener is registered for it.
   * The listener is registered only by the two paths that establish an identity
   * (`claimController`, `bootstrapRoot`), never at extension setup: OMP binds every extension
   * factory again for each in-process `task` subagent, and an identity-less instance writing the
   * bridge's single slot would replace the holder's listener. Never throws: after a definitive
   * 4xx or an exhausted retry budget the daemon's held work stays undelivered until the listener
   * loses the claim again or the process boots afresh.
   */
  const rerunReadyAfterRegain = async (
    endpoint: "controller/ready" | "process/ready",
    role: string,
    reason: RoleRegainReason,
    call: () => Promise<void>
  ): Promise<void> => {
    try {
      await callReadyWithRetry(`${endpoint} after role regain`, call);
      console.error(`[legion] re-ran ${endpoint} after role ${role} was ${reason}`);
    } catch (error) {
      console.error(
        `[legion] ${endpoint} after role ${role} was ${reason} failed; the daemon's held work stays undelivered until the next regain or boot: ${messageFor(error)}`
      );
    }
  };

  const claimController = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const daemon = createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"));
    const secret = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = secret;
    const { project } = await daemon.state();
    const token = controllerToken(project);
    await claimEnvoyRole(sessionID, token, "setInterval" in context ? context : undefined);
    await daemon.controllerReady({ secret, sessionId: sessionID });
    controllerSessionID = sessionID;
    onEnvoyRoleRegained(async (role, reason) => {
      if (role !== token) return;
      await rerunReadyAfterRegain("controller/ready", role, reason, () =>
        daemon.controllerReady({ secret, sessionId: sessionID })
      );
    });
  };

  const reclaimArchitect = async (): Promise<void> => {
    if (capability === undefined || capability.kind !== "root-architect") {
      throw new Error("Legion root architect is not available for reclamation");
    }
    await claimEnvoyRole(capability.sessionID, capability.roleToken);
  };

  const startControlSubscription = async (sessionID: string): Promise<void> => {
    const subject = process.env.LEGION_CONTROL_SUBJECT;
    if (!subject || controlSubscription) return;
    if (defaults.natsUrls.length === 0) {
      throw new Error("ENVOY_NATS_URL is required for Legion control directives");
    }
    const connection = await connect({
      servers: [...defaults.natsUrls],
      name: `legion-control-${sessionID}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    });
    const subscription = connection.subscribe(subject);
    controlConnection = connection;
    controlSubscription = subscription;
    void (async () => {
      for await (const message of subscription) {
        const reply = message.reply;
        let directive: LegionControlDirective;
        try {
          directive = parseControlDirective(controlCodec.decode(message.data));
        } catch (error) {
          if (reply) {
            connection.publish(
              reply,
              controlCodec.encode(
                JSON.stringify({
                  type: "nack",
                  error: messageFor(error),
                })
              )
            );
          }
          continue;
        }
        await handleLegionControlDirective(directive, {
          reclaimArchitect,
          acknowledge: () => {
            if (reply)
              connection.publish(reply, controlCodec.encode(JSON.stringify({ type: "ack" })));
          },
          reject: (error) => {
            if (reply)
              connection.publish(
                reply,
                controlCodec.encode(JSON.stringify({ type: "nack", error }))
              );
          },
        });
      }
    })();
  };

  const bootstrapRoot = async (context: SessionContext, tree: string): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (bootstrap) return bootstrap;

    const bootToken = requiredSecret(process.env, "LEGION_BOOT_TOKEN");

    bootstrap = (async () => {
      const { sessionFile, agentId } = await persistedTranscript(context);

      const started = await (async () => {
        try {
          return await createLegionDaemonClient(
            requiredEnvironment(process.env, "LEGION_DAEMON_URL")
          ).processStarted({
            tree,
            generation: generation(process.env),
            bootToken,
            rootSessionId: sessionID,
            agentId,
            ompSessionFile: sessionFile,
          });
        } catch (error) {
          if (error instanceof LegionDaemonApiError && error.status === 403) {
            console.error(
              `[legion] worker boot token rejected; exiting for the daemon to respawn: ${messageFor(error)}`
            );
            exitProcess(1);
          }
          throw error;
        }
      })();
      const roleToken = started.roleTokens.architect;
      if (!roleToken) throw new Error("Legion daemon did not return an architect role token");

      try {
        await exportJjSessionAttribution(
          sessionFile,
          requiredEnvironment(process.env, "LEGION_STATE_DIR")
        );
        capability = {
          kind: "root-architect",
          sessionID,
          tree,
          issue: tree,
          role: "architect",
          roleToken,
          secret: started.secret,
        };
        await claimEnvoyRole(sessionID, roleToken, context);
        await startControlSubscription(sessionID);
        await callReadyWithRetry("root process/ready", () =>
          roleDaemon().processReady({
            tree,
            sessionId: sessionID,
            secret: started.secret,
            generation: generation(process.env),
          })
        );
        onEnvoyRoleRegained(async (role, reason) => {
          if (role !== roleToken || capability === undefined) return;
          // Read live: `roleDaemon()`'s recovery may have swapped in a reissued secret since boot.
          const { secret } = capability;
          await rerunReadyAfterRegain("process/ready", role, reason, () =>
            roleDaemon().processReady({
              tree,
              sessionId: sessionID,
              secret,
              generation: generation(process.env),
            })
          );
        });
        registerArchitectTools();
        await activateLegionTool();
      } catch (error) {
        if (
          error instanceof LegionDaemonApiError &&
          (error.status === 401 || error.status === 403)
        ) {
          console.error(
            `[legion] root bootstrap authorization failed after process/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
          );
        } else {
          console.error(
            `[legion] root bootstrap failed after process/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
          );
        }
        exitProcess(1);
      }
    })();
    try {
      await bootstrap;
    } catch (error) {
      bootstrap = undefined;
      throw error;
    }
  };

  const bootstrapWorker = async (
    context: SessionContext,
    role: LegionRole,
    tree: string,
    issue: string
  ): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (bootstrap) return bootstrap;

    const bootToken = requiredSecret(process.env, "LEGION_BOOT_TOKEN");
    const workspace = requiredEnvironment(process.env, "LEGION_WORKSPACE");

    bootstrap = (async () => {
      const { sessionFile, agentId } = await persistedTranscript(context);

      const started = await (async () => {
        try {
          return await createLegionDaemonClient(
            requiredEnvironment(process.env, "LEGION_DAEMON_URL")
          ).workerStarted({
            tree,
            issue,
            role,
            bootToken,
            sessionId: sessionID,
            agentId,
            ompSessionFile: sessionFile,
          });
        } catch (error) {
          if (error instanceof LegionDaemonApiError && error.status === 403) {
            console.error(
              `[legion] worker boot token rejected; exiting for the daemon to respawn: ${messageFor(error)}`
            );
            exitProcess(1);
          }
          throw error;
        }
      })();

      try {
        await exportJjSessionAttribution(
          sessionFile,
          requiredEnvironment(process.env, "LEGION_STATE_DIR")
        );
        await setJjIdentity(workspace, started.gitName, started.gitEmail);
        capability = {
          kind: "phase-worker",
          sessionID,
          tree,
          issue,
          role,
          roleToken: started.roleToken,
          secret: started.secret,
        };
        await claimEnvoyRole(sessionID, started.roleToken, context);
        if (role === "architect") {
          registerArchitectTools();
          await activateLegionTool();
        }
        await callReadyWithRetry("worker/ready", () =>
          roleDaemon().workerReady({
            tree,
            issue,
            role,
            sessionId: sessionID,
            secret: started.secret,
            generation: generation(process.env),
          })
        );
      } catch (error) {
        if (
          error instanceof LegionDaemonApiError &&
          (error.status === 401 || error.status === 403)
        ) {
          console.error(
            `[legion] worker bootstrap authorization failed after worker/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
          );
        } else {
          console.error(
            `[legion] worker bootstrap failed after worker/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
          );
        }
        exitProcess(1);
      }
    })();
    try {
      await bootstrap;
    } catch (error) {
      bootstrap = undefined;
      throw error;
    }
  };

  pi.on("session_start", async (_event, context) => {
    // A `task`-spawned subagent session loads a fresh instance of this whole module: bail out
    // before classification, or the inherited LEGION_* environment would look like a fresh
    // root/worker boot and its failure would exit the parent process. See isSubagentSession.
    if (await checkSubagentSession(context)) return;
    const classification = classifySession(process.env);
    switch (classification.kind) {
      case "controller": {
        const sessionID = context.sessionManager.getSessionId();
        if (controllerSessionID === undefined || controllerSessionID === sessionID) {
          await claimController(context);
        }
        return;
      }
      case "phase-worker":
        await bootstrapWorker(
          context,
          classification.role,
          classification.tree,
          classification.issue
        );
        return;
      case "root-architect":
        await bootstrapRoot(context, classification.tree);
        return;
      case "not-legion":
        return;
    }
  });

  pi.on("tool_call", async (toolCall, context): Promise<ToolCallEventResult | undefined> => {
    logger.debug("legion tool_call hook", {
      instance,
      toolCallId: toolCall.toolCallId,
      toolName: toolCall.toolName,
    });
    // LEGION-45: the operation log is shared by every issue workspace (all are jj workspaces of
    // one clone), and a `task` subagent's bash runs in the same pane against it, so this guard is
    // judged from the pane's environment ahead of the subagent exemption below -- the one gate that
    // reaches a subagent -- and before any grant is minted. Classified once per instance, on the
    // first call: a throw for a malformed LEGION_ROLE stays inside the handler, never at load.
    phaseWorkerPane ??= classifySession(process.env).kind === "phase-worker";
    if (phaseWorkerPane) {
      const jjAttempt = jjLogRewriteAttempt(toolCall);
      if (jjAttempt !== undefined) {
        return {
          block: true,
          reason:
            `refused \`${jjAttempt}\`: jj undo, jj abandon, and jj op restore/revert/abandon/undo ` +
            "rewrite the jj operation log, which every Legion issue workspace shares (each is a jj " +
            "workspace of one clone), so they rewrite other trees' commits too. Recover forward with " +
            "a new commit or `jj restore <paths>` of files; anything else, stop and send the owning " +
            'architect the `jj -R "$LEGION_WORKSPACE" log` evidence.',
        };
      }
    }
    // No other gate applies to a subagent's own tool calls: the parent session's gate, running
    // in the parent's own module instance, already governs the parent's `task` call that spawned
    // it (see the architect `task` block above and isSubagentSession).
    if (await checkSubagentSession(context)) return undefined;
    const sessionID = context.sessionManager.getSessionId();
    const active = capability?.sessionID === sessionID ? capability : undefined;
    // A `write` to an `xd://<tool>` path is OMP's tool-device invocation convention (e.g. the
    // nine Dispatch tools), not a file mutation. Short-circuit it out of every mutation gate
    // below so the architect/reviewer/merger role checks apply only to real file writes.
    const isToolDevice = isToolDeviceInvocation(toolCall);
    // `role === "architect"` covers both kinds: the root architect and a sub-architect (a
    // phase worker with role "architect") both delegate all code work to phase workers.
    if (
      active?.role === "architect" &&
      !isToolDevice &&
      (CODE_MUTATION_TOOLS.includes(toolCall.toolName) ||
        (toolCall.toolName === "bash" && !isSingleLegionCommand(toolCall.input.command)))
    ) {
      return { block: true, reason: "the architect delegates all code work to phase workers" };
    }
    // The architect spawns further Legion work only through `spawn_worker`: Legion runs one
    // agent per process, and an in-process `task` subagent would inherit the architect's Legion
    // environment and clash with its own daemon-registered role (see isSubagentSession).
    if (active?.role === "architect" && toolCall.toolName === "task") {
      return {
        block: true,
        reason:
          "the architect delegates only through spawn_worker; Legion runs one agent per process",
      };
    }
    // Only a phase-worker session (never the root or sub-architect kinds above) is further
    // restricted by role below.
    if (active?.kind === "phase-worker") {
      if (
        active.role === "reviewer" &&
        !isToolDevice &&
        CODE_MUTATION_TOOLS.includes(toolCall.toolName)
      ) {
        return {
          block: true,
          reason: "the reviewer edits nothing except the final .legion/ cleanup commit via bash",
        };
      }
      if (
        active.role === "merger" &&
        !isToolDevice &&
        MERGER_BLOCKED_TOOLS.includes(toolCall.toolName)
      ) {
        return { block: true, reason: "the merger only verifies and reports" };
      }
    }
    if (toolCall.toolName !== "bash" || typeof toolCall.input.command !== "string")
      return undefined;
    if (active === undefined) {
      // A worker (root or phase) whose own boot handshake has not completed
      // yet has no capability to mint a grant with. The controller is
      // exempt: the daemon also sets LEGION_ROLE=controller on its process,
      // but a controller never claims a Legion role here.
      if (process.env.LEGION_ROLE !== undefined && process.env.LEGION_CONTROLLER !== "1") {
        return {
          block: true,
          reason: "Legion worker session is not registered; cannot mint its grant",
        };
      }
      return undefined;
    }
    // The daemon names the grant file on every pane it launches; a pane without one was launched
    // by a daemon older than this plugin, and minting for it would only produce a grant nothing
    // could read. A blank value (an operator's own export) is the same absence.
    const grantFile = process.env.LEGION_GRANT_FILE;
    if (grantFile === undefined || grantFile.trim() === "") {
      return {
        block: true,
        reason:
          "LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin; restart the daemon on the matching release",
      };
    }
    try {
      const grant = await roleDaemon().grant({
        tree: active.tree,
        issue: active.issue,
        sessionId: sessionID,
        secret: active.secret,
      });
      // The host writes a hook's revised `input` back into the assistant message (text the model
      // imitates — LEGION-12), and a plugin that replaces the bash tool may drop `env` (secretsd's
      // legacy shim, LEGION-52). The grant therefore travels through neither: it is written to
      // the pane's LEGION_GRANT_FILE, which `legion` reads first. GH_CONFIG_DIR, the shim-first
      // PATH, and the emptied GitHub keys are on the pane from the daemon.
      await writeGrantFile(grantFile, grant.grantId);
      return undefined;
    } catch (error) {
      return { block: true, reason: messageFor(error) };
    }
  });

  pi.on("session_shutdown", async (_event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (
      capability === undefined ||
      capability.kind !== "root-architect" ||
      capability.sessionID !== sessionID
    ) {
      return;
    }
    try {
      await roleDaemon().processExit({
        tree: capability.tree,
        generation: generation(process.env),
        sessionId: sessionID,
        secret: capability.secret,
      });
    } finally {
      controlSubscription?.unsubscribe();
      controlSubscription = undefined;
      await controlConnection?.close();
      controlConnection = undefined;
    }
  });

  const architectSession = (
    context: SessionContext
  ): { tree: string; issue: string; role: LegionRole; secret: string } => {
    const sessionID = context.sessionManager.getSessionId();
    if (
      capability !== undefined &&
      capability.sessionID === sessionID &&
      capability.role === "architect"
    ) {
      return {
        tree: capability.tree,
        issue: capability.issue,
        role: capability.role,
        secret: capability.secret,
      };
    }
    throw new Error("legion is available only to root and sub-architect sessions");
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  let architectToolsRegistered = false;
  const registerArchitectTools = (): void => {
    if (architectToolsRegistered) return;
    architectToolsRegistered = true;
    pi.registerTool(createLegionTool({ pi, roleDaemon, architectSession }));
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    handler: async (_args, context) => claimController(context),
  });
}
