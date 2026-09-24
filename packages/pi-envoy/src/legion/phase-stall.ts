/**
 * The phase-stall check (LEGION-208 Stage 4b, task 4b.15). A phase worker whose turn ends with its
 * phase still open, without `legion handoff complete` having succeeded since the daemon's last
 * assignment, would otherwise sit idle while nothing wakes anyone: in Stage 3 at 1ee62d31 the
 * round-2 implementer wrote its final `legion handoff complete` as text instead of a tool call, so
 * it never ran. `extensions/legion.ts` feeds this state machine from the session's own hooks and
 * returns its follow-up from `session_stop`, Oh My Pi's hook for a top-level run about to settle,
 * which the host turns into one more turn in the same session.
 */

/** Where the worker's phase stands between the daemon's assignment and a successful
 * `legion handoff complete`:
 * - `closed`: no assignment since the last successful completion, or none seen yet;
 * - `open`: assigned and not completed, so a turn that settles now gets the follow-up;
 * - `quiet`: still open, and this stall already had its follow-up or a WAITING reply, so nothing
 *   more is sent until the next inbound event or assignment. */
export type PhaseStall = "closed" | "open" | "quiet";

export type PhaseStallInput =
  | { readonly kind: "assignment" }
  | { readonly kind: "inbound-event" }
  | { readonly kind: "handoff-complete" }
  | { readonly kind: "settle"; readonly lastAssistantText: string };

export interface PhaseStallStep {
  readonly state: PhaseStall;
  /** The model-visible follow-up for this settle, when one is due. */
  readonly followUp?: string;
}

/** The custom transcript entry (`pi.appendEntry`) holding the state after each change, so a worker
 * the daemon relaunches with `--resume` restores it from its branch. */
export const PHASE_STALL_ENTRY = "legion-phase-stall";

/** A line of the final message that starts with WAITING (after any markdown emphasis or quoting). */
const WAITING_REPLY = /^\W*WAITING\b/m;

/** The opening of a tool call written as text in the Anthropic invoke markup, with or without a
 * namespace prefix: the shape the Stage 3 model emitted in place of a `tool_use` block. */
const TOOL_CALL_SHAPED = /<(?:[\w-]+:)?(?:function_calls\b|invoke\s+name\s*=|parameter\s+name\s*=)/;

const FOLLOW_UP =
  "Your turn ended with your Legion phase still open: the `legion` tool's `handoff_complete` has " +
  "not succeeded since your assignment. If your phase's work is done, call the `legion` tool now " +
  "with op `handoff_complete` and the fields your role's instructions give. If you are waiting on " +
  "something (CI, a review, a person), reply with a line starting WAITING that names what you are " +
  "waiting on; nothing more will be sent until a new event arrives.";

const TEXT_TOOL_CALL =
  "Your last message holds a tool call written as text, so it did not run: make it a real tool call. ";

export function stepPhaseStall(state: PhaseStall, input: PhaseStallInput): PhaseStallStep {
  switch (input.kind) {
    case "assignment":
      return { state: "open" };
    case "handoff-complete":
      return { state: "closed" };
    case "inbound-event":
      return { state: state === "quiet" ? "open" : state };
    case "settle":
      if (state !== "open") return { state };
      if (WAITING_REPLY.test(input.lastAssistantText)) return { state: "quiet" };
      return {
        state: "quiet",
        followUp: TOOL_CALL_SHAPED.test(input.lastAssistantText)
          ? TEXT_TOOL_CALL + FOLLOW_UP
          : FOLLOW_UP,
      };
  }
}

/** What an arriving message means for the phase. The daemon delivers its assignment as the RPC
 * `prompt`, which Oh My Pi records as a user message; an Envoy delivery arrives as an
 * `envoy-message` custom message. Anything else (the host's own continuations, this check's
 * follow-up among them, tool results, the model's replies) is neither. */
export function inboundKind(message: unknown): "assignment" | "inbound-event" | undefined {
  if (typeof message !== "object" || message === null || !("role" in message)) return undefined;
  if (message.role === "user") return "assignment";
  if (message.role === "custom" && "customType" in message && message.customType === "envoy-message")
    return "inbound-event";
  return undefined;
}

/** The text blocks of an assistant message, joined; empty for anything else. */
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

function isPhaseStall(value: unknown): value is PhaseStall {
  return value === "closed" || value === "open" || value === "quiet";
}

/** The state the branch's last phase-stall entry recorded; `closed` when it holds none (a fresh
 * session, before the daemon's first assignment). */
export function restorePhaseStall(branch: readonly unknown[]): PhaseStall {
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
      "state" in entry.data &&
      isPhaseStall(entry.data.state)
    ) {
      return entry.data.state;
    }
  }
  return "closed";
}
