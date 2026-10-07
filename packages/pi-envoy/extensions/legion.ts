import { randomUUID } from "node:crypto";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { activeDispatchConfig } from "@legion/envoy-client/dispatch-config";
import { resolveIssueDocumentId } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { messageFor } from "@legion/envoy-client/errors";
import { logger } from "@oh-my-pi/pi-utils";
import { matchInjectedUserTurn } from "../src/dispatch-user-turn";
import { createClaimSession } from "../src/legion/claim-session";
import {
  classifySession,
  type LegionSessionKind,
  requiredEnvironment,
} from "../src/legion/classify";
import { createControllerSession } from "../src/legion/controller-session";
import { createLegionDaemonClient, type LegionDaemonClient } from "../src/legion/daemon-client";
import { writeMintedGrant } from "../src/legion/grant-file";
import {
  assistantText,
  inboundKind,
  PHASE_STALL_ENTRY,
  type PhaseStall,
  type PhaseStallInput,
  restorePhaseStall,
  stepPhaseStall,
} from "../src/legion/phase-stall";
import { applySessionTitle, legionSessionTitle } from "../src/legion/session-title";
import { createLegionTool } from "../src/legion/tools";
import type {
  CommandContext,
  PiApi,
  SessionContext,
  ToolCallEvent,
  ToolCallEventResult,
} from "../src/pi-types";
import { subagentSessionCheck } from "../src/subagent-session";

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

/** A `pr://` or `issue://` URL anywhere Oh My Pi's path pipeline finds one: alone, inside one pair
 * of outer double quotes (which it strips), or as one entry of a list split on `;`, `,`, or
 * whitespace. Its internal-URL router resolves either scheme, in any case, by running `gh`. */
const GH_RESOLVED_URL = /(?:^|[\s;,"])(?:pr|issue):\/\//i;

/** Whether a tool call runs something that redeems the pane's grant: a `bash` command (`legion`,
 * `jj git push`, the `gh` shim), Oh My Pi's `github` tool, and any tool whose `path` or `paths`
 * names a `pr://` or `issue://` URL (`read`, `grep`, `glob`, `ast_grep`, `ast_edit` all resolve
 * internal URLs). Oh My Pi serves the last two by running `gh`, which on a Legion pane is the shim
 * that runs `legion gh`. A grant lives its ttl (60 seconds, or pushTTL for a `legion push`
 * invocation), so a call that reaches `gh` long after the
 * pane's last bash command needs its own. */
function needsGrant({ toolName, input }: ToolCallEvent): boolean {
  if (toolName === "bash") return typeof input.command === "string";
  if (toolName === "github") return true;
  const paths = Array.isArray(input.paths) ? input.paths : [input.path];
  return paths.some((entry) => typeof entry === "string" && GH_RESOLVED_URL.test(entry));
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

// An architect delegates code work, but its prompt requires `legion gh --` to touch GitHub; a
// sub-architect completes its phase through the `legion` tool, never bash: `legion handoff
// complete` is refused ahead of this gate by the pane rules (PANE_RULES), in a sub-architect's pane
// and a root architect's alike.
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

/** The index of the first word that is name itself or ends `/name` (an absolute-path shim, e.g.
 * the worker-bin `jj` or `legion`) and whose immediately following words equal, in order, every
 * word of sequence ([] means no required follow-up: the bare first-mention search
 * jjLogRewriteInvocation uses, which then scans the rest of the words itself). A later mention
 * is tried when an earlier one's sequence does not match (`legion gh -- legion push`: the first
 * `legion` is not followed by `push`, the second is), and neither looks only at words[0], so a
 * prefix before the command name (`time`, `timeout 600`, an env assignment such as `FOO=1`, a
 * leading `!` or `if`, which splitShellCommands's naive split leaves attached ahead of a `;`)
 * never hides it. Shared by jjLogRewriteInvocation, isPushInvocation, and the handoff-complete
 * rule's own invocation matcher. */
function commandMatch(words: readonly string[], name: string, sequence: readonly string[]): number {
  for (let index = 0; index < words.length; index += 1) {
    const word = words[index];
    if (word !== name && !word?.endsWith(`/${name}`)) continue;
    if (sequence.every((expected, offset) => words[index + 1 + offset] === expected)) return index;
  }
  return -1;
}

/** A simple command (one of splitShellCommands's entries) is a `legion push` invocation: a
 * `legion` (or `.../legion`) mention immediately followed by `push`. */
function isPushInvocation(words: readonly string[]): boolean {
  return commandMatch(words, "legion", ["push"]) !== -1;
}

/** Whether tokenised simple commands (splitShellCommands' result, shared with paneRuleRefusal's
 * own tokenisation of the same bash command rather than retokenising it) include a `legion push`
 * invocation -- `legion push`, `cd … && legion push`, a pipeline's last segment -- so the
 * tool_call hook mints its grant with the longer pushTTL (dispatch://LEGION-583): jj's own
 * working-copy snapshot before the network push can outrun the ordinary sixty seconds on a
 * near-full tree volume. undefined (a tool call that is not `bash`, or an unterminated quote) is
 * judged not a push: the ordinary grant is the safe default, since missing a genuine push here
 * costs only the grant expiring before it, which `legion push` already fails loudly on. */
function commandsRunPush(commands: string[][] | undefined): boolean {
  return commands?.some(isPushInvocation) ?? false;
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

// A worker completes its phase with the `legion` tool's `handoff_complete`
// (src/legion/handoff-actions.ts), and the phase stall (src/legion/phase-stall.ts) closes only on
// that call: the same command run from the pane's shell would complete the phase where the stall
// cannot see it and draw a follow-up asking the worker to complete again. Only `complete` is
// refused: `legion handoff write` and `read` from the shell leave no phase open, and the shell can
// pipe a handoff too large for one argv string to `legion handoff write` on stdin. `legion`,
// `handoff` and `complete` separated only by whitespace, quotes, and argv-list punctuation, so
// `["legion", "handoff", "complete"]` counts and a path through `legion/handoff` does not.
const LEGION_HANDOFF_COMPLETE_MENTION = /\blegion[\s"'`,[\]]+handoff[\s"'`,[\]]+complete\b/;

const LEGION_HANDOFF_COMPLETE: PaneRule = {
  mention: (text) =>
    LEGION_HANDOFF_COMPLETE_MENTION.test(text) ? "legion handoff complete" : undefined,
  // Both CLIs take `handoff` straight after the program and `complete` straight after `handoff`.
  invocation: (words) => {
    const legion = commandMatch(words, "legion", ["handoff", "complete"]);
    return legion === -1 ? undefined : words.slice(legion).join(" ");
  },
  refusal: (attempt) =>
    `refused \`${attempt}\`: a phase is completed with the \`legion\` tool's ` +
    "`handoff_complete`, never a shell command: the tool call is what records the phase " +
    "complete. A root architect or a `task` subagent has no handoff actions: it leaves the " +
    "completion to the worker. Text that only names the command (a commit message, a PR body) " +
    "reads as the command: pass it in a file.",
};

/** The rules a pane in an issue workspace is held to, ahead of every role gate, so that they bind
 * a `task` subagent too: it runs in that pane, against that workspace. Every issue workspace is a
 * jj workspace of one clone, sharing its operation log, and every tree pane -- a phase worker's, a
 * sub-architect's, a root architect's -- has one. */
const TREE_PANE_RULES: readonly PaneRule[] = [JJ_LOG_REWRITE, LEGION_HANDOFF_COMPLETE];

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

/** The target a `write` path names, as Oh My Pi's `write` tool reads it before it routes: the
 * path inside a pasted `read` header, and then the `conflict://` URL alone when a prefix stands
 * before it (`recoverConflictUriPrefix`), which writes a workspace file whatever the prefix. */
function writeTarget(path: string): string {
  const unwrapped = unwrapReadHeader(path);
  return PREFIXED_CONFLICT_URL.exec(unwrapped)?.[2] ?? unwrapped;
}

/** The scheme, lowercased, of a `write` into Oh My Pi rather than to a file, or undefined. */
function nonFileWriteScheme(toolCall: ToolCallEvent): string | undefined {
  if (toolCall.toolName !== "write" || typeof toolCall.input.path !== "string") return undefined;
  return NON_FILE_WRITE_URL.exec(writeTarget(toolCall.input.path))?.[1]?.toLowerCase();
}

/** The refusal for the first of `rules` a tool call breaks, or undefined. commands is bash's own
 * tokenisation (splitShellCommands(input.command)), computed once by the caller and shared with
 * the push-grant check later in the same hook, rather than tokenised twice for the same command.
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

// Read by the daemon's boot gate (packages/daemon/internal/daemon/bootgate.go) to prove this
// extension actually loaded from an ambient installed-plugin discovery -- not just that a
// manifest file exists, which stays true even when the plugin is disabled or unregistered in
// OMP's own plugin registry.
const LEGION_LOADED_MARKER = Symbol.for("legion.pi-envoy.legion-loaded");

/** Code-mutation tools, blocked for each role `CODE_TOOL_REFUSAL` names. `write` here means a real
 * filesystem write: a `write` into Oh My Pi (`nonFileWriteScheme`) passes the gate for every role,
 * since a gated role messages and cancels the subagents it launches this way. `task` is
 * deliberately absent: every Legion role may launch `task` subagents. */
const CODE_MUTATION_TOOLS = ["edit", "write", "apply_patch"];

/** Why each gated role is refused a code-mutation tool. A role absent here mutates code freely;
 * `architect` covers a root architect and a sub-architect alike, and is the one role whose `bash`
 * is held to the same refusal (every command but a single `legion` one). */
export const CODE_TOOL_REFUSAL: Readonly<Partial<Record<LegionRole, string>>> = {
  architect: "the architect delegates all code work to phase workers",
  reviewer: "the reviewer edits no code; its only commits are its review handoffs, made via bash",
  merger: "the merger only verifies and reports",
};

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own). The id ties every hook log line
  // below to the instance that emitted it, so a per-call grant count can be attributed.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  (globalThis as Record<symbol, unknown>)[LEGION_LOADED_MARKER] = import.meta.url;

  // Gates session_start, tool_call and the session-change re-claim below, one call per hook; its
  // settled answer is kept, so it runs once per session, not once per tool call.
  const checkSubagentSession = subagentSessionCheck();
  // The pane rules this pane is held to (PANE_RULES), judged from the environment on the first
  // tool_call. A subagent's own instance inherits the pane's environment, so the rules bind it
  // exactly as they bind the worker that spawned it.
  let paneRules: readonly PaneRule[] | undefined;

  // The phase-stall check (src/legion/phase-stall.ts). It runs only in a session holding a
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
  });

  /**
   * Names the session by its Legion identity (`src/legion/session-title.ts`), so every Dispatch
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
    // one clone), and a `task` subagent's bash runs in the same pane against it; a subagent has no
    // `legion` tool, so a handoff from its bash is one the phase stall cannot see. The pane rules
    // are therefore judged from the pane's environment ahead of the subagent exemption below --
    // the one gate that reaches a subagent -- and before any grant is minted. Classified once per
    // instance, on the first call: a throw for a malformed LEGION_ROLE stays inside the handler,
    // never at load.
    paneRules ??= PANE_RULES[classifySession(process.env).kind] ?? [];
    const commands =
      toolCall.toolName === "bash" && typeof toolCall.input.command === "string"
        ? splitShellCommands(toolCall.input.command)
        : undefined;
    const refusal = paneRuleRefusal(toolCall, paneRules, commands);
    if (refusal !== undefined) return { block: true, reason: refusal };
    // No other gate applies to a subagent's own tool calls: the role gates below bind the session
    // that holds the claim, and a subagent shares its parent's identity and claims no role (see
    // isSubagentSession). Every role may launch one with `task`.
    if (await checkSubagentSession(context)) return undefined;
    const sessionID = context.sessionManager.getSessionId();
    const active = claimSession.capability(sessionID);
    const codeToolRefusal = active === undefined ? undefined : CODE_TOOL_REFUSAL[active.role];
    const mutatesCode =
      CODE_MUTATION_TOOLS.includes(toolCall.toolName) ||
      (active?.role === "architect" &&
        toolCall.toolName === "bash" &&
        !isSingleLegionCommand(toolCall.input.command));
    // A `write` into Oh My Pi (a tool device, an agent message, job control) is not a file
    // mutation. Short-circuit it out of the mutation gate so the architect/reviewer/merger role
    // checks apply only to real file writes.
    if (
      codeToolRefusal !== undefined &&
      mutatesCode &&
      nonFileWriteScheme(toolCall) === undefined
    ) {
      return { block: true, reason: codeToolRefusal };
    }
    if (!needsGrant(toolCall)) return undefined;
    // The shared wrapper mints through the caller's client, then writes the grant to the pane's
    // `LEGION_GRANT_FILE`, which `legion` reads ahead of `LEGION_GRANT`. The host writes a hook's
    // revised `input` back into the assistant message (text the model imitates — LEGION-12), and a
    // plugin that replaces the bash tool may drop `env` (secretsd's legacy shim, LEGION-52), so the
    // grant travels through neither and the tool call's input is never rewritten. GH_CONFIG_DIR and
    // the emptied GitHub keys are on the pane from the daemon. The daemon names the grant file on
    // every pane it launches; a pane without one was launched by a daemon older than this plugin,
    // and minting for it would only produce a grant nothing could read. A blank value (an
    // operator's own export) is the same absence.
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
        push: commandsRunPush(commands),
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
    handler: async (_args, context) => controllerSession.claim(context),
  });
}
