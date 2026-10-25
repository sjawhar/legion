import { LOCAL_ENVOY_NOTICE } from "./envoy-notice";

/**
 * The phase-stall check (LEGION-208 Stage 4b, task 4b.15; made endless by INFRA-220). A phase
 * worker whose turn ends with its phase still open, without the `legion` tool's `handoff_complete`
 * having succeeded since the daemon's last assignment, would otherwise sit idle while nothing wakes
 * anyone: in Stage 3 at 1ee62d31 the round-2 implementer wrote its final `legion handoff complete`
 * as text instead of a tool call, so it never ran, and two trees later waited six hours each on a
 * worker that had had its one follow-up. So the worker is reminded at the end of every turn, for as
 * long as its phase stays open. `extensions/legion.ts` feeds this state machine from the session's
 * own hooks and `src/phase-reminder.ts` delivers each reminder: the first of a run from
 * `session_stop`, Oh My Pi's hook for a top-level run about to settle, which the host turns into one
 * more turn in the same session, and every one after it from a timer, so the run settles and the
 * daemon sees the turn end. When `LEGION_STALL_ARCHITECT_AFTER` reminders in a row change nothing,
 * the session tells the architect over Envoy.
 */

/** Where the worker's phase stands between the daemon's assignment and a successful
 * `handoff_complete`:
 * - `closed`: no assignment since the last successful completion, or none seen yet;
 * - `open`: assigned and not completed, so a turn that settles now earns a reminder;
 * - `waiting`: still open, and the worker's last reply said with a WAITING line what it waits on,
 *   which buys the longer quiet wait `LEGION_STALL_WAITING_MS` before the next reminder.
 * The reminding never stops: a reminder that changes nothing is followed by another. */
export type PhaseStall = "closed" | "open" | "waiting";

/** The state and how many reminders the worker has done nothing about: `count` climbs by one on
 * every reminder the host actually put in front of the worker and drops to 0 as soon as the worker,
 * or the daemon, does anything at all. */
export interface PhaseStallState {
  readonly state: PhaseStall;
  readonly count: number;
}

export type PhaseStallInput =
  | { readonly kind: "assignment" }
  | { readonly kind: "inbound-event" }
  | { readonly kind: "phase-closed" }
  /** The settling assistant message ran at least one real tool call (`assistantRanTool`). */
  | { readonly kind: "tool-call" }
  | { readonly kind: "settle"; readonly lastAssistantText: string }
  /** The host has just delivered a reminder to the worker. */
  | { readonly kind: "reminded" };

export interface PhaseStallStep {
  readonly state: PhaseStallState;
  /** The model-visible reminder this settle earns, when one is due. */
  readonly reminder?: string;
}

/** The custom transcript entry (`pi.appendEntry`) holding the state after each change, so a worker
 * the daemon relaunches with `--resume` restores it from its branch. */
export const PHASE_STALL_ENTRY = "legion-phase-stall";

/** A line of the final message that starts with WAITING (after any markdown emphasis or quoting). */
const WAITING_REPLY = /^\W*WAITING\b/m;

/** The opening of a tool call written as text in the Anthropic invoke markup, with or without a
 * namespace prefix: the shape the Stage 3 model emitted in place of a `tool_use` block. */
const TOOL_CALL_SHAPED = /<(?:[\w-]+:)?(?:function_calls\b|invoke\s+name\s*=|parameter\s+name\s*=)/;

/** What a worker whose turn ended with its phase open is told. This is the one copy, read by
 * both hosts: the Claude Code bridge kept its own (`packages/daemon/internal/claudebridge/
 * stall.go`, deleted with this change) until the bridge became pure transport. */
export const REMINDER =
  "Your turn ended with your Legion phase still open: the `legion` tool's `handoff_complete` has " +
  "not succeeded since your assignment. If your phase's work is done, call the `legion` tool now " +
  "with op `handoff_complete` and the fields your role's instructions give. If you are waiting on " +
  "something (CI, a review, a person), reply with a line starting WAITING that names what you are " +
  "waiting on: that buys a quiet wait, and then this reminder comes again. This reminder repeats " +
  "at the end of every turn for as long as your phase is open.";

const TEXT_TOOL_CALL =
  "Your last message holds a tool call written as text, so it did not run: make it a real tool call. ";

/** The reminder a settling message earns: `REMINDER`, led by `TEXT_TOOL_CALL` when that message
 * holds a tool call written as text, which is the step the worker failed to take. A reminder no
 * settling message decided (the resume timer's) passes `""` and gets `REMINDER` alone. */
export function reminderText(lastAssistantText: string): string {
  return TOOL_CALL_SHAPED.test(lastAssistantText) ? TEXT_TOOL_CALL + REMINDER : REMINDER;
}

/**
 * The next state, and the reminder the worker has earned by settling. The count moves only on
 * `reminded`, which the host applies once it has actually put the reminder in front of the worker,
 * so a reminder dropped because a task arrived first counts for nothing; `settle` never changes it.
 * `reminded` from `waiting` goes back to `open`, because the wait is over.
 */
export function stepPhaseStall(state: PhaseStallState, input: PhaseStallInput): PhaseStallStep {
  switch (input.kind) {
    case "assignment":
      return { state: { state: "open", count: 0 } };
    case "phase-closed":
      return { state: { state: "closed", count: 0 } };
    case "inbound-event":
      // A delivery may be what the worker waited on, and it is something happening: an open phase
      // starts its count again. A closed phase is not reopened by one.
      return state.state === "closed" ? { state } : { state: { state: "open", count: 0 } };
    case "tool-call":
      // The worker is working, whatever it is working on: nothing is unanswered any more.
      return { state: { state: state.state, count: 0 } };
    case "reminded":
      return state.state === "closed"
        ? { state }
        : { state: { state: "open", count: state.count + 1 } };
    case "settle": {
      if (state.state !== "open") return { state };
      if (WAITING_REPLY.test(input.lastAssistantText)) {
        return { state: { state: "waiting", count: state.count } };
      }
      return { state, reminder: reminderText(input.lastAssistantText) };
    }
  }
}

/** What an arriving message means for the phase. The daemon delivers its assignment as the RPC
 * `prompt`, which Oh My Pi records as a user message; an Envoy delivery arrives as an
 * `envoy-message` custom message. A person's direct message from Dispatch that the Envoy extension
 * sent in as the user's own turn is a user message as well, or for a `/skill:` the host's
 * `skill-prompt` custom message, which `extensions/legion.ts` tells apart before asking this
 * (`matchInjectedUserTurn`). Anything else (the host's own continuations, this check's reminder
 * among them, the Envoy extension's own notices, tool results, the model's replies) is neither.
 * A reminder the plugin sends through `pi.sendMessage` carries its own custom type for that reason:
 * a `role: "user"` message or an `envoy-message` would make the reminder reset its own count. */
export function inboundKind(message: unknown): "assignment" | "inbound-event" | undefined {
  if (typeof message !== "object" || message === null || !("role" in message)) return undefined;
  if (message.role === "user") return "assignment";
  if (
    message.role === "custom" &&
    "customType" in message &&
    message.customType === "envoy-message" &&
    !(
      "details" in message &&
      typeof message.details === "object" &&
      message.details !== null &&
      "localNotice" in message.details &&
      message.details.localNotice === LOCAL_ENVOY_NOTICE.localNotice
    )
  )
    return "inbound-event";
  return undefined;
}

/** The text blocks of an assistant message, joined; empty for anything else. A text block is
 * `{ type: "text", text }` on both hosts' shapes: Oh My Pi's own message model, which `message_end`
 * and `session_stop` hand the plugin (`packages/pi-envoy/src/agent-stream.ts` reads it the same
 * way), and Claude Code's, whose `Stop` hook hands its host the last message as a string, so that
 * host never comes here. */
export function assistantText(message: unknown): string {
  if (typeof message !== "object" || message === null) return "";
  if (!("role" in message) || message.role !== "assistant" || !("content" in message)) return "";
  if (!Array.isArray(message.content)) return "";
  return message.content
    .map((block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "type" in block &&
      block.type === "text" &&
      "text" in block &&
      typeof block.text === "string"
        ? block.text
        : ""
    )
    .join("\n");
}

/** Whether a settled assistant message ran at least one real tool call: its `content` holds a
 * tool-call block. The plugin reads this from the host's `message_end`; it registers no
 * `tool_call` hook, so nothing it does can block or shape a call the worker makes.
 *
 * Two block names are accepted because the hosts disagree on it: Oh My Pi's `message_end` hands a
 * `toolCall` block (`{ type: "toolCall", id, name, arguments }`, the shape
 * `packages/pi-envoy/src/agent-stream.ts` reads), and Claude Code names the same thing `tool_use`
 * (its stream-json, which `packages/daemon/internal/claudebridge/bridge.go` reads). Matching only
 * `tool_use` once left a working Oh My Pi worker reported to its architect as stalled. */
export function assistantRanTool(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  if (!("role" in message) || message.role !== "assistant" || !("content" in message)) return false;
  if (!Array.isArray(message.content)) return false;
  return message.content.some(
    (block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "type" in block &&
      (block.type === "toolCall" || block.type === "tool_use")
  );
}

/** The state one recorded entry holds, or undefined when it holds none this build understands.
 * `quiet` and `text-call` are the build now in the pods: both mean the phase is open and nothing
 * has been reminded about it yet, so a session resumed on one is reminded rather than left alone. */
function phaseStallFrom(data: object): PhaseStallState | undefined {
  if (!("state" in data) || typeof data.state !== "string") return undefined;
  const recorded = "count" in data ? data.count : undefined;
  const count =
    typeof recorded === "number" && Number.isInteger(recorded) && recorded >= 0 ? recorded : 0;
  switch (data.state) {
    case "closed":
    case "open":
    case "waiting":
      return { state: data.state, count };
    case "quiet":
    case "text-call":
      return { state: "open", count: 0 };
    default:
      return undefined;
  }
}

/** The data of the branch's last entry this build understands, or undefined when it holds none:
 * the one place the restore reads, so the state and the plugin's own report fields come from one
 * entry rather than two scans that could disagree. */
function lastPhaseStallData(branch: readonly unknown[]): object | undefined {
  for (let index = branch.length - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    if (
      typeof entry === "object" &&
      entry !== null &&
      "type" in entry &&
      entry.type === "custom" &&
      "customType" in entry &&
      entry.customType === PHASE_STALL_ENTRY &&
      "data" in entry &&
      typeof entry.data === "object" &&
      entry.data !== null &&
      phaseStallFrom(entry.data) !== undefined
    ) {
      return entry.data;
    }
  }
  return undefined;
}

/** The head of the worker's last message the architect notice quotes, and so all of it the record
 * keeps: a file and a transcript written on every settle hold no more than the notice can use. */
export const LAST_MESSAGE_LIMIT = 500;

/** What one recorded entry carries. The machine never reads `reported`, `reportKey` and
 * `lastMessage`, and each host persists them its own way — the Oh My Pi transcript entry, the
 * Claude Code state file — but they ride the same record, so a relaunch mid-run neither repeats an
 * accepted notice, nor loses the key a refused one is retried under, nor sends one that has
 * forgotten what the worker last said. */
export interface PhaseStallEntry {
  readonly state: PhaseStall;
  readonly count: number;
  readonly reported: boolean;
  readonly reportKey?: string;
  /** The head (`LAST_MESSAGE_LIMIT`) of what the worker said as its last turn settled; absent
   * while it has said nothing, and in a record an earlier build wrote. */
  readonly lastMessage?: string;
}

/** Whether the architect has already been told about the run of reminders one record belongs to,
 * and the idempotency key that notice was sent under. */
function phaseStallReportFrom(data: object): {
  readonly reported: boolean;
  readonly reportKey: string | undefined;
} {
  const reported = "reported" in data && data.reported === true;
  const key =
    "reportKey" in data && typeof data.reportKey === "string" ? data.reportKey : undefined;
  return { reported, reportKey: key };
}

/** The record one entry's data holds, or undefined when it holds no state this build understands.
 * The one assembly of a restored record: the Oh My Pi transcript entry reaches it through
 * `restorePhaseStallEntry` and the Claude Code state file through `readPhaseStallRecord`
 * (`packages/claude-envoy/src/legion-phase-hook.ts`), so no host assembles its own. */
export function phaseStallEntryFrom(data: object | undefined): PhaseStallEntry | undefined {
  if (data === undefined) return undefined;
  const stall = phaseStallFrom(data);
  if (stall === undefined) return undefined;
  const report = phaseStallReportFrom(data);
  const lastMessage =
    "lastMessage" in data && typeof data.lastMessage === "string"
      ? data.lastMessage.slice(0, LAST_MESSAGE_LIMIT)
      : "";
  return {
    state: stall.state,
    count: stall.count,
    reported: report.reported,
    ...(report.reportKey === undefined ? {} : { reportKey: report.reportKey }),
    ...(lastMessage === "" ? {} : { lastMessage }),
  };
}

/** The record the branch's last phase-stall entry holds, or undefined when it holds none (a fresh
 * session, before the daemon's first assignment). */
export function restorePhaseStallEntry(branch: readonly unknown[]): PhaseStallEntry | undefined {
  return phaseStallEntryFrom(lastPhaseStallData(branch));
}
