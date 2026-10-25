// AskGate: before a root session's scoped Dispatch write executes, one model call judges it
// against AskGate's charter (rules.md) and answers `allow` or `revise`, and a call that opens a
// new ask for a person also gets the rules check (`rules-check.ts`, below). `askgate.ts` is the
// entry that binds Oh My Pi's completion call and buildSessionContext; the new asks a call opens
// are read in `new-asks.ts` and the text both checks share is in `text.ts`; everything else lives
// here so the tests drive it through a fake `pi`, a fake `ctx` and stubs.
//
// Scope: eight calls on the command line and six of them as devices, each recorded under one tool
// name (dispatch_ask, …).
//   - A `bash` call whose whole command is one `dispatch` command (`dispatchCommandHead`: the
//     head rules of `@legion/pi-shared/shell-command`, kept here on purpose with AskGate's own
//     additions: the program taken by a literal path to `dispatch` as well as by name, and two
//     harmless wrappers, one leading `cd <path> &&` with a path the shell does not expand and one
//     `2>&1` at the end of its first line, before or after a here-document opener)
//     naming `ask`, `message`, `edit-ask`, `comment`, `doc-edit`, `request-approval` (its
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
//     reviewed. An upload given with `--path` prints the path, not the text: the gate reads that
//     file (`readFile`) and adds its text as `content`, so both forms reach the model alike. The
//     arguments are cut at 64 KiB as one JSON block, so the document gets less than 64 KiB of it.
//     A relative path resolves where the CLI resolves it, against its
//     `process.cwd()`, the physical directory the command runs in after its own `cd`: bash names
//     it (the `cd`, then `pwd -P`, run as the dry run runs). A file the
//     gate cannot read is `error`, and the call runs. An upload is judged against failure 10 alone
//     (askgate-upload.md), since a plan or a report is long by nature.
//   - A `bash` call those head rules do not take but whose one scoped write the composite reader
//     reads (`compositeScoped`): around the write only `\`-newline continuations, the write's own
//     output redirects (`2>&1`, `>`/`2>` to a literal path) and later pipeline stages that hold no
//     `dispatch` word (`| head -3`), literal `NAME=value` prefixes and one `timeout <duration>`,
//     a literal file as its stdin (`< q.md`), and before it, joined by `&&`, `;` or a newline,
//     `cd <literal path>`, literal assignments (`export A=B`), read-only commands (`wc`, `cat`,
//     `ls`, `grep`, `head`, `[` …) and `dispatch` calls that are no reviewed write (`claim`,
//     `read` …); after it, anything that holds no reviewed write. `$NAME` in the write counts
//     when an earlier statement sets NAME to a literal, and a `~/` program path is read too. The
//     dry run keeps the `cd` and assignment statements, the prefixes and the stdin, and drops
//     the rest. The call itself runs as written.
//   - A `bash` call that holds a scoped `dispatch` write neither reader takes — after a command
//     that may change a file or the text (`python3 - <<'EOF' …`, `sed -i … f`, `cat > f <<EOF`,
//     `make`), with a command or process substitution (`"$(cat note.md)"`, `< <(awk …)`), sent to
//     the background (`&`), beside a second reviewed write, with an unquoted here-document or a
//     variable the call does not set, or calling `dispatch` by a path the shell expands
//     (`$HOME/…/dispatch`) — or a scoped subcommand run by a program an expansion names (`$D
//     comment …`, `"${D}" ask …`, `$(command -v dispatch) …`), which may be `dispatch`, is
//     refused before anything in it runs, whatever the mode, with a reason telling the agent to
//     send that `dispatch` command again on its own; when the word that runs it is what AskGate
//     cannot read, the reason names the characters in it and says to call `dispatch` by name
//     (`unreadableDispatchWrite`, a scan aware of quotes, here-documents and command
//     substitutions, for a scoped write at a command position). The entry reads `unreadable`; it
//     never counts toward the halt. A halt, or a session the gate would skip (a primary on
//     another provider), lets it run; a session with no model role refuses it.
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
// Deadline: OMP_ASKGATE_TIMEOUT_MS, or 90 s, clamped under the runner's own ceiling for a
// tool_call handler (extensionHandlers.toolCallTimeoutMs, 30 s unless configured) by the two stops
// the handler still pays once that deadline passes — the aborted call's usage report and the
// decision record — and raced from handler entry. A ceiling that leaves under 10 s, and an
// OMP_ASKGATE_TIMEOUT_MS that is no positive integer, throw when the gate binds: the entry then
// refuses each write the gate would review (`loadFailureGate`), since only `askgate.enabled:
// false` turns AskGate off.
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
//   - breaker: two refusals on one target (the device plus its issue/ask/artifact/project
//     values) send the third attempt unasked, and say so; the second refusal already warns that
//     it will. Only a refusal counts: a warn-mode charter revise sends the call, so it never
//     advances the breaker, while a rules match refuses in either mode and always does. The pass
//     covers the thing that was refused and nothing else: a target whose refusals named no ask
//     (the call as a whole was refused), or a call opening one of the asks the rules check
//     refused there. Any other call is reviewed as usual, its count untouched. An allow, a
//     rebuttal or a trip resets the target.
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
//   - rules: a call that opens a new ask for a person gets a second model call beside the charter
//     call, on the same chain, deadline, transcript and stop, which judges only those asks and
//     only against two rules (rule-check.md): an ask whose own recommendation is to change
//     nothing, and an ask for the person's hands where the agent did not try the access it has.
//     A new ask (`new-asks.ts`) is a `dispatch ask`, each `:::ask{` opener a `dispatch doc-edit`
//     insert, a `dispatch issue` spec or a markdown `dispatch artifact` upload writes, and each
//     block a doc-edit retypes into an ask. For a retype the gate reads the block's own text from
//     Dispatch, and for an upload the ids the version it replaces already holds, so a re-upload
//     judges only the asks it adds; a read that fails sets `ruleTextMissing` and the ask is judged
//     without it. The system prompt is rule-check-system.md and rule-check.md, and the request
//     (rule-check-request.md) carries the same escaped system prompt and transcript, then each ask
//     in an `<ask>` tag of its own, the list bounded at 32 KiB and each ask at 8 KiB
//     (`ruleAsksCut`). When the answer names an ask that matches `no_change` or `untried_access`,
//     AskGate refuses the call in every mode, naming each matching ask and putting the charter's
//     revise reason after the rules reason. A rules call that fails, times out or gives no verdict
//     changes nothing; three of those in a row write one log line, and show one UI notice where
//     there is a UI. A rules verdict, a session_switch or a session_branch resets that count. The
//     rules reason never feeds the charter request, and only the charter call moves the halt
//     count. The entry adds `rule`, `ruleAsk`, `ruleQuote`, `ruleOutcome`, `ruleError`,
//     `ruleModel`, `ruleLatencyMs`, `ruleTextMissing` and `ruleAsksCut`, and its `usage` and
//     `promptBytes` sum both calls. The two calls run side by side, so `charterLatencyMs` and
//     `ruleLatencyMs` say what each of them cost of the gate's whole `latencyMs`.
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
// A gated call whose rules check ran is also published to Dispatch (`decision-record.ts`): the
// entry below reaches only this machine's disk, and the session that counts the rule decisions
// over a fortnight holds a Dispatch token and nothing else. That write carries no ask text and no
// quote, takes its own 5 s stop, never changes the decision, and leaves `recordError` on the entry
// when it fails.
import { createHash } from "node:crypto";
import * as path from "node:path";
import requestTemplate from "./askgate-request.md" with { type: "text" };
import systemTemplate from "./askgate-system.md" with { type: "text" };
import uploadRule from "./askgate-upload.md" with { type: "text" };
import { createDecisionRecorder, RECORD_TIMEOUT_MS } from "./decision-record";
import { type NewAsk, newAsks } from "./new-asks";
import ruleCheckRules from "./rule-check.md" with { type: "text" };
import ruleCheckSystem from "./rule-check-system.md" with { type: "text" };
import rules from "./rules.md" with { type: "text" };
import {
  type DispatchSession,
  matchLines,
  type RuleName,
  type RuleOutcome,
  renderRuleRefusal,
  runRulesCheck,
} from "./rules-check";
import {
  agentFacing,
  byteLength,
  clipBytes,
  escaped,
  escapeXml,
  GATE_REASON_MAX_BYTES,
  lastLineJson,
  renderTemplate,
} from "./text";

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
const ABORT_GRACE_MS = 2_000;
// The runner refuses a tool_call handler that outlives extensionHandlers.toolCallTimeoutMs, so the
// gate's deadline stays this far under it: the two stops the handler pays after its deadline, the
// aborted call's usage report (ABORT_GRACE_MS) and the decision record `handleCall` awaits
// (RECORD_TIMEOUT_MS). Below MIN_DEADLINE_MS a verdict is hopeless and the gate does not bind.
const CEILING_MARGIN_MS = ABORT_GRACE_MS + RECORD_TIMEOUT_MS;
const MIN_DEADLINE_MS = 10_000;
// The runner gives a session_shutdown handler 2 s (SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS) and then seals
// the session, dropping any later entry: gates abandoned there get a shorter grace, and the handler
// waits at most SHUTDOWN_WAIT_MS for their records.
const SHUTDOWN_GRACE_MS = 1_000;
const SHUTDOWN_WAIT_MS = 1_500;
export const GATE_CONTEXT_MAX_BYTES = 256 * 1024;
const GATE_ARGS_MAX_BYTES = 64 * 1024;
const GATE_BREAKER_REVISES = 2;
const GATE_HALT_AFTER_FAILURES = 3;
/** Rules calls in a row without a rules verdict that bring the session's one notice and log line. */
const RULE_NOTICE_AFTER_FAILURES = 3;
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
// Room kept for the transcript window's elision line.
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
/** Both model calls' spend on one entry; undefined when neither was billed. */
function addUsage(a: Usage | undefined, b: Usage | undefined): { usage: Usage } | undefined {
  if (a === undefined) return b === undefined ? undefined : { usage: b };
  if (b === undefined) return { usage: a };
  return {
    usage: {
      input: a.input + b.input,
      output: a.output + b.output,
      cacheRead: a.cacheRead + b.cacheRead,
      cacheWrite: a.cacheWrite + b.cacheWrite,
      cost: a.cost + b.cost,
    },
  };
}
export interface GateEntry {
  advisor: "AskGate";
  tool: string;
  path: string;
  toolCallId: string;
  /**
   * `revise`: the gate did not pass the call as sent (a revise verdict, or in block mode a call given up
   * without a verdict, or in any mode a command it cannot read, a gate that did not load, or a new
   * ask a rule matched). A call was refused exactly when this is `revise` and `verdictMode` is
   * `block`, or `outcome` is `unreadable`, `no-model-role` or `load-failed`, or `rule` is set.
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
  /** On a revise: the reason exactly as the agent received it (the one agent-facing transform's output), the rules reason first when a rule matched. */
  deliveredReason?: string;
  /** The answer's tail when it held no verdict. */
  raw?: string;
  latencyMs: number;
  revisesForKey: number;
  argsDigest: string;
  /** The call's breaker target in readable form: the tool, then each of the issue, ask, artifact, project and reply it names. */
  target?: string;
  /** Bytes of every prompt sent: the charter call's and, for a call that opens a new ask, the rules call's. */
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
  /** Every attempt's spend of both calls, the aborted one included. */
  usage?: Usage;
  /** The rule the first new ask that matched broke. Set exactly when the rules check refused the call. */
  rule?: RuleName;
  /** That ask's number in the rules request, from 1. */
  ruleAsk?: number;
  /** The words of that ask the rules call quoted. */
  ruleQuote?: string;
  /** How the rules check ended, on each call that opens a new ask and reached the model call. */
  ruleOutcome?: RuleOutcome;
  /** The provider's error text, `timeout` or `no-verdict`, on a rules call that gave no verdict. */
  ruleError?: string;
  /** `provider/id` that answered the rules call (or, with no answer, the last one asked). */
  ruleModel?: string;
  /** The rules check's own time, its document read included. */
  ruleLatencyMs?: number;
  /** The charter call's own time, on the clock `ruleLatencyMs` uses, so the two concurrent calls' shares of `latencyMs` can be told apart. */
  charterLatencyMs?: number;
  /** An ask's text could not be read, so the rules call judged that ask without it. */
  ruleTextMissing?: true;
  /** New asks the rules request left out or cut to its bounds. */
  ruleAsksCut?: number;
  /** Why this decision reached no Dispatch decision record (`decision-record.ts`); the call itself was unaffected. */
  recordError?: string;
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
  /** Where the gate's notices go besides the session's UI: the host's logger. */
  logger: { warn(message: string): void };
  /** The session's envoy.json, whose `askgate.enabled` is the switch: named in the notice for an OMP_ASKGATE the gate ignores. */
  configFile: string;
  /** Appends to a file; serves OMP_ASKGATE_DUMP. */
  appendFile: (p: string, text: string) => void;
  /** File text, or undefined when no file is there; reads the document an upload's `--path` names. */
  readFile: (p: string) => string | undefined;
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
   * The session's Dispatch as the directory a call runs in configures it, every request ending with
   * that call's stop; undefined when the session has no Dispatch configuration. The rules check
   * reads a retyped block's text and an upload's current ask ids through it.
   */
  dispatchClient: (directory: string, signal: AbortSignal) => DispatchSession | undefined;
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

// Where a `dispatch` command's own text ends: `headLine`, `headScan` and their helpers below start
// from the rules `@legion/pi-shared/shell-command` applies (`dispatchCommandHead` there, which the
// Envoy entry uses to tell a dispatch-only call), and differ from them on purpose: the gate also
// reads a literal path to `dispatch` and the two wrappers below, and must read exactly the text
// the command sends, while the plugin's rule only has to tell a dispatch-only call from work. So
// the two are separate, and a change to one is checked against the other. A conservative character
// scan, not a shell parser: whatever it cannot reason about is no `dispatch` command. Whitespace is
// bash's blanks, space and tab, never JavaScript's `\s`, which also takes a no-break space that
// bash reads as part of a word.

/** The one here-document a command's first line may end with, opened with a quoted delimiter so
 * the shell expands nothing in its body. The second group is `-` for `<<-`, the only form under
 * which bash strips leading tabs from the delimiter line. The fourth is a `2>&1` after the opener,
 * which bash applies to the command as it would before it. */
const HEREDOC_OPENING = /^(.*?)[ \t]*<<(-?)[ \t]*'([A-Za-z_][A-Za-z0-9_]*)'([ \t]+2>&1)?[ \t]*$/d;

/** A line of nothing but blanks, which bash runs as no command. */
const BLANK_LINE = /^[ \t]*$/;

/** Outside quotes, a `dispatch` head refuses whatever would end the command, start
 * another, expand something, redirect, start a comment (bash reads the rest of the line as one, a
 * here-document opener included) or escape a character (the shell's to interpret, so never
 * modelled here). */
const REFUSED_UNQUOTED = "\n;&|$`<>()#\\";

/** Inside double quotes bash still expands `$` and backticks and interprets `\`. */
const REFUSED_DOUBLE_QUOTED = "$`\\";

/** A command's first line, without its here-document opener and its blanks, and where that text
 * starts in the command, when the command is that one line, optionally fed one quoted
 * here-document on stdin; undefined for any other command. `stderr` is where a `2>&1` after the
 * opener starts and ends in the command. */
function headLine(
  command: string
): { text: string; at: number; stderr?: [number, number] } | undefined {
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
  if (lines.length > 1) {
    const opening = HEREDOC_OPENING.exec(head);
    if (opening === null) return undefined;
    const stripTabs = opening[2] === "-";
    const delimiter = opening[3];
    const end = lines.findIndex(
      (line, index) => index > 0 && (stripTabs ? line.replace(/^\t+/, "") : line) === delimiter
    );
    // The first line equal to the delimiter ends the here-document in the shell, so it must be
    // the last line: anything after it would run as a command.
    if (end !== lines.length - 1) return undefined;
    head = opening[1] ?? "";
    const redirect = opening.indices?.[4];
    if (redirect) stderr = [lineAt + redirect[0], lineAt + redirect[1]];
  }
  const leading = /^[ \t]*/.exec(head)?.[0].length ?? 0;
  return {
    text: head.replace(/^[ \t]+|[ \t]+$/g, ""),
    at: lineAt + leading,
    ...(stderr ? { stderr } : {}),
  };
}

// AskGate's own additions to those rules: the program may be a literal path to `dispatch` as well
// as the bare name, which is all the plugin's rule takes, and the two wrappers agents put around a
// `dispatch` command that change neither its words nor what it sends: one leading `cd <path> && `,
// whose path is one word the shell does not expand, and one trailing `2>&1`.

/** Characters an unquoted literal word (a `cd` path, a path to `dispatch`) may hold: none the shell
 * expands (`~`, globs, braces, `$`) or interprets (`\`). */
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
  // assignment, whose words the shell takes as written.
  const program = scanWord(head, 0);
  if (
    program.unread.length > 0 ||
    program.value.split("/").at(-1) !== "dispatch" ||
    ASSIGNMENT.test(head) ||
    !headScan(head)
  )
    return undefined;
  const at = line.at + offset;
  return {
    head,
    at,
    ...(cdEnd === undefined ? {} : { cdEnd }),
    end: at + text.length,
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

/** Whether a `dispatch` head is one command whose words the shell takes as written:
 * single quotes make every character literal, and an unterminated quote is refused. */
function headScan(head: string): boolean {
  let quote: '"' | "'" | undefined;
  for (const char of head) {
    if (quote === "'") {
      if (char === "'") quote = undefined;
      continue;
    }
    if (quote === '"') {
      if (char === '"') quote = undefined;
      else if (REFUSED_DOUBLE_QUOTED.includes(char)) return false;
      continue;
    }
    if (char === "'" || char === '"') quote = char;
    else if (REFUSED_UNQUOTED.includes(char)) return false;
  }
  return quote === undefined;
}

type Word = { raw: string; value: string };

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
  for (const char of head) {
    if (quote === undefined && (char === " " || char === "\t")) {
      if (word) words.push(word);
      word = undefined;
      continue;
    }
    word ??= { raw: "", value: "" };
    word.raw += char;
    if (quote === undefined && (char === "'" || char === '"')) quote = char;
    else if (char === quote) quote = undefined;
    else word.value += char;
  }
  if (word) words.push(word);
  return words;
}

/** A scoped `dispatch` command: the tool it calls, the text to run (an override flag removed),
 * the same text with `--dry-run` after the command name, the override's line, and whether a flag
 * names a file the gate reads (a `--<field>-file` other than `-`, the here-document, or an
 * upload's `--path`). */
export interface ScopedCommand {
  tool: string;
  label: string;
  command: string;
  dryRunScript: string;
  rebuttal?: string;
  readsFile: boolean;
  /** A `dispatch artifact` upload of a markdown document, reviewed against failure 10 alone. */
  upload?: true;
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

/** Whether a flag names a file the gate reads: a `--<field>-file` but `-`, or an upload's `--path`. */
function namesFile(flags: readonly Word[], upload: boolean): boolean {
  return flags.some(({ value }, index) => {
    const eq = value.indexOf("=");
    const flag = eq === -1 ? value : value.slice(0, eq);
    const file = eq === -1 ? flags[index + 1]?.value : value.slice(eq + 1);
    const named =
      (flag.startsWith("--") && flag.endsWith("-file")) || (upload && flag === "--path");
    return named && file !== undefined && file !== "-";
  });
}

/**
 * The `bash` command as a scoped `dispatch` call, or undefined for any other command: read by the
 * strict head rules first (`strictScoped`), else by the composite reader (`compositeScoped`), which
 * reads the common safe forms around one write. A call neither reads is no scoped call; the gate
 * then refuses it when `unreadableDispatchWrite` finds a write in it.
 */
export function scopedCommand(command: unknown): ScopedCommand | undefined {
  if (typeof command !== "string") return undefined;
  return strictScoped(command) ?? compositeScoped(command);
}

function strictScoped(command: string): ScopedCommand | undefined {
  const found = dispatchCommandHead(command);
  if (found === undefined) return undefined;
  const { head, at, cdEnd, end, stderr } = found;
  const [program, name, ...flags] = headWords(head);
  if (name === undefined || !reviewedWrite(name.value, flags)) return undefined;
  const { rebuttal, removed } = splitRebuttal(flags);
  const kept = flags.filter((_, index) => !removed.has(index));
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
  const rest = kept.map((word) => word.raw);
  const upload = name.value === UPLOAD_COMMAND;
  return {
    tool: `dispatch_${name.value.replaceAll("-", "_")}`,
    label: `dispatch ${name.value}`,
    command: `${command.slice(0, at)}${[program.raw, name.raw, ...rest].join(" ")}${command.slice(at + head.length)}`,
    dryRunScript: `${before}${[program.raw, name.raw, "--dry-run", ...rest].join(" ")}${after}`,
    ...(rebuttal === undefined ? {} : { rebuttal }),
    readsFile: namesFile(kept, upload),
    ...(upload ? { upload: true as const } : {}),
    dirScript: cdEnd === undefined ? "pwd -P" : `${command.slice(0, cdEnd)} >/dev/null && pwd -P`,
  };
}

/**
 * A word of a call the composite reader reads: where it lies, as written, and its parts, each
 * literal text or a variable (`$NAME`, `${NAME}`). `unknown` marks a part this reader cannot know
 * (a glob, a brace, another expansion, a command substitution); `tilde` a leading unquoted `~`;
 * `bracket` an unquoted `[` that a later `]` of the same word closes into a glob.
 */
interface CallWord {
  raw: string;
  start: number;
  end: number;
  parts: Array<string | { name: string }>;
  unknown: boolean;
  tilde: boolean;
  bracket: boolean;
}
/** A redirect: its operator and file descriptor, its word, and a here-document's body, from the
 * newline that ends its opener's line to the end of its delimiter line. */
interface CallRedirect {
  op: string;
  fd: string;
  target?: CallWord;
  start: number;
  end: number;
  heredoc?: { quoted: boolean; body: [number, number] };
}
interface CallStage {
  words: CallWord[];
  redirects: CallRedirect[];
  substitution: boolean;
  start: number;
  end: number;
}
/** A pipeline and the separator after it: `;`, `&&`, `||`, a newline, or none at the end. */
interface CallStatement {
  stages: CallStage[];
  sep: string;
  start: number;
  end: number;
}

/**
 * The statements of a `bash` command as bash splits them, or undefined for a shape this reader
 * does not take: a subshell or group in parentheses, `&` (background), `|&`, process substitution,
 * an unterminated quote, substitution or here-document, or a control character. Quotes, `\`-newline
 * continuations, comments and here-document bodies are read as bash reads them.
 */
function readCall(command: string): CallStatement[] | undefined {
  if (hasControlCharacter(command)) return undefined;
  const n = command.length;
  const statements: CallStatement[] = [];
  const blank = (): CallStage => ({
    words: [],
    redirects: [],
    substitution: false,
    start: -1,
    end: -1,
  });
  let stages: CallStage[] = [];
  let stage = blank();
  let word: CallWord | undefined;
  let target: CallRedirect | undefined;
  let afterPipe = false;
  const heredocs: Array<{ redirect: CallRedirect; delimiter: string; stripTabs: boolean }> = [];
  const extend = (from: number, to: number) => {
    if (stage.start === -1) stage.start = from;
    stage.end = Math.max(stage.end, to);
    afterPipe = false;
  };
  const open = (at: number): CallWord => {
    word ??= {
      raw: "",
      start: at,
      end: at,
      parts: [],
      unknown: false,
      tilde: false,
      bracket: false,
    };
    return word;
  };
  const literal = (at: number, text: string, to: number) => {
    const current = open(at);
    const last = current.parts.at(-1);
    if (typeof last === "string") current.parts[current.parts.length - 1] = last + text;
    else current.parts.push(text);
    current.end = to;
  };
  const endWord = (): boolean => {
    if (word === undefined) return true;
    const done = word;
    word = undefined;
    done.raw = command.slice(done.start, done.end);
    if (target === undefined) {
      stage.words.push(done);
      extend(done.start, done.end);
      return true;
    }
    const redirect = target;
    target = undefined;
    redirect.target = done;
    redirect.end = done.end;
    if (redirect.op === "<<" || redirect.op === "<<-") {
      if (done.unknown || done.parts.some((part) => typeof part !== "string")) return false;
      heredocs.push({ redirect, delimiter: done.parts.join(""), stripTabs: redirect.op === "<<-" });
    }
    stage.redirects.push(redirect);
    extend(redirect.start, redirect.end);
    return true;
  };
  const endStatement = (sep: string): boolean => {
    if (!endWord() || target !== undefined) return false;
    if (stage.start !== -1) {
      stages.push(stage);
      stage = blank();
    } else if (afterPipe) return sep === "\n";
    if (stages.length === 0) return sep === "\n";
    statements.push({ stages, sep, start: stages[0].start, end: stages[stages.length - 1].end });
    stages = [];
    return true;
  };
  const backtick = (from: number): number | undefined => {
    for (let k = from + 1; k < n; k++) {
      if (command[k] === "\\") k++;
      else if (command[k] === "`") return k;
    }
    return undefined;
  };
  const parens = (from: number): number | undefined => {
    let depth = 0;
    for (let k = from; k < n; k++) {
      const ch = command[k];
      if (ch === "\\") k++;
      else if (ch === "'") {
        const close = command.indexOf("'", k + 1);
        if (close === -1) return undefined;
        k = close;
      } else if (ch === '"') {
        for (k++; k < n && command[k] !== '"'; k++) if (command[k] === "\\") k++;
        if (k >= n) return undefined;
      } else if (ch === "`") {
        const close = backtick(k);
        if (close === undefined) return undefined;
        k = close;
      } else if (ch === "(") depth++;
      else if (ch === ")" && --depth === 0) return k;
    }
    return undefined;
  };
  const substitution = (at: number, close: number) => {
    const current = open(at);
    current.unknown = true;
    current.end = close + 1;
    stage.substitution = true;
  };
  const dollar = (at: number, quoted: boolean): number | undefined => {
    const next = command[at + 1] ?? "";
    if (next === "(") {
      const close = parens(at + 1);
      if (close === undefined) return undefined;
      substitution(at, close);
      return close;
    }
    const current = open(at);
    if (next === "{") {
      const close = command.indexOf("}", at + 2);
      if (close === -1) return undefined;
      const name = command.slice(at + 2, close);
      if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) current.parts.push({ name });
      else current.unknown = true;
      current.end = close + 1;
      return close;
    }
    const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(command.slice(at + 1))?.[0];
    if (name !== undefined) {
      current.parts.push({ name });
      current.end = at + 1 + name.length;
      return at + name.length;
    }
    if (/^[0-9@*#?$!-]$/.test(next) || (!quoted && (next === "'" || next === '"'))) {
      // A positional or special parameter, or $'…' and $"…": nothing this reader knows.
      current.unknown = true;
      current.end = at + 1;
      return at;
    }
    literal(at, "$", at + 1);
    return at;
  };
  const doubleQuoted = (at: number): number | undefined => {
    open(at);
    for (let k = at + 1; k < n; k++) {
      const ch = command[k];
      if (ch === '"') {
        (word as CallWord).end = k + 1;
        return k;
      }
      if (ch === "\\") {
        const next = command[k + 1];
        if (next === "\n") k++;
        else if (next !== undefined && '$`"\\'.includes(next)) literal(k, next, ++k + 1);
        else literal(k, "\\", k + 1);
      } else if (ch === "$") {
        const to = dollar(k, true);
        if (to === undefined) return undefined;
        k = to;
      } else if (ch === "`") {
        const close = backtick(k);
        if (close === undefined) return undefined;
        substitution(k, close);
        k = close;
      } else literal(k, ch, k + 1);
    }
    return undefined;
  };
  const redirect = (at: number): number | undefined => {
    if (!endWord() || target !== undefined) return undefined;
    const match = /^(\d*)(&>>|&>|>>|>\||>&|<<<|<<-|<<|<&|<>|>|<)/.exec(command.slice(at, at + 24));
    if (match === null) return undefined;
    const [all, fd = "", op = ""] = match;
    let to = at + all.length;
    // `<(…)` and `>(…)` are process substitutions.
    if (command[to] === "(") return undefined;
    target = { op, fd, start: at, end: to };
    while (command[to] === " " || command[to] === "\t") to++;
    return to - 1;
  };
  /** Past the bodies of the here-documents opened on the line the newline at `newline` ends. */
  const bodies = (newline: number): number | undefined => {
    let at = newline + 1;
    for (const { redirect: opened, delimiter, stripTabs } of heredocs.splice(0)) {
      const begin = at - 1;
      for (;;) {
        const nl = command.indexOf("\n", at);
        const lineEnd = nl === -1 ? n : nl;
        const line = command.slice(at, lineEnd);
        if ((stripTabs ? line.replace(/^\t+/, "") : line) === delimiter) {
          opened.heredoc = {
            quoted: /['"\\]/.test(opened.target?.raw ?? ""),
            body: [begin, lineEnd],
          };
          at = lineEnd + 1;
          break;
        }
        if (nl === -1) return undefined;
        at = nl + 1;
      }
    }
    return at - 1;
  };
  for (let i = 0; i < n; i++) {
    const c = command[i];
    const next = command[i + 1];
    if (c === "'") {
      const close = command.indexOf("'", i + 1);
      if (close === -1) return undefined;
      open(i);
      literal(i, command.slice(i + 1, close), close + 1);
      i = close;
    } else if (c === '"') {
      const close = doubleQuoted(i);
      if (close === undefined) return undefined;
      i = close;
    } else if (c === "\\") {
      if (next === undefined) return undefined;
      if (next !== "\n") literal(i, next, i + 2);
      i++;
    } else if (c === " " || c === "\t") {
      if (!endWord()) return undefined;
    } else if (c === "#" && word === undefined) {
      const nl = command.indexOf("\n", i);
      i = (nl === -1 ? n : nl) - 1;
    } else if (c === "\n") {
      if (!endStatement("\n")) return undefined;
      if (heredocs.length > 0) {
        const to = bodies(i);
        if (to === undefined) return undefined;
        i = to;
      }
    } else if (c === ";") {
      if (next === ";" || !endStatement(";")) return undefined;
    } else if (c === "&" && next === "&") {
      if (!endStatement("&&")) return undefined;
      i++;
    } else if (c === "|" && next === "|") {
      if (!endStatement("||")) return undefined;
      i++;
    } else if (c === "|") {
      if (next === "&" || !endWord() || target !== undefined || stage.start === -1)
        return undefined;
      stages.push(stage);
      stage = blank();
      afterPipe = true;
    } else if (
      c === "<" ||
      c === ">" ||
      (c === "&" && next === ">") ||
      (word === undefined && /^\d+[<>]/.test(command.slice(i, i + 12)))
    ) {
      const to = redirect(i);
      if (to === undefined) return undefined;
      i = to;
    } else if (c === "&" || c === "(" || c === ")") {
      return undefined;
    } else if (c === "$") {
      const to = dollar(i, false);
      if (to === undefined) return undefined;
      i = to;
    } else if (c === "`") {
      const close = backtick(i);
      if (close === undefined) return undefined;
      substitution(i, close);
      i = close;
    } else {
      const current = open(i);
      if (c === "~" && current.start === i) current.tilde = true;
      literal(i, c, i + 1);
      // A glob is `*`, `?` or a bracket expression `[…]` closed in the same word; a lone `[`, the
      // test command, is literal. A brace is read as unknown.
      if (c === "*" || c === "?" || c === "{" || c === "}" || (c === "]" && current.bracket))
        current.unknown = true;
      if (c === "[") current.bracket = true;
    }
  }
  if (!endWord() || target !== undefined || heredocs.length > 0) return undefined;
  if (stage.start !== -1) stages.push(stage);
  else if (afterPipe) return undefined;
  if (stages.length > 0)
    statements.push({
      stages,
      sep: "",
      start: stages[0].start,
      end: stages[stages.length - 1].end,
    });
  else if (["&&", "||"].includes(statements.at(-1)?.sep ?? "")) return undefined;
  return statements;
}

/** The programs a statement before the write may run: each reads and prints, and writes no file. */
const READ_ONLY: Record<string, true> = {
  wc: true,
  cat: true,
  ls: true,
  test: true,
  "[": true,
  stat: true,
  grep: true,
  head: true,
  tail: true,
  sleep: true,
  date: true,
  echo: true,
  printf: true,
  true: true,
};
const DURATION = /^\d+(\.\d+)?[smhd]?$/;

/**
 * The composite reader: a call holding one scoped write, read when everything around it can only
 * change what the dry run also runs, or nothing the write sends. Read around the write:
 *   - `\`-newline continuations, quotes, comments and quoted here-documents;
 *   - on the write: `2>&1`, `>`/`>>`/`2>` to a literal path (`/dev/null` too), `< <literal path>`,
 *     one quoted here-document, literal `NAME=value` prefixes and one `timeout <duration>`;
 *   - later stages of the write's pipeline that hold no `dispatch` word, no here-document and no
 *     substitution: they only read the write's output;
 *   - before it, joined by `&&`, `;` or a newline: `cd <literal path>`, `export NAME=literal …`
 *     or `NAME=literal …` (a value may start with `~/`), a pipeline of read-only programs
 *     (`READ_ONLY`) with no redirect but to /dev/null, `2>&1` or from a literal path, and a
 *     `dispatch` call that is no reviewed write, with no redirect to a file;
 *   - after it, any statement that holds no reviewed write;
 *   - `$NAME` or `${NAME}` in the write's words when an earlier statement assigns NAME a literal.
 *     A prefix assignment does not count: bash expands the words before it applies.
 * The dry run keeps the `cd`, `export` and assignment statements and the write's prefixes and
 * stdin, and drops the rest. Anything else is no read, and the gate refuses the call as unreadable.
 */
function compositeScoped(command: string): ScopedCommand | undefined {
  const statements = readCall(command);
  if (statements === undefined) return undefined;
  // An unquoted here-document's body expands, and its expansions may run anything.
  for (const statement of statements)
    for (const stage of statement.stages)
      if (stage.redirects.some((r) => r.heredoc !== undefined && !r.heredoc.quoted))
        return undefined;
  const vars = new Map<string, string>();
  const wordValue = (word: CallWord | undefined): string | undefined => {
    if (word === undefined || word.unknown) return undefined;
    let out = "";
    for (const part of word.parts) {
      if (typeof part === "string") out += part;
      else {
        const known = vars.get(part.name);
        if (known === undefined) return undefined;
        out += known;
      }
    }
    return out;
  };
  const isLiteral = (word: CallWord | undefined): word is CallWord =>
    word !== undefined && !word.unknown && word.parts.every((part) => typeof part === "string");
  const asWords = (list: readonly CallWord[]): Word[] | undefined => {
    const out: Word[] = [];
    for (const word of list) {
      const value = wordValue(word);
      if (value === undefined) return undefined;
      out.push({ raw: word.raw, value });
    }
    return out;
  };
  const targetOf = (r: CallRedirect) => (isLiteral(r.target) ? wordValue(r.target) : undefined);
  const quiet = (r: CallRedirect) => {
    const to = targetOf(r);
    return (
      ((((r.op === ">" || r.op === ">>") && ["", "1", "2"].includes(r.fd)) || r.op === "&>") &&
        to === "/dev/null") ||
      (r.op === ">&" && r.fd === "2" && to === "1")
    );
  };
  const input = (r: CallRedirect) =>
    r.op === "<" && (r.fd === "" || r.fd === "0") && targetOf(r) !== undefined;
  /** Where a stage's program is, past literal assignments and one `timeout <duration>`. */
  const commandAt = (stage: CallStage): number | undefined => {
    let at = 0;
    while (at < stage.words.length && ASSIGNMENT.test(stage.words[at].raw)) {
      if (!isLiteral(stage.words[at])) return undefined;
      at++;
    }
    if (wordValue(stage.words[at]) === "timeout") {
      const duration = wordValue(stage.words[at + 1]);
      if (duration === undefined || !DURATION.test(duration)) return undefined;
      at += 2;
    }
    return at;
  };
  /** A stage's part in the call: the write, a `dispatch` call that sends nothing reviewed, another
   * program, or `unread` when it may be a write this reader cannot tell. */
  const classify = (stage: CallStage): "write" | "dispatch" | "other" | "unread" => {
    const text = command.slice(stage.start, stage.end);
    const at = commandAt(stage);
    if (at === undefined) return text.includes("dispatch") ? "unread" : "other";
    const [program, name, ...flags] = stage.words.slice(at);
    if (program === undefined) return "other";
    const programValue = wordValue(program);
    const nameValue = wordValue(name);
    const scoped = (SCOPED_COMMANDS as readonly string[]).includes(nameValue ?? "");
    if (programValue === undefined) return scoped || nameValue === undefined ? "unread" : "other";
    if (programValue.split("/").at(-1) !== "dispatch") return "other";
    if (name === undefined) return "dispatch";
    const words = asWords(flags);
    if (nameValue === undefined || (scoped && (words === undefined || stage.substitution)))
      return "unread";
    return words !== undefined && reviewedWrite(nameValue, words) ? "write" : "dispatch";
  };
  const setup: CallStatement[] = [];
  const cds = new Set<CallStatement>();
  let write: CallStatement | undefined;
  for (const statement of statements) {
    if (write !== undefined) {
      // After the write: it has already posted, so only another reviewed write in it matters.
      if (unreadableDispatchWrite(command.slice(statement.start, statement.end)) !== undefined)
        return undefined;
      continue;
    }
    const kinds = statement.stages.map(classify);
    if (kinds.includes("unread")) return undefined;
    if (kinds[0] === "write") {
      // Later stages of the write's pipeline only read its output.
      for (const later of statement.stages.slice(1))
        if (
          later.substitution ||
          later.redirects.some((r) => r.heredoc !== undefined) ||
          later.words.some((w) => w.raw.includes("dispatch"))
        )
          return undefined;
      write = statement;
      continue;
    }
    if (kinds.includes("write") || statement.sep === "||") return undefined;
    const [only] = statement.stages;
    if (statement.stages.length === 1 && only.redirects.length === 0 && !only.substitution) {
      const words = only.words;
      const first = isLiteral(words[0]) ? wordValue(words[0]) : undefined;
      if (first === "cd") {
        const dir = isLiteral(words[1]) && !words[1].tilde ? wordValue(words[1]) : undefined;
        if (words.length !== 2 || dir === undefined || dir === "" || dir.startsWith("-"))
          return undefined;
        setup.push(statement);
        cds.add(statement);
        continue;
      }
      const assignments = first === "export" ? words.slice(1) : words;
      if (
        assignments.length > 0 &&
        assignments.every((w) => ASSIGNMENT.test(w.raw) && isLiteral(w))
      ) {
        for (const assignment of assignments) {
          const text = wordValue(assignment) ?? "";
          vars.set(text.slice(0, text.indexOf("=")), text.slice(text.indexOf("=") + 1));
        }
        setup.push(statement);
        continue;
      }
    }
    // A statement the dry run drops: it reads and prints, and changes no file the write reads.
    for (const [index, stage] of statement.stages.entries()) {
      const words = asWords(stage.words);
      const program = words?.[0]?.value;
      if (words === undefined || program === undefined || stage.substitution) return undefined;
      if (kinds[index] === "dispatch" && stage.redirects.every(quiet)) continue;
      if (
        !Object.hasOwn(READ_ONLY, program) ||
        (program === "printf" && words.some((w) => w.value === "-v")) ||
        !stage.redirects.every((r) => quiet(r) || input(r))
      )
        return undefined;
    }
  }
  if (write === undefined) return undefined;
  const stage = write.stages[0];
  const at = commandAt(stage) ?? 0;
  const name = stage.words[at + 1];
  const flagWords = stage.words.slice(at + 2);
  const flags = asWords(flagWords) ?? [];
  const nameValue = wordValue(name) ?? "";
  // The write's redirects: output ones the dry run drops (its stdout must be the CLI's JSON), one
  // stdin it keeps, and nothing else.
  const dropped: CallRedirect[] = [];
  let stdin = 0;
  for (const r of stage.redirects) {
    const to = targetOf(r);
    const output =
      (((r.op === ">" || r.op === ">>" || r.op === ">|") && ["", "1", "2"].includes(r.fd)) ||
        r.op === "&>" ||
        r.op === "&>>") &&
      to !== undefined;
    const dup = r.op === ">&" && ["", "1", "2"].includes(r.fd) && (to === "1" || to === "2");
    if (output || dup) dropped.push(r);
    else if (input(r) || ((r.op === "<<" || r.op === "<<-") && r.heredoc?.quoted)) stdin++;
    else return undefined;
  }
  if (stdin > 1) return undefined;
  const { rebuttal, removed } = splitRebuttal(flags);
  const tokens: Array<{ start: number; end: number }> = [...stage.words, ...stage.redirects].sort(
    (a, b) => a.start - b.start
  );
  /** A token's text with the blanks before it. */
  const cut = (token: { start: number; end: number }): [number, number] => [
    tokens[tokens.indexOf(token) - 1]?.end ?? stage.start,
    token.end,
  ];
  const rebuttalCuts = [...removed].map((index) => cut(flagWords[index]));
  const edit = (
    from: number,
    to: number,
    cuts: Array<[number, number]>,
    insert?: { at: number; text: string }
  ): string => {
    let out = "";
    let at = from;
    let pending = insert;
    for (const [a, b] of [...cuts].sort((x, y) => x[0] - y[0])) {
      if (pending !== undefined && pending.at <= a) {
        out += command.slice(at, pending.at) + pending.text;
        at = pending.at;
        pending = undefined;
      }
      out += command.slice(at, Math.max(at, a));
      at = Math.max(at, b);
    }
    if (pending !== undefined) {
      out += command.slice(at, pending.at) + pending.text;
      at = pending.at;
    }
    return out + command.slice(at, to);
  };
  const separator = (sep: string) => (sep === "&&" ? " && " : sep === ";" ? "; " : "\n");
  const kept = (statement: CallStatement) =>
    `${command.slice(statement.start, statement.end)}${cds.has(statement) ? " >/dev/null" : ""}${separator(statement.sep)}`;
  const heredoc = stage.redirects.find((r) => r.heredoc !== undefined)?.heredoc;
  const upload = nameValue === UPLOAD_COMMAND;
  const keptFlags = flags.filter((_, index) => !removed.has(index));
  return {
    tool: `dispatch_${nameValue.replaceAll("-", "_")}`,
    label: `dispatch ${nameValue}`,
    command: edit(0, command.length, rebuttalCuts),
    dryRunScript: `${setup.map(kept).join("")}${edit(
      stage.start,
      stage.end,
      [...rebuttalCuts, ...dropped.map(cut)],
      { at: name.end, text: " --dry-run" }
    )}${heredoc === undefined ? "" : command.slice(heredoc.body[0], heredoc.body[1])}`,
    ...(rebuttal === undefined ? {} : { rebuttal }),
    readsFile: namesFile(keptFlags, upload),
    ...(upload ? { upload: true as const } : {}),
    dirScript: `${setup
      .filter((statement) => cds.has(statement))
      .map(kept)
      .join("")}pwd -P`,
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

/** A scoped `dispatch` write the gate cannot review: the tool it calls, the command's name and,
 * when the word that runs it is not literal, whether that word calls `dispatch` by name or path
 * and the characters in it AskGate does not take as literal (only its expansions when it does not). */
export interface UnreadableCommand {
  tool: string;
  label: string;
  program?: { namesDispatch: boolean; unread: string[] };
}

/**
 * A scoped `dispatch` write anywhere in a `bash` command, found at a command's position (after any
 * assignments and prefixes such as `env -u HOME --` or `timeout -s KILL 30`, each prefix's options
 * and its `--` read as its own parser reads them: `commandStart`), or a scoped subcommand run by a
 * program an expansion names (`$D comment …`, `$(command -v dispatch) ask …`), which may be
 * `dispatch`. One carrying `--dry-run` or `--help`, or a `dispatch issue` without
 * `--spec`/`--spec-file`, sends nothing for review and is not one. Only for a command
 * `scopedCommand` could not read.
 */
export function unreadableDispatchWrite(command: unknown): UnreadableCommand | undefined {
  // A write names `dispatch`, or reaches it through an expansion.
  if (typeof command !== "string" || !/dispatch|[$`]/.test(command)) return undefined;
  for (const segment of commandWords(command)) {
    const start = commandStart(segment);
    if (start === undefined) continue;
    const [program, name, ...flags] = start.words.slice(start.at);
    if (program === undefined || name === undefined || !reviewedWrite(name.value, flags)) continue;
    const named = program.value.split("/").at(-1) === "dispatch";
    const unread = [...new Set(scanWord(program.raw, 0).unread)];
    const expansions = unread.filter((char) => EXPANSIONS.includes(char));
    if (!named && expansions.length === 0) continue;
    const found = {
      tool: `dispatch_${name.value.replaceAll("-", "_")}`,
      label: `dispatch ${name.value}`,
    };
    return unread.length === 0
      ? found
      : { ...found, program: { namesDispatch: named, unread: named ? unread : expansions } };
  }
  return undefined;
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
 * The call's target: its tool and each of the issue, ask, artifact, project and reply its arguments
 * name. The NUL separators keep two values apart in the breaker's map; `targetLabel` is the same
 * key with spaces for them, which the entry records and a reader joins calls on.
 */
function breakerKey(toolName: string, input: Input): string {
  let key = toolName;
  for (const k of BREAKER_KEYS) if (input[k] !== undefined) key += `\u0000${k}=${String(input[k])}`;
  return key;
}
const targetLabel = (key: string) => key.replaceAll("\u0000", " ");

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
  const parsed = lastLineJson(text);
  if (parsed === undefined) return undefined;
  if (parsed.decision === "allow") return { decision: "allow" };
  if (parsed.decision === "revise" && typeof parsed.reason === "string" && parsed.reason.trim())
    return { decision: "revise", reason: parsed.reason.trim() };
  return undefined;
}

/**
 * How the breaker tells one new ask of a call from another: the ask's opening, the first words
 * that name it in a refusal. A retype carries no text of its own in this scan — only the rules
 * check reads the block from Dispatch — so the block it names stands for it.
 */
const askIdentity = (ask: NewAsk): string =>
  ask.kind === "retype" ? `retype\u0000${ask.id ?? ""}` : `ask\u0000${ask.opening}`;

/**
 * One revise on a target: the charter's reason, the rules check's, or both when both answered on
 * one call. `refused` is set when the call did not go out, which is what the breaker counts: a
 * warn-mode charter revise sends the call as the agent wrote it, and a loop needs a refusal to
 * loop on. `asks` names each new ask a rules refusal refused (`askIdentity`); a refusal of the
 * call as a whole names none.
 */
type Revise = { charter?: string; rules?: string; refused: boolean; asks?: readonly string[] };

/** Revises per target since its last allowed call. */
class Breaker {
  readonly #revises = new Map<string, Revise[]>();
  revised(key: string, revise: Revise): void {
    const revises = this.#revises.get(key) ?? [];
    revises.push(revise);
    this.#revises.set(key, revises);
  }
  tripped(key: string): boolean {
    return this.count(key) >= GATE_BREAKER_REVISES;
  }
  reset(key: string): void {
    this.#revises.delete(key);
  }
  clear(): void {
    this.#revises.clear();
  }
  /** How many times the gate refused this target, whichever check refused it. */
  count(key: string): number {
    return (this.#revises.get(key) ?? []).filter((revise) => revise.refused).length;
  }
  /** How many times the gate answered revise on this target, the warn-mode ones included. */
  revises(key: string): number {
    return (this.#revises.get(key) ?? []).length;
  }
  /**
   * Whether a tripped breaker's pass covers a call opening these asks (`askIdentity`). The
   * breaker ends a loop on one refused thing. Where the refusals named no ask, that thing is the
   * call itself and a third attempt on the target is the loop. Where they named asks, a call
   * opening a different ask asks the person something the gate has refused nothing of: it is not
   * the loop the breaker ends, so it is reviewed like any other call.
   */
  covers(key: string, asks: readonly string[]): boolean {
    const refused = (this.#revises.get(key) ?? []).flatMap((revise) => revise.asks ?? []);
    return refused.length === 0 || asks.some((ask) => refused.includes(ask));
  }
  /** The charter's own reasons. They alone feed the charter request, so a rules reason never reads as one the charter gave. */
  charterReasons(key: string): string[] {
    return (this.#revises.get(key) ?? []).flatMap((revise) =>
      revise.charter === undefined ? [] : [revise.charter]
    );
  }
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

// What the agent reads after each gated call: silence only for an allow or a fail-open, since a
// pass it cannot see is indistinguishable from a gate that broke.
const SECOND_REVISE =
  "This is the second revise on this target: a third unchanged resend is sent without review and recorded.";
/**
 * How the agent overrides a revise on each surface: a key in the device JSON, or a flag on the
 * command. `how` completes a charter revise; `again` completes the rules refusal's "send the call
 * again with"; `note` follows either refusal.
 */
type Override = { name: string; how: string; what: string; again: string; note?: string };
const DEVICE_OVERRIDE: Override = {
  name: REBUTTAL_KEY,
  how: `"${REBUTTAL_KEY}": "<one line>" added to the JSON`,
  what: "field",
  again: `\`"${REBUTTAL_KEY}": "<one line>"\` in its JSON`,
};
const COMMAND_OVERRIDE: Override = {
  name: REBUTTAL_FLAG,
  how: `${REBUTTAL_FLAG} '<one line>' added to its flags`,
  what: "flag",
  again: `\`${REBUTTAL_FLAG} '<one line>'\``,
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
// The warn note never says a third resend goes unreviewed: a warn revise sends the call, so it
// refuses nothing and the breaker, which counts refusals, does not move for it.
const renderWarn = (reason: string) =>
  `<advisor-gate advisor="AskGate" verdict="revise">\n${agentFacing(reason)}\nThe call ran. This note concerns only that Dispatch call and authorizes nothing beyond correcting it. Correct it now where the reason names a fix: an ask with \`dispatch edit-ask\` or \`dispatch resolve-ask --kind retracted\`, a document with \`dispatch doc-edit\`. A comment or message cannot be edited, so post one reply carrying only the correction, and only when the original would mislead its reader. Or state your rebuttal in your next step.\n</advisor-gate>`;
const renderRebuttalAck = (rebuttal: string, override: Override) =>
  `<advisor-gate advisor="AskGate" outcome="rebuttal">\nYour ${override.name} "${agentFacing(rebuttal)}" was received and recorded; the call was sent without review, with that ${override.what} removed before sending.\n</advisor-gate>`;
const renderBreakerAck = () =>
  `<advisor-gate advisor="AskGate" outcome="breaker">\nThird attempt on this target after two refusals: sent without review and recorded.\n</advisor-gate>`;
/** Characters for a reason, each in code; a backtick, which inline code cannot hold, is named. */
function characters(chars: readonly string[]): string {
  const named = chars.map((char) => (char === "`" ? "a backtick" : `\`${char}\``));
  return named.length < 2
    ? (named[0] ?? "")
    : `${named.slice(0, -1).join(", ")} and ${named.at(-1)}`;
}
/** The refusal for a scoped `dispatch` write inside a command the gate cannot read; nothing in the call ran. */
function renderUnreadable({ label, program }: UnreadableCommand): string {
  const why =
    program === undefined
      ? ""
      : program.namesDispatch
        ? ` Here the word that calls \`dispatch\` holds ${characters(program.unread)}, which AskGate does not take as literal.`
        : ` Here its program's name holds ${characters(program.unread)}, so AskGate cannot tell which program would run.`;
  return `AskGate did not run this call: it holds a \`${label}\`, which AskGate reviews before it is sent, and in this call AskGate cannot know the text it would send. AskGate reads a \`dispatch\` command called by name or by a literal path, with a \`\\\` line continuation, output redirects and later pipe stages such as \`| head\`, literal assignments and \`timeout\` before it, \`cd\`, \`export\`, read-only commands and \`dispatch\` calls that write nothing before it, any commands after it, a variable set to a literal earlier in the call, and a literal file as its standard input. It does not read a call that runs another program before the write (a script, \`sed -i\`, \`cat > f\`), a command substitution, two writes, or an unquoted here-document.${why} Nothing in the call ran. Run any other commands it held in a call of their own first, then send that \`${label}\` again on its own${program === undefined ? "" : ", calling `dispatch` by name"}: no pipe, no \`;\`, \`||\` or second \`&&\`, no other redirect and no other command in the same call. Text it reads from a file goes in a \`--<field>-file <path>\` flag or in one here-document opened with a quoted delimiter (\`<<'EOF'\`) as the call's only other content.`;
}

/**
 * The gate of a session where AskGate did not load: a host module that did not import, an
 * OMP_ASKGATE_TIMEOUT_MS that is no positive integer, or a handler ceiling too short for a verdict.
 * Only `askgate.enabled: false` turns AskGate off, so a load failure is no off switch: each call of
 * the main agent the gate would review (`scopedCommand`, `unreadableDispatchWrite`, `scopedDevice`,
 * an upload counted as the gate counts it) is refused in both modes, naming the error and the
 * switch, and recorded as `load-failed`. Every other call runs.
 */
export function loadFailureGate(
  error: unknown,
  deps: Pick<Deps, "env" | "configFile">,
  pi: Pick<Pi, "appendEntry">
): (event: ToolCallEvent, ctx: GateCtx) => ToolCallResult {
  const message = messageOf(error);
  const reason = `AskGate did not send this call: it reviews each Dispatch write before it posts, and it did not load: ${message}. Send the call again once AskGate loads. To turn AskGate off for this session, set askgate.enabled to false in ${deps.configFile}.`;
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
    // The system role holds only the gate's own text and its rules; the agent's system prompt and
    // transcript are data about the agent under review, escaped into the user message.
    const system = `${systemTemplate.trim()}\n\n${rules.trim()}`;
    // The rules call's system role holds its own frame and the two rules, nothing of the charter's.
    const ruleSystem = `${ruleCheckSystem.trim()}\n\n${ruleCheckRules.trim()}`;
    // The fork binds this factory once per process and keeps the binding across /new, /resume
    // and a branch switch, so the per-session state below resets on session_switch and
    // session_branch: a halt, a count, revise reasons or a rebuttal never outlive their session.
    const rebuttals = new Map<string, string>();
    const breaker = new Breaker();
    const dumpPath = deps.env.OMP_ASKGATE_DUMP;
    // Keyed by session id and not reset with the state below: a session the process returns to
    // appends to the decision document it already uploaded.
    const recordDecision = createDecisionRecorder(deps);
    let failures = 0;
    /** Rules calls in a row that ended without a rules verdict. */
    let ruleFailures = 0;
    let halted = false;
    // Set once the session starts shutting down (only dispose emits session_shutdown): a scoped call
    // that reaches the gate after that asks no model and takes the shutdown branch at once.
    let closing = false;
    const reset = () => {
      rebuttals.clear();
      breaker.clear();
      failures = 0;
      ruleFailures = 0;
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
    /** Why a waiting gate was given up: its own deadline, the user or the runner ending its call or run, or the session shutting down. */
    type Cause = "timeout" | "abandoned" | "shutdown";
    /** Why a waiting gate was given up by something other than its own deadline. */
    type Abandon = (cause: Exclude<Cause, "timeout">, graceMs?: number) => void;
    /** Each waiting gated call's one stop, by its tool call id. */
    const inflight = new Map<string, Abandon>();
    /**
     * Everything one gated call waits on — its dry run, its document read, the charter call and the
     * rules call — shares one signal, one `inflight` entry and one deadline, so its tool's end, a
     * stopped run or a shutdown stops all of it at once. The first cause wins.
     */
    type Stop = {
      readonly signal: AbortSignal;
      readonly stopped: Promise<Cause>;
      cause?: Cause;
      graceMs: number;
      /** Starts the call's deadline, once the model calls are about to go out. */
      arm(ms: number): void;
      /** Ends the call: its timer, its entry in `inflight`, and anything of it still running. */
      close(): void;
    };
    const openStop = (toolCallId: string): Stop => {
      const controller = new AbortController();
      const { promise: stopped, resolve } = Promise.withResolvers<Cause>();
      let timer: Timer | undefined;
      const stopWith = (cause: Cause, graceMs = ABORT_GRACE_MS) => {
        if (handle.cause !== undefined) return;
        handle.cause = cause;
        handle.graceMs = graceMs;
        resolve(cause);
        controller.abort();
      };
      const handle: Stop = {
        signal: controller.signal,
        stopped,
        graceMs: ABORT_GRACE_MS,
        arm: (ms) => {
          timer = setTimeout(() => stopWith("timeout"), ms);
        },
        close: () => {
          clearTimeout(timer);
          if (inflight.get(toolCallId) === stopWith) inflight.delete(toolCallId);
          // Nothing reads an answer after the decision: a call still in flight is abandoned.
          controller.abort();
        },
      };
      inflight.set(toolCallId, stopWith);
      return handle;
    };
    pi.on("tool_execution_end", (event: { toolCallId: string }) => {
      inflight.get(event.toolCallId)?.("abandoned");
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

    /**
     * A rules verdict resets the count of rules calls in a row without one. The call that brings the
     * count to three writes one log line, which is the record in a session whose UI no reader sees,
     * and shows the notice where there is a UI.
     */
    const settleRules = (outcome: RuleOutcome | undefined, ctx: GateCtx) => {
      if (outcome === "match" || outcome === "none") ruleFailures = 0;
      if (outcome !== "error" && outcome !== "timeout" && outcome !== "no-verdict") return;
      ruleFailures++;
      if (ruleFailures !== RULE_NOTICE_AFTER_FAILURES) return;
      const notice = `AskGate's rule check failed ${ruleFailures} times in a row; new asks are not checked against the two rules.`;
      deps.logger.warn(notice);
      if (ctx.hasUI) ctx.ui.notify(notice, "warning");
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
    const pass = (
      outcome: GateEntry["outcome"],
      fields: Partial<GateEntry> = {},
      effect: Decision["effect"] = "none"
    ): Decision => ({ fields: { decision: "allow", outcome, ...fields }, effect });
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
    /** The first of the gate's roles that resolves to a model in this session, with that model. */
    const resolveRole = (ctx: GateCtx): { role: GateRole; model: GateModel } | undefined => {
      for (const role of GATE_ROLES) {
        const model = ctx.models.resolve(`@${role}`);
        if (model) return { role, model };
      }
      return undefined;
    };

    /** What one model walk answered: the answer, the fields every entry that reached the model records, and the walk's own time. */
    type Called = Pick<GateEntry, "promptBytes" | "model" | "failedModels" | "usage">;
    type Walked = { answer: Answer; called: Called; elapsedMs: number };
    /**
     * One call to the gate's model: appends its prompts to OMP_ASKGATE_DUMP under `label`, then walks
     * `chain` under the call's one stop, which its deadline is armed on. The charter check and the
     * rules check each make one of these, on the same chain and the same stop, so a session is asked
     * on one provider and both calls end together. `user` is rendered here, once the gate knows it
     * will ask.
     */
    const walk = async (
      ctx: GateCtx,
      call: { toolCallId: string; stop: Stop },
      prompt: { system: string; user: () => string; label: string },
      chain: readonly GateModel[]
    ): Promise<Walked> => {
      // Timed from here, so a walk's own share of the gate's latency covers its render as well as its call.
      const began = deps.now();
      const user = prompt.user();
      const promptBytes = byteLength(prompt.system) + byteLength(user);
      if (dumpPath)
        deps.appendFile(
          dumpPath,
          `===== AskGate ${call.toolCallId}${prompt.label} system =====\n${prompt.system}\n===== AskGate ${call.toolCallId}${prompt.label} user =====\n${user}\n\n`
        );
      const sessionId = ctx.sessionManager.getSessionId();
      const failedModels: Array<{ model: string; error: string }> = [];
      const usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0 };
      let billed = false;
      let answer: Answer = { kind: "error", reason: "no model asked" };
      let asked = chain[0];
      for (const next of chain) {
        asked = next;
        answer = await ask({ ctx, model: next, system: prompt.system, user, sessionId }, call.stop);
        if (answer.usage) {
          billed = true;
          for (const k of Object.keys(usage) as Array<keyof Usage>) usage[k] += answer.usage[k];
        }
        if (answer.kind !== "error" || next === chain.at(-1)) break;
        failedModels.push({ model: `${next.provider}/${next.id}`, error: answer.reason });
      }

      return {
        answer,
        called: {
          promptBytes,
          model: `${asked.provider}/${asked.id}`,
          ...(failedModels.length > 0 ? { failedModels } : {}),
          ...(billed ? { usage } : {}),
        },
        elapsedMs: deps.now() - began,
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
      stop: Stop,
      directory: () => Promise<string | undefined>,
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
      // The new asks this call opens, read from its own arguments as the rules check reads them:
      // a parse of the arguments, which costs no model call and no Dispatch read. Only the
      // breaker needs them here, so they are read where it asks.
      let opened: NewAsk[] | undefined;
      const callAsks = () => (opened ??= newAsks(event.toolName, event.input));
      // A tripped breaker sends the call unasked only where this call is the refused thing it
      // ended the loop on: a target whose refusals named no ask, or one of the asks refused here.
      // Any other ask on the same target is a question the gate has refused nothing of, so the
      // review runs and the count stays where it is, leaving the loop its own way out.
      if (breaker.tripped(key) && breaker.covers(key, callAsks().map(askIdentity))) {
        breaker.reset(key);
        return {
          fields: { decision: "allow", outcome: "breaker" },
          result: { additionalContext: renderBreakerAck() },
          effect: "none",
        };
      }

      if (closing) return givenUp(base.verdictMode, "shutdown");
      const resolved = resolveRole(ctx);
      if (resolved === undefined) return noModelRole;
      const { role, model } = resolved;
      // A session routed off the gate's provider keeps its transcript there: nothing is rendered or sent.
      if (ctx.model?.provider !== model.provider)
        return pass("skipped", { reason: `primary on ${ctx.model?.provider ?? "no model"}` });
      // Raced from handler entry, so each render of this call counts against the runner's ceiling too.
      const deadlineMs = Math.min(timeoutMs, deps.handlerCeilingMs() - CEILING_MARGIN_MS);
      stop.arm(Math.max(0, deadlineMs - (deps.now() - started)));
      // A model that answers with an error hands the request to the next one in the role's chain, inside
      // the same deadline. Only models on the primary's provider are asked: the transcript stays there.
      const chain = [
        model,
        ...deps.fallbackModels(ctx, model, role).filter((next) => next.provider === model.provider),
      ];
      // Both calls read one rendering of the agent under review, the newer of them reusing it.
      let rendered: { primarySystemPrompt: string; transcript: string } | undefined;
      const agentContext = () =>
        (rendered ??= {
          primarySystemPrompt: ctx.getSystemPrompt().join("\n\n"),
          transcript: renderTranscript(deps.contextMessages(ctx), GATE_CONTEXT_MAX_BYTES),
        });
      // The arguments as the model sees them, cut at GATE_ARGS_MAX_BYTES, also go in the entry, so a
      // later read of the review has the text the model judged, not the command or a mutable file.
      const args = renderArgs(event.input);
      const call = { toolCallId: event.toolCallId, stop };
      // The charter call starts first (its request is rendered and sent before this returns); a call
      // that opens a new ask also gets the rules call, beside it on the same chain, deadline and stop.
      const charterRun = walk(
        ctx,
        call,
        {
          system,
          label: "",
          user: () => {
            const { primarySystemPrompt, transcript } = agentContext();
            return renderRequest(
              primarySystemPrompt,
              transcript,
              base.path,
              args,
              breaker.charterReasons(key),
              upload
            );
          },
        },
        chain
      );
      const rulesRun = runRulesCheck(
        {
          now: deps.now,
          env: deps.env,
          dispatchClient: deps.dispatchClient,
          walk: (prompt) => walk(ctx, call, prompt, chain),
        },
        {
          toolName: event.toolName,
          tool: base.path,
          input: event.input,
          directory,
          system: ruleSystem,
          context: agentContext,
          signal: stop.signal,
        }
      );
      const [charter, ruled] = await Promise.all([charterRun, rulesRun]);
      const called: Partial<GateEntry> = {
        ...charter.called,
        deadlineMs,
        reviewedText: args,
        charterLatencyMs: charter.elapsedMs,
        // Both calls' prompts and spend are the gated call's own.
        promptBytes: (charter.called.promptBytes ?? 0) + (ruled?.promptBytes ?? 0),
        ...(addUsage(charter.called.usage, ruled?.usage) ?? {}),
        ...(ruled === undefined
          ? {}
          : {
              ruleOutcome: ruled.outcome,
              ruleLatencyMs: ruled.latencyMs,
              ...(ruled.model === undefined ? {} : { ruleModel: ruled.model }),
              ...(ruled.error === undefined ? {} : { ruleError: ruled.error }),
              ...(ruled.textMissing ? { ruleTextMissing: true as const } : {}),
              ...(ruled.asksCut > 0 ? { ruleAsksCut: ruled.asksCut } : {}),
            }),
      };
      const { answer } = charter;
      const verdict = answer.kind === "answer" ? parseVerdict(answer.text) : undefined;
      const matches = ruled?.matches ?? [];
      if (ruled !== undefined && matches.length > 0) {
        // A rule matched: refused in every mode, the charter's revise reason after the rules reason. The
        // charter's own outcome stays on the record and alone moves the halt count. The refusal counts
        // once toward the breaker, the rules reason labelled apart so it never feeds the charter request.
        const rulesReason = [
          ...matchLines(matches, ruled.names),
          ...(ruled.reason === "" ? [] : [ruled.reason]),
        ].join("\n");
        const charterReason = verdict?.decision === "revise" ? verdict.reason : undefined;
        // Which of this call's asks the refusal refused, so that a later call opening one of them
        // is the loop the breaker ends and a call opening another ask is not. The rules check
        // numbers the asks it judged in the order this scan of the same arguments reads them,
        // except for an upload, whose judged list leaves out the asks the document it replaces
        // already holds: where the two do not line up, every ask of the call counts as refused,
        // which keeps the pass the breaker may later give inside the call that earned it.
        const asks = callAsks();
        breaker.revised(key, {
          refused: true,
          rules: rulesReason,
          asks:
            asks.length === ruled.names.length
              ? matches.map((match) => askIdentity(asks[match.ask - 1]))
              : asks.map(askIdentity),
          ...(charterReason === undefined ? {} : { charter: charterReason }),
        });
        const delivered = [
          agentFacing(rulesReason),
          ...(charterReason === undefined
            ? []
            : [agentFacing(`AskGate's charter check also answered revise: ${charterReason}`)]),
        ].join("\n");
        const outcome =
          answer.kind !== "answer" ? answer.kind : verdict === undefined ? "no-verdict" : "verdict";
        const charterFields =
          answer.kind === "error"
            ? { reason: answer.reason }
            : answer.kind === "answer" && verdict === undefined
              ? { raw: answer.text.slice(-2000) }
              : charterReason === undefined
                ? {}
                : { reason: charterReason };
        const [first] = matches;
        return {
          fields: {
            decision: "revise",
            outcome,
            ...charterFields,
            deliveredReason: delivered,
            revisesForKey: base.revisesForKey + 1,
            rule: first.rule,
            ruleAsk: first.ask,
            ruleQuote: first.quote,
            ...called,
          },
          result: {
            block: true,
            reason: renderRuleRefusal(
              delivered,
              matches.map((match) => match.rule),
              breaker.count(key) === GATE_BREAKER_REVISES,
              override,
              SECOND_REVISE
            ),
          },
          effect:
            outcome === "verdict"
              ? "verdict"
              : outcome === "abandoned" || outcome === "shutdown"
                ? "none"
                : "failure",
        };
      }
      // No rule matched, or the rules check ended without a verdict: the charter decides, as it did
      // before there was one.
      if (answer.kind === "error")
        return pass("error", { reason: answer.reason, ...called }, "failure");
      if (answer.kind !== "answer")
        return answer.kind === "timeout"
          ? pass("timeout", called, "failure")
          : givenUp(base.verdictMode, answer.kind, called);
      if (!verdict)
        return pass("no-verdict", { raw: answer.text.slice(-2000), ...called }, "failure");
      if (verdict.decision === "allow" || verdict.reason === undefined) {
        // The charter passed the call, but its new asks were never judged: no verdict is not
        // permission, so block mode refuses it as it refuses any call given up (E5).
        if (ruled?.outcome === "abandoned" || ruled?.outcome === "shutdown")
          return givenUp(base.verdictMode, ruled.outcome, called);
        breaker.reset(key);
        return pass("verdict", called, "verdict");
      }
      // A warn-mode revise sends the call as the agent wrote it: it refused nothing, so it is no
      // loop to break and the breaker's count of refusals stays where it is. Its reason is kept
      // all the same, since the next charter request on this target carries the reasons already
      // given.
      const refused = base.verdictMode === "block";
      breaker.revised(key, { charter: verdict.reason, refused });
      // The third attempt goes out unreviewed only where the breaker's pass would cover it: say
      // so only there.
      const second =
        refused &&
        breaker.count(key) === GATE_BREAKER_REVISES &&
        breaker.covers(key, callAsks().map(askIdentity));
      return {
        fields: {
          decision: "revise",
          outcome: "verdict",
          reason: verdict.reason,
          deliveredReason: agentFacing(verdict.reason),
          revisesForKey: base.revisesForKey + 1,
          ...called,
        },
        result: refused
          ? { block: true, reason: renderRevise(verdict.reason, second, override) }
          : { additionalContext: renderWarn(verdict.reason) },
        effect: "verdict",
      };
    };

    /** One model call of a gated call, raced against that call's one stop: its deadline, its tool's end, a stopped run or the shutdown. */
    type Answer =
      | { kind: "answer"; text: string; usage?: Usage }
      | { kind: "error"; reason: string; usage?: Usage }
      | { kind: "timeout" | "abandoned" | "shutdown"; usage?: Usage };
    const ask = async (req: Omit<CompleteRequest, "signal">, stop: Stop): Promise<Answer> => {
      try {
        const call = deps.complete({ ...req, signal: stop.signal });
        // Promise.race subscribes to the completion, so a late rejection is handled.
        const first = await Promise.race([call, stop.stopped]);
        if (typeof first !== "string") {
          return first.error !== undefined
            ? { kind: "error", reason: first.error, usage: first.usage }
            : { kind: "answer", text: first.text, usage: first.usage };
        }
        // The stop aborted the request, which was billed for what it sent; give it a moment to report that.
        const { promise: grace, resolve: graceOver } = Promise.withResolvers<undefined>();
        const graceTimer = setTimeout(graceOver, stop.graceMs);
        const settled = await Promise.race([call.catch(() => undefined), grace]);
        clearTimeout(graceTimer);
        return { kind: first, usage: settled?.usage };
      } catch (error) {
        return { kind: "error", reason: messageOf(error) };
      }
    };

    /**
     * A scoped command's arguments as the CLI's own --dry-run prints them, with the directory it
     * runs in, or why there are none, or `unscoped`: an upload whose resolved name and path make no
     * markdown document. `directory` runs the command's own `cd` and `pwd -P` once, when a read first
     * needs it, and gives undefined when bash could not say.
     */
    type DryRun =
      | { args: Input; directory: () => Promise<string | undefined> }
      | { failure: string }
      | { cause: Cause }
      | { unscoped: true };
    const readArgs = async (
      scoped: ScopedCommand,
      input: Input,
      ctx: GateCtx,
      stop: Stop
    ): Promise<DryRun> => {
      // The bash tool's own resolution: `~` is home, a bare `/` is the session's directory, and a
      // relative path resolves against it.
      const raw =
        typeof input.cwd === "string" && input.cwd !== ""
          ? input.cwd.replace(/^~(?=$|\/)/, deps.home)
          : undefined;
      const cwd = raw === undefined || /^\/+$/.test(raw) ? ctx.cwd : path.resolve(ctx.cwd, raw);
      /** Runs one script as the dry run runs: in `cwd`, under the limit, stopped with the call. */
      const runScript = async (
        script: string
      ): Promise<{ run: DryRunResult } | { cause: Cause }> => {
        // Stopped like a waiting model call: the run's stop or the session's shutdown kills it.
        const run = await deps.dryRun(script, {
          cwd,
          env: deps.env,
          timeoutMs: DRY_RUN_TIMEOUT_MS,
          signal: stop.signal,
        });
        return stop.cause === undefined ? { run } : { cause: stop.cause };
      };
      /** The script's stdout, or why it gave none: a stop, the limit, or an exit other than 0. */
      type Ran = { stdout: string; stderr: string } | { failure: string } | { cause: Cause };
      const stdoutOf = async (script: string, what: string): Promise<Ran> => {
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
      if (scoped.upload) {
        // The name and the path as the CLI sends them: Dispatch trims the name it stores.
        const resolved = {
          ...args,
          ...(typeof args.name === "string" ? { name: args.name.trim() } : {}),
        };
        if (!uploadIsDocument(resolved)) return { unscoped: true };
      }
      // Where the command runs is the CLI's process.cwd(): the physical directory after the
      // command's own `cd`, which bash resolves (CDPATH and symlinks included), so the gate asks bash
      // for it rather than guessing. An upload's relative `--path` resolves there, and the rules
      // check reads a document as that directory's Dispatch configuration names it; the script runs
      // once, and only for a read that needs it.
      let reached: Promise<Ran> | undefined;
      const reach = () => {
        reached ??= stdoutOf(scoped.dirScript, `${scoped.label}'s directory`);
        return reached;
      };
      // `pwd` ends its line with a newline; nothing else is trimmed, since a directory's name may end
      // in a blank.
      const withoutNewline = (stdout: string) =>
        stdout.endsWith("\n") ? stdout.slice(0, -1) : stdout;
      const directory = async () => {
        const ran = await reach();
        return "stdout" in ran ? withoutNewline(ran.stdout) : undefined;
      };
      // An upload given with `--path` prints the path, not the text: the gate reads the file and adds
      // its text as `content`, so the model sees one shape for both forms of the upload.
      if (!scoped.upload || typeof args.path !== "string" || args.content !== undefined)
        return { args, directory };
      // A relative path resolves where the CLI runs, against its process.cwd(): the directory above.
      let base = cwd;
      if (!path.isAbsolute(args.path)) {
        const ran = await reach();
        if (!("stdout" in ran)) return ran;
        base = withoutNewline(ran.stdout);
      }
      const file = path.resolve(base, args.path);
      let content: string | undefined;
      try {
        content = deps.readFile(file);
      } catch (error) {
        return { failure: `${scoped.label}: ${file} could not be read: ${messageOf(error)}` };
      }
      if (content === undefined)
        return { failure: `${scoped.label}: ${file}, which --path names, does not exist` };
      return { args: { ...args, content }, directory };
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
      // A `dispatch` command with its override flag removed, which runs whatever the gate decides.
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
            // Refused before anything in the call runs, in every mode, when its resend would be reviewed;
            // a halt, or a session whose primary is off the gate's provider, lets it run as the resend
            // would, and a session with no model role refuses it as it would refuse the resend.
            base = { ...identity(event, mode), tool: unreadable.tool, path: unreadable.label };
            const resolved = halted ? undefined : resolveRole(ctx);
            if (halted) decision = pass("halted");
            else if (resolved === undefined) decision = noModelRole;
            else if (ctx.model?.provider !== resolved.model.provider)
              decision = pass("skipped", {
                reason: `primary on ${ctx.model?.provider ?? "no model"}`,
              });
            else
              decision = {
                fields: { decision: "revise", outcome: "unreadable" },
                result: { block: true, reason: renderUnreadable(unreadable) },
                effect: "none",
              };
          } else {
            if (scoped.rebuttal !== undefined)
              revised = { ...event.input, command: scoped.command };
            base = { ...identity(event, mode), tool: scoped.tool, path: scoped.label };
            // One stop for everything this call waits on: its dry run, its document read and both
            // model calls.
            const stop = openStop(event.toolCallId);
            try {
              const run = await readArgs(scoped, event.input, ctx, stop);
              // An upload its dry run shows is no markdown document passes as an unscoped call does,
              // with no entry; an override flag on it is still removed, since the CLI would refuse it.
              if ("unscoped" in run) return revised === undefined ? undefined : { input: revised };
              if ("cause" in run)
                decision =
                  run.cause === "timeout"
                    ? pass("timeout", {}, "failure")
                    : givenUp(mode, run.cause);
              // The CLI refuses arguments it cannot parse before it sends anything, so the command runs.
              else if ("failure" in run) decision = pass("error", { reason: run.failure });
              else {
                const key = breakerKey(scoped.tool, run.args);
                base = {
                  ...base,
                  revisesForKey: breaker.revises(key),
                  argsDigest: createHash("sha256").update(JSON.stringify(run.args)).digest("hex"),
                  target: targetLabel(key),
                };
                decision = await decide(
                  { toolName: scoped.tool, toolCallId: event.toolCallId, input: run.args },
                  ctx,
                  started,
                  base,
                  key,
                  scoped.rebuttal,
                  scoped.readsFile ? FILE_OVERRIDE : COMMAND_OVERRIDE,
                  stop,
                  run.directory,
                  scoped.upload === true
                );
              }
            } finally {
              stop.close();
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
            revisesForKey: breaker.revises(key),
            argsDigest: createHash("sha256").update(JSON.stringify(event.input)).digest("hex"),
            target: targetLabel(key),
          };
          const stop = openStop(event.toolCallId);
          try {
            // A device call runs in the session's own directory; it has no `cd` of its own.
            decision = await decide(
              event,
              ctx,
              started,
              base,
              key,
              rebuttal,
              DEVICE_OVERRIDE,
              stop,
              async () => ctx.cwd
            );
          } finally {
            stop.close();
          }
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
      try {
        settleRules(decision.fields.ruleOutcome, ctx);
      } catch {
        // The rules notice is best effort too.
      }
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
      const entry: GateEntry = {
        ...(base ?? identity(event, mode)),
        ...decision.fields,
        latencyMs: deps.now() - started,
      };
      // The entry below reaches only this machine's disk, so a rules decision also goes to
      // Dispatch, where a session with no access to this machine counts it (`decision-record.ts`).
      // The gated call's own stop is closed by now, so the write gets a stop of its own; whatever
      // it costs or loses, the decision above and the call's result stand.
      let recordError: string | undefined;
      const recordStop = new AbortController();
      const recordTimer = setTimeout(() => recordStop.abort(), RECORD_TIMEOUT_MS);
      try {
        recordError = await recordDecision({
          entry,
          sessionId: ctx.sessionManager.getSessionId(),
          directory: ctx.cwd,
          signal: recordStop.signal,
        });
      } catch (error) {
        recordError = messageOf(error);
      } finally {
        clearTimeout(recordTimer);
      }
      try {
        pi.appendEntry(ADVISOR_GATE_ENTRY_TYPE, {
          ...entry,
          ...(recordError === undefined ? {} : { recordError }),
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
