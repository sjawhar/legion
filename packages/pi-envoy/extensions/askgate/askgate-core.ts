// AskGate: before a root session's scoped Dispatch write executes, one model call judges it
// against AskGate's rules (rules.md) and answers `allow` or `revise`. `askgate.ts` is the entry
// that binds Oh My Pi's completion call and buildSessionContext; everything else lives here so the
// tests drive it through a fake `pi`, a fake `ctx` and stubs.
//
// What this is not: AskGate reviews the writes of a cooperating agent, and it is no sandbox. A
// process running as the same user defeats it. It can remove the record or a marker, turn the
// gate off in its own envoy.json, or point the session at another envoy.json
// through OMP_PROFILE, PI_CODING_AGENT_DIR, ENVOY_CONFIG_FILE, XDG_CONFIG_HOME or HOME in the
// launching shell (the variables `envoyConfigPath` reads, in that order). Every check here is for
// a write an agent meant to send; none of them is weakened, and none is added for an attacker of
// that kind.
//
// Enforcement is the `dispatch` CLI's, not a scan of the call's text. The gate reads one form, the
// literal grammar below, and refuses no call by reading any other: a call in another form runs as
// written. For each literal call it lets run it mints one single-use marker
// (`@legion/envoy-client/askgate-marker`), keyed by the argv the CLI will receive and, behind a
// review or a deliberate pass, bound to the digest of the dry run's arguments and to the sha256 of
// each file the argv names, as the gate read it for the review. The CLI posts a
// gated write in a session whose record a live Oh My Pi process keeps (envoy.ts writes it) only
// when it consumes such a marker, so a write sent in any other form, from a pipe, a script,
// `python3 -c`, an eval cell's process or anything else, is refused when it runs. The CLI's check
// does not cover a direct HTTP request to Dispatch, an older or a different CLI, or a same-user
// process that removes the record or a marker, acts as another session id or state directory, or
// otherwise goes around the CLI. Precision comes first: the gate must not refuse a call that holds
// no Dispatch write, since a false refusal in block mode stops real work.
//
// Scope: eight calls on the command line and six of them as devices, each recorded under one tool
// name (dispatch_ask, …).
//   - A `bash` call whose whole command is the literal grammar of one `dispatch` call
//     (`dispatchCommandHead`): `[cd <literal path> && ] dispatch <subcommand> <literal words…>
//     [2>&1] [<<'DELIM'] [| head|tail [-n N]]`, the here-document's body on the lines after its
//     opener's line. The program is `dispatch` by name or by a literal path. Every word is
//     literal: outside quotes it holds only letters, digits and `._/+@%:,=-` (so no `$`, `{`,
//     `}`, backtick, glob character, `~` or backslash), inside single quotes anything, and inside
//     double quotes no `$`, backtick or backslash; a quoted word may run over several lines. A
//     backslash at the end of a line continues the command, as bash reads it. The one
//     here-document has a quoted delimiter (`'EOF'` or `"EOF"`), before or after the one `2>&1`.
//     The one pipe stage is `head` or `tail`, alone or with `-n N`, `-nN` or `-N`, which reads the
//     output after the CLI has its words. Nothing else: no assignment, other statement, other
//     pipe, `;`, `||`, `&`, other redirect or comment.
//     The subcommand is `ask`, `message`, `edit-ask`, `comment`, `doc-edit`, `request-approval` (its
//     `summary` is what the person approves), `issue` with `--spec` or `--spec-file`, or
//     `artifact` unless its literal `--name` and literal `--path` both show it is no markdown
//     document (`uploadMayBeDocument`): its dry run then decides, from the name and path it
//     prints, and an upload that is no markdown document runs with no entry. The gate learns
//     the exact arguments by running that command with `--dry-run` after the command name, in the
//     call's working directory (then its `cd`, as the call will) and environment, its
//     here-document still on stdin and without its `2>&1` and its pipe stage, under a 10 s limit,
//     and reviews the JSON it prints. A dry-run that fails, times out or prints no JSON object
//     is recorded as `error` and the call runs (the CLI refuses arguments it cannot parse,
//     before it sends anything). A `dispatch` command with `--dry-run` or `--help` sends nothing and is not
//     reviewed. An upload given with `--path` prints the path, not the text. Every file whose
//     text the write posts, that path and each `--<field>-file`, is read in place before the
//     model is asked, and the call runs as written. The model reviews those bytes (an upload's
//     as `content`), and the marker binds the sha256 of each file in argv order. The CLI reads
//     each file again before it posts and refuses one whose bytes changed since the review (a
//     call earlier in the same message can change it: Oh My Pi runs every `tool_call` hook of a
//     message before any call of it runs), and an upload sends the bytes the CLI read for that
//     check. An `--image` file is no part of that: the model reads its path and not its bytes.
//     It reads a regular file alone: a missing file, a directory, a device, a FIFO and
//     `/dev/stdin` are refused as `unreadable` before any read, with the advice to send the
//     call again in a later message when an earlier call of the same message writes the file.
//     The arguments are cut at 64 KiB as one JSON block, so the document gets less than 64 KiB of
//     it. A relative path resolves where the CLI resolves it, against its
//     `process.cwd()`, the physical directory the command runs in after its own `cd`: bash names
//     it (the `cd`, then `pwd -P`, run as the dry run runs, with no CDPATH, since Oh My Pi reads
//     none), except a leading `cd /` in a call that names no directory, which Oh My Pi runs in
//     the session's own directory. A `--<field>-file` operand is joined
//     to that directory and given to the kernel as written, as the CLI's own `readFileSync`
//     gives it (so a `..` after a symbolic link leaves the link's target, which a lexical
//     `path.resolve` would collapse first and name another file); an upload's `--path`, relative
//     or absolute, keeps the CLI's own `resolve`, which removes a `..` first. The gate then walks
//     the path one component at a time and follows each
//     link itself, refuses a component that reaches `/proc` or `/dev` (`/proc/self/fd/0`,
//     `/proc/thread-self/fd/0`, `/dev/fd/0` and every link to one name a descriptor of the
//     process that opens it, which is Oh My Pi's and not the command's shell), and opens with
//     `O_NOFOLLOW`, which guards the last path component only: a link put there after the walk is
//     refused, but a directory on the path swapped for a link after the walk is followed (a
//     concurrent process of the same user, the limit above). A directory lookup that fails is
//     `unreadable`, since a failed lookup is no proof that the write would fail. An upload is
//     judged against failure 10 alone (askgate-upload.md), since a plan or a report is long by
//     nature.
//   - Any other call is not the gate's: a `bash` call in any other form, or an `eval` call, runs
//     with no entry and no marker, and the CLI refuses a gated write it sends. The gate records
//     the mode in its marker store at each main-agent `bash` or `eval` call (the CLI reads it:
//     `warn` lets an unmarked write post with a warning), and envoy.ts records an `unmarked`
//     entry for each such write the CLI logged. When such a call's text holds a `dispatch` word
//     and a gated write subcommand, the gate finds the first cause that puts it outside the
//     grammar (`nonLiteralCause`) and, when the call's result holds the CLI's refusal of a write
//     with no marker, appends a note that names the cause and the literal form (`causeNote`).
//   - `write xd://<device>` calls, seen on the nested `tool_call` event the device wrapper emits
//     with `toolName = <device>` and the validated arguments, for dispatch_ask, dispatch_message,
//     dispatch_edit_ask, dispatch_comment, dispatch_doc_edit, and dispatch_issue when its
//     arguments carry `spec`. Only sessions started before the `dispatch` CLI still have them.
//     They never reach the CLI, so they take no marker.
// Top-level sessions only (`ctx.agent.kind === "main"`); the factory is rebound in every
// subagent, which this check keeps out of review. A task subagent shares its parent's session id,
// so its literal `dispatch` write gets an argv-only marker with no review and no entry, as before
// the CLI asked for one.
//
// The switch is not here: the session's envoy.json turns AskGate off (`askgate.enabled`, read in
// envoy.ts), and with it off nothing is bound. The mode, read for each gated call: OMP_ASKGATE when
// it is `warn` or `block`; any other value is ignored, and the first gated call shows one notice that
// names it and the switch. Else the session's latest `experiments` entry (the experiments
// extension's record): its `askgate_block` arm off is `warn`, and anything else is `block`. `warn`
// runs a revised call and returns the reason as additional context; `block` refuses a revised call
// with the reason as the call's error result. Every note the gate hands back (a warn revise, a
// write that went out unreviewed, a rebuttal, a breaker trip) also rides on the call's own result,
// appended as one text block on its `tool_result` event: the loop's own sink takes additional
// context only for a call the loop dispatched that succeeded, so a failing call, and every call
// an eval cell makes through the tool bridge, would otherwise drop the note. Each entry
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
// usage, the aborted one included. That record is what lets a write out unreviewed: when the
// entry of a call the gate did not review throws, the call is refused in both modes, with a
// reason naming the failure and the switch. A write the model allowed was reviewed, so its own
// record failing does not hold it back. Every path fails open but
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
import * as path from "node:path";
import {
  bytesDigest,
  type FileDigest,
  type FileOperand,
  fileOperands,
  gatedWrite,
  mintMarker,
  operandPath,
  setStoreMode,
  uploadIsDocument,
} from "@legion/envoy-client/askgate-marker";
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
/** How many undelivered notices the gate keeps (`notices`), as it caps unspent rebuttals. */
const NOTICE_CAP = 64;
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
   * `revise`: the call was not passed as sent (a revise verdict; in block mode a call given up
   * without a verdict, a literal call whose file the gate could not read, or a gated write the
   * CLI found no marker for, `unmarked`; in any mode no model role or a gate that did not load). A
   * call was refused exactly when this is `revise` and either `verdictMode` is `block` or
   * `outcome` is `no-model-role` or `load-failed`.
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
    | "load-failed"
    | "unmarked";
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
  /**
   * This process's AskGate marker store (`@legion/envoy-client/askgate-marker`): the directory
   * the session's record names, where the gate mints a marker for each literal call it lets run
   * and records its mode, and where the CLI consumes the marker.
   */
  markerStore: string;
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
/** One block of a tool's result, as the host's `tool_result` event carries it. */
export type ResultBlock = { type: string } & Record<string, unknown>;
/**
 * The `tool_result` event: a handler may give back the call's content, which the runner then
 * returns to whoever called the tool. Oh My Pi emits it for every call, the ones an eval cell
 * makes included, which is what makes it the one surface every caller reads.
 */
export type ToolResultEvent = {
  toolCallId: string;
  details?: unknown;
  content?: readonly ResultBlock[];
};
export type ToolResultResult = { content: ResultBlock[] } | undefined;
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

/** The `on` value of the session's latest `experiments` entry's `askgate_block` arm, if it has one. */
function blockArm(entries: readonly unknown[]): boolean | undefined {
  const latest = entries.findLast(
    (entry) =>
      isRecord(entry) && entry.type === "custom" && entry.customType === EXPERIMENTS_ENTRY_TYPE
  );
  const features = isRecord(latest) && isRecord(latest.data) ? latest.data.features : undefined;
  const arm = isRecord(features) ? features.askgate_block : undefined;
  return isRecord(arm) && typeof arm.on === "boolean" ? arm.on : undefined;
}

/**
 * The mode of a model's revise verdict that the session's latest `experiments` entry sets: `warn`
 * when its `askgate_block` arm is off, else `block` (no such entry, or one that carries no arm,
 * included). This is the verdict's default; the CLI's marker checks have their own (`checkMode`).
 */
export function sessionMode(entries: readonly unknown[]): Mode {
  return blockArm(entries) === false ? "warn" : "block";
}

/**
 * The mode of the checks a call's marker carries: no matching marker, a file changed after the
 * review, a file absent at review. `block` only when the session chose block explicitly
 * (OMP_ASKGATE=block, or with OMP_ASKGATE not a mode, its latest `experiments` entry's arm on);
 * else `warn`, which posts with a warning and an entry the report counts. The gate records it in
 * its store, where the CLI reads it.
 */
export function checkMode(envMode: Mode | undefined, entries: readonly unknown[]): Mode {
  if (envMode !== undefined) return envMode;
  return blockArm(entries) === true ? "block" : "warn";
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

// Where a `dispatch` command's own text ends: `shellTokens` and `dispatchCommandHead` below start
// from the rules `@legion/pi-shared/shell-command` applies (`dispatchCommandHead` there, which the
// Envoy entry uses to tell a dispatch-only call), and differ from them on purpose: the gate reads
// only the literal grammar of one call, a literal path to `dispatch` and the wrappers below
// included, and must read exactly the words the command sends, while the plugin's rule only has to
// tell a dispatch-only call from work. So the two are separate, and a change to one is checked
// against the other. A conservative scan, not a shell parser: whatever it cannot reason about is
// no literal `dispatch` command. Whitespace is bash's blanks, space and tab, never JavaScript's
// `\s`, which also takes a no-break space that bash reads as part of a word.

/** Inside double quotes bash still expands `$` and backticks and interprets `\`. */
const REFUSED_DOUBLE_QUOTED = "$`\\";

/** Characters that end an unquoted word: bash's blanks, a newline and its operator characters. */
const WORD_END = " \t\n|&;()<>";

/** An operator bash reads between commands, longest first. A newline is one too (`"\n"`). */
const OPERATORS = ["&&", "||", ";;", "|&", ";", "&", "|", "(", ")"];

/** A redirect at the start of a word: an optional descriptor number, then the operator. */
const REDIRECT = /^(\d*)(&>>|&>|<<<|<<-|<<|<>|<&|>&|>>|>\||<|>)/;

/** A here-document delimiter in single or double quotes, so the shell expands nothing in its body. */
const QUOTED_DELIMITER = /^(?:'[A-Za-z_][A-Za-z0-9_]*'|"[A-Za-z_][A-Za-z0-9_]*")$/;

/** Characters bash expands in an unquoted word: a parameter, a command, a tilde, a glob. A brace
 * expands only with a comma or `..` in it, so `{` is unread but named no expansion. */
const EXPANDING = "$`~*?[";

/**
 * A word as bash reads it: its text, its value with quotes and line continuations removed, each
 * character AskGate does not take as literal (`unread`), the first expansion in it, and the first
 * line of a double-quoted part over several lines that holds `$`, a backtick or `\`.
 */
type WordToken = {
  kind: "word";
  start: number;
  end: number;
  raw: string;
  value: string;
  unread: string[];
  expansion?: string;
  linesExpansion?: string;
};
type HeredocToken = {
  kind: "heredoc";
  start: number;
  end: number;
  delimiter: string;
  strip: boolean;
  /** The delimiter is one name in single or double quotes. */
  literal: boolean;
  /** The body ended at a line that is the delimiter. */
  closed: boolean;
  bodyStart?: number;
};
type ShellToken =
  | WordToken
  | HeredocToken
  | { kind: "op"; op: string; start: number; end: number }
  | { kind: "redirect"; fd: string; op: string; target?: string; start: number; end: number }
  | { kind: "comment"; start: number; end: number };

/** Where a `$(…)`, `${…}` or backtick expansion that starts at `from` ends, quotes and nesting
 * followed roughly: a mistake here can only move a cause, since any `$` or backtick is unread. */
function expansionEnd(text: string, from: number): number {
  if (text[from] === "`") {
    let index = from + 1;
    while (index < text.length && text[index] !== "`") index += text[index] === "\\" ? 2 : 1;
    return Math.min(index + 1, text.length);
  }
  const open = text[from + 1];
  if (open === "(" || open === "{") {
    const close = open === "(" ? ")" : "}";
    let depth = 0;
    for (let index = from + 1; index < text.length; index++) {
      const char = text[index];
      if (char === "\\") index++;
      else if (char === "'") {
        const end = text.indexOf("'", index + 1);
        index = end === -1 ? text.length : end;
      } else if (char === '"') {
        index++;
        while (index < text.length && text[index] !== '"') index += text[index] === "\\" ? 2 : 1;
      } else if (char === open) depth++;
      else if (char === close && --depth === 0) return index + 1;
    }
    return text.length;
  }
  if (open === "'") {
    let index = from + 2;
    while (index < text.length && text[index] !== "'") index += text[index] === "\\" ? 2 : 1;
    return Math.min(index + 1, text.length);
  }
  const name = /^(?:[A-Za-z_][A-Za-z0-9_]*|[0-9#?$!@*-])/.exec(text.slice(from + 1));
  return from + 1 + (name?.[0].length ?? 0);
}

/** The word at `from`, read as bash reads it, up to an unquoted blank, newline or operator. */
function readWord(text: string, from: number): WordToken {
  let value = "";
  const unread: string[] = [];
  let expansion: string | undefined;
  let linesExpansion: string | undefined;
  const expand = (start: number, end: number) => {
    expansion ??= text.slice(start, end);
  };
  let index = from;
  while (index < text.length) {
    const char = text[index];
    if (WORD_END.includes(char)) break;
    if (char === "\\") {
      // A line continuation: bash removes the backslash and the newline.
      if (text[index + 1] === "\n") {
        index += 2;
        continue;
      }
      unread.push(char);
      value += text[index + 1] ?? "";
      index += 2;
    } else if (char === "'") {
      const end = text.indexOf("'", index + 1);
      if (end === -1) {
        unread.push(char);
        value += text.slice(index + 1);
        index = text.length;
      } else {
        value += text.slice(index + 1, end);
        index = end + 1;
      }
    } else if (char === '"') {
      const start = index;
      let refused = false;
      index++;
      while (index < text.length && text[index] !== '"') {
        const inner = text[index];
        if (REFUSED_DOUBLE_QUOTED.includes(inner)) {
          unread.push(inner);
          refused = true;
          if (inner === "\\") {
            index += 2;
            continue;
          }
          const end = expansionEnd(text, index);
          expand(index, end);
          index = end;
          continue;
        }
        value += inner;
        index++;
      }
      if (index >= text.length) unread.push(char);
      const part = text.slice(start, index);
      if (refused && part.includes("\n")) linesExpansion ??= part.split("\n")[0];
      index++;
    } else if (char === "$" || char === "`") {
      unread.push(char);
      const end = Math.max(expansionEnd(text, index), index + 1);
      expand(index, end);
      value += text.slice(index, end);
      index = end;
    } else {
      if (!LITERAL_UNQUOTED.test(char)) {
        unread.push(char);
        if (EXPANDING.includes(char)) expand(index, index + 1);
      }
      value += char;
      index++;
    }
  }
  const end = Math.min(index, text.length);
  return {
    kind: "word",
    start: from,
    end,
    raw: text.slice(from, end),
    value,
    unread,
    ...(expansion === undefined ? {} : { expansion }),
    ...(linesExpansion === undefined ? {} : { linesExpansion }),
  };
}

/** Reads the body of `doc` from `from`, the start of the line after its opener's line: the body
 * ends at the first line that is exactly the delimiter (leading tabs stripped under `<<-`). */
function readBody(text: string, from: number, doc: HeredocToken): number {
  doc.bodyStart = from;
  for (let start = from; start < text.length; ) {
    const newline = text.indexOf("\n", start);
    const end = newline === -1 ? text.length : newline;
    const line = text.slice(start, end);
    if ((doc.strip ? line.replace(/^\t+/, "") : line) === doc.delimiter) {
      doc.closed = true;
      return newline === -1 ? text.length : newline + 1;
    }
    start = end + 1;
  }
  return text.length;
}

/**
 * A bash command as the shell splits it: words, operators (a newline among them), redirects with
 * their target, here-document openers with their body read after the line that opens them, and
 * comments. A line continuation (backslash, newline) outside single quotes is removed, as bash
 * removes it before it reads words.
 */
function shellTokens(text: string): ShellToken[] {
  const tokens: ShellToken[] = [];
  const pending: HeredocToken[] = [];
  let at = 0;
  while (at < text.length) {
    const char = text[at];
    if (char === " " || char === "\t") {
      at++;
      continue;
    }
    if (char === "\\" && text[at + 1] === "\n") {
      at += 2;
      continue;
    }
    if (char === "\n") {
      tokens.push({ kind: "op", op: "\n", start: at, end: at + 1 });
      at++;
      for (const doc of pending.splice(0)) at = readBody(text, at, doc);
      continue;
    }
    if (char === "#") {
      const newline = text.indexOf("\n", at);
      const end = newline === -1 ? text.length : newline;
      tokens.push({ kind: "comment", start: at, end });
      at = end;
      continue;
    }
    const redirect = REDIRECT.exec(text.slice(at));
    if (redirect !== null) {
      const [whole, fd, op] = redirect;
      let next = at + whole.length;
      while (text[next] === " " || text[next] === "\t") next++;
      const target =
        next < text.length && !WORD_END.includes(text[next]) ? readWord(text, next) : undefined;
      const end = target?.end ?? at + whole.length;
      if (op === "<<" || op === "<<-") {
        const doc: HeredocToken = {
          kind: "heredoc",
          start: at,
          end,
          delimiter: target?.value ?? "",
          strip: op === "<<-",
          literal: fd === "" && target !== undefined && QUOTED_DELIMITER.test(target.raw),
          closed: false,
        };
        tokens.push(doc);
        pending.push(doc);
      } else
        tokens.push({
          kind: "redirect",
          fd,
          op,
          ...(target === undefined ? {} : { target: target.raw }),
          start: at,
          end,
        });
      at = end;
      continue;
    }
    const op = OPERATORS.find((candidate) => text.startsWith(candidate, at));
    if (op !== undefined) {
      tokens.push({ kind: "op", op, start: at, end: at + op.length });
      at += op.length;
      continue;
    }
    const word = readWord(text, at);
    tokens.push(word);
    at = Math.max(word.end, at + 1);
  }
  return tokens;
}

/** A line of a pipe stage after the write that AskGate takes: `head` or `tail`, alone or with a
 * line count (`-n N`, `-nN` or `-N`), every word plain. */
function headOrTail(words: readonly WordToken[]): boolean {
  const [program, ...options] = words.map((word) => (word.raw === word.value ? word.raw : ""));
  if (program !== "head" && program !== "tail") return false;
  const count = /^[0-9]+$/;
  if (options.length === 0) return true;
  if (options.length === 1) return /^-n?[0-9]+$/.test(options[0]);
  return options.length === 2 && options[0] === "-n" && count.test(options[1]);
}

// AskGate's own additions to those rules: the program may be a literal path to `dispatch` as well
// as the bare name, which is all the plugin's rule takes, and the wrappers agents put around a
// `dispatch` command that change neither its words nor what it sends: one leading `cd <path> && `,
// whose path is one word the shell does not expand, one `2>&1`, and one trailing `| head` or
// `| tail` stage, which reads the CLI's output after the CLI has its words.

/** Characters an unquoted literal word (every word of the literal grammar) may hold: none the shell
 * expands (`~`, globs, braces, `$`), interprets (`\`) or reads as an operator. */
const LITERAL_UNQUOTED = /^[A-Za-z0-9._/+@%:,=-]$/;

/** Whether a program word's value names the Dispatch CLI: its last path segment is `dispatch`,
 * or `dispatch.js`, the bundle the plugin's `bin/dispatch` runs with bun. */
const namesCli = (value: string): boolean =>
  ["dispatch", "dispatch.js"].includes(value.split("/").at(-1) ?? "");

/** How many leading words of a command run the Dispatch CLI: 1 for a word that names it, 2 for
 * `bun` and the bundle's path, 0 when they do not run it. */
function cliProgramLength(words: readonly Pick<Word, "value">[]): number {
  const [first, second] = words;
  if (first === undefined) return 0;
  if (namesCli(first.value)) return 1;
  return first.value.split("/").at(-1) === "bun" &&
    second?.value.split("/").at(-1) === "dispatch.js"
    ? 2
    : 0;
}

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

/** The `dispatch` command of a `bash` call in the literal grammar: its words (the program first)
 * with where each starts and ends in the call, where the first starts and the last ends, where a
 * leading `cd` path ends (to silence `cd` in the dry-run), and the spans the dry-run leaves out:
 * a `2>&1` and a `| head` or `| tail` stage, each with the blanks before it, since the dry-run's
 * stdout must be the CLI's JSON alone. `leadingCd` is that `cd`'s path when the `cd` opens the
 * whole call, which is where Oh My Pi's bash tool looks for one to take as the call's directory
 * (`extractLeadingCdTarget` at the pin). */
interface DispatchHead {
  words: Word[];
  at: number;
  end: number;
  cdEnd?: number;
  leadingCd?: string;
  drop: Array<[number, number]>;
  heredoc?: true;
}

const isWord = (token: ShellToken | undefined): token is WordToken => token?.kind === "word";
const isOp = (token: ShellToken | undefined, op: string): boolean =>
  token?.kind === "op" && token.op === op;

function dispatchCommandHead(command: string): DispatchHead | undefined {
  if (hasControlCharacter(command)) return undefined;
  const tokens = shellTokens(command);
  let index = 0;
  while (isOp(tokens[index], "\n")) index++;
  let cdEnd: number | undefined;
  let leadingCd: string | undefined;
  const cd = tokens[index];
  if (isWord(cd) && cd.raw === "cd") {
    const path = tokens[index + 1];
    if (
      !isWord(path) ||
      path.unread.length > 0 ||
      path.value === "" ||
      path.value.startsWith("-") ||
      !isOp(tokens[index + 2], "&&")
    )
      return undefined;
    cdEnd = path.end;
    if (cd.start === 0) leadingCd = path.value;
    index += 3;
  }
  // One `dispatch` command, called by name, by a literal path ending in it or by `bun` and the
  // bundle's literal path, that is no assignment, whose every word the shell takes as written.
  const words: WordToken[] = [];
  for (let token = tokens[index]; isWord(token); token = tokens[++index]) words.push(token);
  if (
    words.length === 0 ||
    words.some((word) => word.unread.length > 0) ||
    ASSIGNMENT.test(words[0].raw) ||
    cliProgramLength(words) === 0
  )
    return undefined;
  // After its words: at most one `2>&1` and one here-document with a quoted delimiter, in either
  // order, then at most one `| head` or `| tail` stage, then the end of the line.
  const drop: Array<[number, number]> = [];
  let stderr = false;
  let heredoc: HeredocToken | undefined;
  for (let token = tokens[index]; token !== undefined; token = tokens[++index]) {
    const before = tokens[index - 1]?.end ?? 0;
    if (
      token.kind === "redirect" &&
      token.fd === "2" &&
      token.op === ">&" &&
      token.target === "1"
    ) {
      if (stderr) return undefined;
      stderr = true;
      drop.push([before, token.end]);
    } else if (token.kind === "heredoc") {
      if (heredoc !== undefined || !token.literal) return undefined;
      heredoc = token;
    } else break;
  }
  if (isOp(tokens[index], "|")) {
    const before = tokens[index - 1]?.end ?? 0;
    const stage: WordToken[] = [];
    for (let token = tokens[++index]; isWord(token); token = tokens[++index]) stage.push(token);
    if (!headOrTail(stage)) return undefined;
    drop.push([before, stage[stage.length - 1].end]);
  }
  // Nothing else runs: only newlines (blank lines) are left, the here-document's body included.
  if (tokens.slice(index).some((token) => !isOp(token, "\n"))) return undefined;
  if (heredoc !== undefined && !heredoc.closed) return undefined;
  const last = words[words.length - 1];
  return {
    words: words.map(({ raw, value, start, end }) => ({ raw, value, start, end })),
    at: words[0].start,
    end: last.end,
    ...(cdEnd === undefined ? {} : { cdEnd }),
    ...(leadingCd === undefined ? {} : { leadingCd }),
    drop,
    ...(heredoc === undefined ? {} : { heredoc: true as const }),
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

/** A word as written and with its quotes removed. */
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
 * from a file, or a name or path that is not literal (an expansion, a glob), or whose literal value
 * makes one by the CLI's own predicate (`uploadIsDocument`, which trims the name as Dispatch does).
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
    !named.literal || !file.literal || uploadIsDocument({ name: named.value, path: file.value })
  );
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
  /** Each file the CLI will read (`fileOperands`, the list its own check reads), in argv order. */
  fileInputs: readonly FileOperand[];
  /** A `--field-file -` needs the call's quoted here-document. */
  stdinFileFlag?: string;
  /** `--path-file` chooses another path after the gate has no way to bind it. */
  unsafeFile?: UnsafeFile;
  /** A `dispatch artifact` upload of a markdown document, reviewed against failure 10 alone. */
  upload?: true;
  /** A leading `cd` changes where relative file operands resolve. */
  changesDirectory?: true;
  /**
   * The call opens with `cd /` (any number of slashes). Oh My Pi's bash tool takes that `cd` as
   * the call's directory when the call names none, and `resolveToCwd` reads a directory of only
   * slashes as the session's own, so the command runs there and not in `/`.
   */
  cdToSessionDirectory?: true;
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
 * Every file operand the CLI will open after the gate returns: `fileOperands`, the one list the
 * gate and the CLI read. `--path-file` is different: its contents choose another path, which would
 * be a new open outside this proof and is refused.
 */
function fileInputs(
  flags: readonly Word[],
  upload: boolean
): { inputs: FileOperand[]; stdinFileFlag?: string; unsafeFile?: UnsafeFile } {
  const values = flags.map((word) => word.value);
  let stdinFileFlag: string | undefined;
  for (let index = 0; index < values.length; index++) {
    const word = values[index] as string;
    const eq = word.indexOf("=");
    const flag = eq === -1 ? word : word.slice(0, eq);
    const source = eq === -1 ? values[index + 1] : word.slice(eq + 1);
    if (upload && flag === "--path-file")
      return {
        inputs: [],
        ...(stdinFileFlag === undefined ? {} : { stdinFileFlag }),
        unsafeFile: {
          flag,
          path: source ?? "",
          reason: "is an indirect path that AskGate cannot read before Dispatch opens it",
        },
      };
    const named = flag.startsWith("--") && flag.endsWith("-file");
    if (source === "-" && (named || (upload && flag === "--path"))) stdinFileFlag ??= flag;
  }
  return {
    inputs: fileOperands([upload ? UPLOAD_COMMAND : "", ...values]),
    ...(stdinFileFlag === undefined ? {} : { stdinFileFlag }),
  };
}

/**
 * The `bash` command as a scoped `dispatch` call, or undefined for any other command. Only the
 * literal grammar of one call is read (`dispatchCommandHead`). A call in any other form is no
 * scoped call: it runs, and the CLI refuses a gated write in it, which carries no marker.
 */
export function scopedCommand(command: unknown): ScopedCommand | undefined {
  if (typeof command !== "string") return undefined;
  const found = dispatchCommandHead(command);
  if (found === undefined) return undefined;
  const { words: all, at, cdEnd, leadingCd, end, heredoc, drop } = found;
  const lead = cliProgramLength(all);
  const program = all.slice(0, lead);
  const [name, ...flags] = all.slice(lead);
  if (name === undefined || !reviewedWrite(name.value, flags)) return undefined;
  const { rebuttal, removed } = splitRebuttal(flags);
  const kept = flags.filter((_, index) => !removed.has(index));
  const words = [...program, name, ...kept];
  const positioned: Word[] = [];
  let cursor = at;
  for (const word of words) {
    positioned.push({ ...word, start: cursor, end: cursor + word.raw.length });
    cursor += word.raw.length + 1;
  }
  const commandHead = words.map((word) => word.raw).join(" ");
  const upload = name.value === UPLOAD_COMMAND;
  const files = fileInputs(positioned.slice(lead + 1), upload);
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
  // it drops the `2>&1`, before or after a here-document opener, and a `| head` or `| tail` stage,
  // so the CLI's JSON is all its stdout holds.
  const before =
    cdEnd === undefined
      ? command.slice(0, at)
      : `${command.slice(0, cdEnd)} >/dev/null${command.slice(cdEnd, at)}`;
  let after = "";
  let from = end;
  for (const [start, stop] of drop) {
    after += command.slice(from, start);
    from = stop;
  }
  after += command.slice(from);
  return {
    tool: `dispatch_${name.value.replaceAll("-", "_")}`,
    label: `dispatch ${name.value}`,
    command: `${command.slice(0, at)}${commandHead}${command.slice(end)}`,
    dryRunScript: `${before}${[...program.map((word) => word.raw), name.raw, "--dry-run", ...kept.map((word) => word.raw)].join(" ")}${after}`,
    ...(rebuttal === undefined ? {} : { rebuttal }),
    readsFile: files.inputs.length > 0,
    fileInputs: files.inputs,
    ...(files.stdinFileFlag === undefined ? {} : { stdinFileFlag: files.stdinFileFlag }),
    ...(unsafeFile === undefined ? {} : { unsafeFile }),
    ...(upload ? { upload: true as const } : {}),
    ...(cdEnd === undefined ? {} : { changesDirectory: true as const }),
    ...(leadingCd !== undefined && /^\/+$/.test(leadingCd)
      ? { cdToSessionDirectory: true as const }
      : {}),
    dirScript: cdEnd === undefined ? "pwd -P" : `${command.slice(0, cdEnd)} >/dev/null && pwd -P`,
  };
}

const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/;

/**
 * The argument vector the Dispatch CLI receives from a `bash` command in the literal grammar: the
 * words after the program, with their quotes removed, as `Bun.argv.slice(2)` holds them. A marker
 * is keyed by it. Undefined for any other command.
 */
export function commandArgv(command: unknown): string[] | undefined {
  if (typeof command !== "string") return undefined;
  const found = dispatchCommandHead(command);
  if (found === undefined) return undefined;
  return found.words.slice(cliProgramLength(found.words)).map((word) => word.value);
}

/** What puts a `bash` call that runs a gated write outside the literal grammar, as the note on its
 * refusal names it, in the order `nonLiteralCause` looks for them. */
export type NonLiteralCause =
  | "lines-expansion"
  | "expansion"
  | "wrapper"
  | "pipe"
  | "commands"
  | "assignment"
  | "redirect"
  | "other";

/** A `dispatch` word: the name, a path that ends in it, or the bundle `dispatch.js`. */
const DISPATCH_WORD = /(^|[\s;&|(`'"=])(?:[^\s;&|()<>`'"]*\/)?dispatch(?:\.js)?(?=$|[\s;&|)`'"])/;
const GATED_SUBCOMMAND = new RegExp(`(?:^|[\\s'"])(?:${SCOPED_COMMANDS.join("|")})(?=$|[\\s'"])`);
/** The operators that join or end commands, which the literal grammar has none of but its own. */
const SEPARATORS: Record<string, true> = {
  "\n": true,
  ";": true,
  "&&": true,
  "||": true,
  "&": true,
  "|": true,
  "|&": true,
  ";;": true,
  "(": true,
  ")": true,
};
const separates = (token: ShellToken | undefined): boolean =>
  token?.kind === "op" && SEPARATORS[token.op] === true;
const isPipe = (token: ShellToken | undefined) => isOp(token, "|") || isOp(token, "|&");

/** Text from the call, on one line and cut, to name in a note. */
const snippet = (text: string) => {
  const line = text.replace(/\s+/g, " ").replaceAll("`", "'").trim();
  return line.length > 60 ? `${line.slice(0, 59)}…` : line;
};

/**
 * Why a `bash` call whose text holds a `dispatch` word and a gated write subcommand is outside the
 * literal grammar, with the call's own text that shows it; undefined for a call in the grammar or
 * one with no such words. A note only: the gate refuses nothing for it. Looked for in this order: a
 * double-quoted value over lines with an expansion in it, an expansion in a value, a program that
 * runs the write (`xargs`, `bash -c`, `eval`, `env`, `timeout`, …), a pipe stage other than one
 * trailing `head` or `tail`, another command (`;`, `&&`, `||`, `&`, a new line), an assignment, a
 * redirect, and anything else.
 */
export function nonLiteralCause(
  command: unknown
): { cause: NonLiteralCause; detail: string } | undefined {
  if (typeof command !== "string") return undefined;
  const found = DISPATCH_WORD.exec(command);
  if (found === null || !GATED_SUBCOMMAND.test(command)) return undefined;
  if (dispatchCommandHead(command) !== undefined) return undefined;
  const position = found.index + found[1].length;
  const tokens = shellTokens(command);
  // The first and the last token of the simple command that holds the token at `at`.
  const bounds = (at: number): [number, number] => {
    let first = at;
    while (first > 0 && !separates(tokens[first - 1])) first--;
    let last = at;
    while (last + 1 < tokens.length && !separates(tokens[last + 1])) last++;
    return [first, last];
  };
  // The program of the simple command that holds the token at `at`, which runs the write.
  const runner = (at: number): { cause: NonLiteralCause; detail: string } => {
    const [first] = bounds(at);
    const program = tokens
      .slice(first, at + 1)
      .find((token): token is WordToken => isWord(token) && !ASSIGNMENT.test(token.raw));
    return program === undefined
      ? { cause: "other", detail: snippet(command) }
      : { cause: "wrapper", detail: snippet(program.raw) };
  };
  const at = tokens.findIndex(
    (token) => token.kind !== "op" && token.start <= position && position < token.end
  );
  if (at === -1) {
    // Inside a here-document's body: the program it feeds runs the write.
    const doc = tokens.findLastIndex(
      (token) => token.kind === "heredoc" && (token.bodyStart ?? Infinity) <= position
    );
    return doc === -1 ? { cause: "other", detail: snippet(command) } : runner(doc);
  }
  const word = tokens[at];
  if (word.kind === "comment") return { cause: "other", detail: "#" };
  // A variable set to the program, which an expansion later runs.
  if (isWord(word) && ASSIGNMENT.test(word.raw) && !namesCli(word.value))
    return { cause: "assignment", detail: `${word.raw.split("=", 1)[0]}=` };
  // Inside a quoted string or an expansion: the program that reads it runs the write.
  if (!isWord(word) || !namesCli(word.value)) return runner(at);
  // The simple command of the write, and a leading `cd <path> &&` the literal grammar allows.
  const [first, last] = bounds(at);
  const own = tokens.slice(first, last + 1);
  let lead = 0;
  while (isOp(tokens[lead], "\n")) lead++;
  const cdWord = tokens[lead];
  const cdPath = tokens[lead + 1];
  const cd =
    first === lead + 3 &&
    isWord(cdWord) &&
    cdWord.raw === "cd" &&
    isWord(cdPath) &&
    isOp(tokens[lead + 2], "&&")
      ? cdPath
      : undefined;
  const values = [...(cd === undefined ? [] : [cd]), ...own.filter(isWord)];
  const lines = values.find((token) => token.linesExpansion !== undefined);
  if (lines?.linesExpansion !== undefined)
    return { cause: "lines-expansion", detail: snippet(lines.linesExpansion) };
  const expanded = values.find((token) => token.expansion !== undefined);
  if (expanded?.expansion !== undefined)
    return { cause: "expansion", detail: snippet(expanded.expansion) };
  const prefix = tokens.slice(first, at).filter(isWord);
  const wrapper = prefix.find((token) => !ASSIGNMENT.test(token.raw));
  if (wrapper !== undefined) return { cause: "wrapper", detail: snippet(wrapper.raw) };
  // A pipe into the write, or a stage after it that is not one trailing `head` or `tail`.
  if (isPipe(tokens[first - 1])) return { ...runner(first - 2), cause: "pipe" };
  let allowedPipe: number | undefined;
  if (isPipe(tokens[last + 1])) {
    const stage: WordToken[] = [];
    let next = last + 2;
    for (let token = tokens[next]; isWord(token); token = tokens[++next]) stage.push(token);
    if (!isOp(tokens[last + 1], "|") || !headOrTail(stage))
      return { cause: "pipe", detail: snippet(stage.map((token) => token.raw).join(" ") || "|") };
    // A `head` or `tail` stage the grammar takes, with a further stage after it.
    if (isPipe(tokens[next])) return { ...runner(next + 1), cause: "pipe" };
    allowedPipe = last + 1;
  }
  // Another command in the call: any operator but the leading `cd`'s `&&`, an allowed trailing
  // pipe, and the new lines before the first command or after the last.
  const other = tokens.find(
    (token, index) =>
      token.kind === "op" &&
      !(cd !== undefined && index === lead + 2) &&
      index !== allowedPipe &&
      (token.op !== "\n" ||
        (tokens.slice(index + 1).some((later) => !isOp(later, "\n")) &&
          tokens.slice(0, index).some((earlier) => !isOp(earlier, "\n"))))
  );
  if (other?.kind === "op")
    return { cause: "commands", detail: other.op === "\n" ? "a new line" : other.op };
  const assignment = prefix.find((token) => ASSIGNMENT.test(token.raw));
  if (assignment !== undefined)
    return { cause: "assignment", detail: snippet(assignment.raw.split("=", 1)[0] + "=") };
  let stderr = 0;
  let docs = 0;
  for (const token of own) {
    if (token.kind === "redirect") {
      if (token.fd === "2" && token.op === ">&" && token.target === "1" && stderr++ === 0) continue;
      return { cause: "redirect", detail: snippet(command.slice(token.start, token.end)) };
    }
    if (token.kind === "heredoc") {
      if (token.literal && docs++ === 0) continue;
      return { cause: "redirect", detail: snippet(command.slice(token.start, token.end)) };
    }
  }
  if (tokens.some((token) => token.kind === "comment")) return { cause: "other", detail: "#" };
  const unread = values.find((token) => token.unread.length > 0);
  return { cause: "other", detail: snippet(unread?.raw ?? command.slice(own[0]?.start ?? 0)) };
}

/** What the CLI prints when it refuses, or in warn mode posts, a gated write that has no marker. */
const UNMARKED_WRITE =
  /AskGate is on for this session(?: in warn mode)?, and this call carries no AskGate marker/;

/** The note a refused write gets: the cause the gate found in the call, and how to send it. */
export function causeNote({ cause, detail }: { cause: NonLiteralCause; detail: string }): string {
  const fix: Record<NonLiteralCause, string> = {
    "lines-expansion": `A double-quoted value on more than one line holds \`$\`, a backtick or \`\\\` (\`${detail}\`), and the shell changes it. Put the text in a here-document with a quoted delimiter: \`--<field>-file - <<'EOF'\`, the text on the lines after it, and \`EOF\` on the last line.`,
    expansion: `The value \`${detail}\` is an expansion: the shell changes it before the CLI gets it. Write the value as literal text. For text that holds \`$\` or a backtick, use single quotes or a here-document with a quoted delimiter (\`--<field>-file - <<'EOF'\`).`,
    wrapper: `\`${detail}\` runs the write, so AskGate cannot read the words that the CLI gets. Make \`dispatch\` the first word of the bash call.`,
    pipe: `The write is in a pipeline with \`${detail}\`. Send the write in a bash call of its own, and do the other step in a different call.`,
    commands: `The bash call has more than one command (${detail === "a new line" ? detail : `\`${detail}\``}). Send the write in a bash call of its own.`,
    assignment: `The call sets a variable (\`${detail}\`). Remove the assignment, and write the value into the command.`,
    redirect: `The call has the redirect \`${detail}\`. Remove it. To send text from a file, use \`--<field>-file <path>\`.`,
    other: `AskGate does not read \`${detail}\`. Write each word as plain text or in quotes.`,
  };
  return `AskGate note: the dispatch CLI found no AskGate marker for this write, because the call is not in the literal form that AskGate reads. ${fix[cause]} Then send it as \`dispatch <command> <words…>\`, in a bash call of its own; the CLI's message above gives the full literal form.`;
}

/**
 * A literal `dispatch` write whose file the gate cannot read: the tool, the command's name, and
 * the flag, the path and why. `inMessage`: the file was missing or not safe to read when the gate
 * ran, which a call earlier in the same message can explain, since the gate reads every call of a
 * message before any of them runs.
 */
export interface UnreadableCommand {
  tool: string;
  label: string;
  file: { flag: string; path: string; reason: string; inMessage?: true };
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

/**
 * A path that names a file descriptor of whichever process opens it: AskGate would resolve it in
 * Oh My Pi's own process and the command in its shell, so the two read different files. The gate
 * refuses it by name, before any `stat`, since with a regular file on that descriptor the `stat`
 * passes and the gate would review and post Oh My Pi's own file (census, contract 2). The name is
 * not the whole check: `resolveForOpen` refuses such a path however the operand spells it.
 */
const PROCESS_FD_PATH =
  /^\/dev\/fd\/|^\/dev\/(stdin|stdout|stderr)$|^\/proc\/(self|thread-self|\d+)\/fd\/|^\/proc\/\d+\/task\/\d+\/fd\//;

/**
 * The directories whose files name whatever the process that opens them holds, not bytes on a
 * disk: a path that reaches one is refused however it spells it (a symlink to `/proc/self/fd/0`,
 * `/proc/thread-self/fd/0`, `/dev/fd/0`).
 */
const MAGIC_DIRECTORIES = ["/proc", "/dev"];
/** How many symbolic links one path may go through, as the kernel's own limit (ELOOP). */
const MAX_LINKS = 40;

/**
 * The file `source` reaches, resolved as the kernel resolves it for the CLI's own open: each
 * component in turn, a symbolic link followed where it is found (so a `..` after a link leaves
 * the link's target, which `path.resolve` would collapse first). A component that reaches `/proc`
 * or `/dev` throws: such a file names a descriptor of the process that opens it, and AskGate's
 * process is not the command's shell.
 */
function resolveForOpen(source: string): string {
  let resolved = "";
  const rest = source.split("/").reverse();
  for (let links = 0; rest.length > 0; ) {
    const part = rest.pop() as string;
    if (part === "" || part === ".") continue;
    if (part === "..") {
      resolved = resolved.slice(0, Math.max(0, resolved.lastIndexOf("/")));
      continue;
    }
    const candidate = `${resolved}/${part}`;
    if (MAGIC_DIRECTORIES.includes(candidate))
      throw new Error(
        `reaches ${candidate}, whose files name a file descriptor of the process that opens it, which here is Oh My Pi's own and not the command's shell`
      );
    const stats = fs.lstatSync(candidate);
    if (!stats.isSymbolicLink()) {
      resolved = candidate;
      continue;
    }
    if (++links > MAX_LINKS) throw new Error("goes through too many symbolic links");
    const target = fs.readlinkSync(candidate);
    if (target.startsWith("/")) resolved = "";
    rest.push(...target.split("/").reverse());
  }
  return resolved === "" ? "/" : resolved;
}

/**
 * The bytes of one file the CLI will read, read in place and only from a regular file. The source
 * is resolved as the kernel resolves it (`resolveForOpen`), which refuses a descriptor path
 * whatever names it; the open is `O_NOFOLLOW`, which guards the last path component only, so a
 * link put in the file's place after that walk is refused and never followed (a directory on the
 * path swapped for a link after the walk is followed, which needs a concurrent process of the
 * same user); and `O_NONBLOCK` with an `fstat` on the open descriptor refuses a FIFO, a
 * device, a directory or anything else swapped in, before the first read.
 */
function readReviewedFile(source: string): Buffer {
  const input = fs.openSync(
    resolveForOpen(source),
    fs.constants.O_RDONLY | fs.constants.O_NONBLOCK | fs.constants.O_NOFOLLOW
  );
  try {
    if (!fs.fstatSync(input).isFile()) throw new Error("is not a regular file AskGate can read");
    return fs.readFileSync(input);
  } finally {
    fs.closeSync(input);
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
/**
 * A `dispatch` command's arguments are read by running that command with `--dry-run`, so the
 * program the call names has run once by the time a revise comes back. The refusal says so: for
 * a checkout's own `./dispatch`, "nothing ran" would not be true.
 */
const DRY_RUN_RAN =
  "To read this call AskGate ran the command it names with `--dry-run`, which sends nothing; the write itself was not sent.";
const renderRevise = (reason: string, second: boolean, override: Override) =>
  `AskGate did not send this call.\n${agentFacing(reason)}\nSend the corrected call, or resend this one unchanged with ${override.how} to override; an override is always sent and recorded, and the ${override.what} is removed before sending.${override.what === "flag" ? `\n${DRY_RUN_RAN}` : ""}${override.note ? `\n${override.note}` : ""}${second ? `\n${SECOND_REVISE}` : ""}`;
/** The refusal block mode returns for a call given up without a verdict, by cause. */
const NO_VERDICT_REFUSAL: Record<"abandoned" | "shutdown", string> = {
  abandoned:
    "AskGate did not send this call: it was stopped before a verdict came back. Send it again if it is still wanted.",
  shutdown:
    "AskGate did not send this call: the session shut down before a verdict came back. Send it again from a live session if it is still wanted.",
};
/** The refusal for a call in a session where no model role the gate asks resolves. */
const NO_MODEL_ROLE_REFUSAL = `AskGate did not send this call: it reviews each Dispatch write before it posts, and this session has no model for it. Set modelRoles.askgate or modelRoles.default in the Oh My Pi settings, then send the call again. To turn AskGate off for this session, set askgate.enabled to false in the session's envoy.json.`;
/**
 * The refusal for a write that no review stands behind and whose record could not be written.
 * Every such write is recorded, so that a reader of the session knows what went out unreviewed;
 * with no record the write does not go.
 */
const recordFailureRefusal = (message: string, configFile: string) =>
  `AskGate did not send this call: no review stands behind it, and AskGate could not record that in the session (${message}). Every Dispatch write that posts unreviewed is recorded, so AskGate holds this one back. Send it again once the session takes its entries. To turn AskGate off for this session, set askgate.enabled to false in ${configFile}.`;
/** The refusal for a call whose marker could not be minted: the CLI would refuse it with none. */
const mintFailureRefusal = (message: string, configFile: string) =>
  `AskGate did not send this call: the dispatch CLI posts a Dispatch write in this session only with a marker AskGate mints for it, and AskGate could not mint one (${message}). Send the call again once the CLI's state directory takes files. To turn AskGate off for this session, set askgate.enabled to false in ${configFile}.`;
/**
 * The refusal, in block mode, of a literal call whose final command gives the gate no argument
 * vector to key a marker by: the CLI would refuse it with no marker, naming a form it already has.
 */
const UNKEYED_REFUSAL =
  "AskGate did not send this call: it could not read the arguments the dispatch CLI gets from the command that runs, so it could not mint the marker the CLI needs to post the write. Send the write again as one bash call of literal words.";
/** The note, in warn mode, of the same call: it runs, and the CLI posts it with a warning. */
const UNKEYED_NOTE = `<advisor-gate advisor="AskGate" outcome="unkeyed">\nAskGate minted no marker for this Dispatch write: it could not read the arguments the dispatch CLI gets from the command that runs. The call ran, and the CLI posts it with a warning that it went out unreviewed.\n</advisor-gate>`;
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
  // Given up in warn mode (`givenUp`). The note still reaches an agent whose call goes on: an eval
  // cell that keeps running after the user's stop reads it on its call's own result.
  abandoned: "its call or its run ended before the review answered",
  shutdown: "the session shut down before the review answered",
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
/**
 * What a literal `dispatch` write whose file the gate cannot read tells the agent. In `block`
 * mode the write was not sent, and this is the call's refusal. In `warn` mode the call ran with no
 * review, and this is its note. A file missing or not safe to read when the gate ran
 * (`inMessage`) can be one a call earlier in the same message writes: Oh My Pi runs the gate for
 * every call of a message before any of them runs, so the text says to send the call again in a
 * later message.
 */
function renderUnreadable({ file }: UnreadableCommand, mode: Mode): string {
  const ran = mode === "warn";
  const { stdin, regular } = resendFlags(file.flag);
  // The file refusals come after AskGate read the call: to find where a relative operand resolves
  // it ran the command's own `cd` and `pwd -P`, and for some calls the command with `--dry-run`.
  // Neither sends anything, and nothing else in the call ran, so the claim here says that and not
  // "nothing ran".
  const read = ran
    ? "The call ran unreviewed."
    : "The write was not sent. To read the call AskGate ran at most the command's own `cd` and `pwd -P`, and the command with `--dry-run`, which send nothing; nothing else in the call ran.";
  const later = file.inMessage
    ? " AskGate reads each file a write posts before any call of the same message runs. If a call earlier in this message writes or changes the file, send this call again in a later message. Else send"
    : " AskGate reads and posts only a regular file. Send";
  const body = `AskGate ${ran ? "did not review" : "did not send"} this call: \`${file.flag}\` names ${JSON.stringify(file.path)}, which ${file.reason}.${later} the text with \`${stdin} -\` and a quoted here-document (\`<<'EOF'\`), or name a regular file with ${regular}. ${read}`;
  return ran
    ? `<advisor-gate advisor="AskGate" outcome="unreadable">\n${body}\n</advisor-gate>`
    : body;
}

/**
 * The gate of a session where AskGate did not load: a host module that did not import, an
 * OMP_ASKGATE_TIMEOUT_MS that is no positive integer, or a handler ceiling too short for a
 * verdict. Only `askgate.enabled: false` turns AskGate off, so none is an off switch: each literal
 * write of the main agent the gate would review (`scopedCommand`, `scopedDevice`) is refused in
 * both modes, naming the error and the switch, and recorded as `load-failed`. It mints no marker,
 * so the CLI refuses every other gated write of a process that acts as the session, a subagent's
 * included, since the session's record stays live. Every other call runs.
 */
export function loadFailureGate(
  error: unknown,
  deps: Pick<Deps, "env"> & { configFile: string },
  pi: Pick<Pi, "appendEntry">
): (event: ToolCallEvent, ctx: GateCtx) => ToolCallResult {
  const message = messageOf(error);
  const reason = `AskGate did not send this call: it reviews each Dispatch write before it posts, and it did not load: ${message}. Send the call again once AskGate loads. To turn AskGate off for this session, set askgate.enabled to false in ${deps.configFile}.`;
  const envMode = parseMode(deps.env.OMP_ASKGATE);
  return (event, ctx) => {
    if (ctx.agent.kind !== "main") return undefined;
    let found: { tool: string; path: string } | undefined;
    if (event.toolName === "bash") {
      const write = scopedCommand(event.input.command);
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
    /**
     * The markers minted for each call (`mintFor`), by tool call id: each goes once its call ends,
     * is refused, or the session shuts down. A marker the CLI consumed is gone already, and its
     * release is a no-op.
     */
    const markers = new Map<string, string>();
    const releaseMarker = (toolCallId: string) => {
      const file = markers.get(toolCallId);
      if (file === undefined) return;
      markers.delete(toolCallId);
      fs.rmSync(file, { force: true });
    };
    /** The mode last written to the store, so the file is rewritten only when it changes. */
    let storeMode: Mode | undefined;
    const recordMode = (mode: Mode) => {
      if (storeMode === mode) return;
      setStoreMode(deps.markerStore, mode);
      storeMode = mode;
    };
    /**
     * What each gated call is owed back: the note of a write that went out unreviewed, a warn
     * mode revise, a rebuttal or a breaker trip, by tool call id. The `tool_call` result's
     * `additionalContext` reaches the agent only through the loop's own sink, which a call made
     * from an eval cell never has (the bridge calls the tool with no loop context), so the note
     * is put on the call's own result as well, where every caller reads it. Delivered once, and
     * at most NOTICE_CAP are kept, since a call that never reports a result leaves its own.
     */
    const notices = new Map<string, string>();
    const rememberNotice = (toolCallId: string, notice: string) => {
      notices.set(toolCallId, notice);
      if (notices.size > NOTICE_CAP) notices.delete(notices.keys().next().value as string);
    };
    const deliverNotice = (event: ToolResultEvent): ToolResultResult => {
      const notice = notices.get(event.toolCallId);
      if (notice === undefined) return undefined;
      notices.delete(event.toolCallId);
      return { content: [...(event.content ?? []), { type: "text", text: notice }] };
    };
    /**
     * The cause each `bash` call outside the literal grammar that runs a gated write has, as the
     * note its result gets (`causeNote`), by tool call id. The gate refuses nothing for it: the
     * CLI refuses such a write, and cannot see the shell text that put it outside the grammar.
     * So the note goes on the call's result only when that result holds the CLI's refusal (or
     * warn mode's warning) of a write with no marker. At most NOTICE_CAP are kept.
     */
    const causes = new Map<string, string>();
    const rememberCause = (toolCallId: string, command: unknown) => {
      try {
        const found = nonLiteralCause(command);
        if (found === undefined) return;
        causes.set(toolCallId, causeNote(found));
        if (causes.size > NOTICE_CAP) causes.delete(causes.keys().next().value as string);
      } catch {
        // The note is help, not a check: a call it cannot read runs as it would without it.
      }
    };
    const deliverCause = (event: ToolResultEvent, result: ToolResultResult): ToolResultResult => {
      const note = causes.get(event.toolCallId);
      if (note === undefined) return result;
      causes.delete(event.toolCallId);
      const content = result?.content ?? [...(event.content ?? [])];
      const output = content.map((block) => (typeof block.text === "string" ? block.text : ""));
      if (!UNMARKED_WRITE.test(output.join("\n"))) return result;
      return { content: [...content, { type: "text", text: note }] };
    };
    /**
     * The session changed, so nothing of the one before it stands: its halt, its count, its
     * revise reasons, its unspent rebuttals and the notes its calls are owed. Its markers stay: a
     * call whose own result has not come, or a job still in the background, may not have run its
     * CLI yet. Each goes on its own call's end event below, or at shutdown.
     */
    const reset = () => {
      rebuttals.clear();
      breaker.clear();
      notices.clear();
      causes.clear();
      failures = 0;
      halted = false;
    };
    pi.on("session_switch", reset);
    pi.on("session_branch", reset);
    // A marker goes only once the command that consumes it has run, whichever session is current
    // then. A result that reports a backgrounded job's start comes before the shell runs it
    // (`startedInBackground`), and nothing later names that call's id, so its marker is kept until
    // `session_shutdown`; each mint sweeps the markers older than MARKER_MAX_AGE_MS.
    pi.on("tool_execution_end", (event: { toolCallId: string; result?: unknown }) => {
      inflight.get(event.toolCallId)?.("abandoned");
      if (isRecord(event.result) && startedInBackground(event.result.details)) return;
      releaseMarker(event.toolCallId);
    });
    pi.on("tool_result", (event: ToolResultEvent): ToolResultResult => {
      if (!startedInBackground(event.details)) releaseMarker(event.toolCallId);
      return deliverCause(event, deliverNotice(event));
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
      const { promise: waited, resolve: stopWaiting } = Promise.withResolvers<void>();
      const timer = setTimeout(stopWaiting, SHUTDOWN_WAIT_MS);
      await Promise.race([Promise.allSettled([...handling]), waited]);
      clearTimeout(timer);
      // After the wait, so a call that settled during it leaves no marker behind.
      for (const toolCallId of markers.keys()) releaseMarker(toolCallId);
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
     * went out unreviewed and why, also `abandoned` and `shutdown`, whose note reaches the agent
     * only where its call goes on after its tool, run or session ended.
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
     * A literal call whose file the gate cannot read: a judgment on one call's file, not a gate
     * that failed. `block` mode refuses it before it sends, so the agent sends the text in a form
     * AskGate reads; `warn` mode runs it and hands the same explanation back as context, with the
     * entry recording `unreadable` and `allow`. Neither counts toward the halt.
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
      // Before any path that lets the write out: a session with no model role refuses in both
      // modes, and a rebuttal, a halt, a breaker trip or a shutdown is no exception, since none
      // of them is a review and the gate cannot review anything at all here.
      if (resolveRole(ctx) === undefined) return noModelRole;
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
     * A scoped command's arguments as the CLI's own --dry-run prints them (`args`), what the model
     * reviews (`review`: those arguments, and an upload's `--path` text as `content`), and each
     * file the argv names with the digest of the bytes the gate read; or why there are none; or
     * `unscoped`: an upload whose resolved name and path make no markdown document.
     */
    type DryRun =
      | { args: Input; review: Input; files: FileDigest[] }
      | { failure: string }
      | { unreadable: UnreadableCommand }
      | { cause: "abandoned" | "shutdown" }
      | { unscoped: true };
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
      // Oh My Pi reads no CDPATH where a call's `cd` goes: its bash tool takes a leading
      // `cd X &&` into the call's directory (`resolveToCwd`), and its shell's `cd` builtin has no
      // CDPATH search. So the dry run runs without it, and its `cd` reaches the directory the
      // command reaches.
      const { CDPATH: _cdpath, ...env } = deps.env;
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
            env,
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

      // Each file the argv names, read here in place, as the CLI will open it: the model reviews
      // these bytes, and the marker binds their digests, which the CLI checks before it posts.
      const files: FileDigest[] = [];
      let uploadText: string | undefined;
      if (scoped.fileInputs.length > 0) {
        const relativeFile = scoped.fileInputs.find((file) => !path.isAbsolute(file.operand));
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
                  path: relativeFile.operand,
                  reason: `could not resolve the tool directory: ${messageOf(error)}`,
                },
              },
            };
          }
        }
        // A leading `cd /` in a call that names no directory runs in the session's own directory
        // (`cdToSessionDirectory`), which `base` already is; any other `cd` is the probe's.
        const hostDirectory = scoped.cdToSessionDirectory === true && raw === undefined;
        const firstRelative =
          scoped.changesDirectory === true && !hostDirectory ? relativeFile : undefined;
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
                  path: firstRelative.operand,
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
                  path: firstRelative.operand,
                  reason: "could not determine an absolute command directory",
                },
              },
            };
        }

        // Refused by name, before any `stat`: such a path resolves in whichever process opens it.
        const descriptor = scoped.fileInputs.find(
          (file) =>
            PROCESS_FD_PATH.test(file.operand) || PROCESS_FD_PATH.test(operandPath(base, file))
        );
        if (descriptor !== undefined)
          return {
            unreadable: {
              tool: scoped.tool,
              label: scoped.label,
              file: {
                flag: descriptor.flag,
                path: descriptor.operand,
                reason:
                  "names a file descriptor of the process that opens it, which here is Oh My Pi's own and not the command's shell",
                inMessage: true,
              },
            },
          };
        for (const file of scoped.fileInputs) {
          let bytes: Buffer;
          try {
            bytes = readReviewedFile(operandPath(base, file));
          } catch (error) {
            return {
              unreadable: {
                tool: scoped.tool,
                label: scoped.label,
                file: {
                  flag: file.flag,
                  path: file.operand,
                  reason:
                    (error as NodeJS.ErrnoException).code === "ENOENT"
                      ? "does not exist"
                      : `could not be read: ${messageOf(error)}`,
                  inMessage: true,
                },
              },
            };
          }
          files.push({ ...file, sha256: bytesDigest(bytes) });
          if (file.flag === "--path") uploadText = bytes.toString("utf8");
        }
      }

      const dry = await stdoutOf(scoped.dryRunScript, `${scoped.label} --dry-run`);
      if (!("stdout" in dry)) return dry;
      let args: Input | undefined;
      try {
        const parsed: unknown = JSON.parse(dry.stdout);
        if (isRecord(parsed)) args = parsed;
      } catch {
        // Not JSON: reported below.
      }
      if (args === undefined) {
        const printed = clipBytes((dry.stdout || dry.stderr).trim(), GATE_REASON_MAX_BYTES);
        return { failure: `${scoped.label} --dry-run printed no JSON object: ${printed}` };
      }
      // The CLI's own predicate, over the arguments it will parse: an upload that is no markdown
      // document is no gated write, and needs neither a review nor a marker.
      if (!gatedWrite(scoped.tool, args)) return { unscoped: true };
      // An upload's dry run prints its `--path`, not its text: the model reviews the bytes the
      // gate read from that path, which the marker binds and the CLI uploads.
      if (!scoped.upload || typeof args.path !== "string" || args.content !== undefined)
        return { args, review: args, files };
      if (uploadText === undefined)
        return {
          unreadable: {
            tool: scoped.tool,
            label: scoped.label,
            file: { flag: "--path", path: args.path, reason: "was not read before review" },
          },
        };
      return { args, review: { ...args, content: uploadText }, files };
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

    /**
     * Mints the marker the CLI consumes for a literal call that will run: keyed by the argument
     * vector of the command that runs, the final one the gate returns, bound to the dry run's
     * arguments and the digests of the files the gate read when a review or a deliberate pass
     * stands behind them (`bound`), argv-only otherwise. Undefined when it was minted; else the
     * refusal of a mint that failed, since the CLI would refuse the call anyway. A command whose
     * argument vector the gate cannot read gets no marker: `block` mode refuses it, and `warn`
     * mode lets it run with a note, as the CLI then posts it with a warning.
     */
    const mintFor = (
      toolCallId: string,
      command: unknown,
      bound: { args: Input; files: readonly FileDigest[] } | null,
      outcome: string,
      mode: Mode = envMode ?? "warn"
    ): ToolCallResult => {
      const argv = commandArgv(command);
      if (argv === undefined)
        return mode === "block"
          ? { block: true, reason: UNKEYED_REFUSAL }
          : { additionalContext: UNKEYED_NOTE };
      try {
        markers.set(
          toolCallId,
          mintMarker(
            deps.markerStore,
            { argv, args: bound?.args ?? null, files: bound?.files ?? [], toolCallId, outcome },
            deps.now()
          )
        );
        return undefined;
      } catch (error) {
        return { block: true, reason: mintFailureRefusal(messageOf(error), deps.configFile) };
      }
    };

    const handleCall = async (event: ToolCallEvent, ctx: GateCtx): Promise<ToolCallResult> => {
      const started = deps.now();
      let base: Base | undefined;
      let decision: Decision;
      // A `dispatch` command with its override flag removed, which runs whatever the gate decides.
      let revised: Input | undefined;
      // What the call's dry run printed and the digests of the files the gate read, which a
      // marker for the command it ran binds.
      let dryRun: { args: Input; files: readonly FileDigest[] } | undefined;
      // OMP_ASKGATE when it is a mode; else this session's latest experiments entry, read per call
      // because one binding serves every session the process switches to.
      let mode: Mode = envMode ?? "block";
      // The mode of the checks the CLI makes on this call's marker, and of a file the gate could not
      // read: block only when the session chose it (`checkMode`).
      let checks: Mode = envMode ?? "warn";
      try {
        if (event.toolName === "bash") rememberCause(event.toolCallId, event.input.command);
        // A task subagent's literal write is neither reviewed nor recorded, as before the CLI
        // checked markers; it shares its parent's session, so it gets an argv-only marker.
        if (ctx.agent.kind !== "main")
          return event.toolName === "bash" && scopedCommand(event.input.command) !== undefined
            ? mintFor(event.toolCallId, event.input.command, null, "subagent")
            : undefined;
        if (event.toolName === "write") return stripRebuttal(event);
        if (envMode === undefined) {
          const entries = ctx.sessionManager.getEntries();
          mode = sessionMode(entries);
          checks = checkMode(envMode, entries);
        }
        // The CLI reads the check mode from the store: every shell or cell could run it.
        if (event.toolName === "bash" || event.toolName === "eval") {
          try {
            recordMode(checks);
          } catch {
            // A store that takes no mode leaves the CLI's default, warn.
          }
        }
        // Only a `bash` call in the literal grammar is read. Any other call runs, and the CLI
        // refuses a gated write in it, which carries no marker.
        if (event.toolName === "bash") {
          const scoped = scopedCommand(event.input.command);
          if (scoped === undefined) return undefined;
          if (scoped.rebuttal !== undefined) {
            revised = { ...event.input, command: scoped.command };
          }
          base = { ...identity(event, mode), tool: scoped.tool, path: scoped.label };
          // Before the dry run and before any path that lets the write out: with no model role
          // the gate can review nothing, and every one of those paths would otherwise allow.
          if (resolveRole(ctx) === undefined) decision = noModelRole;
          else {
            const run = await readArgs(scoped, event.input, ctx, event.toolCallId);
            // An upload its dry run shows is no markdown document passes as an ungated call
            // does, with no entry and no marker, which the CLI needs for no such upload; an
            // override flag on it is still removed, since the CLI would refuse it.
            if ("unscoped" in run) return revised === undefined ? undefined : { input: revised };
            if ("cause" in run) decision = givenUp(mode, run.cause);
            else if ("unreadable" in run) {
              if (halted) decision = pass("halted");
              else if (ctx.model?.provider !== resolveRole(ctx)?.model.provider)
                decision = pass("skipped", {
                  reason: `primary on ${ctx.model?.provider ?? "no model"}`,
                });
              else decision = unreadableCall(run.unreadable, checks);
            }
            // The CLI refuses arguments it cannot parse before it sends anything, so the
            // command runs.
            else if ("failure" in run) decision = pass("error", { reason: run.failure });
            else {
              dryRun = { args: run.args, files: run.files };
              const key = breakerKey(scoped.tool, run.review);
              base = {
                ...base,
                revisesForKey: breaker.reasons(key).length,
                argsDigest: createHash("sha256").update(JSON.stringify(run.review)).digest("hex"),
              };
              decision = await decide(
                { toolName: scoped.tool, toolCallId: event.toolCallId, input: run.review },
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
        // verdict, so it does not count toward the halt.
        decision = pass("error", { reason: messageOf(error) });
      }
      // Settled once and recorded once, each on its own, so neither failing can repeat the other.
      try {
        settle(decision.effect, ctx);
      } catch {
        // The halt notice is best effort.
      }
      if (decision.result?.block) revised = undefined;
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
      } catch (error) {
        // A write no review stands behind posts only when the record of that is written: the
        // record is the whole account of what went out unreviewed. So a record that fails
        // refuses the call, in both modes. A write the model allowed was reviewed, and still
        // posts; a call already refused keeps its own refusal.
        if (decision.fields.decision === "allow" && decision.fields.outcome !== "verdict") {
          notices.delete(event.toolCallId);
          return {
            block: true,
            reason: recordFailureRefusal(messageOf(error), deps.configFile),
          };
        }
      }
      // The marker the CLI needs to post a literal write that will run, minted once its record is
      // written (a write whose record failed is refused above). A review, or a pass the gate took
      // over the dry run's arguments (a rebuttal, a breaker trip, a halt, another provider), binds
      // those arguments and the files' digests; a call no verdict stands behind is argv-only.
      if (event.toolName === "bash" && base !== undefined && !decision.result?.block) {
        const { outcome } = decision.fields;
        const bound =
          dryRun !== undefined && !UNREVIEWED_OUTCOMES.includes(outcome) ? dryRun : null;
        const minted = mintFor(
          event.toolCallId,
          (revised ?? event.input).command,
          bound,
          outcome,
          checks
        );
        if (minted?.block) {
          notices.delete(event.toolCallId);
          return minted;
        }
        if (minted?.additionalContext !== undefined)
          decision.result = {
            ...decision.result,
            additionalContext: [decision.result?.additionalContext, minted.additionalContext]
              .filter((note) => note !== undefined)
              .join("\n"),
          };
      }
      // The same note goes on the call's own result (`deliverNotice`), which every caller reads:
      // a call an eval cell made has no loop sink for `additionalContext`, and a note the agent
      // never sees reads like a gate that is working.
      if (decision.result?.additionalContext !== undefined)
        rememberNotice(event.toolCallId, decision.result.additionalContext);
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
