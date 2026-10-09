import { randomUUID } from "node:crypto";
import path from "node:path";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { activeDispatchConfig } from "@legion/envoy-client/dispatch-config";
import { resolveIssueDocumentId } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { messageFor } from "@legion/envoy-client/errors";
import { matchInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_VERSION,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  readEnvoyPluginInterface,
} from "@legion/pi-shared/interface";
import type {
  CommandContext,
  PiApi,
  SessionContext,
  ToolCallEvent,
  ToolCallEventResult,
} from "@legion/pi-shared/pi-types";
import { subagentSessionCheck } from "@legion/pi-shared/subagent-session";
import { logger } from "@oh-my-pi/pi-utils";
import { createClaimSession } from "../src/claim-session";
import { classifySession, type LegionSessionKind, requiredEnvironment } from "../src/classify";
import { createControllerSession } from "../src/controller-session";
import { createLegionDaemonClient, type LegionDaemonClient } from "../src/daemon-client";
import { writeMintedGrant } from "../src/grant-file";
import {
  assistantText,
  inboundKind,
  PHASE_STALL_ENTRY,
  type PhaseStall,
  type PhaseStallInput,
  restorePhaseStall,
  stepPhaseStall,
} from "../src/phase-stall";
import { applySessionTitle, legionSessionTitle } from "../src/session-title";
import { createLegionTool } from "../src/tools";

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

/** Whether a tool call runs something that redeems the pane's grant: a `bash` command one of whose
 * simple commands (`commands`, `splitShellCommands`' tokenisation of it) invokes `legion` — a word
 * equal to `legion` or ending `/legion`, in any position, so `legion threads resolve` and
 * `legion status` (which call the daemon with the grant) count, and so, harmlessly, do `legion
 * push`, `legion state` and `legion handoff write`. Nothing else redeems one: `gh` and `git`, and
 * Oh My Pi's `github` tool and `pr://`/`issue://` reads, which it serves by running `gh`, read the
 * role's GitHub App token from the gh files under the pane's `GH_CONFIG_DIR`; the grant is only a
 * `legion` command's authentication to the daemon. A command that does not tokenise (`commands`
 * undefined: an unterminated quote) mints, since for a credential the safe default is to mint. */
function needsGrant({ toolName, input }: ToolCallEvent, commands: string[][] | undefined): boolean {
  if (toolName !== "bash" || typeof input.command !== "string") return false;
  return commands?.some((words) => commandMatch(words, "legion", []) !== -1) ?? true;
}

async function wrapWithGrant(
  mint: () => Promise<LegionGrant>
): Promise<ToolCallEventResult | undefined> {
  try {
    await writeMintedGrant(async () => (await mint()).grantId);
    return undefined;
  } catch (error) {
    return { block: true, reason: messageFor(error) };
  }
}

async function persistedTranscript(
  context: CommandContext | SessionContext
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

// Every Legion issue workspace is a `jj workspace` of one shared clone, so they all share one
// operation log: `jj undo`, `jj abandon`, and `jj op restore|revert|abandon|undo` rewrite it for
// every tree at once (LEGION-45). The tool_call hook refuses them in every tree pane
// (`TREE_PANE_RULES`). `restore`/`revert` are operation-log commands only under `op`/`operation`;
// `jj restore <paths>` is file-level and stays allowed.
const JJ_LOG_REWRITE_WORDS = ["undo", "abandon"];
const JJ_OP_WORDS = ["op", "operation"];
const JJ_OP_LOG_REWRITE_WORDS = ["restore", "revert"];
const JJ_MENTION = /\bjj\b/;
// Derived from the word lists above so the tokenised and plain-text paths refuse the same verbs.
// Any non-word run between `op` and `restore`, so `"op", "restore"` in an argv literal counts.
const JJ_LOG_REWRITE_MENTION = new RegExp(
  `\\b(?:${JJ_LOG_REWRITE_WORDS.join("|")})\\b|\\b(?:${JJ_OP_WORDS.join("|")})\\b\\W+(?:${JJ_OP_LOG_REWRITE_WORDS.join("|")})\\b`
);

/** The plain-text rule for text the extension does not tokenise as a shell command -- `eval`
 * code, stdin written to a supervised service (`proc://<id>`), each word of a tokenised `bash`
 * command (`sh -c "jj undo"`), and a `bash` command with unbalanced quoting: the jj command `text`
 * mentions with a blocked word (e.g. `jj undo`, `jj op restore`), or undefined. */
function jjLogRewriteMention(text: string): string | undefined {
  if (!JJ_MENTION.test(text)) return undefined;
  const match = JJ_LOG_REWRITE_MENTION.exec(text);
  return match === null ? undefined : `jj ${match[0].replace(/\W+/g, " ")}`;
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
      (command.charAt(i - 1) === ">" ||
        command.charAt(i - 1) === "<" ||
        command.charAt(i + 1) === ">")
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

/** The index of the first word that is name itself or ends `/name` (an absolute path, e.g. the
 * launcher `<state_dir>/bin/legion`) and whose immediately following words equal, in order, every
 * word of sequence ([] means no required follow-up: the bare first-mention search
 * jjLogRewriteInvocation and needsGrant use, the former then scanning the rest of the words
 * itself). A later mention is tried when an earlier one's sequence does not match, and neither
 * looks only at words[0], so a prefix before the command name (`time`, `timeout 600`, an env
 * assignment such as `FOO=1`, a leading `!` or `if`, which splitShellCommands's naive split
 * leaves attached ahead of a `;`) never hides it. Shared by jjLogRewriteInvocation and
 * needsGrant. */
function commandMatch(words: readonly string[], name: string, sequence: readonly string[]): number {
  for (let index = 0; index < words.length; index += 1) {
    const word = words[index];
    if (word !== name && !word?.endsWith(`/${name}`)) continue;
    if (sequence.every((expected, offset) => words[index + 1 + offset] === expected)) return index;
  }
  return -1;
}

/** A rule the tool_call hook holds a pane's shell-running tool calls to. */
interface PaneRule {
  /** The plain-text rule: what `text` names that the rule refuses, or undefined. */
  readonly mention: (text: string) => string | undefined;
  /** The tokenised rule: what one simple command's words run that the rule refuses, or
   * undefined. */
  readonly invocation: (words: readonly string[]) => string | undefined;
  readonly refusal: (attempt: string) => string;
}

/** A `jj` invocation (the first `jj` or `.../jj` word) that would rewrite the shared jj operation
 * log. It is judged on the whole argument list after `jj` -- never only the first word after it --
 * so `jj -R <path> undo`, `jj --at-op <id> op restore <id>`, and `jj operation restore` count. A
 * word is judged by its text whatever its quoting, since bash hands jj the same argv either way:
 * `jj "undo"`, `"jj" undo`, and `jj \u\n\d\o` are `jj undo`, and a one-word `-m "undo"` is refused
 * with them (the spec's tradeoff: one rephrase), while `-m "undo this"` is a different word and
 * stays allowed. `undo`/`abandon` count anywhere; `restore`/`revert` only beside `op`/`operation`. */
function jjLogRewriteInvocation(words: readonly string[]): string | undefined {
  const jj = commandMatch(words, "jj", []);
  if (jj === -1) return undefined;
  const args = words.slice(jj + 1);
  const rewritesLog =
    args.some((arg) => JJ_LOG_REWRITE_WORDS.includes(arg)) ||
    (args.some((arg) => JJ_OP_WORDS.includes(arg)) &&
      args.some((arg) => JJ_OP_LOG_REWRITE_WORDS.includes(arg)));
  return rewritesLog ? words.slice(jj).join(" ") : undefined;
}

const JJ_LOG_REWRITE: PaneRule = {
  mention: jjLogRewriteMention,
  invocation: jjLogRewriteInvocation,
  refusal: (attempt) =>
    `refused \`${attempt}\`: jj undo, jj abandon, and jj op restore/revert/abandon/undo ` +
    "rewrite the jj operation log, which every Legion issue workspace shares (each is a jj " +
    "workspace of one clone), so they rewrite other trees' commits too. Recover forward with " +
    "a new commit or `jj restore <paths>` of files; anything else, stop and send the owning " +
    'architect the `jj -R "$LEGION_WORKSPACE" log` evidence.',
};

/** The rules a pane in an issue workspace is held to, judged from the pane environment so that
 * they bind a `task` subagent too: it runs in that pane, against that workspace. Every issue
 * workspace is a jj workspace of one clone, sharing its operation log, and every tree pane -- a
 * phase worker's, a sub-architect's, a root architect's -- has one. */
const TREE_PANE_RULES: readonly PaneRule[] = [JJ_LOG_REWRITE];

/** The rules each kind of Legion pane is held to. The controller has no issue workspace. */
const PANE_RULES: Readonly<Partial<Record<LegionSessionKind["kind"], readonly PaneRule[]>>> = {
  "phase-worker": TREE_PANE_RULES,
  "root-architect": TREE_PANE_RULES,
};

/** The first thing in a `bash` command that `rule` refuses, named for the refusal, or undefined.
 * Each simple command of `commands` (`command` as `splitShellCommands` tokenised it), in any
 * position of a pipeline or `&&` chain, is held to the tokenised rule, and each of its words to the
 * plain-text rule (`sh -c "jj undo"`); so is the whole command when it does not tokenise. */
function refusedCommand(
  command: string,
  commands: string[][] | undefined,
  rule: PaneRule
): string | undefined {
  if (commands === undefined) {
    return rule.mention(command) === undefined ? undefined : command.trim();
  }
  for (const words of commands) {
    const mentioned = words.find((word) => rule.mention(word) !== undefined);
    if (mentioned !== undefined) return mentioned;
    const invocation = rule.invocation(words);
    if (invocation !== undefined) return invocation;
  }
  return undefined;
}

/** The `write` targets that are not files, each scheme in any case, as Oh My Pi routes it: a tool
 * device (`xd://<tool>` carrying the tool's JSON args as `content`, e.g. the Dispatch tools), a
 * message to an agent of this process (`agent://<id>`), or job and service control (`proc://<id>`:
 * `content` goes to a supervised service's stdin; `/kill` stops a job, `/mode` sets its lifetime). */
const NON_FILE_WRITE_URL = /^(xd|agent|proc):\/\//iu;

/** The 4-hex tag that may end a `read` header, `#XXXX`. */
const READ_HEADER_TAG = /#[0-9A-Fa-f]{4}$/u;

/** A `conflict://` URL behind a prefix, `<prefix>:conflict://N`; the last `:conflict://` wins. */
const PREFIXED_CONFLICT_URL = /^(.+):(conflict:\/\/.+)$/u;

/** The path a pasted `read` header names (`unwrapHashlineHeaderPath`): `[path]` or `[path#XXXX]`
 * names `path` (a valid tag lets `path` hold a `#` of its own); any other shape is left as
 * written. */
function unwrapReadHeader(path: string): string {
  const trimmed = path.trimEnd();
  if (trimmed.length < 2 || !trimmed.startsWith("[") || !trimmed.endsWith("]")) return path;
  const inner = trimmed.slice(1, -1);
  const tag = READ_HEADER_TAG.exec(inner);
  const target = tag === null ? inner : inner.slice(0, tag.index);
  if (target.length === 0 || (tag === null && target.includes("#"))) return path;
  return target;
}

/** The scheme, lowercased, of a `write` into Oh My Pi rather than to a file, or undefined. The
 * target is read as Oh My Pi's `write` routes it: the path inside a pasted `read` header, then
 * the `conflict://` URL alone when a prefix stands before it (`recoverConflictUriPrefix`), which
 * writes a workspace file whatever the prefix. */
function nonFileWriteScheme(toolCall: ToolCallEvent): string | undefined {
  if (toolCall.toolName !== "write" || typeof toolCall.input.path !== "string") return undefined;
  const unwrapped = unwrapReadHeader(toolCall.input.path);
  const target = PREFIXED_CONFLICT_URL.exec(unwrapped)?.[2] ?? unwrapped;
  return NON_FILE_WRITE_URL.exec(target)?.[1]?.toLowerCase();
}

/** The refusal for the first of `rules` a tool call breaks, or undefined. commands is bash's own
 * tokenisation (splitShellCommands(input.command)), computed once by the caller and shared with
 * needsGrant later in the same hook, rather than tokenised twice for the same command.
 * A supervised service's start is included (a `bash` call with a `name`); `eval` code and the
 * content a `write` sends to a `proc://` target (stdin for a supervised service) are held to the
 * plain-text rule, since each runs a shell from the pane exactly as `bash` does. */
function paneRuleRefusal(
  toolCall: ToolCallEvent,
  rules: readonly PaneRule[],
  commands: string[][] | undefined
): string | undefined {
  if (rules.length === 0) return undefined;
  const { toolName, input } = toolCall;
  if (toolName === "bash") {
    if (typeof input.command !== "string") return undefined;
    for (const rule of rules) {
      const attempt = refusedCommand(input.command, commands, rule);
      if (attempt !== undefined) return rule.refusal(attempt);
    }
    return undefined;
  }
  let text: string;
  if (toolName === "eval" && typeof input.code === "string") text = input.code;
  else if (nonFileWriteScheme(toolCall) === "proc" && typeof input.content === "string") {
    text = input.content;
  } else return undefined;
  for (const rule of rules) {
    const mention = rule.mention(text);
    if (mention !== undefined) return rule.refusal(`${toolName}: ${mention}`);
  }
  return undefined;
}

/**
 * Why this Legion entry cannot run in this process, or undefined when it can. Legion claims roles
 * and reads deliveries through the interface the Envoy plugin publishes
 * (`@legion/pi-shared/interface`), so a process without `@sjawhar/pi-envoy`, or with one that
 * speaks another interface version, has nothing to claim through; and the pre-split
 * `@sjawhar/pi-legion-envoy` still installed beside this plugin would run a second Legion entry
 * against the same pane. The sentence names the remedy, since the daemon's boot log and the
 * operator's own session are where it lands.
 */
function envoyPluginRefusal(): string | undefined {
  const legacy = (globalThis as Record<symbol, unknown>)[LEGACY_LEGION_LOADED_KEY];
  if (legacy !== undefined) {
    const where = typeof legacy === "string" ? ` from ${legacy}` : "";
    return `Legion plugin at ${import.meta.url} found the pre-split @sjawhar/pi-legion-envoy loaded${where}; uninstall @sjawhar/pi-legion-envoy (omp plugin uninstall @sjawhar/pi-legion-envoy) and keep @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
  }
  const reading = readEnvoyPluginInterface();
  switch (reading.kind) {
    case "present":
      return undefined;
    case "absent":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}, found none; install @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
    case "mismatch":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${reading.expected}, found version ${reading.found} from ${reading.from}; install the @sjawhar/pi-envoy released with this @sjawhar/pi-legion`;
  }
}

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own). The id ties every hook log line
  // below to the instance that emitted it, so a per-call grant count can be attributed.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  // Read by the daemon's boot gate to prove this extension actually loaded from an ambient
  // installed-plugin discovery -- not just that a manifest file exists, which stays true even when
  // the plugin is disabled or unregistered in OMP's own plugin registry -- and which interface
  // version it speaks.
  (globalThis as Record<symbol, unknown>)[LEGION_PLUGIN_LOADED_KEY] = {
    from: import.meta.url,
    envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION,
  };

  // Gates session_start, tool_call and the session-change re-claim below, one call per hook; its
  // settled answer is kept, so it runs once per session, not once per tool call.
  const checkSubagentSession = subagentSessionCheck();
  // The pane rules this pane is held to (PANE_RULES), judged from the environment on the first
  // tool_call. A subagent's own instance inherits the pane's environment, so the rules bind it
  // exactly as they bind the worker that spawned it.
  let paneRules: readonly PaneRule[] | undefined;

  // The phase-stall check (src/phase-stall.ts). It runs only in a session holding a
  // claim for its own id with a phase role, so never in an architect (a root or a sub-architect),
  // the controller (whose claim lives in controllerSession), a session with no Legion environment,
  // or a `task` subagent (whose instance returns at checkSubagentSession before any capability
  // exists). Restored from the transcript at session_start and appended to it on every change.
  let phaseStall: PhaseStall = "closed";
  const phaseWorkerSession = (context: SessionContext): boolean => {
    const role = claimSession.capability(context.sessionManager.getSessionId())?.role;
    return role !== undefined && role !== "architect";
  };
  const advancePhaseStall = (input: PhaseStallInput): string | undefined => {
    const step = stepPhaseStall(phaseStall, input);
    if (step.state !== phaseStall) {
      phaseStall = step.state;
      pi.appendEntry(PHASE_STALL_ENTRY, { state: phaseStall });
    }
    return step.followUp;
  };

  let daemonClient: LegionDaemonClient | undefined;
  const roleDaemon = (): LegionDaemonClient => {
    daemonClient ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL")
    );
    return daemonClient;
  };

  // A root architect's or phase worker's claim, and the operator-launched controller's.
  const claimSession = createClaimSession({
    daemon: roleDaemon,
    persistedTranscript,
    exitProcess: (code) => exitProcess(code),
  });
  const controllerSession = createControllerSession({
    daemon: roleDaemon,
    persistedTranscript,
    pi,
    exitProcess: (code) => exitProcess(code),
  });

  /**
   * Names the session by its Legion identity (`src/session-title.ts`), so every Dispatch
   * write stamps it as `origin.session_title` and the Envoy listener lists it. Runs before the
   * session claims its Envoy role: that claim registers the session, and the registration carries
   * the title then rather than at the next heartbeat.
   */
  const titleSession = async (context: SessionContext): Promise<void> => {
    const title = legionSessionTitle(classifySession(process.env), process.env.LEGION_PROJECT);
    if (title !== undefined) await applySessionTitle(pi, context, title);
  };

  pi.on("session_start", async (_event, context) => {
    // A `task`-spawned subagent session loads a fresh instance of this whole module: bail out
    // before classification, or the inherited LEGION_* environment would look like a fresh
    // root/worker boot and its failure would exit the parent process. See isSubagentSession.
    if (await checkSubagentSession(context)) return;
    await titleSession(context);
    // A worker the daemon relaunched with --resume keeps its phase: its next turn may start from
    // an Envoy notice rather than a new assignment, and must find the phase still open.
    phaseStall = restorePhaseStall(context.sessionManager.getBranch?.() ?? []);
    const { kind } = classifySession(process.env);
    if (kind === "not-legion") return;
    // Every Legion session (a root architect, a phase worker, the controller) claims through the
    // Envoy plugin's interface, so a process without it ends here, the way a refused boot
    // registration does: one log line, then the exit the daemon sees and relaunches from. A
    // person's own session returned above and never exits.
    const refusal = envoyPluginRefusal();
    if (refusal !== undefined) {
      logger.error(refusal);
      exitProcess(1);
    }
    // The operator-launched controller registers through the controller session and carries no
    // `legion` tool: its operations are an architect's and a worker's.
    if (kind === "controller") {
      await controllerSession.handleSessionStart(context);
      return;
    }
    await claimSession.bootstrap(context);
    registerLegionTool();
    await activateLegionTool();
  });

  // Mirrors envoy.ts: only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason, and every one of them can leave the pane on a new session id.
  // The controller re-claims whatever session the pane is left on, so that session takes the
  // controller's title first; a session that keeps its id keeps its title. A `task` subagent's
  // instance does neither, and the subagent check is asked once for both.
  const afterSessionChange = async (context: SessionContext): Promise<void> => {
    if (await checkSubagentSession(context)) return;
    if (classifySession(process.env).kind === "controller") await titleSession(context);
    await controllerSession.reclaimAfterSessionChange(context);
  };
  pi.on("session_switch", (_event, context) => afterSessionChange(context));
  pi.on("session_branch", (_event, context) => afterSessionChange(context));
  pi.on("session_tree", (_event, context) => afterSessionChange(context));

  pi.on("tool_call", async (toolCall, context): Promise<ToolCallEventResult | undefined> => {
    logger.debug("legion tool_call hook", {
      instance,
      toolCallId: toolCall.toolCallId,
      toolName: toolCall.toolName,
    });
    // LEGION-45: the operation log is shared by every issue workspace (all are jj workspaces of
    // one clone), and a `task` subagent's bash runs in the same pane against it, so the pane rule
    // is judged from the pane's environment ahead of the subagent exemption below and before any
    // grant is minted. Classified once per instance, on the first call: a throw for a malformed
    // LEGION_ROLE stays inside the handler, never at load.
    paneRules ??= PANE_RULES[classifySession(process.env).kind] ?? [];
    const commands =
      toolCall.toolName === "bash" && typeof toolCall.input.command === "string"
        ? splitShellCommands(toolCall.input.command)
        : undefined;
    const refusal = paneRuleRefusal(toolCall, paneRules, commands);
    if (refusal !== undefined) return { block: true, reason: refusal };
    // A subagent's own tool calls mint no grant: the grant minting below binds the session that
    // holds the claim, and a subagent shares its parent's identity and claims no role (see
    // isSubagentSession). Every role may launch one with `task`.
    if (await checkSubagentSession(context)) return undefined;
    const sessionID = context.sessionManager.getSessionId();
    const active = claimSession.capability(sessionID);
    if (!needsGrant(toolCall, commands)) return undefined;
    // The shared wrapper mints through the caller's client, then writes the grant to the pane's
    // `LEGION_GRANT_FILE`, which `legion` reads ahead of `LEGION_GRANT`. The host writes a hook's
    // revised `input` back into the assistant message (text the model imitates — LEGION-12), and a
    // plugin that replaces the bash tool may drop `env` (secretsd's legacy shim, LEGION-52), so the
    // grant travels through neither and the tool call's input is never rewritten. The daemon names
    // the grant file on every pane it launches; a pane without one was launched by a daemon older
    // than this plugin, and minting for it would only produce a grant nothing could read. A blank
    // value (an operator's own export) is the same absence.
    if (active === undefined) {
      // A claimed controller session mints a controller grant (`/grants` `{sessionId, secret}`,
      // authenticated by its registration's secret) and is wrapped exactly like a worker.
      if (controllerSession.isClaimedSession(sessionID)) {
        return wrapWithGrant(() => controllerSession.mintGrant(sessionID));
      }
      // A worker (root or phase) whose own boot handshake has not completed yet has no
      // capability to mint a grant with, so it is blocked. A controller that has not yet
      // claimed (`legion controller start` also sets LEGION_ROLE=controller) is not: nothing
      // here can mint for it until `controllerSession.claim` runs, and a wrong secret is what blocks it.
      if (process.env.LEGION_ROLE !== undefined && process.env.LEGION_CONTROLLER !== "1") {
        return {
          block: true,
          reason: "Legion worker session is not registered; cannot mint its grant",
        };
      }
      return undefined;
    }
    return wrapWithGrant(() =>
      roleDaemon().grant({
        tree: active.tree,
        issue: active.issue,
        sessionId: sessionID,
        secret: active.secret,
      })
    );
  });

  // The daemon's assignment (a user message) opens the phase; an Envoy delivery re-arms a stall
  // that already had its follow-up or a WAITING reply. A person's direct message that envoy.ts
  // sent in as the user's own turn is a user message too, but it is an inbound event, as its Envoy
  // card was: the record envoy.ts keeps of the turns it sent in says which user message that is,
  // whichever of the two extensions asks first. A turn that record misses falls through to
  // `inboundKind` and counts as an assignment; `packages/pi-envoy/AGENTS.md` (the phase-worker
  // section) says which turns it misses. The `legion` tool's successful `handoff_complete` closes
  // the phase (`onPhaseCompleted`, below).
  pi.on("message_start", async (event, context) => {
    if (!phaseWorkerSession(context)) return;
    const injected =
      matchInjectedUserTurn(context.sessionManager.getSessionId(), event.message) !== undefined;
    const kind = injected ? "inbound-event" : inboundKind(event.message);
    if (kind !== undefined) advancePhaseStall({ kind });
  });

  // The run is about to settle with nothing left for the host to continue: a phase still open
  // gets its one follow-up, which the host sends as the next turn of this same session.
  pi.on("session_stop", async (event, context) => {
    if (event.signal.aborted || !phaseWorkerSession(context)) return undefined;
    const followUp = advancePhaseStall({
      kind: "settle",
      lastAssistantText: assistantText(event.last_assistant_message),
    });
    return followUp === undefined ? undefined : { continue: true, additionalContext: followUp };
  });

  const onPhaseCompleted = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) advancePhaseStall({ kind: "handoff-complete" });
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  let legionToolRegistered = false;
  const registerLegionTool = (): void => {
    if (legionToolRegistered) return;
    legionToolRegistered = true;
    pi.registerTool(
      createLegionTool({
        pi,
        daemon: roleDaemon,
        session: (context) => {
          const active = claimSession.capability(context.sessionManager.getSessionId());
          if (active === undefined) {
            throw new Error("legion is available only to this session's registered claim");
          }
          return {
            kind: active.role === "architect" ? "architect" : "phase-worker",
            sessionId: active.sessionID,
            tree: active.tree,
            issue: active.issue,
            secret: active.secret,
          };
        },
        onPhaseCompleted,
        resolveDocument: async (issue, reference) => {
          const config = activeDispatchConfig(process.env, { cwd: process.cwd() });
          if (config === null) {
            throw new Error(
              `register_gate cannot look up document "${reference}" on ${issue}: this pane has no Dispatch configured; pass the document's id`
            );
          }
          try {
            return await resolveIssueDocumentId(
              new DispatchClient(config.url, config.token, fetch),
              issue,
              reference
            );
          } catch (error) {
            throw new Error(
              `register_gate could not look up document "${reference}" on ${issue} in Dispatch: ${messageFor(error)}`
            );
          }
        },
      })
    );
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    // Inside the controller's own session (the `LEGION_CONTROLLER` marker) this is the manual
    // override for a lost claim. From a hand-started session it is an interactive takeover: the
    // role and recorded session id move to this session (see `controllerSession.claim`).
    // The operator started this session and reads it, so a missing Envoy plugin is answered as
    // the command's error, never an exit, and nothing is claimed.
    handler: async (_args, context) => {
      const refusal = envoyPluginRefusal();
      if (refusal !== undefined) throw new Error(refusal);
      await controllerSession.claim(context);
    },
  });
}
