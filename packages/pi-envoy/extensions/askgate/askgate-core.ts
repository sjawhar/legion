// AskGate: before a root session's scoped Dispatch write executes, one model call judges it
// against AskGate's rules (rules.md) and answers `allow` or `revise`. `askgate.ts` is the entry
// that binds Oh My Pi's completion call and buildSessionContext; everything else lives here so the
// tests drive it through a fake `pi`, a fake `ctx` and stubs.
//
// Scope: eight calls on the command line and six of them as devices, each recorded under one tool
// name (dispatch_ask, …).
//   - A `bash` call whose whole command is the literal grammar of one `dispatch` call
//     (`dispatchCommandHead`): `[cd <literal path> && ] dispatch <subcommand> <literal words…>
//     [2>&1] [<<'DELIM' … DELIM]`. The program is `dispatch` by name or by a literal path. Every
//     word is literal: outside quotes it holds only letters, digits and `._/+@%:,=-` (so no `$`,
//     `{`, `}`, backtick, glob character, `~` or backslash), and inside double quotes no `$`,
//     backtick or backslash. The one here-document has a quoted delimiter (`'EOF'` or `"EOF"`)
//     and opens on the command's first line, before or after the one `2>&1`. Nothing else: no
//     assignment, other statement, pipe, `;`, `||`, `&`, other redirect or line continuation.
//     The subcommand is `ask`, `message`, `edit-ask`, `comment`, `doc-edit`, `request-approval` (its
//     `summary` is what the person approves), `issue` with `--spec` or `--spec-file`, or
//     `artifact` unless its literal `--name` and literal `--path` both show it is no markdown
//     document (`uploadMayBeDocument`): its dry run then decides, from the name and path it
//     prints, and an upload that is no markdown document runs with no entry. The gate learns
//     the exact arguments by running that command with `--dry-run` after the command name, in the
//     call's working directory (then its `cd`, as the call will) and environment, its
//     here-document still on stdin and without its `2>&1`, under a 10 s limit, and reviews the
//     JSON it prints. A dry-run that fails, times out or prints no JSON object is recorded as
//     `error` and the call runs (the CLI refuses arguments it cannot parse, before it sends
//     anything). A `dispatch` command with `--dry-run` or `--help` sends nothing and is not
//     reviewed. An upload given with `--path` prints the path, not the text. Every file the
//     write posts, that path and each `--<field>-file`, is copied
//     into a private snapshot the gate owns before the model is asked. The gate reviews the
//     snapshot's bytes as `content` and rewrites the call to post the snapshot, so the bytes that
//     post are the bytes reviewed. It reads a regular file alone: a missing file, a directory, a
//     device, a FIFO and `/dev/stdin` are refused as `unreadable` before any read.
//     The arguments are cut at 64 KiB as one JSON block, so the document gets less than 64 KiB of
//     it. A relative path resolves where the CLI resolves it, against its
//     `process.cwd()`, the physical directory the command runs in after its own `cd`: bash names
//     it (the `cd`, then `pwd -P`, run as the dry run runs). A directory lookup that fails is
//     `unreadable`, since a failed lookup is no proof that the write would fail. An upload is
//     judged against failure 10 alone (askgate-upload.md), since a plan or a report is long by
//     nature.
//   - A `bash` call that holds a scoped `dispatch` write in any other form — after another
//     command, with an expansion, a redirect or a pipe, sent to the background, beside a second
//     reviewed write, or calling `dispatch` by a path the shell expands (`$HOME/…/dispatch`) — or
//     a scoped subcommand run by a program an expansion names (`$D comment …`, `"${D}" ask …`,
//     `$(command -v dispatch) …`), or a `dispatch` whose subcommand an expansion names (`dispatch
//     $SUB …`), is `unreadable`. In `block` mode the call is refused before anything in it runs;
//     in `warn` mode it runs and the same explanation comes back as additional context, with the
//     entry's decision `allow`. The explanation states the literal grammar and tells the agent to
//     send that `dispatch` command on its own; when the word that runs it or names its subcommand
//     is what AskGate cannot read, it names the characters in it (`unreadableDispatchWrite`, a
//     scan aware of quotes, here-documents and command substitutions, for a scoped write at a
//     command position, and of the call's own text for a write another command would run). The
//     entry reads `unreadable`; it never counts toward the halt. A halt, or a session the gate
//     would skip (a primary on another provider), lets it run; a session with no model role
//     refuses it in both modes, as a gate that did not load does: neither can review anything,
//     and a gate that cannot review is no off switch.
//   - `write xd://<device>` calls, seen on the nested `tool_call` event the device wrapper emits
//     with `toolName = <device>` and the validated arguments, for dispatch_ask, dispatch_message,
//     dispatch_edit_ask, dispatch_comment, dispatch_doc_edit, and dispatch_issue when its
//     arguments carry `spec`. Only sessions started before the `dispatch` CLI still have them.
// Top-level sessions only (`ctx.agent.kind === "main"`); the factory is rebound in every
// subagent, which this check keeps out.
//
// The switch is not here: the session's envoy.json turns AskGate off (`askgate.enabled`, read in
// envoy.ts), and with it off nothing is bound. The mode, read for each gated call: OMP_ASKGATE when
// it is `warn` or `block`; any other value is ignored, and the first gated call shows one notice that
// names it and the switch. Else the session's latest `experiments` entry (the experiments
// extension's record): its `askgate_block` arm off is `warn`, and anything else is `block`. `warn`
// runs a revised call and returns the reason as additional context, which the fork delivers only
// when the call succeeds and ran through the agent loop (a failing call, or a write made from eval,
// drops it); `block` refuses a revised call with the reason as the call's error result. Each entry
// records the mode it ran under as `verdictMode`.
//
// Deadline: OMP_ASKGATE_TIMEOUT_MS, or 90 s, clamped 5 s under the runner's own ceiling for a
// tool_call handler (extensionHandlers.toolCallTimeoutMs, 30 s unless configured) and raced from
// handler entry. A ceiling that leaves under 10 s, and an OMP_ASKGATE_TIMEOUT_MS that is no
// positive integer, throw when the gate binds: the entry then refuses each write the gate would
// review (`loadFailureGate`), since only `askgate.enabled: false` turns AskGate off. A deadline
// under 30 s binds, and logs one warning that names the ceiling, the deadline and the key to raise:
// at the default ceiling the deadline is 25 s, which a review of a long session often exceeds, and a
// review that times out lets the write post.
//
// Per gated call, in order:
//   - rebuttal: the agent put `"advisor_rebuttal": "<one line>"` in the device JSON, or
//     `--advisor-rebuttal '<one line>'` among a `dispatch` command's flags. The key is stripped
//     on the outer `write` event (before the device validates its arguments), the flag from the
//     `bash` command whatever the gate decides (the CLI would refuse it), and the call runs
//     unasked; the agent is told its rebuttal was recorded and removed before sending. At most
//     64 unspent device rebuttals are remembered.
//   - halted: three consecutive failures to reach a verdict (no model, timeout, every model of the
//     chain erroring, no parsable answer) stop the gate for the session, with one UI notice. The
//     halt, the count, the breaker and the rebuttals reset on session_switch and session_branch.
//   - breaker: two revises on one target (the device plus its issue/ask/artifact/project
//     values) send the third attempt unasked, and say so; the second revise already warns that
//     it will. An allow, a rebuttal or a trip resets the target.
//   - no-model-role / skipped: neither `@askgate` (modelRoles.askgate) nor `@default` resolves, or
//     the session's primary model is on another provider than the gate's. Nothing is rendered or
//     sent. With no model role the call is refused in both modes, naming the roles to set; it never
//     counts toward the halt, since a halt would let the writes out. Another provider lets it run.
//   - verdict: `@askgate` is asked once, else `@default`, raced against the deadline. A model
//     that answers with an error (a provider 404 or 5xx) hands the request to the next model of
//     the role's retry.fallbackChains, as omp resolves it for the role in use (`fallbackModels`),
//     within the same deadline and on the primary's provider only; the entry records the model
//     that answered as `model` and each one that failed first in `failedModels`. Its
//     system prompt is askgate-system.md and rules.md, nothing else. The user message
//     (askgate-request.md) carries the agent under review as data: its system prompt and the
//     newest 256 KiB of its context (the fork's buildSessionContext), every piece XML-escaped
//     inside its own block, then the call. The last line of the answer must be
//     {"decision":"allow"} or {"decision":"revise","reason":"…"}. The reason reaches the agent
//     verbatim but for a tag-opening `<` (see `agentFacing`), capped at 2 KiB, and is recorded
//     as `deliveredReason`.
// Every outcome other than an unscoped or subagent call appends one `advisor-gate` entry
// (pi.appendEntry, never sent to the model) with the deadline it raced and every attempt's
// usage, the aborted one included. Every path fails open but
// one: a throw inside the handler is recorded as `error` and the call runs, and the deadline keeps
// the handler inside the runner's ceiling, whose expiry would refuse the call. The exception is a
// call given up without a verdict. When the call's tool ends first anyway (a user abort, or that
// ceiling), or the user stops the run while it waits (the path an eval-bridged write takes), the
// model call is cancelled and the entry reads `abandoned`. When the session shuts down while it
// waits, or a gated call arrives once shutdown has begun, the entry reads `shutdown`, written
// before the session is sealed. No verdict is not permission: block mode refuses either, warn mode
// lets it go, and neither counts toward the halt. OMP_ASKGATE_DUMP names a file each gate's system
// and user prompts are appended to, owner-only and never through a symlink.
import { createHash } from "node:crypto";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import requestTemplate from "./askgate-request.md" with { type: "text" };
import systemTemplate from "./askgate-system.md" with { type: "text" };
import uploadRule from "./askgate-upload.md" with { type: "text" };
import rules from "./rules.md" with { type: "text" };

const SCOPED_DEVICES = [
  "dispatch_ask",
  "dispatch_message",
  "dispatch_edit_ask",
  "dispatch_comment",
  "dispatch_doc_edit",
  "dispatch_issue",
] as const;
const SPEC_ONLY_DEVICE = "dispatch_issue";
const SCOPED_COMMANDS = [
  "ask",
  "message",
  "edit-ask",
  "comment",
  "doc-edit",
  "issue",
  "request-approval",
  "artifact",
] as const;
const SPEC_ONLY_COMMAND = "issue";
const SPEC_FLAGS = ["--spec", "--spec-file"];
/**
 * `dispatch artifact` is reviewed only as an upload of a markdown document. Dispatch stores an
 * upload as one when the CLI sends it as text/markdown: inline text (`--content`, `--content-file`),
 * whatever its name, or a `--path` file whose own name ends in `.md` or `.markdown`, since Bun takes
 * a file's type from its path. The gate also reviews an upload whose `--name` ends in `.md`.
 */
const UPLOAD_COMMAND = "artifact";
const MARKDOWN_NAME = /\.md$/i;
const MARKDOWN_PATH = /\.(md|markdown)$/i;
/** The upload flags whose text is the document itself. */
const INLINE_FLAGS = ["--content", "--content-file"];
/** The upload flags that read the name or the path from a file, which only the dry run resolves. */
const INDIRECT_FLAGS = ["--name-file", "--path-file"];
/** Flags under which a `dispatch` command sends nothing. */
const NO_SEND_FLAGS = ["--dry-run", "--help"];
const BREAKER_KEYS = ["issue", "ask", "artifact", "project", "in_reply_to"] as const;
const REBUTTAL_KEY = "advisor_rebuttal";
const REBUTTAL_FLAG = "--advisor-rebuttal";
export const DRY_RUN_TIMEOUT_MS = 10_000;
const GATE_TIMEOUT_MS = 90_000;
// The runner refuses a tool_call handler that outlives extensionHandlers.toolCallTimeoutMs, so the
// gate's deadline stays this far under it: room for the aborted call to report its usage
// (ABORT_GRACE_MS) and for the record. Below MIN_DEADLINE_MS a verdict is hopeless and the gate does not bind.
const CEILING_MARGIN_MS = 5_000;
const ABORT_GRACE_MS = 2_000;
const MIN_DEADLINE_MS = 10_000;
// A deadline under WARN_DEADLINE_MS binds with one warning at bind: a review of a long session often
// takes longer, and a review that times out lets the write post. The warning names
// RECOMMENDED_CEILING_MS as the ceiling to set (the README's AskGate section).
const WARN_DEADLINE_MS = 30_000;
const RECOMMENDED_CEILING_MS = 120_000;
// The runner gives a session_shutdown handler 2 s (SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS) and then seals
// the session, dropping any later entry: gates abandoned there get a shorter grace, and the handler
// waits at most SHUTDOWN_WAIT_MS for their records.
const SHUTDOWN_GRACE_MS = 1_000;
const SHUTDOWN_WAIT_MS = 1_500;
// What bounds the snapshots the gate keeps: a backgrounded call's snapshot stays until the session
// shuts down, and a session the host kills leaves its own behind, so each gated call that takes a
// snapshot first removes every snapshot directory older than this.
const SNAPSHOT_MAX_AGE_MS = 24 * 60 * 60 * 1000;
/**
 * The outcomes of a call the gate let through without a verdict, the gate's own failure or a stop
 * it took: no review stands behind the bytes, so the command the agent wrote runs as written.
 */
const UNREVIEWED_OUTCOMES: ReadonlyArray<GateEntry["outcome"]> = [
  "error",
  "timeout",
  "no-verdict",
  "abandoned",
  "shutdown",
];
export const GATE_CONTEXT_MAX_BYTES = 256 * 1024;
const GATE_ARGS_MAX_BYTES = 64 * 1024;
const GATE_REASON_MAX_BYTES = 2 * 1024;
const GATE_BREAKER_REVISES = 2;
const GATE_HALT_AFTER_FAILURES = 3;
export const ADVISOR_GATE_ENTRY_TYPE = "advisor-gate";
/**
 * The model roles the gate asks, in order: `askgate` when it is configured, else `default`, so a
 * session with no askgate role runs the gate on its own default model. The role used keys its
 * retry.fallbackChains chain, walked on a model error.
 */
export const GATE_ROLES = ["askgate", "default"] as const;
export type GateRole = (typeof GATE_ROLES)[number];
export const GATE_EFFORT = "high";
const REBUTTAL_CAP = 64;
const MODES = ["warn", "block"] as const;
/** The custom entry type the experiments extension records a session's arms under. */
const EXPERIMENTS_ENTRY_TYPE = "experiments";
const XD_PATH = /^xd:\/\/([a-z_]+)$/;
// Room kept for a clip marker (` … [elided N bytes]`, at most 32 bytes) and the window's elision line.
const CLIP_MARKER_BYTES = 32;
const WINDOW_MARKER_BYTES = 64;
const SEPARATOR = "\n\n";

export type Mode = (typeof MODES)[number];
type Input = Record<string, unknown>;
export type Usage = {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
};
export interface GateEntry {
  advisor: "AskGate";
  tool: string;
  path: string;
  toolCallId: string;
  /**
   * `revise`: the gate did not pass the call as sent (a revise verdict, or in block mode a call given up
   * without a verdict, or in any mode a command it cannot read or a gate that did not load). A call was
   * refused exactly when this is `revise` and either `verdictMode` is `block` or `outcome` is
   * `unreadable`, `no-model-role` or `load-failed`.
   */
  decision: "allow" | "revise";
  outcome:
    | "verdict"
    | "rebuttal"
    | "breaker"
    | "timeout"
    | "error"
    | "no-verdict"
    | "no-model-role"
    | "halted"
    | "skipped"
    | "abandoned"
    | "shutdown"
    | "unreadable"
    | "load-failed";
  verdictMode: Mode;
  reason?: string;
  rebuttal?: string;
  /** On a revise: the reason exactly as the agent received it (the one agent-facing transform's output). */
  deliveredReason?: string;
  /** The answer's tail when it held no verdict. */
  raw?: string;
  latencyMs: number;
  revisesForKey: number;
  argsDigest: string;
  promptBytes?: number;
  /**
   * On every entry that asked the model: the call's arguments exactly as the model saw them (JSON,
   * an upload's document text as `content`), cut at GATE_ARGS_MAX_BYTES as the request cut them.
   */
  reviewedText?: string;
  /** `provider/id` that answered (or, with no answer, the last one asked), on every entry that reached the model call. */
  model?: string;
  /** Each model of the chain that answered with an error before `model` was asked, with that error. */
  failedModels?: Array<{ model: string; error: string }>;
  /** The deadline this call raced: OMP_ASKGATE_TIMEOUT_MS or 90 s, clamped under the runner's handler ceiling. */
  deadlineMs?: number;
  /** Every attempt's spend, the aborted one included. */
  usage?: Usage;
}
/** A model as the gate passes it around: the fork's full Model, of which the core reads only these. */
export type GateModel = { provider: string; id: string };
export interface CompleteRequest {
  ctx: unknown;
  model: unknown;
  system: string;
  user: string;
  sessionId: string;
  signal: AbortSignal;
}
export type Completion = { text: string; error?: string; usage?: Usage };
export interface DryRunOptions {
  cwd: string;
  env: Record<string, string | undefined>;
  timeoutMs: number;
  signal?: AbortSignal;
}
/** `exitCode` is null when the run was killed. */
export interface DryRunResult {
  exitCode: number | null;
  stdout: string;
  stderr: string;
  timedOut: boolean;
}
interface Deps {
  env: Record<string, string | undefined>;
  home: string;
  now: () => number;
  /** Where the gate's notices and its bind-time warning go besides the session's UI: the host's logger. */
  logger: { warn(message: string): void };
  /** The session's envoy.json, whose `askgate.enabled` is the switch: named in the notice for an OMP_ASKGATE the gate ignores. */
  configFile: string;
  /** Appends to a file; serves OMP_ASKGATE_DUMP. */
  appendFile: (p: string, text: string) => void;
  complete: (req: CompleteRequest) => Promise<Completion>;
  /**
   * The models after `primary` in `role`'s retry.fallbackChains chain, in the order omp walks it,
   * resolved to the session's available models; empty when the chain is empty or model fallback is
   * off. Bound in the entry to omp's own chain resolution.
   */
  fallbackModels: (ctx: GateCtx, primary: GateModel, role: GateRole) => readonly GateModel[];
  /** The primary's context as its model sees it: the fork's buildSessionContext over this session's branch. */
  contextMessages: (ctx: GateCtx) => readonly Message[];
  /** The runner's ceiling for a tool_call handler: extensionHandlers.toolCallTimeoutMs, read when asked. */
  handlerCeilingMs: () => number;
  /** Runs a `dispatch` command's --dry-run script under bash; `spawnDryRun` in the entry. */
  dryRun: (script: string, opts: DryRunOptions) => Promise<DryRunResult>;
}
// Handlers of any event shape register here (`never` parameters accept every typed handler), so the
// fork's ExtensionAPI is assignable to it.
export interface Pi {
  on(event: string, handler: (event: never, ctx: never) => unknown): void;
  appendEntry(customType: string, data?: unknown): void;
}
export type ToolCallEvent = { toolName: string; toolCallId: string; input: Input };
export type ToolCallResult =
  | { block?: boolean; reason?: string; input?: Input; additionalContext?: string }
  | undefined;
export type GateCtx = {
  agent: { kind: string };
  hasUI: boolean;
  ui: { notify(message: string, type?: "info" | "warning" | "error"): void };
  getSystemPrompt(): string[];
  models: { resolve(spec: string): GateModel | undefined };
  /** The session's primary model. */
  model?: GateModel;
  sessionManager: { getSessionId(): string; getEntries(): readonly unknown[] };
  /** The session's working directory, against which a `bash` call's `cwd` resolves. */
  cwd: string;
};
/** One AgentMessage of that context, read by `role` (user, assistant, toolResult, custom, compactionSummary, …). */
export type Message = { role: string } & Record<string, unknown>;

/** The module's one object guard: transcript and entry data are untyped JSON. */
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);
const byteLength = (text: string) => Buffer.byteLength(text, "utf8");
const messageOf = (error: unknown) => (error instanceof Error ? error.message : String(error));

/** OMP_ASKGATE as a mode: `warn` or `block`, else undefined, which leaves the mode to the session. */
export function parseMode(raw: string | undefined): Mode | undefined {
  const value = raw?.trim();
  return (MODES as readonly (string | undefined)[]).includes(value) ? (value as Mode) : undefined;
}

/**
 * The mode the session's latest `experiments` entry sets: `warn` when its `askgate_block` arm is
 * off, else `block` (no such entry, or one that carries no arm, included).
 */
export function sessionMode(entries: readonly unknown[]): Mode {
  const latest = entries.findLast(
    (entry) =>
      isRecord(entry) && entry.type === "custom" && entry.customType === EXPERIMENTS_ENTRY_TYPE
  );
  const features = isRecord(latest) && isRecord(latest.data) ? latest.data.features : undefined;
  const arm = isRecord(features) ? features.askgate_block : undefined;
  return isRecord(arm) && arm.on === false ? "warn" : "block";
}

function parseTimeout(raw: string | undefined): number {
  if (raw === undefined) return GATE_TIMEOUT_MS;
  if (/^[1-9]\d*$/.test(raw.trim())) return Number(raw.trim());
  throw new Error(`OMP_ASKGATE_TIMEOUT_MS must be a positive integer, got ${JSON.stringify(raw)}`);
}

export function scopedDevice(toolName: string, input: Input): boolean {
  return (
    (SCOPED_DEVICES as readonly string[]).includes(toolName) &&
    (toolName !== SPEC_ONLY_DEVICE || input.spec !== undefined)
  );
}

// Where a `dispatch` command's own text ends: `headLine`, `literalWords` and their helpers below
// start from the rules `@legion/pi-shared/shell-command` applies (`dispatchCommandHead` there,
// which the Envoy entry uses to tell a dispatch-only call), and differ from them on purpose: the
// gate reads only the literal grammar of one call, a literal path to `dispatch` and the two
// wrappers below included, and must read exactly the text the command sends, while the plugin's
// rule only has to tell a dispatch-only call from work. So the two are separate, and a change to
// one is checked against the other. A conservative character scan, not a shell parser: whatever it
// cannot reason about is no `dispatch` command. Whitespace is bash's blanks, space and tab, never
// JavaScript's `\s`, which also takes a no-break space that bash reads as part of a word.

/** The one here-document a command's first line may end with, opened with a delimiter in single
 * or double quotes so the shell expands nothing in its body. The second group is `-` for `<<-`,
 * the only form under which bash strips leading tabs from the delimiter line. The third and fourth
 * are the delimiter, in single or in double quotes. The fifth is a `2>&1` after the opener, which
 * bash applies to the command as it would before it. */
const HEREDOC_OPENING =
  /^(.*?)[ \t]*<<(-?)[ \t]*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")([ \t]+2>&1)?[ \t]*$/d;

/** A line of nothing but blanks, which bash runs as no command. */
const BLANK_LINE = /^[ \t]*$/;

/** Inside double quotes bash still expands `$` and backticks and interprets `\`. */
const REFUSED_DOUBLE_QUOTED = "$`\\";

/** A command's first line, without its here-document opener and its blanks, and where that text
 * starts in the command, when the command is that one line, optionally fed one quoted
 * here-document on stdin; undefined for any other command. `stderr` is where a `2>&1` after the
 * opener starts and ends in the command. */
function headLine(
  command: string
): { text: string; at: number; heredoc?: true; stderr?: [number, number] } | undefined {
  if (hasControlCharacter(command)) return undefined;
  // Blank lines before the head and after the command's last line run as no command, so they are
  // dropped whole. No other line is trimmed: bash ends a here-document only at a line that is
  // exactly its delimiter (with leading tabs stripped under `<<-`), whatever blanks the line
  // carries before or after it.
  const all = command.split("\n");
  const first = all.findIndex((line) => !BLANK_LINE.test(line));
  if (first === -1) return undefined;
  const last = all.findLastIndex((line) => !BLANK_LINE.test(line));
  const lines = all.slice(first, last + 1);
  const lineAt = all.slice(0, first).reduce((sum, line) => sum + line.length + 1, 0);
  let head = lines[0] ?? "";
  let stderr: [number, number] | undefined;
  let heredoc = false;
  if (lines.length > 1) {
    heredoc = true;
    const opening = HEREDOC_OPENING.exec(head);
    if (opening === null) return undefined;
    const stripTabs = opening[2] === "-";
    const delimiter = opening[3] ?? opening[4];
    const end = lines.findIndex(
      (line, index) => index > 0 && (stripTabs ? line.replace(/^\t+/, "") : line) === delimiter
    );
    // The first line equal to the delimiter ends the here-document in the shell, so it must be
    // the last line: anything after it would run as a command.
    if (end !== lines.length - 1) return undefined;
    head = opening[1] ?? "";
    const redirect = opening.indices?.[5];
    if (redirect) stderr = [lineAt + redirect[0], lineAt + redirect[1]];
  }
  const leading = /^[ \t]*/.exec(head)?.[0].length ?? 0;
  return {
    text: head.replace(/^[ \t]+|[ \t]+$/g, ""),
    at: lineAt + leading,
    ...(heredoc ? { heredoc: true as const } : {}),
    ...(stderr ? { stderr } : {}),
  };
}

// AskGate's own additions to those rules: the program may be a literal path to `dispatch` as well
// as the bare name, which is all the plugin's rule takes, and the two wrappers agents put around a
// `dispatch` command that change neither its words nor what it sends: one leading `cd <path> && `,
// whose path is one word the shell does not expand, and one trailing `2>&1`.

/** Characters an unquoted literal word (every word of the literal grammar) may hold: none the shell
 * expands (`~`, globs, braces, `$`), interprets (`\`) or reads as an operator. */
const LITERAL_UNQUOTED = /^[A-Za-z0-9._/+@%:,=-]$/;

/** The characters that start an expansion, unquoted or in double quotes: a program word holding
 * one names a program AskGate cannot know. */
const EXPANSIONS = "$`";

/**
 * The word at `from` in `text`, up to an unquoted blank or the text's end: where it ends, its value
 * with quotes removed, and each character in it AskGate does not take as literal, in order (an
 * unterminated quote's opening one last). The word is literal when that list is empty.
 */
function scanWord(text: string, from: number): { end: number; value: string; unread: string[] } {
  let quote: '"' | "'" | undefined;
  let value = "";
  const unread: string[] = [];
  let index = from;
  for (; index < text.length; index++) {
    const char = text[index];
    if (quote === "'") {
      if (char === "'") quote = undefined;
      else value += char;
    } else if (quote === '"') {
      if (char === '"') quote = undefined;
      else if (REFUSED_DOUBLE_QUOTED.includes(char)) {
        unread.push(char);
        // A backslash takes the next character with it.
        if (char === "\\") index++;
      } else value += char;
    } else if (char === " " || char === "\t") break;
    else if (char === "'" || char === '"') quote = char;
    else if (LITERAL_UNQUOTED.test(char)) value += char;
    else {
      unread.push(char);
      if (char === "\\") index++;
    }
  }
  if (quote !== undefined) unread.push(quote);
  return { end: Math.min(index, text.length), value, unread };
}

/** The end of the one-word path after a leading `cd ` at `from`, or undefined when the word is
 * empty, an option, unterminated, or holds anything the shell would expand or treat specially. */
function cdPathEnd(text: string, from: number): number | undefined {
  const path = scanWord(text, from);
  return path.unread.length === 0 && path.value !== "" && !path.value.startsWith("-")
    ? path.end
    : undefined;
}

/** A `2>&1` after the `dispatch` command's words, with the blanks before it. */
const STDERR_TO_STDOUT = /[ \t]+2>&1$/;

/** A `dispatch` command in a `bash` command, bare or in the two wrappers above: its head, where the
 * head starts, where a leading `cd` path ends (to silence `cd` in the dry-run), where the head's
 * line ends, past a trailing `2>&1`, and where a `2>&1` after a here-document opener lies (both
 * `2>&1` forms are left out of the dry-run, whose stdout must be the CLI's JSON). */
interface DispatchHead {
  head: string;
  at: number;
  cdEnd?: number;
  end: number;
  stderr?: [number, number];
  heredoc?: true;
}

function dispatchCommandHead(command: string): DispatchHead | undefined {
  const line = headLine(command);
  if (line === undefined) return undefined;
  let text = line.text;
  let offset = 0;
  let cdEnd: number | undefined;
  const cd = /^cd[ \t]+/.exec(text);
  if (cd !== null) {
    const pathEnd = cdPathEnd(text, cd[0].length);
    const and = pathEnd === undefined ? null : /^[ \t]*&&[ \t]*/.exec(text.slice(pathEnd));
    if (pathEnd === undefined || and === null) return undefined;
    cdEnd = line.at + pathEnd;
    offset = pathEnd + and[0].length;
    text = text.slice(offset);
  }
  const suffix = STDERR_TO_STDOUT.exec(text);
  const head = suffix === null ? text : text.slice(0, suffix.index);
  // One `dispatch` command, called by name or by a literal path ending in it that is no
  // assignment, whose every word the shell takes as written.
  const program = scanWord(head, 0);
  if (
    program.unread.length > 0 ||
    program.value.split("/").at(-1) !== "dispatch" ||
    ASSIGNMENT.test(head) ||
    !literalWords(head)
  )
    return undefined;
  const at = line.at + offset;
  return {
    head,
    at,
    ...(cdEnd === undefined ? {} : { cdEnd }),
    end: at + text.length,
    ...(line.heredoc ? { heredoc: true as const } : {}),
    ...(line.stderr ? { stderr: line.stderr } : {}),
  };
}

/** Any control character but newline and tab: a `\r` before a line end would make the delimiter
 * this scan finds differ from the one bash finds (bash ends a here-document at `EOF\r`, not `EOF`). */
function hasControlCharacter(text: string): boolean {
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index);
    if ((code < 0x20 && code !== 0x0a && code !== 0x09) || code === 0x7f) return true;
  }
  return false;
}

/** Whether every blank-separated word of `text` is literal (`scanWord`): single quotes make every
 * character literal, and an unterminated quote is refused. */
function literalWords(text: string): boolean {
  for (let at = 0; at < text.length; ) {
    if (text[at] === " " || text[at] === "\t") {
      at++;
      continue;
    }
    const word = scanWord(text, at);
    if (word.unread.length > 0) return false;
    at = word.end;
  }
  return true;
}

type Word = { raw: string; value: string; start?: number; end?: number };

/**
 * Whether `name` with `flags` is a write the gate reviews: a scoped command, with no flag under
 * which it sends nothing, an `issue` only with a spec, and an `artifact` unless its words show it
 * is no markdown document (`uploadMayBeDocument`).
 */
function reviewedWrite(name: string, flags: readonly Word[]): boolean {
  if (!(SCOPED_COMMANDS as readonly string[]).includes(name)) return false;
  if (flags.some((flag) => NO_SEND_FLAGS.includes(flag.value))) return false;
  if (name === SPEC_ONLY_COMMAND) return flags.some((flag) => SPEC_FLAGS.includes(flagName(flag)));
  if (name === UPLOAD_COMMAND) return uploadMayBeDocument(flags);
  return true;
}

const flagName = (flag: Word) => flag.value.split("=", 1)[0];

/**
 * Whether an upload's words leave it possibly a markdown document: inline text, a name or path read
 * from a file, or a name or path that is not literal (an expansion, a glob), or that is markdown's.
 * Only an upload whose literal name and literal path are both no markdown's is not one. An upload
 * with no `--name` or no `--path` and no inline text is one the CLI refuses before it sends.
 */
function uploadMayBeDocument(flags: readonly Word[]): boolean {
  if (flags.some((flag) => [...INLINE_FLAGS, ...INDIRECT_FLAGS].includes(flagName(flag))))
    return true;
  const flagValue = (name: string): { value: string; literal: boolean } | undefined => {
    const at = flags.findIndex((flag) => flagName(flag) === name);
    if (at === -1) return undefined;
    const flag = flags[at];
    const eq = flag.raw.indexOf("=");
    const word =
      eq === -1
        ? flags[at + 1]
        : { raw: flag.raw.slice(eq + 1), value: flag.value.slice(flag.value.indexOf("=") + 1) };
    if (word === undefined) return undefined;
    return { value: word.value, literal: scanWord(word.raw, 0).unread.length === 0 };
  };
  const named = flagValue("--name");
  const file = flagValue("--path");
  if (named === undefined || file === undefined) return false;
  return (
    !named.literal ||
    !file.literal ||
    MARKDOWN_NAME.test(named.value) ||
    MARKDOWN_PATH.test(file.value)
  );
}

/** Whether an upload's arguments, as its dry run prints them, make a markdown document. */
function uploadIsDocument(args: Input): boolean {
  return (
    typeof args.content === "string" ||
    (typeof args.name === "string" && MARKDOWN_NAME.test(args.name)) ||
    (typeof args.path === "string" && MARKDOWN_PATH.test(args.path))
  );
}

/** A head's words as bash splits them at unquoted blanks, each as written and with its quotes
 * removed. Only for a head `dispatchCommandHead` accepted, which holds no escape or expansion. */
function headWords(head: string): Word[] {
  const words: Word[] = [];
  let word: Word | undefined;
  let quote: string | undefined;
  for (let index = 0; index < head.length; index++) {
    const char = head[index];
    if (quote === undefined && (char === " " || char === "\t")) {
      if (word) {
        word.end = index;
        words.push(word);
      }
      word = undefined;
      continue;
    }
    word ??= { raw: "", value: "", start: index };
    word.raw += char;
    if (quote === undefined && (char === "'" || char === '"')) quote = char;
    else if (char === quote) quote = undefined;
    else word.value += char;
  }
  if (word) {
    word.end = head.length;
    words.push(word);
  }
  return words;
}

/**
 * One file the command asks Dispatch to read. `span` names the complete shell word whose source
 * becomes the owned snapshot: the value word for `--field-file path`, or the whole flag for
 * `--field-file=path`.
 */
interface FileInput {
  flag: string;
  source: string;
  span: [number, number];
  equals: boolean;
}
type UnsafeFile = { flag: string; path: string; reason: string };

/** A scoped `dispatch` command and every file whose bytes it would make Dispatch read. */
export interface ScopedCommand {
  tool: string;
  label: string;
  command: string;
  dryRunScript: string;
  rebuttal?: string;
  readsFile: boolean;
  fileInputs: readonly FileInput[];
  /** A `--field-file -` needs the call's quoted here-document. */
  stdinFileFlag?: string;
  /** `--path-file` chooses another path after the gate has no way to bind it. */
  unsafeFile?: UnsafeFile;
  /** A `dispatch artifact` upload of a markdown document, reviewed against failure 10 alone. */
  upload?: true;
  /** A leading `cd` changes where relative file operands resolve. */
  changesDirectory?: true;
  /**
   * The command's own leading `cd`, if any, then `pwd -P`: run where the dry run runs, it prints
   * the physical directory the command reaches, the CLI's `process.cwd()`, against which an
   * upload's relative `path` resolves.
   */
  dirScript: string;
}

/**
 * An override among a write's flags: `--advisor-rebuttal <line>` or `--advisor-rebuttal=<line>`; a
 * value that starts with `--` is the next flag, as the CLI reads it. A blank line is no override.
 * `removed` holds the indexes of the words it took.
 */
function splitRebuttal(flags: readonly Word[]): { rebuttal?: string; removed: Set<number> } {
  let rebuttal: string | undefined;
  const removed = new Set<number>();
  for (let index = 0; index < flags.length; index++) {
    const { value } = flags[index];
    const next = flags[index + 1]?.value;
    const line =
      value === REBUTTAL_FLAG && next !== undefined && !next.startsWith("--")
        ? next
        : value.startsWith(`${REBUTTAL_FLAG}=`)
          ? value.slice(REBUTTAL_FLAG.length + 1)
          : undefined;
    if (line === undefined || !line.trim()) continue;
    rebuttal = line.trim();
    removed.add(index);
    if (value === REBUTTAL_FLAG) removed.add(++index);
  }
  return { ...(rebuttal === undefined ? {} : { rebuttal }), removed };
}

/**
 * Every file operand the CLI will open after the gate returns. `--path-file` is different: its
 * contents choose another path, which would be a new open outside this proof and is refused.
 */
function fileInputs(
  flags: readonly Word[],
  upload: boolean
): { inputs: FileInput[]; stdinFileFlag?: string; unsafeFile?: UnsafeFile } {
  const inputs: FileInput[] = [];
  let stdinFileFlag: string | undefined;
  for (let index = 0; index < flags.length; index++) {
    const word = flags[index];
    const eq = word.value.indexOf("=");
    const flag = eq === -1 ? word.value : word.value.slice(0, eq);
    if (upload && flag === "--path-file") {
      const source = eq === -1 ? flags[index + 1]?.value : word.value.slice(eq + 1);
      return {
        inputs,
        ...(stdinFileFlag === undefined ? {} : { stdinFileFlag }),
        unsafeFile: {
          flag,
          path: source ?? "",
          reason: "is an indirect path that AskGate cannot capture before Dispatch opens it",
        },
      };
    }
    const named = flag.startsWith("--") && flag.endsWith("-file");
    const artifactPath = upload && flag === "--path";
    if (!named && !artifactPath) continue;
    const sourceWord =
      eq === -1
        ? flags[index + 1]
        : {
            raw: word.raw.slice(word.raw.indexOf("=") + 1),
            value: word.value.slice(eq + 1),
            start: word.start === undefined ? undefined : word.start,
            end: word.end,
          };
    if (sourceWord === undefined) continue;
    const source = sourceWord.value;
    if (source === "-") {
      stdinFileFlag ??= flag;
      continue;
    }
    const span =
      eq === -1
        ? sourceWord.start === undefined || sourceWord.end === undefined
          ? undefined
          : ([sourceWord.start, sourceWord.end] as [number, number])
        : word.start === undefined || word.end === undefined
          ? undefined
          : ([word.start, word.end] as [number, number]);
    if (span === undefined) continue;
    inputs.push({ flag, source, span, equals: eq !== -1 });
  }
  return { inputs, ...(stdinFileFlag === undefined ? {} : { stdinFileFlag }) };
}

/**
 * The `bash` command as a scoped `dispatch` call, or undefined for any other command. Only the
 * literal grammar of one call is read (`dispatchCommandHead`). A call in any other form is no
 * scoped call; the gate then refuses it when `unreadableDispatchWrite` finds a write in it.
 */
export function scopedCommand(command: unknown): ScopedCommand | undefined {
  if (typeof command !== "string") return undefined;
  const found = dispatchCommandHead(command);
  if (found === undefined) return undefined;
  const { head, at, cdEnd, end, heredoc, stderr } = found;
  const [program, name, ...flags] = headWords(head);
  if (name === undefined || !reviewedWrite(name.value, flags)) return undefined;
  const { rebuttal, removed } = splitRebuttal(flags);
  const kept = flags.filter((_, index) => !removed.has(index));
  const words = [program, name, ...kept];
  const positioned: Word[] = [];
  let cursor = at;
  for (const word of words) {
    positioned.push({ ...word, start: cursor, end: cursor + word.raw.length });
    cursor += word.raw.length + 1;
  }
  const commandHead = words.map((word) => word.raw).join(" ");
  const upload = name.value === UPLOAD_COMMAND;
  const files = fileInputs(positioned.slice(2), upload);
  const unsafeFile =
    files.unsafeFile ??
    (files.stdinFileFlag !== undefined && !heredoc
      ? {
          flag: files.stdinFileFlag,
          path: "-",
          reason: "reads standard input that is not a quoted here-document",
        }
      : undefined);
  // The command runs as written, its wrappers included. The dry-run keeps the `cd`, with its output
  // silenced (bash prints the directory when CDPATH found it), so it runs where the command will;
  // it drops the `2>&1`, before or after a here-document opener, so the CLI's JSON is all its
  // stdout holds.
  const before =
    cdEnd === undefined
      ? command.slice(0, at)
      : `${command.slice(0, cdEnd)} >/dev/null${command.slice(cdEnd, at)}`;
  const after =
    stderr === undefined
      ? command.slice(end)
      : `${command.slice(end, stderr[0])}${command.slice(stderr[1])}`;
  return {
    tool: `dispatch_${name.value.replaceAll("-", "_")}`,
    label: `dispatch ${name.value}`,
    command: `${command.slice(0, at)}${commandHead}${command.slice(at + head.length)}`,
    dryRunScript: `${before}${[program.raw, name.raw, "--dry-run", ...kept.map((word) => word.raw)].join(" ")}${after}`,
    ...(rebuttal === undefined ? {} : { rebuttal }),
    readsFile: files.inputs.length > 0,
    fileInputs: files.inputs,
    ...(files.stdinFileFlag === undefined ? {} : { stdinFileFlag: files.stdinFileFlag }),
    ...(unsafeFile === undefined ? {} : { unsafeFile }),
    ...(upload ? { upload: true as const } : {}),
    ...(cdEnd === undefined ? {} : { changesDirectory: true as const }),
    dirScript: cdEnd === undefined ? "pwd -P" : `${command.slice(0, cdEnd)} >/dev/null && pwd -P`,
  };
}

/**
 * Words that may stand before a command's name without being it: assignments are matched apart. A
 * set, not a record: `then` as an object key would make the table a thenable.
 */
const COMMAND_PREFIXES: ReadonlySet<string> = new Set([
  "!",
  "{",
  "then",
  "do",
  "else",
  "elif",
  "if",
  "while",
  "until",
  "time",
  "command",
  "exec",
  "nohup",
  "env",
  "timeout",
]);
/** The prefixes that are programs of their own, also called by a path (`/usr/bin/env`). */
const PROGRAM_PREFIXES: Record<string, true> = { env: true, nohup: true, timeout: true };
/**
 * How a prefix that runs the command after it reads its own options, as its parser (getopt, or
 * bash's for a builtin) reads them: up to `--`, which it consumes, or the first word that is no
 * option. `values` are the short options that take a value (the rest of the word, else the next
 * word) and `longValues` the long ones (after `=`, else the next word). An option in `runsNothing`
 * makes the prefix run nothing (`command -v` only prints), `split` names the options whose value
 * holds the command's own words (`env -S`), `dash` lets a lone `-` be an option (`env -`, as
 * `env -i`), and `operands` counts the words after the options that are not the command
 * (`timeout`'s duration). Every other word that starts with `-` there is an option with no value.
 */
interface PrefixOptions {
  values?: string;
  longValues?: readonly string[];
  runsNothing?: string;
  split?: readonly string[];
  dash?: true;
  operands?: number;
}
const PREFIX_OPTIONS: Record<string, PrefixOptions> = {
  command: { runsNothing: "vV" },
  exec: { values: "a" },
  nohup: {},
  time: {},
  env: {
    values: "uCSa",
    longValues: ["--unset", "--chdir", "--split-string", "--argv0"],
    split: ["-S", "--split-string"],
    dash: true,
  },
  timeout: { values: "sk", longValues: ["--signal", "--kill-after"], operands: 1 },
};
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/;

/**
 * Where the command a simple command runs starts, past its assignments and prefixes and each
 * prefix's options; undefined when a prefix runs nothing. `words` comes back with the words an
 * `env -S` value splits into put where the command starts.
 */
function commandStart(segment: readonly Word[]): { words: Word[]; at: number } | undefined {
  let words = [...segment];
  let at = 0;
  while (at < words.length) {
    const prefix = words[at];
    if (ASSIGNMENT.test(prefix.raw)) {
      at++;
      continue;
    }
    const base = prefix.value.split("/").at(-1) ?? "";
    const name = COMMAND_PREFIXES.has(prefix.value)
      ? prefix.value
      : Object.hasOwn(PROGRAM_PREFIXES, base)
        ? base
        : undefined;
    if (name === undefined) break;
    at++;
    if (!Object.hasOwn(PREFIX_OPTIONS, name)) continue;
    const options = PREFIX_OPTIONS[name];
    const split: Word[] = [];
    while (at < words.length) {
      const word = words[at].value;
      if (word === "--") {
        at++;
        break;
      }
      if (!word.startsWith("-") || (word === "-" && !options.dash)) break;
      at++;
      let flag = word;
      let value: string | undefined;
      if (word.startsWith("--")) {
        const eq = word.indexOf("=");
        if (eq !== -1) {
          flag = word.slice(0, eq);
          value = word.slice(eq + 1);
        } else if (options.longValues?.includes(word)) value = words[at++]?.value;
      } else {
        for (let index = 1; index < word.length; index++) {
          const char = word[index];
          if (options.runsNothing?.includes(char)) return undefined;
          if (!options.values?.includes(char)) continue;
          flag = `-${char}`;
          value = index + 1 < word.length ? word.slice(index + 1) : words[at++]?.value;
          break;
        }
      }
      if (value !== undefined && options.split?.includes(flag))
        split.push(...commandWords(value).flat());
    }
    at += options.operands ?? 0;
    words = [...words.slice(0, at), ...split, ...words.slice(at)];
  }
  return { words, at };
}

/** An open subshell, or an open command substitution with the command and word it sits in, which
 * go on at its end. */
type Open =
  | { kind: "(" }
  | { kind: "$(" | "`"; words: Word[]; start: number; value: string; quote: '"' | undefined };

/**
 * The words of each simple command in a `bash` command, each as written and with its quotes
 * removed, as far as a character scan can tell them apart: a new command starts at an unquoted
 * newline, `;`, `&`, `|`, `(` or `)` (a `&` inside `2>&1` or `&>` is a redirect, not a separator).
 * A command substitution, `$(…)` or backticks, quoted or not, holds commands of its own and is part
 * of the word it sits in, which goes on after it. Single and double quotes and backslashes are
 * honoured, a comment runs to the line's end, and a here-document body is skipped, so text a
 * command only writes (`cat > f <<EOF … EOF`, `echo "dispatch ask"`) is never a command. Not a
 * shell parser: a `)` in a `case` pattern ends a substitution early.
 */
function commandWords(command: string): Word[][] {
  const segments: Word[][] = [];
  const pending: Array<{ delimiter: string; stripTabs: boolean }> = [];
  const open: Open[] = [];
  let words: Word[] = [];
  /** The word being read: where it starts in `command`, and its value so far; undefined between words. */
  let start = 0;
  let value: string | undefined;
  let quote: '"' | "'" | undefined;
  const add = (at: number, text: string) => {
    if (value === undefined) {
      start = at;
      value = "";
    }
    value += text;
  };
  const endWord = (at: number) => {
    if (value !== undefined) words.push({ raw: command.slice(start, at), value });
    value = undefined;
  };
  const endSegment = (at: number) => {
    endWord(at);
    if (words.length > 0) segments.push(words);
    words = [];
  };
  /** A command substitution opens at `at`: the command and word it sits in wait for its end. */
  const openSubstitution = (kind: "$(" | "`", at: number) => {
    add(at, "");
    open.push({ kind, words, start, value: value ?? "", quote: quote === '"' ? quote : undefined });
    words = [];
    value = undefined;
    quote = undefined;
  };
  /** The innermost command substitution, `outer`, ends at `at`: its last command ends and the word around it goes on. */
  const closeSubstitution = (at: number, outer: Exclude<Open, { kind: "(" }>) => {
    endSegment(at);
    open.pop();
    ({ words, start, value, quote } = outer);
  };
  /** Past the bodies of the here-documents opened on the line that ended just before `from`. */
  const skipBodies = (from: number): number => {
    let at = from;
    for (const { delimiter, stripTabs } of pending.splice(0)) {
      while (at < command.length) {
        const end = command.indexOf("\n", at);
        const line = command.slice(at, end === -1 ? command.length : end);
        at = end === -1 ? command.length : end + 1;
        if ((stripTabs ? line.replace(/^\t+/, "") : line) === delimiter) break;
      }
    }
    return at;
  };
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    const top = open.at(-1);
    if (quote === "'") {
      if (c === "'") quote = undefined;
      else value += c;
      continue;
    }
    if (quote === '"') {
      if (c === '"') quote = undefined;
      else if (c === "\\" && '$`"\\\n'.includes(command[i + 1] ?? "")) {
        if (command[i + 1] !== "\n") value += command[i + 1];
        i++;
      } else if (c === "$" && command[i + 1] === "(") {
        openSubstitution("$(", i);
        i++;
      } else if (c === "`") openSubstitution("`", i);
      else value += c;
      continue;
    }
    if (c === "\\") {
      if (command[i + 1] !== "\n") add(i, command[i + 1] ?? "");
      i++;
    } else if (c === "'" || c === '"') {
      add(i, "");
      quote = c;
    } else if (c === "#" && value === undefined) {
      const end = command.indexOf("\n", i);
      i = (end === -1 ? command.length : end) - 1;
    } else if (c === "\n") {
      endSegment(i);
      i = skipBodies(i + 1) - 1;
    } else if (c === " " || c === "\t") {
      endWord(i);
    } else if (c === "<" && command[i + 1] === "<" && command[i + 2] !== "<") {
      // A here-document opener: its delimiter, quotes removed, ends the body that starts on the next line.
      endWord(i);
      i += 2;
      const stripTabs = command[i] === "-";
      if (stripTabs) i++;
      while (command[i] === " " || command[i] === "\t") i++;
      let delimiter = "";
      for (; i < command.length && !" \t\n;&|<>()".includes(command[i]); i++)
        if (command[i] !== "'" && command[i] !== '"' && command[i] !== "\\")
          delimiter += command[i];
      pending.push({ delimiter, stripTabs });
      i--;
    } else if (c === "$" && command[i + 1] === "(") {
      openSubstitution("$(", i);
      i++;
    } else if (c === "`") {
      if (top?.kind === "`") closeSubstitution(i, top);
      else openSubstitution("`", i);
    } else if (c === ")" && top?.kind === "$(") {
      closeSubstitution(i, top);
    } else if (
      c === "&" &&
      (command[i - 1] === ">" || command[i - 1] === "<" || command[i + 1] === ">")
    ) {
      endWord(i);
    } else if (c === "(") {
      endSegment(i);
      open.push({ kind: "(" });
    } else if (c === ")") {
      if (top?.kind === "(") open.pop();
      endSegment(i);
    } else if (";&|".includes(c)) {
      endSegment(i);
    } else if (c === "<" || c === ">") {
      endWord(i);
    } else {
      add(i, c);
    }
  }
  endSegment(command.length);
  return segments;
}

/** A scoped `dispatch` write the gate cannot review: the tool it calls, the command's name and
 * which word AskGate could not read, with the characters in it AskGate does not take as literal. */
export interface UnreadableCommand {
  tool: string;
  label: string;
  /** The word that runs the write is not literal: whether it calls `dispatch` by name or by path. */
  program?: { namesDispatch: boolean; unread: string[] };
  /** The word after `dispatch` is not literal, so the subcommand it names is unknown. */
  subcommand?: { unread: string[] };
  /** The write is in the call's text, where another command (`bash -c`, `eval`, a function) runs it. */
  wrapped?: true;
  /** A file the call would make Dispatch read cannot be captured safely. */
  file?: { flag: string; path: string; reason: string };
}

/**
 * `dispatch`, by name or by a path ending in it, with a gated write subcommand after it. The
 * pattern is applied to the whole command's text, quotes and here-document bodies included: the
 * text a `bash -c '…'`, `sh -c`, `eval`, `xargs`, a function or a variable would run is one word
 * to the shell, so no scan of the call's own commands finds the write inside it.
 */
const DISPATCH_IN_TEXT = new RegExp(
  `(?:^|[^A-Za-z0-9_.-])(?:[^\\s'";|&<>()]*/)?dispatch[ \\t]+(${SCOPED_COMMANDS.join("|")})(?![A-Za-z0-9_-])`,
  "g"
);

/**
 * A gated write in the command's own text, with the flags that follow it to the end of its line,
 * which say whether it would send anything (`--dry-run`, `--help`, an `issue` with no spec, an
 * upload that is no markdown document).
 */
function textDispatchWrite(command: string): { tool: string; label: string } | undefined {
  for (const match of command.matchAll(DISPATCH_IN_TEXT)) {
    const name = match[1];
    const from = (match.index ?? 0) + match[0].length;
    const lineEnd = command.indexOf("\n", from);
    const flags = commandWords(command.slice(from, lineEnd === -1 ? command.length : lineEnd))[0];
    if (!reviewedWrite(name, flags ?? [])) continue;
    return { tool: `dispatch_${name.replaceAll("-", "_")}`, label: `dispatch ${name}` };
  }
  return undefined;
}

/**
 * A scoped `dispatch` write in a `bash` command AskGate does not read. Three scans, in order: a
 * write at a command's position (after any assignments and prefixes such as `env -u HOME --` or
 * `timeout -s KILL 30`, each prefix's options and its `--` read as its own parser reads them:
 * `commandStart`), or a scoped subcommand run by a program an expansion names (`$D comment …`,
 * `$(command -v dispatch) ask …`), which may be `dispatch`; a `dispatch` whose own subcommand an
 * expansion names (`dispatch $SUB …`), which may be a write; and `dispatch` beside a write
 * subcommand anywhere in the call's text (`textDispatchWrite`), which is the write another
 * command would run. One carrying `--dry-run` or `--help`, or a `dispatch issue` without
 * `--spec`/`--spec-file`, sends nothing for review and is not one. Only for a command
 * `scopedCommand` could not read.
 */
export function unreadableDispatchWrite(command: unknown): UnreadableCommand | undefined {
  // No prefilter on the raw text: bash removes a backslash or empty quotes inside a word
  // (`d\ispatch`, `di""spatch`), so a write need not hold `dispatch` as written. `commandWords`
  // removes them as bash does and names the program.
  if (typeof command !== "string") return undefined;
  let unknownSubcommand: UnreadableCommand | undefined;
  for (const segment of commandWords(command)) {
    const start = commandStart(segment);
    if (start === undefined) continue;
    const [program, name, ...flags] = start.words.slice(start.at);
    if (program === undefined || name === undefined) continue;
    const named = program.value.split("/").at(-1) === "dispatch";
    const unread = [...new Set(scanWord(program.raw, 0).unread)];
    const expansions = unread.filter((char) => EXPANSIONS.includes(char));
    if (!named && expansions.length === 0) continue;
    if (!reviewedWrite(name.value, flags)) {
      // `dispatch $SUB …` may be any subcommand, a write among them.
      const subcommand = [...new Set(scanWord(name.raw, 0).unread)].filter((char) =>
        EXPANSIONS.includes(char)
      );
      if (named && subcommand.length > 0)
        unknownSubcommand ??= {
          tool: "dispatch_unknown",
          label: "dispatch",
          subcommand: { unread: subcommand },
        };
      continue;
    }
    const found = {
      tool: `dispatch_${name.value.replaceAll("-", "_")}`,
      label: `dispatch ${name.value}`,
    };
    return unread.length === 0
      ? found
      : { ...found, program: { namesDispatch: named, unread: named ? unread : expansions } };
  }
  const wrapped = textDispatchWrite(command);
  return wrapped === undefined ? unknownSubcommand : { ...wrapped, wrapped: true as const };
}

/** Runs `script` under bash in a process group of its own, which is killed at the limit or on the signal. */
export async function spawnDryRun(
  script: string,
  { cwd, env, timeoutMs, signal }: DryRunOptions
): Promise<DryRunResult> {
  const child = Bun.spawn(["bash", "-c", script], {
    cwd,
    env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    detached: true,
  });
  let timedOut = false;
  const kill = () => {
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      // The group already exited.
    }
  };
  const timer = setTimeout(() => {
    timedOut = true;
    kill();
  }, timeoutMs);
  signal?.addEventListener("abort", kill, { once: true });
  try {
    const [stdout, stderr] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    await child.exited;
    return { exitCode: child.exitCode, stdout, stderr, timedOut };
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", kill);
  }
}

interface CapturedFile {
  input: FileInput;
  path: string;
}
interface SnapshotSet {
  directory: string;
  files: CapturedFile[];
}

/** The one shell quoting transform for absolute snapshot paths inserted into a command. */
function quoteSnapshotPath(value: string): string {
  const escaped = value.replaceAll("'", "'\\''");
  return `'${escaped}'`;
}

/** Rewrites only the operands Dispatch opens, leaving every other command word byte-for-byte intact. */
function commandWithSnapshots(command: string, files: readonly CapturedFile[]): string {
  let cursor = 0;
  let out = "";
  for (const captured of [...files].sort(
    (left, right) => left.input.span[0] - right.input.span[0]
  )) {
    const [from, to] = captured.input.span;
    if (from < cursor) throw new Error("AskGate found overlapping file operands");
    out += command.slice(cursor, from);
    const snapshot = quoteSnapshotPath(captured.path);
    out += captured.input.equals ? `${captured.input.flag}=${snapshot}` : snapshot;
    cursor = to;
  }
  return out + command.slice(cursor);
}

/**
 * A path that names a file descriptor of whichever process opens it: AskGate would resolve it in
 * Oh My Pi's own process and the command in its shell, so the two read different files. The gate
 * refuses it by name, before any `stat`, since with a regular file on that descriptor the `stat`
 * passes and the gate would review and post Oh My Pi's own file (census, contract 2).
 */
const PROCESS_FD_PATH = /^\/dev\/fd\/|^\/dev\/(stdin|stdout|stderr)$|^\/proc\/(self|\d+)\/fd\//;

/**
 * Copies one source into an exclusive owner-only snapshot, reading only a regular file. `stat`
 * refuses a FIFO, a device, a directory or a link to one before the open, and `O_NONBLOCK` with a
 * second `fstat` on the open descriptor refuses one swapped in between, before the first read.
 */
function captureSnapshot(source: string, directory: string, index: number): string {
  if (!fs.statSync(source).isFile()) throw new Error("is not a regular file AskGate can read");
  const target = path.join(directory, `${index}${path.extname(source)}`);
  const input = fs.openSync(source, fs.constants.O_RDONLY | fs.constants.O_NONBLOCK);
  let output: number | undefined;
  let complete = false;
  try {
    if (!fs.fstatSync(input).isFile()) throw new Error("is not a regular file AskGate can read");
    output = fs.openSync(
      target,
      fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW,
      0o600
    );
    fs.fchmodSync(output, 0o600);
    const buffer = Buffer.allocUnsafe(64 * 1024);
    for (;;) {
      const read = fs.readSync(input, buffer);
      if (read === 0) break;
      for (let written = 0; written < read; )
        written += fs.writeSync(output, buffer, written, read - written);
    }
    complete = true;
    return target;
  } finally {
    if (output !== undefined) fs.closeSync(output);
    fs.closeSync(input);
    if (!complete) fs.rmSync(target, { force: true });
  }
}

function removeSnapshots(snapshot: SnapshotSet): void {
  for (const captured of snapshot.files) fs.rmSync(captured.path, { force: true });
  fs.rmSync(snapshot.directory, { recursive: true, force: true });
}

/** A snapshot directory of this gate: `askgate-` and the six characters `mkdtemp` adds. */
const SNAPSHOT_DIRECTORY = /^askgate-[A-Za-z0-9]{6}$/;

/**
 * Removes the gate's own snapshot directories under `root` that are older than
 * SNAPSHOT_MAX_AGE_MS, at each call that takes one. A backgrounded call's snapshot is kept until
 * the session shuts down, and a session the host kills leaves its own behind, so this is what
 * bounds the files the gate owns. Each directory is 0700 and holds only the gate's own copies.
 */
function sweepSnapshots(root: string, now: number): void {
  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(root, { withFileTypes: true });
  } catch {
    return;
  }
  for (const entry of entries) {
    if (!entry.isDirectory() || !SNAPSHOT_DIRECTORY.test(entry.name)) continue;
    const directory = path.join(root, entry.name);
    try {
      if (now - fs.statSync(directory).mtimeMs <= SNAPSHOT_MAX_AGE_MS) continue;
      fs.rmSync(directory, { recursive: true, force: true });
    } catch {
      // Another session swept it first, or it is not this user's to remove.
    }
  }
}

/**
 * Whether a `bash` result only reports a backgrounded job's start, so the command it names has not
 * run yet: `async: true`, or `bash.autoBackground` past its threshold, returns
 * `details.async.state === "running"` and the shell runs the command after that.
 */
function startedInBackground(details: unknown): boolean {
  return isRecord(details) && isRecord(details.async) && details.async.state === "running";
}
function breakerKey(toolName: string, input: Input): string {
  let key = toolName;
  for (const k of BREAKER_KEYS) if (input[k] !== undefined) key += `\u0000${k}=${String(input[k])}`;
  return key;
}

/** Splits a top-level REBUTTAL_KEY off a device's JSON arguments; anything else comes back unchanged. */
function extractRebuttal(content: string): { rebuttal?: string; content: string } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(content);
  } catch {
    return { content };
  }
  if (!isRecord(parsed) || !(REBUTTAL_KEY in parsed)) return { content };
  const { [REBUTTAL_KEY]: value, ...rest } = parsed;
  if (typeof value !== "string" || !value.trim()) return { content };
  return { rebuttal: value.trim(), content: JSON.stringify(rest) };
}

/** `text` cut to at most `max` UTF-8 bytes, marker included. */
function clipBytes(text: string, max: number): string {
  const size = byteLength(text);
  if (size <= max) return text;
  // A cut inside a multi-byte character decodes to U+FFFD; drop it.
  const head = Buffer.from(text, "utf8")
    .subarray(0, Math.max(0, max - CLIP_MARKER_BYTES))
    .toString("utf8")
    .replace(/\uFFFD$/, "");
  return `${head} … [elided ${size - byteLength(head)} bytes]`;
}

function renderArgs(input: Input): string {
  return clipBytes(JSON.stringify(input, null, 2), GATE_ARGS_MAX_BYTES);
}

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const parts: string[] = [];
  for (const part of content) {
    if (typeof part === "string") parts.push(part);
    else if (isRecord(part) && part.type === "text" && typeof part.text === "string")
      parts.push(part.text);
    else if (isRecord(part) && part.type === "image") parts.push("[image]");
  }
  return parts.join("\n");
}

const escapeXml = (text: string) =>
  text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
/** Untrusted text escaped, then cut to `max` bytes: no transcript text can close or open a tag. */
const escaped = (text: string, max = Number.POSITIVE_INFINITY) => clipBytes(escapeXml(text), max);

function renderAssistant(content: unknown): string {
  const parts: string[] = [];
  for (const block of Array.isArray(content) ? content : []) {
    if (!isRecord(block)) continue;
    if (block.type === "text" && typeof block.text === "string" && block.text.trim())
      parts.push(escaped(block.text));
    else if (
      block.type === "thinking" &&
      typeof block.thinking === "string" &&
      block.thinking.trim()
    )
      parts.push(`(thinking) ${escaped(block.thinking, 1024)}`);
    else if (block.type === "toolCall") {
      const args =
        typeof block.arguments === "string"
          ? block.arguments
          : JSON.stringify(block.arguments ?? {});
      parts.push(`→ ${escaped(String(block.name))}(${escaped(args, 2048)})`);
    }
  }
  return parts.length > 0 ? `**agent**: ${parts.join("\n")}` : "";
}

function renderMessage(message: Message): string {
  switch (message.role) {
    case "user":
      return `**user**: ${escaped(textOf(message.content))}`;
    case "assistant":
      return renderAssistant(message.content);
    case "toolResult":
      return `⇒ ${escaped(String(message.toolName))}${message.isError ? " (error)" : ""}: ${escaped(textOf(message.content), 8 * 1024)}`;
    case "bashExecution":
      return message.excludeFromContext
        ? ""
        : `→ user-bash ${escaped(String(message.command))} ⇒ ${escaped(String(message.output ?? ""), 8 * 1024)}`;
    case "pythonExecution":
      return message.excludeFromContext
        ? ""
        : `→ user-python ${escaped(String(message.code))} ⇒ ${escaped(String(message.output ?? ""), 8 * 1024)}`;
    case "developer":
      return `**system**: ${escaped(textOf(message.content), 2 * 1024)}`;
    case "custom":
    case "hookMessage": {
      const text = textOf(message.content);
      // A Dispatch answer from a human arrives this way: in full, even when hidden.
      if (message.customType === "envoy-message")
        return `<primary-message kind="envoy-message">\n${escaped(text)}\n</primary-message>`;
      if (message.display === false && message.customType !== "advisor") return "";
      return `[${escaped(String(message.customType))}] ${escaped(text.replace(/\s+/g, " ").trim().slice(0, 120))}`;
    }
    case "compactionSummary":
      return `[compaction] ${escaped(String(message.summary ?? ""), 8 * 1024)}`;
    case "branchSummary":
      return `[branch] ${escaped(String(message.summary ?? ""), 2 * 1024)}`;
    default:
      return "";
  }
}

/** The newest whole messages of the primary's context that fit in `capBytes`, oldest first, behind an elision line when older ones were dropped. */
export function renderTranscript(messages: readonly Message[], capBytes: number): string {
  const rendered = messages.map(renderMessage).filter((text) => text !== "");
  const sizes = rendered.map(byteLength);
  // Bytes of rendered[from..to) joined by SEPARATOR.
  const span = (from: number, to: number) =>
    sizes.slice(from, to).reduce((sum, size) => sum + size, 0) +
    SEPARATOR.length * Math.max(0, to - from - 1);
  if (span(0, rendered.length) <= capBytes) return rendered.join(SEPARATOR);
  const budget = capBytes - WINDOW_MARKER_BYTES;
  let first = rendered.length;
  for (let used = 0; first > 0; first--) {
    const next = used + sizes[first - 1] + (first < rendered.length ? SEPARATOR.length : 0);
    if (next > budget) break;
    used = next;
  }
  if (first === rendered.length) {
    // The newest message alone overflows the window: keep its head.
    rendered[rendered.length - 1] = clipBytes(rendered[rendered.length - 1], budget);
    first = rendered.length - 1;
  }
  return `… [elided ${span(0, first)} bytes of earlier transcript]${SEPARATOR}${rendered.slice(first).join(SEPARATOR)}`;
}

/** The verdict on the answer's last line (a closing code fence after it is ignored); a revise needs a non-empty reason. */
export function parseVerdict(
  text: string
): { decision: "allow" | "revise"; reason?: string } | undefined {
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  while (lines.length > 0 && lines[lines.length - 1].startsWith("```")) lines.pop();
  let parsed: unknown;
  try {
    parsed = JSON.parse(lines.at(-1) ?? "");
  } catch {
    return undefined;
  }
  if (!isRecord(parsed)) return undefined;
  if (parsed.decision === "allow") return { decision: "allow" };
  if (parsed.decision === "revise" && typeof parsed.reason === "string" && parsed.reason.trim())
    return { decision: "revise", reason: parsed.reason.trim() };
  return undefined;
}

/** Revise reasons per target since its last allowed call. */
class Breaker {
  readonly #reasons = new Map<string, string[]>();
  revised(key: string, reason: string): void {
    const reasons = this.#reasons.get(key) ?? [];
    reasons.push(reason);
    this.#reasons.set(key, reasons);
  }
  tripped(key: string): boolean {
    return this.reasons(key).length >= GATE_BREAKER_REVISES;
  }
  reset(key: string): void {
    this.#reasons.delete(key);
  }
  clear(): void {
    this.#reasons.clear();
  }
  reasons(key: string): readonly string[] {
    return this.#reasons.get(key) ?? [];
  }
}

/** `{{name}}` placeholders replaced in one pass, values pasted verbatim (never re-scanned, never read as `$` patterns); a placeholder with no value throws. */
export function renderTemplate(template: string, values: Record<string, string>): string {
  return template.replace(/\{\{(\w+)\}\}/g, (_, name: string) => {
    if (!Object.hasOwn(values, name))
      throw new Error(`AskGate prompt template names {{${name}}}, which has no value`);
    return values[name];
  });
}

/**
 * The gate's user message: the agent's system prompt and transcript as escaped data, then the call,
 * its arguments as `renderArgs` rendered them. An upload of a markdown document carries
 * askgate-upload.md, which limits the review to failure 10.
 */
function renderRequest(
  primarySystemPrompt: string,
  transcript: string,
  tool: string,
  args: string,
  priorReasons: readonly string[],
  upload: boolean
): string {
  const prior =
    priorReasons.length > 0
      ? // Each reason is model text inside the governing request: escaped, and every whitespace run
        // (any line break included: CR, VT, FF, U+0085, U+2028, U+2029) folded to one space, so none
        // can start a line of its own, such as a forged `### Gate request`.
        `You answered revise ${priorReasons.length} time(s) for this target since its last allowed call:\n${priorReasons.map((r) => `- ${escapeXml(r.replace(/[\s\u0085]+/g, " "))}`).join("\n")}`
      : "";
  return renderTemplate(requestTemplate, {
    primarySystemPrompt: escapeXml(primarySystemPrompt),
    transcript,
    tool,
    args,
    uploadRule: upload ? `${uploadRule.trim()}\n` : "",
    priorReasons: prior,
  });
}

/**
 * The one transform for text handed to the agent (a model's reason, the agent's own rebuttal): verbatim,
 * except that a `<` which could start a tag (before a letter, `/`, `!` or `?`) becomes `&lt;`, then capped
 * at 2 KiB. The two defects are disjoint. A frame closed or a harness tag opened needs a tag-opening `<`,
 * and this rule stops it. Full escaping also turned `&&`, `->` and a `> ` quote line into entities, so a fix
 * sent back verbatim posted `&gt;` and a check for the quote failed; those now pass through unchanged. Text
 * rendered into the gate's own prompt is escaped in full instead (`escaped`). Nothing between here and
 * delivery decodes entities: warn joins the text into a developer message as-is, block's Error message
 * becomes the tool result as-is.
 */
const agentFacing = (text: string) =>
  clipBytes(text.replace(/<(?=[A-Za-z/!?])/g, "&lt;"), GATE_REASON_MAX_BYTES);
// What the agent reads after each gated call: silence only for an allow or a fail-open, since a
// pass it cannot see is indistinguishable from a gate that broke.
const SECOND_REVISE =
  "This is the second revise on this target: a third unchanged resend is sent without review and recorded.";
/** How the agent overrides a revise on each surface: a key in the device JSON, or a flag on the command. */
type Override = { name: string; how: string; what: string; note?: string };
const DEVICE_OVERRIDE: Override = {
  name: REBUTTAL_KEY,
  how: `"${REBUTTAL_KEY}": "<one line>" added to the JSON`,
  what: "field",
};
const COMMAND_OVERRIDE: Override = {
  name: REBUTTAL_FLAG,
  how: `${REBUTTAL_FLAG} '<one line>' added to its flags`,
  what: "flag",
};
/** Oh My Pi runs the tool_call hooks of a whole batch before any call of it, so the dry run reads a
 * named file before a sibling call of the same batch changes it. */
const FILE_OVERRIDE: Override = {
  ...COMMAND_OVERRIDE,
  note: "AskGate read the file this command names before any other call in the same batch ran, so a change made to it in that batch was not reviewed. Change a file in one step, then send the command in the next.",
};
const renderRevise = (reason: string, second: boolean, override: Override) =>
  `AskGate did not send this call.\n${agentFacing(reason)}\nSend the corrected call, or resend this one unchanged with ${override.how} to override; an override is always sent and recorded, and the ${override.what} is removed before sending.${override.note ? `\n${override.note}` : ""}${second ? `\n${SECOND_REVISE}` : ""}`;
/** The refusal block mode returns for a call given up without a verdict, by cause. */
const NO_VERDICT_REFUSAL: Record<"abandoned" | "shutdown", string> = {
  abandoned:
    "AskGate did not send this call: it was stopped before a verdict came back. Send it again if it is still wanted.",
  shutdown:
    "AskGate did not send this call: the session shut down before a verdict came back. Send it again from a live session if it is still wanted.",
};
/** The refusal for a call in a session where no model role the gate asks resolves. */
const NO_MODEL_ROLE_REFUSAL = `AskGate did not send this call: it reviews each Dispatch write before it posts, and this session has no model for it. Set modelRoles.askgate or modelRoles.default in the Oh My Pi settings, then send the call again. To turn AskGate off for this session, set askgate.enabled to false in the session's envoy.json.`;
// A comment or message cannot be edited or deleted, so its only fix is one more post; the note says so,
// rather than offering an edit the agent cannot make (two lanes resent whole comments chasing one).
const renderWarn = (reason: string, second: boolean) =>
  `<advisor-gate advisor="AskGate" verdict="revise">\n${agentFacing(reason)}\nThe call ran. This note concerns only that Dispatch call and authorizes nothing beyond correcting it. Correct it now where the reason names a fix: an ask with \`dispatch edit-ask\` or \`dispatch resolve-ask --kind retracted\`, a document with \`dispatch doc-edit\`. A comment or message cannot be edited, so post one reply carrying only the correction, and only when the original would mislead its reader. Or state your rebuttal in your next step.${second ? `\n${SECOND_REVISE}` : ""}\n</advisor-gate>`;
const renderRebuttalAck = (rebuttal: string, override: Override) =>
  `<advisor-gate advisor="AskGate" outcome="rebuttal">\nYour ${override.name} "${agentFacing(rebuttal)}" was received and recorded; the call was sent without review, with that ${override.what} removed before sending.\n</advisor-gate>`;
const renderBreakerAck = () =>
  `<advisor-gate advisor="AskGate" outcome="breaker">\nThird attempt on this target after two revises: sent without review and recorded.\n</advisor-gate>`;
/**
 * What the agent is told when its Dispatch write went out with no verdict. Every such outcome
 * says so: a pass the agent cannot see reads the same as a gate that is working, and a headless
 * session gets no UI notice at all (`hasUI`), so the note is the only record it sees.
 */
const UNREVIEWED_CAUSE: Partial<Record<GateEntry["outcome"], string>> = {
  timeout: "the review did not answer within its deadline",
  error: "the review failed",
  "no-verdict": "the review answered with no verdict AskGate could read",
  skipped: "this session's model is on another provider than the gate's",
  halted:
    "AskGate is paused for this session after three calls without a verdict, and reviews no Dispatch write until the session changes",
};
const renderUnreviewed = (outcome: GateEntry["outcome"], reason: string | undefined) => {
  const cause = UNREVIEWED_CAUSE[outcome];
  if (cause === undefined) return undefined;
  const why = reason === undefined || reason === "" ? "" : `: ${agentFacing(reason)}`;
  const opening =
    outcome === "halted" ? cause : `AskGate did not review this Dispatch write: ${cause}`;
  return `<advisor-gate advisor="AskGate" outcome="${outcome}">\n${opening}${why}. The call ran unreviewed.\n</advisor-gate>`;
};
/** Characters for a reason, each in code; a backtick, which inline code cannot hold, is named. */
function characters(chars: readonly string[]): string {
  const named = chars.map((char) => (char === "`" ? "a backtick" : `\`${char}\``));
  return named.length < 2
    ? (named[0] ?? "")
    : `${named.slice(0, -1).join(", ")} and ${named.at(-1)}`;
}
/**
 * The flags a refused file's text can go through instead: a `--<field>-file` reads `-` as stdin and
 * any other path as a file. An upload's `--path` (and `--path-file`, which names it) is a path the
 * CLI sends as it is, `-` included, so an upload's text goes through `--content-file`, and its
 * regular file through `--path`.
 */
function resendFlags(flag: string): { stdin: string; regular: string } {
  if (flag === "--path" || flag === "--path-file")
    return { stdin: "--content-file", regular: "`--path` or `--content-file`" };
  return { stdin: flag, regular: `\`${flag}\`` };
}
/** The one form AskGate reads, as the refusal states it to the agent. */
const LITERAL_GRAMMAR =
  "AskGate reads one form only: one `dispatch` call alone, with at most one `cd <path> &&` before it, one `2>&1` after it, and one here-document opened with a quoted delimiter (`<<'EOF'`). Every word is literal: no `$`, backtick, `~`, `*`, `?`, `[`, `{`, `}` or backslash. Nothing else is in the call: no second command, no pipe, no `;`, `&&`, `||` or `&`, no assignment and no other redirect.";
/**
 * What a scoped `dispatch` write inside a command the gate cannot read tells the agent. In `block`
 * mode nothing in the call ran, and this is the call's refusal. In `warn` mode the call ran with
 * no review, and this is its note: the same explanation of why AskGate could not read it and of
 * the form it reads, so the next write goes in that form.
 */
function renderUnreadable(
  { label, program, subcommand, wrapped, file }: UnreadableCommand,
  mode: Mode
): string {
  const ran = mode === "warn";
  const note = (body: string) =>
    ran ? `<advisor-gate advisor="AskGate" outcome="unreadable">\n${body}\n</advisor-gate>` : body;
  const ending = ran ? "The call ran unreviewed." : "Nothing in the call ran.";
  if (file !== undefined) {
    const { stdin, regular } = resendFlags(file.flag);
    return note(
      `AskGate ${ran ? "did not review" : "did not run"} this call: \`${file.flag}\` names ${JSON.stringify(file.path)}, which ${file.reason}. AskGate reads and posts only a regular file it captured before review. Send the text with \`${stdin} -\` and a quoted here-document (\`<<'EOF'\`), or name a regular file with ${regular}. ${ending}`
    );
  }
  const opening =
    subcommand === undefined
      ? `AskGate ${ran ? "did not review" : "did not run"} this call: it holds a \`${label}\`, which AskGate reviews before it is sent, and in this call AskGate cannot know the text it would send.`
      : `AskGate ${ran ? "did not review" : "did not run"} this call: the word after \`dispatch\` holds ${characters(subcommand.unread)}, so AskGate cannot tell which subcommand would run, and AskGate reviews each Dispatch write before it is sent.`;
  const why =
    program !== undefined
      ? program.namesDispatch
        ? ` Here the word that calls \`dispatch\` holds ${characters(program.unread)}, which AskGate does not take as literal.`
        : ` Here its program's name holds ${characters(program.unread)}, so AskGate cannot tell which program would run.`
      : wrapped === true
        ? ` Here \`${label}\` is in this call's text, where another command (\`bash -c\`, \`sh -c\`, \`eval\`, \`xargs\`, a function or a variable) can run it.`
        : "";
  const resend =
    subcommand === undefined
      ? ` ${ran ? "Send the next one" : `Run any other commands it held in a call of their own first, then send that \`${label}\` again`} on its own${program === undefined ? "" : ", calling `dispatch` by name"}.`
      : " Send the `dispatch` command again with its subcommand written out.";
  return note(
    `${opening} ${LITERAL_GRAMMAR}${why} ${ending}${resend} Text it posts from a file goes in a \`--<field>-file <path>\` flag or in one here-document opened with a quoted delimiter (\`<<'EOF'\`) as the call's only other content.${wrapped === true ? ` If \`${label}\` here is text and no Dispatch write (a search pattern, a message, a file's content), keep it out of the command line: put that text in a file and name the file.` : ""}`
  );
}

/**
 * The gate of a session where AskGate did not load: a host module that did not import, an
 * OMP_ASKGATE_TIMEOUT_MS that is no positive integer, a handler ceiling too short for a verdict,
 * or no envoy.json location at all (`configFile` null), which is where the switch lives.
 * Only `askgate.enabled: false` turns AskGate off, so neither is an off switch: each call of
 * the main agent the gate would review (`scopedCommand`, `unreadableDispatchWrite`, `scopedDevice`,
 * an upload counted as the gate counts it) is refused in both modes, naming the error and the
 * switch, and recorded as `load-failed`. Every other call runs.
 */
export function loadFailureGate(
  error: unknown,
  deps: Pick<Deps, "env"> & { configFile: string | null },
  pi: Pick<Pi, "appendEntry">
): (event: ToolCallEvent, ctx: GateCtx) => ToolCallResult {
  const message = messageOf(error);
  const reason =
    deps.configFile === null
      ? `AskGate did not send this call: it reviews each Dispatch write before it posts, and this session's envoy.json location could not be worked out (${message}), so AskGate cannot read its switch. Give the session a location — an absolute XDG_CONFIG_HOME, ENVOY_CONFIG_FILE or PI_CODING_AGENT_DIR — then send the call again.`
      : `AskGate did not send this call: it reviews each Dispatch write before it posts, and it did not load: ${message}. Send the call again once AskGate loads. To turn AskGate off for this session, set askgate.enabled to false in ${deps.configFile}.`;
  const envMode = parseMode(deps.env.OMP_ASKGATE);
  return (event, ctx) => {
    if (ctx.agent.kind !== "main") return undefined;
    let found: { tool: string; path: string } | undefined;
    if (event.toolName === "bash") {
      const write =
        scopedCommand(event.input.command) ?? unreadableDispatchWrite(event.input.command);
      if (write !== undefined) found = { tool: write.tool, path: write.label };
    } else if (scopedDevice(event.toolName, event.input))
      found = { tool: event.toolName, path: `xd://${event.toolName}` };
    if (found === undefined) return undefined;
    try {
      pi.appendEntry(ADVISOR_GATE_ENTRY_TYPE, {
        advisor: "AskGate",
        ...found,
        toolCallId: event.toolCallId,
        decision: "revise",
        outcome: "load-failed",
        verdictMode: envMode ?? sessionMode(ctx.sessionManager.getEntries()),
        reason: message,
        latencyMs: 0,
        revisesForKey: 0,
        argsDigest: "",
      } satisfies GateEntry);
    } catch {
      // The entry is best effort; the call is refused either way.
    }
    return { block: true, reason };
  };
}

export function createAskGate(deps: Deps): (pi: Pi) => void {
  return function askGate(pi: Pi): void {
    const rawMode = deps.env.OMP_ASKGATE;
    const envMode = parseMode(rawMode);
    // An OMP_ASKGATE that is not a mode (`off` included) is not a switch: the gate stays on, and the
    // first gated call says so once, naming the switch.
    let ignoredModeNotice =
      rawMode === undefined || envMode !== undefined
        ? undefined
        : `AskGate: OMP_ASKGATE is ${JSON.stringify(rawMode)}, which is not a mode, so AskGate ignores it and stays on. OMP_ASKGATE takes warn or block. To turn AskGate off for this session, set askgate.enabled to false in ${deps.configFile}.`;
    // Both checks throw before any handler binds, so the entry turns them into its load-failure
    // refusal (`loadFailureGate`): a gate that cannot review is never an off switch.
    const timeoutMs = parseTimeout(deps.env.OMP_ASKGATE_TIMEOUT_MS);
    const ceiling = deps.handlerCeilingMs();
    // A deadline this short would time out nearly every verdict; the runner would refuse a longer one.
    if (ceiling - CEILING_MARGIN_MS < MIN_DEADLINE_MS)
      throw new Error(
        `extensionHandlers.toolCallTimeoutMs is ${ceiling} ms, which leaves no room for a verdict (it needs at least ${MIN_DEADLINE_MS + CEILING_MARGIN_MS} ms)`
      );
    // A short deadline is no off switch: the gate binds, and says once how to give it more time. The
    // ceiling is read again on each call (`askModel`); this is the deadline the session starts with.
    const deadlineAtBind = Math.min(timeoutMs, ceiling - CEILING_MARGIN_MS);
    if (deadlineAtBind < WARN_DEADLINE_MS) {
      const raise = [
        ...(ceiling - CEILING_MARGIN_MS < WARN_DEADLINE_MS
          ? [
              `extensionHandlers.toolCallTimeoutMs to ${RECOMMENDED_CEILING_MS} or more in the session's settings`,
            ]
          : []),
        ...(timeoutMs < WARN_DEADLINE_MS
          ? [`OMP_ASKGATE_TIMEOUT_MS to ${WARN_DEADLINE_MS} or more`]
          : []),
      ];
      const envNote =
        deps.env.OMP_ASKGATE_TIMEOUT_MS === undefined
          ? ""
          : ` OMP_ASKGATE_TIMEOUT_MS is ${timeoutMs}.`;
      deps.logger.warn(
        `AskGate: the review deadline is ${deadlineAtBind} ms. A review of a long session often takes longer, and a review that times out lets the write post. extensionHandlers.toolCallTimeoutMs is ${ceiling} ms, and the deadline is held ${CEILING_MARGIN_MS} ms under it.${envNote} Set ${raise.join(" and ")}.`
      );
    }
    // The system role holds only the gate's own text and its rules; the agent's system prompt and
    // transcript are data about the agent under review, escaped into the user message.
    const system = `${systemTemplate.trim()}\n\n${rules.trim()}`;
    const dumpPath = deps.env.OMP_ASKGATE_DUMP;
    // The fork binds this factory once per process and keeps the binding across /new, /resume
    // and a branch switch, so the per-session state below resets on session_switch and
    // session_branch: a halt, a count, revise reasons or a rebuttal never outlive their session.
    const rebuttals = new Map<string, string>();
    const breaker = new Breaker();
    let failures = 0;
    let halted = false;
    // Set once the session starts shutting down (only dispose emits session_shutdown): a scoped call
    // that reaches the gate after that asks no model and takes the shutdown branch at once.
    let closing = false;
    const reset = () => {
      rebuttals.clear();
      breaker.clear();
      failures = 0;
      halted = false;
    };
    pi.on("session_switch", reset);
    pi.on("session_branch", reset);
    // A gate still waiting when its tool's execution ends was abandoned: the runner gave up on the
    // handler (a user abort, or its ceiling) and the call already went its way. Cancel the model call.
    // An eval-bridged write never shows its own id on a loop tool_execution_end, so a run the user
    // stopped (its last assistant message aborted) abandons every gate still waiting. Any other end
    // leaves them be: the session may continue (willContinue), or a backgrounded eval cell may keep
    // running past the run, and abandoning its gate would let its write out unchecked. A call a cell
    // makes after it finished, from its own timer or thread, never reaches a gate: both eval bridges
    // refuse a call from a finished cell ("Run no longer active", "No active Python tool bridge
    // session").
    /** Why a waiting gate was given up: the user or the runner ended its call or run, or the session shut down. */
    type Abandon = (cause: "abandoned" | "shutdown", graceMs?: number) => void;
    const inflight = new Map<string, Abandon>();
    const snapshots = new Map<string, SnapshotSet>();
    const releaseSnapshots = (toolCallId: string) => {
      const snapshot = snapshots.get(toolCallId);
      if (snapshot === undefined) return;
      snapshots.delete(toolCallId);
      removeSnapshots(snapshot);
    };
    // A snapshot goes only once the command that reads it has run. A result that reports a
    // backgrounded job's start comes before the shell runs it (`startedInBackground`), and nothing
    // later names that call's id, so its snapshot is kept until `session_shutdown`; a new gated
    // call sweeps whatever is older than SNAPSHOT_MAX_AGE_MS.
    pi.on("tool_execution_end", (event: { toolCallId: string; result?: unknown }) => {
      inflight.get(event.toolCallId)?.("abandoned");
      if (isRecord(event.result) && startedInBackground(event.result.details)) return;
      releaseSnapshots(event.toolCallId);
    });
    pi.on("tool_result", (event: { toolCallId: string; details?: unknown }) => {
      if (startedInBackground(event.details)) return;
      releaseSnapshots(event.toolCallId);
    });
    pi.on("agent_end", (event: { messages?: readonly Message[]; willContinue?: boolean }) => {
      if (event.willContinue) return;
      if (event.messages?.findLast((m) => m.role === "assistant")?.stopReason !== "aborted") return;
      for (const abandon of inflight.values()) abandon("abandoned");
    });
    // A gate still waiting at shutdown (left running past a normal end) would bill to its deadline and
    // record into a sealed session. Abandon it, and hold shutdown until its entry is written; a gated
    // call that starts after this point asks no model (`closing`). Either way the entry reads
    // `shutdown`, and block mode refuses the call as it refuses any call given up without a verdict.
    const handling = new Set<Promise<ToolCallResult>>();
    pi.on("session_shutdown", async () => {
      closing = true;
      for (const abandon of inflight.values()) abandon("shutdown", SHUTDOWN_GRACE_MS);
      for (const toolCallId of snapshots.keys()) releaseSnapshots(toolCallId);
      const { promise: waited, resolve: stopWaiting } = Promise.withResolvers<void>();
      const timer = setTimeout(stopWaiting, SHUTDOWN_WAIT_MS);
      await Promise.race([Promise.allSettled([...handling]), waited]);
      clearTimeout(timer);
    });

    /** A call without a verdict counts toward the halt; a verdict resets the count; anything else leaves it. */
    const settle = (effect: Decision["effect"], ctx: GateCtx) => {
      if (effect === "verdict") failures = 0;
      if (effect !== "failure") return;
      failures++;
      if (failures < GATE_HALT_AFTER_FAILURES || halted) return;
      halted = true;
      if (ctx.hasUI)
        ctx.ui.notify(
          `AskGate paused for this session after ${failures} consecutive calls without a verdict; Dispatch writes now go out unchecked.`,
          "warning"
        );
    };

    /** The outer `write xd://<device>` event: strip a rebuttal before the device validates its arguments. */
    const stripRebuttal = (event: ToolCallEvent): ToolCallResult => {
      const { input } = event;
      const device = typeof input.path === "string" ? XD_PATH.exec(input.path)?.[1] : undefined;
      if (
        !device ||
        !(SCOPED_DEVICES as readonly string[]).includes(device) ||
        typeof input.content !== "string"
      )
        return undefined;
      const { rebuttal, content } = extractRebuttal(input.content);
      if (rebuttal === undefined) return undefined;
      rebuttals.set(event.toolCallId, rebuttal);
      if (rebuttals.size > REBUTTAL_CAP) rebuttals.delete(rebuttals.keys().next().value as string);
      return { input: { ...input, content } };
    };

    type Base = Omit<GateEntry, "decision" | "outcome" | "latencyMs">;
    /** What one gated call records, returns, and does to the halt count. */
    type Decision = {
      fields: Pick<GateEntry, "decision" | "outcome"> & Partial<GateEntry>;
      result?: ToolCallResult;
      effect: "failure" | "verdict" | "none";
    };
    /**
     * The write goes out. Every outcome `UNREVIEWED_CAUSE` names hands the agent a note saying it
     * went out unreviewed and why; `abandoned` and `shutdown` get none, their own tool or session
     * having ended before anything could reach the agent.
     */
    const pass = (
      outcome: GateEntry["outcome"],
      fields: Partial<GateEntry> = {},
      effect: Decision["effect"] = "none"
    ): Decision => {
      const note = renderUnreviewed(outcome, fields.reason);
      return {
        fields: { decision: "allow", outcome, ...fields },
        ...(note === undefined ? {} : { result: { additionalContext: note } }),
        effect,
      };
    };
    /**
     * A call given up without a verdict: its call, its run or the session ended first. Not the gate's
     * failure, so it never counts toward the halt. No verdict is not permission: block mode refuses it
     * (defence in depth: the runner awaiting it has usually refused the call already), warn mode lets it
     * go. The cause stays recorded.
     */
    const givenUp = (
      mode: Mode,
      cause: "abandoned" | "shutdown",
      fields: Partial<GateEntry> = {}
    ): Decision =>
      mode === "block"
        ? {
            fields: { decision: "revise", outcome: cause, ...fields },
            result: { block: true, reason: NO_VERDICT_REFUSAL[cause] },
            effect: "none",
          }
        : pass(cause, fields);
    /**
     * A session in which no model role the gate asks resolves: refused in both modes, since the write
     * would otherwise post unreviewed, and never toward the halt, which would let every later write out.
     */
    const noModelRole: Decision = {
      fields: {
        decision: "revise",
        outcome: "no-model-role",
        reason: "neither modelRoles.askgate nor modelRoles.default resolves to a model",
      },
      result: { block: true, reason: NO_MODEL_ROLE_REFUSAL },
      effect: "none",
    };
    /**
     * A call the gate cannot read: a judgment on one call's form, not a gate that failed. `block`
     * mode refuses it before anything in it runs, so the agent sends the write in a form AskGate
     * reads; `warn` mode runs it and hands the same explanation back as context, with the entry
     * recording `unreadable` and `allow`. Neither counts toward the halt.
     */
    const unreadableCall = (found: UnreadableCommand, mode: Mode): Decision =>
      mode === "block"
        ? {
            fields: { decision: "revise", outcome: "unreadable" },
            result: { block: true, reason: renderUnreadable(found, mode) },
            effect: "none",
          }
        : {
            fields: { decision: "allow", outcome: "unreadable" },
            result: { additionalContext: renderUnreadable(found, mode) },
            effect: "none",
          };
    /** The first of the gate's roles that resolves to a model in this session, with that model. */
    const resolveRole = (ctx: GateCtx): { role: GateRole; model: GateModel } | undefined => {
      for (const role of GATE_ROLES) {
        const model = ctx.models.resolve(`@${role}`);
        if (model) return { role, model };
      }
      return undefined;
    };

    /** What one model walk answered: the answer, and the fields every entry that reached the model records. */
    type Called = Pick<
      GateEntry,
      "promptBytes" | "model" | "deadlineMs" | "failedModels" | "usage" | "reviewedText"
    >;
    type Asked =
      | { kind: "no-model-role" }
      | { kind: "skipped"; reason: string }
      | { kind: "asked"; answer: Answer; called: Called };
    /**
     * One review by the gate's model: resolves the role, skips a session whose primary is on another
     * provider, appends both prompts to OMP_ASKGATE_DUMP, and walks the role's chain on the primary's
     * provider under the call's deadline (raced from `started`). `user` is rendered only once the
     * model is known, so a skipped session reads nothing of its transcript. Every check of a Dispatch
     * write that asks the model goes through here.
     */
    const askModel = async (
      ctx: GateCtx,
      call: { toolCallId: string; started: number },
      prompt: { system: string; user: () => string; reviewedText: string }
    ): Promise<Asked> => {
      const resolved = resolveRole(ctx);
      if (resolved === undefined) return { kind: "no-model-role" };
      const { role, model } = resolved;
      // A session routed off the gate's provider keeps its transcript there: nothing is rendered or sent.
      if (ctx.model?.provider !== model.provider)
        return { kind: "skipped", reason: `primary on ${ctx.model?.provider ?? "no model"}` };
      const user = prompt.user();
      const promptBytes = byteLength(prompt.system) + byteLength(user);
      if (dumpPath)
        deps.appendFile(
          dumpPath,
          `===== AskGate ${call.toolCallId} system =====\n${prompt.system}\n===== AskGate ${call.toolCallId} user =====\n${user}\n\n`
        );
      // Raced from handler entry, so the render above counts against the runner's ceiling too.
      const deadlineMs = Math.min(timeoutMs, deps.handlerCeilingMs() - CEILING_MARGIN_MS);
      const sessionId = ctx.sessionManager.getSessionId();
      // A model that answers with an error hands the request to the next one in the role's chain, inside
      // the same deadline. Only models on the primary's provider are asked: the transcript stays there.
      const chain = [
        model,
        ...deps.fallbackModels(ctx, model, role).filter((next) => next.provider === model.provider),
      ];
      const failedModels: Array<{ model: string; error: string }> = [];
      const usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0 };
      let billed = false;
      let answer: Answer = { kind: "error", reason: "no model asked" };
      let asked = model;
      for (const next of chain) {
        asked = next;
        answer = await ask(
          { ctx, model: next, system: prompt.system, user, sessionId },
          call.toolCallId,
          Math.max(0, deadlineMs - (deps.now() - call.started))
        );
        if (answer.usage) {
          billed = true;
          for (const k of Object.keys(usage) as Array<keyof Usage>) usage[k] += answer.usage[k];
        }
        if (answer.kind !== "error" || next === chain.at(-1)) break;
        failedModels.push({ model: `${next.provider}/${next.id}`, error: answer.reason });
      }
      return {
        kind: "asked",
        answer,
        called: {
          promptBytes,
          reviewedText: prompt.reviewedText,
          model: `${asked.provider}/${asked.id}`,
          deadlineMs,
          ...(failedModels.length > 0 ? { failedModels } : {}),
          ...(billed ? { usage } : {}),
        },
      };
    };

    const decide = async (
      event: ToolCallEvent,
      ctx: GateCtx,
      started: number,
      base: Base,
      key: string,
      rebuttal: string | undefined,
      override: Override,
      upload = false
    ): Promise<Decision> => {
      if (rebuttal !== undefined) {
        breaker.reset(key);
        return {
          fields: { decision: "allow", outcome: "rebuttal", rebuttal },
          result: { additionalContext: renderRebuttalAck(rebuttal, override) },
          effect: "none",
        };
      }
      if (halted) return pass("halted");
      if (breaker.tripped(key)) {
        breaker.reset(key);
        return {
          fields: { decision: "allow", outcome: "breaker" },
          result: { additionalContext: renderBreakerAck() },
          effect: "none",
        };
      }

      if (closing) return givenUp(base.verdictMode, "shutdown");
      // The arguments as the model sees them, cut at GATE_ARGS_MAX_BYTES, also go in the entry, so a
      // later read of the review has the text the model judged, not the command or a mutable file.
      const args = renderArgs(event.input);
      const reviewed = await askModel(
        ctx,
        { toolCallId: event.toolCallId, started },
        {
          system,
          reviewedText: args,
          user: () =>
            renderRequest(
              ctx.getSystemPrompt().join("\n\n"),
              renderTranscript(deps.contextMessages(ctx), GATE_CONTEXT_MAX_BYTES),
              base.path,
              args,
              breaker.reasons(key),
              upload
            ),
        }
      );
      if (reviewed.kind === "no-model-role") return noModelRole;
      if (reviewed.kind === "skipped") return pass("skipped", { reason: reviewed.reason });
      const { answer, called } = reviewed;
      if (answer.kind === "error")
        return pass("error", { reason: answer.reason, ...called }, "failure");
      if (answer.kind !== "answer")
        return answer.kind === "timeout"
          ? pass("timeout", called, "failure")
          : givenUp(base.verdictMode, answer.kind, called);
      const verdict = parseVerdict(answer.text);
      if (!verdict)
        return pass("no-verdict", { raw: answer.text.slice(-2000), ...called }, "failure");
      if (verdict.decision === "allow" || verdict.reason === undefined) {
        breaker.reset(key);
        return pass("verdict", called, "verdict");
      }
      breaker.revised(key, verdict.reason);
      const second = breaker.reasons(key).length === GATE_BREAKER_REVISES;
      return {
        fields: {
          decision: "revise",
          outcome: "verdict",
          reason: verdict.reason,
          deliveredReason: agentFacing(verdict.reason),
          revisesForKey: base.revisesForKey + 1,
          ...called,
        },
        result:
          base.verdictMode === "block"
            ? { block: true, reason: renderRevise(verdict.reason, second, override) }
            : { additionalContext: renderWarn(verdict.reason, second) },
        effect: "verdict",
      };
    };

    /** The model call raced against the deadline and against its tool ending without it. */
    type Answer =
      | { kind: "answer"; text: string; usage?: Usage }
      | { kind: "error"; reason: string; usage?: Usage }
      | { kind: "timeout" | "abandoned" | "shutdown"; usage?: Usage };
    const ask = async (
      req: Omit<CompleteRequest, "signal">,
      toolCallId: string,
      remainingMs: number
    ): Promise<Answer> => {
      const controller = new AbortController();
      const { promise: stopped, resolve: stop } = Promise.withResolvers<
        "timeout" | "abandoned" | "shutdown"
      >();
      const timer = setTimeout(() => stop("timeout"), remainingMs);
      let graceMs = ABORT_GRACE_MS;
      inflight.set(toolCallId, (cause, grace = ABORT_GRACE_MS) => {
        graceMs = grace;
        stop(cause);
      });
      try {
        const call = deps.complete({ ...req, signal: controller.signal });
        // Promise.race subscribes to the completion, so a late rejection is handled.
        const first = await Promise.race([call, stopped]);
        if (typeof first !== "string") {
          return first.error !== undefined
            ? { kind: "error", reason: first.error, usage: first.usage }
            : { kind: "answer", text: first.text, usage: first.usage };
        }
        controller.abort();
        // The aborted request was billed for what it sent; give it a moment to report that.
        const { promise: grace, resolve: graceOver } = Promise.withResolvers<undefined>();
        const graceTimer = setTimeout(graceOver, graceMs);
        const settled = await Promise.race([call.catch(() => undefined), grace]);
        clearTimeout(graceTimer);
        return { kind: first, usage: settled?.usage };
      } catch (error) {
        return { kind: "error", reason: messageOf(error) };
      } finally {
        clearTimeout(timer);
        inflight.delete(toolCallId);
      }
    };

    /**
     * A scoped command's arguments as the CLI's own --dry-run prints them, or why there are none,
     * or `unscoped`: an upload whose resolved name and path make no markdown document.
     */
    type DryRun =
      | { args: Input; command?: string }
      | { failure: string; command?: string }
      | { unreadable: UnreadableCommand }
      | { cause: "abandoned" | "shutdown"; command?: string }
      | { unscoped: true; command?: string };
    const readArgs = async (
      scoped: ScopedCommand,
      input: Input,
      ctx: GateCtx,
      toolCallId: string
    ): Promise<DryRun> => {
      // The bash tool's own resolution: `~` is home, a bare `/` is the session's directory, and a
      // relative path resolves against it.
      const raw =
        typeof input.cwd === "string" && input.cwd !== ""
          ? input.cwd.replace(/^~(?=$|\/)/, deps.home)
          : undefined;
      const cwd = raw === undefined || /^\/+$/.test(raw) ? ctx.cwd : path.resolve(ctx.cwd, raw);
      /** Runs one script as the dry run runs: in `cwd`, under the limit, abandoned with the call. */
      const runScript = async (
        script: string
      ): Promise<{ run: DryRunResult } | { cause: "abandoned" | "shutdown" }> => {
        // Abandoned like a waiting model call: the run's stop or the session's shutdown kills it.
        const controller = new AbortController();
        let cause: "abandoned" | "shutdown" | undefined;
        inflight.set(toolCallId, (given) => {
          cause = given;
          controller.abort();
        });
        try {
          const run = await deps.dryRun(script, {
            cwd,
            env: deps.env,
            timeoutMs: DRY_RUN_TIMEOUT_MS,
            signal: controller.signal,
          });
          return cause === undefined ? { run } : { cause };
        } finally {
          inflight.delete(toolCallId);
        }
      };
      /** The script's stdout, or why it gave none: a stop, the limit, or an exit other than 0. */
      const stdoutOf = async (
        script: string,
        what: string
      ): Promise<
        | { stdout: string; stderr: string }
        | { failure: string }
        | { cause: "abandoned" | "shutdown" }
      > => {
        const ran = await runScript(script);
        if ("cause" in ran) return ran;
        const { run } = ran;
        if (run.timedOut)
          return { failure: `${what} did not finish within ${DRY_RUN_TIMEOUT_MS} ms` };
        if (run.exitCode !== 0) {
          const printed = clipBytes((run.stdout || run.stderr).trim(), GATE_REASON_MAX_BYTES);
          return { failure: `${what} exited ${run.exitCode ?? "on a signal"}: ${printed}` };
        }
        return { stdout: run.stdout, stderr: run.stderr };
      };
      if (scoped.unsafeFile !== undefined)
        return {
          unreadable: {
            tool: scoped.tool,
            label: scoped.label,
            file: scoped.unsafeFile,
          },
        };

      let executionCommand = scoped.command;
      if (scoped.fileInputs.length > 0) {
        const relativeFile = scoped.fileInputs.find((file) => !path.isAbsolute(file.source));
        let base = cwd;
        if (relativeFile !== undefined) {
          try {
            // A child shell's process.cwd() is physical even when the tool's cwd used a symlink.
            base = fs.realpathSync(cwd);
          } catch (error) {
            return {
              unreadable: {
                tool: scoped.tool,
                label: scoped.label,
                file: {
                  flag: relativeFile.flag,
                  path: relativeFile.source,
                  reason: `could not resolve the tool directory: ${messageOf(error)}`,
                },
              },
            };
          }
        }
        const firstRelative = scoped.changesDirectory === true ? relativeFile : undefined;
        if (firstRelative !== undefined) {
          const reached = await stdoutOf(scoped.dirScript, `${scoped.label}'s directory`);
          if ("cause" in reached) return reached;
          if (!("stdout" in reached))
            return {
              unreadable: {
                tool: scoped.tool,
                label: scoped.label,
                file: {
                  flag: firstRelative.flag,
                  path: firstRelative.source,
                  reason: `could not determine the command directory: ${reached.failure}`,
                },
              },
            };
          base = reached.stdout.endsWith("\n") ? reached.stdout.slice(0, -1) : reached.stdout;
          if (!path.isAbsolute(base))
            return {
              unreadable: {
                tool: scoped.tool,
                label: scoped.label,
                file: {
                  flag: firstRelative.flag,
                  path: firstRelative.source,
                  reason: "could not determine an absolute command directory",
                },
              },
            };
        }

        // Refused by name, before any `stat`: such a path resolves in whichever process opens it.
        const descriptor = scoped.fileInputs.find(
          (file) =>
            PROCESS_FD_PATH.test(file.source) ||
            PROCESS_FD_PATH.test(path.resolve(base, file.source))
        );
        if (descriptor !== undefined)
          return {
            unreadable: {
              tool: scoped.tool,
              label: scoped.label,
              file: {
                flag: descriptor.flag,
                path: descriptor.source,
                reason:
                  "names a file descriptor of the process that opens it, which here is Oh My Pi's own and not the command's shell",
              },
            },
          };

        const root = path.resolve(os.tmpdir());
        sweepSnapshots(root, deps.now());
        const directory = fs.mkdtempSync(path.join(root, "askgate-"));
        fs.chmodSync(directory, 0o700);
        const snapshot: SnapshotSet = { directory, files: [] };
        let current = scoped.fileInputs[0];
        let source = current.source;
        try {
          for (const inputFile of scoped.fileInputs) {
            current = inputFile;
            source = inputFile.source;
            const resolved = path.isAbsolute(source) ? source : path.resolve(base, source);
            snapshot.files.push({
              input: inputFile,
              path: captureSnapshot(resolved, directory, snapshot.files.length),
            });
          }
          snapshots.set(toolCallId, snapshot);
          executionCommand = commandWithSnapshots(scoped.command, snapshot.files);
        } catch (error) {
          removeSnapshots(snapshot);
          return {
            unreadable: {
              tool: scoped.tool,
              label: scoped.label,
              file: {
                flag: current.flag,
                path: source,
                reason:
                  (error as NodeJS.ErrnoException).code === "ENOENT"
                    ? "does not exist"
                    : `could not be captured: ${messageOf(error)}`,
              },
            },
          };
        }
      }

      const execution = executionCommand === scoped.command ? {} : { command: executionCommand };
      const reviewed = execution.command === undefined ? scoped : scopedCommand(execution.command);
      if (reviewed === undefined)
        return {
          unreadable: {
            tool: scoped.tool,
            label: scoped.label,
            file: {
              flag: scoped.fileInputs[0]?.flag ?? "--file",
              path: "",
              reason: "could not rewrite to the snapshot AskGate captured",
            },
          },
        };
      const dry = await stdoutOf(reviewed.dryRunScript, `${scoped.label} --dry-run`);
      if (!("stdout" in dry)) return { ...dry, ...execution };
      let args: Input | undefined;
      try {
        const parsed: unknown = JSON.parse(dry.stdout);
        if (isRecord(parsed)) args = parsed;
      } catch {
        // Not JSON: reported below.
      }
      if (args === undefined) {
        const printed = clipBytes((dry.stdout || dry.stderr).trim(), GATE_REASON_MAX_BYTES);
        return {
          failure: `${scoped.label} --dry-run printed no JSON object: ${printed}`,
          ...execution,
        };
      }
      if (scoped.upload) {
        // The name and the path as the CLI sends them: Dispatch trims the name it stores.
        const resolved = {
          ...args,
          ...(typeof args.name === "string" ? { name: args.name.trim() } : {}),
        };
        if (!uploadIsDocument(resolved)) return { unscoped: true, ...execution };
      }
      // The artifact dry run prints its snapshot path rather than its body. Read only the owned
      // snapshot here, so the model sees the exact bytes the rewritten command will post.
      if (!scoped.upload || typeof args.path !== "string" || args.content !== undefined)
        return { args, ...execution };
      const snapshot = snapshots
        .get(toolCallId)
        ?.files.find((file) => file.input.flag === "--path");
      if (snapshot === undefined)
        return {
          unreadable: {
            tool: scoped.tool,
            label: scoped.label,
            file: {
              flag: "--path",
              path: args.path,
              reason: "was not captured before review",
            },
          },
        };
      try {
        return { args: { ...args, content: fs.readFileSync(snapshot.path, "utf8") }, ...execution };
      } catch (error) {
        return {
          unreadable: {
            tool: scoped.tool,
            label: scoped.label,
            file: {
              flag: "--path",
              path: snapshot.path,
              reason: `could not read its captured snapshot: ${messageOf(error)}`,
            },
          },
        };
      }
    };

    /** The fields every entry of a call carries; a gated call fills in its target's revise count and the digest. */
    const identity = (event: ToolCallEvent, mode: Mode): Base => ({
      advisor: "AskGate",
      tool: event.toolName,
      path: `xd://${event.toolName}`,
      toolCallId: event.toolCallId,
      verdictMode: mode,
      revisesForKey: 0,
      argsDigest: "",
    });

    const handleCall = async (event: ToolCallEvent, ctx: GateCtx): Promise<ToolCallResult> => {
      const started = deps.now();
      let base: Base | undefined;
      let decision: Decision;
      // A `dispatch` command with its override flag removed, which runs whatever the gate decides,
      // and, once a file operand is rewritten, the command that names the snapshot.
      let withoutRebuttal: Input | undefined;
      let revised: Input | undefined;
      // OMP_ASKGATE when it is a mode; else this session's latest experiments entry, read per call
      // because one binding serves every session the process switches to.
      let mode: Mode = envMode ?? "block";
      try {
        if (ctx.agent.kind !== "main") return undefined;
        if (event.toolName === "write") return stripRebuttal(event);
        if (envMode === undefined) mode = sessionMode(ctx.sessionManager.getEntries());
        if (event.toolName === "bash") {
          const scoped = scopedCommand(event.input.command);
          if (scoped === undefined) {
            const unreadable = unreadableDispatchWrite(event.input.command);
            if (unreadable === undefined) return undefined;
            // A judgment on one call's form: block mode refuses it before anything in it runs, and
            // warn mode runs it and hands the reason back (`unreadableCall`). A halt, or a session
            // whose primary is off the gate's provider, lets it run as the resend would, and a
            // session with no model role refuses it as it would refuse the resend.
            base = { ...identity(event, mode), tool: unreadable.tool, path: unreadable.label };
            const resolved = halted ? undefined : resolveRole(ctx);
            if (halted) decision = pass("halted");
            else if (resolved === undefined) decision = noModelRole;
            else if (ctx.model?.provider !== resolved.model.provider)
              decision = pass("skipped", {
                reason: `primary on ${ctx.model?.provider ?? "no model"}`,
              });
            else decision = unreadableCall(unreadable, mode);
          } else {
            if (scoped.rebuttal !== undefined) {
              withoutRebuttal = { ...event.input, command: scoped.command };
              revised = withoutRebuttal;
            }
            base = { ...identity(event, mode), tool: scoped.tool, path: scoped.label };
            const run = await readArgs(scoped, event.input, ctx, event.toolCallId);
            if ("command" in run && run.command !== undefined)
              revised = { ...event.input, command: run.command };
            // An upload its dry run shows is no markdown document passes as an unscoped call does,
            // with no entry; an override flag on it is still removed, since the CLI would refuse it.
            if ("unscoped" in run) return revised === undefined ? undefined : { input: revised };
            if ("cause" in run) decision = givenUp(mode, run.cause);
            else if ("unreadable" in run) {
              const resolved = halted ? undefined : resolveRole(ctx);
              if (halted) decision = pass("halted");
              else if (resolved === undefined) decision = noModelRole;
              else if (ctx.model?.provider !== resolved.model.provider)
                decision = pass("skipped", {
                  reason: `primary on ${ctx.model?.provider ?? "no model"}`,
                });
              else decision = unreadableCall(run.unreadable, mode);
            }
            // The CLI refuses arguments it cannot parse before it sends anything, so the command runs.
            else if ("failure" in run) decision = pass("error", { reason: run.failure });
            else {
              const key = breakerKey(scoped.tool, run.args);
              base = {
                ...base,
                revisesForKey: breaker.reasons(key).length,
                argsDigest: createHash("sha256").update(JSON.stringify(run.args)).digest("hex"),
              };
              decision = await decide(
                { toolName: scoped.tool, toolCallId: event.toolCallId, input: run.args },
                ctx,
                started,
                base,
                key,
                scoped.rebuttal,
                scoped.readsFile ? FILE_OVERRIDE : COMMAND_OVERRIDE,
                scoped.upload === true
              );
            }
          }
        } else {
          // A remembered rebuttal is spent by its device event, whatever happens to the call.
          const rebuttal = rebuttals.get(event.toolCallId);
          rebuttals.delete(event.toolCallId);
          if (!scopedDevice(event.toolName, event.input)) return undefined;
          const key = breakerKey(event.toolName, event.input);
          base = {
            ...identity(event, mode),
            revisesForKey: breaker.reasons(key).length,
            argsDigest: createHash("sha256").update(JSON.stringify(event.input)).digest("hex"),
          };
          decision = await decide(event, ctx, started, base, key, rebuttal, DEVICE_OVERRIDE);
        }
      } catch (error) {
        // Fail open: the runner turns a thrown handler into a refusal. A throw here is the
        // gate's own (the dump, the render, the dry-run's spawn), not a missing
        // verdict, so it does not count toward the halt. The snapshot stays until the call ends,
        // since the command may still name it.
        decision = pass("error", { reason: messageOf(error) });
      }
      // Settled once and recorded once, each on its own, so neither failing can repeat the other.
      try {
        settle(decision.effect, ctx);
      } catch {
        // The halt notice is best effort.
      }
      if (decision.result?.block) {
        releaseSnapshots(event.toolCallId);
        revised = undefined;
      }
      // No verdict came back, so nothing proves the snapshot's bytes are the reviewed bytes, and a
      // gate that failed must break no valid write: the agent's own command runs instead of the
      // one naming a snapshot it never saw (its override flag still removed, which the CLI needs).
      if (UNREVIEWED_OUTCOMES.includes(decision.fields.outcome)) revised = withoutRebuttal;
      // The first gated call says once that the OMP_ASKGATE it ignored is not the switch.
      if (ignoredModeNotice !== undefined) {
        const notice = ignoredModeNotice;
        ignoredModeNotice = undefined;
        deps.logger.warn(notice);
        try {
          if (ctx.hasUI) ctx.ui.notify(notice, "warning");
        } catch {
          // The notice is best effort; the logger has it.
        }
      }
      try {
        pi.appendEntry(ADVISOR_GATE_ENTRY_TYPE, {
          ...(base ?? identity(event, mode)),
          ...decision.fields,
          latencyMs: deps.now() - started,
        } satisfies GateEntry);
      } catch {
        // The entry is best effort; the call still runs.
      }
      return revised === undefined ? decision.result : { ...decision.result, input: revised };
    };

    pi.on("tool_call", async (event: ToolCallEvent, ctx: GateCtx): Promise<ToolCallResult> => {
      const done = handleCall(event, ctx);
      handling.add(done);
      try {
        return await done;
      } finally {
        handling.delete(done);
      }
    });
  };
}
