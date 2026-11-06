import { isSessionAnswersText } from "@legion/envoy-client/session-answers";
import { leadingText, userTextMessage } from "./context-message";

/** The first line of Oh My Pi's side-turn notice (`side-channel-no-tools.md`): the `developer`
 *  message `AgentSession.runEphemeralTurn` puts after the conversation, before the side prompt. */
const SIDE_TURN_NOTICE = "Ephemeral side-channel turn; reuses current conversation context.";

/** How Oh My Pi's handoff prompt (`handoff-document.md`) begins. */
const HANDOFF_PROMPT = "<critical>\nWrite a handoff document for another instance of yourself.";

/** The requests a copy of this extension already decided the list for: the messages array the
 *  host hands each `context` handler of one request in turn, which a handler that returns nothing
 *  passes on as it is. It lives on `globalThis` under a registered symbol, as the read state of
 *  `@legion/envoy-client/session-answers` does, because Oh My Pi loads each copy of this extension
 *  as a module of its own. A change to its shape takes a new key (`.v2`). */
const DECIDED_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-requests.v1");
const processGlobal = globalThis as typeof globalThis & { [DECIDED_KEY]?: WeakSet<object> };
const decided = processGlobal[DECIDED_KEY] ?? new WeakSet<object>();
processGlobal[DECIDED_KEY] = decided;

/** The text of `message` when the host wrote it (`attribution: "agent"`) with `role`. */
function hostText(message: unknown, role: string): string | undefined {
  return typeof message === "object" &&
    message !== null &&
    "attribution" in message &&
    message.attribution === "agent"
    ? leadingText(message, role)
    : undefined;
}

/**
 * Whether the request's last message is the list of people's answers as an older copy of this
 * extension appends it, after the last message on every request: a user message whose first text
 * block starts with the list's marker or the failure line's. Only that one position is checked,
 * as `withDispatchFirst` checks only its own: a tool result, delivered message or reply quoting
 * the marker earlier in the conversation is conversation, which must not switch the list off. A
 * copy of this version records the array it returns in `decided` instead, wherever its list sits.
 */
function endsWithSessionAnswers(messages: readonly unknown[]): boolean {
  const text = leadingText(messages.at(-1), "user");
  return text !== undefined && isSessionAnswersText(text);
}

/** A context message as matching reads it: its role and timestamp, and a tool result's call id,
 *  since the tool results of one round can share a timestamp. */
function messageKey(message: unknown): string {
  if (typeof message !== "object" || message === null) return "";
  const role = "role" in message ? String(message.role) : "";
  const timestamp = "timestamp" in message ? String(message.timestamp) : "";
  const call = "toolCallId" in message ? `#${String(message.toolCallId)}` : "";
  return `${role}@${timestamp}${call}`;
}

/**
 * Whether the request is a side turn: Oh My Pi's `runEphemeralTurn` (a Dispatch BTW, the stop-time
 * self-check, `/btw`, the idle recap) converts `[...conversation, notice, ...history, prompt]`,
 * where the notice is a host `developer` message, the history `/btw`'s earlier questions and
 * answers, and the prompt a host user message.
 */
function isSideTurn(messages: readonly unknown[]): boolean {
  if (hostText(messages.at(-1), "user") === undefined) return false;
  for (let index = messages.length - 2; index >= 0; index -= 1) {
    const message = messages[index];
    const role =
      typeof message === "object" && message !== null && "role" in message
        ? message.role
        : undefined;
    if (role !== "user" && role !== "assistant") {
      return hostText(message, "developer")?.includes(SIDE_TURN_NOTICE) === true;
    }
  }
  return false;
}

/**
 * The context message a branch entry becomes, as far as matching reads it, or undefined for an
 * entry that becomes none. The pinned host (18.12.2) builds a compaction's messages from the
 * branch with `MI`:
 *   if (e.type === "message") return e.message;
 *   if (e.type === "custom_message") return yu(e.customType, e.content, e.display, e.details, e.timestamp, e.attribution);
 *   if (e.type === "branch_summary") return hm(e.summary, e.fromId, e.timestamp);
 * where `yu` returns `{role: "custom", ..., timestamp: new Date(s).getTime()}` and `hm` returns
 * `{role: "branchSummary", ..., timestamp: new Date(o).getTime()}`. A `compaction` entry becomes
 * no message there: the earlier summary heads the conversion, so no cut ends on it.
 */
function entryMessage(entry: unknown):
  | {
      readonly role?: unknown;
      readonly timestamp?: unknown;
      readonly stopReason?: unknown;
      readonly content?: unknown;
      readonly toolCallId?: unknown;
    }
  | undefined {
  if (typeof entry !== "object" || entry === null || !("type" in entry)) return undefined;
  if (entry.type === "message") {
    return "message" in entry && typeof entry.message === "object" && entry.message !== null
      ? entry.message
      : undefined;
  }
  const role =
    entry.type === "custom_message"
      ? "custom"
      : entry.type === "branch_summary"
        ? "branchSummary"
        : undefined;
  if (role === undefined) return undefined;
  const timestamp =
    "timestamp" in entry && typeof entry.timestamp === "string"
      ? new Date(entry.timestamp).getTime()
      : undefined;
  return { role, timestamp };
}

/** Where a request ends on the session's branch: at a message with a later one after it, at the
 *  branch's last message (`tip`, the id of the branch's last entry that becomes a message), or at
 *  a message the branch does not hold. */
type BranchPosition =
  | { readonly at: "before" }
  | { readonly at: "last"; readonly tip: string }
  | { readonly at: "absent" };

/** The tool call ids of `message` when it is an assistant reply that failed or was aborted. */
function failedCalls(message: {
  readonly role?: unknown;
  readonly stopReason?: unknown;
  readonly content?: unknown;
}): string[] {
  if (
    message.role !== "assistant" ||
    (message.stopReason !== "error" && message.stopReason !== "aborted")
  ) {
    return [];
  }
  return (Array.isArray(message.content) ? message.content : []).flatMap((block: unknown) =>
    typeof block === "object" &&
    block !== null &&
    "type" in block &&
    block.type === "toolCall" &&
    "id" in block
      ? [String(block.id)]
      : []
  );
}

/**
 * Where the request's last message sits on the session's branch, matched by role and timestamp.
 * Oh My Pi's Anthropic remote compaction (`buildLiveProviderContext`) converts the conversation up
 * to its cut alone, counts the result, and keeps that many messages of the whole conversation's
 * conversion: a list on the first would make it keep one message past the cut. A main-loop request
 * ends at a message not yet written, or at the branch's last. A failed or aborted reply, and the
 * results the host wrote for its tool calls, which a retry drops from the context but not from the
 * branch (`turn-recovery`), do not count as later.
 */
function branchPosition(messages: readonly unknown[], entries: readonly unknown[]): BranchPosition {
  const last = messages.at(-1);
  if (typeof last !== "object" || last === null || !("timestamp" in last) || !("role" in last)) {
    return { at: "absent" };
  }
  const dropped = new Set(
    entries.flatMap((entry) => {
      const message = entryMessage(entry);
      return message === undefined ? [] : failedCalls(message);
    })
  );
  let later = false;
  let tip: string | undefined;
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    const message = entryMessage(entry);
    if (message === undefined) continue;
    tip ??= typeof entry === "object" && entry !== null && "id" in entry ? String(entry.id) : "";
    if (message.role === last.role && message.timestamp === last.timestamp) {
      return later ? { at: "before" } : { at: "last", tip };
    }
    const failed =
      (message.role === "assistant" &&
        (message.stopReason === "error" || message.stopReason === "aborted")) ||
      (message.role === "toolResult" && dropped.has(String(message.toolCallId)));
    if (!failed) later = true;
  }
  return { at: "absent" };
}

/** A session's last decided conversion: a remote compaction's cut (`"cut"`), or a request with its
 *  key (undefined unless it ended at the branch's last message) and whether it came right after a
 *  cut. */
type Conversion = "cut" | { readonly key: string | undefined; readonly afterCut: boolean };

/** Each session's last decided conversion, by the session's `sessionManager`. It lives on
 *  `globalThis` as `decided` does, since only the copy that decides a request records it. */
const CONVERSIONS_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-conversions.v1");
const conversionsGlobal = globalThis as typeof globalThis & {
  [CONVERSIONS_KEY]?: WeakMap<object, Conversion>;
};
const conversions = conversionsGlobal[CONVERSIONS_KEY] ?? new WeakMap<object, Conversion>();
conversionsGlobal[CONVERSIONS_KEY] = conversions;

/** What a session's list carries: its text, and what the people wrote in it
 *  (`SessionAnswersRead.words`) when the read gave that. */
export interface CarriedAnswers {
  readonly text: string;
  readonly words?: readonly string[] | undefined;
}

/** Where a session's list sits: right after the message (`messageKey`) that was the last of the
 *  request that first showed what it shows, and what it carried last. */
interface Placement {
  readonly after: string;
  readonly carried: CarriedAnswers;
}

/** Each session's placement, by the session's `sessionManager` and then its session id (Oh My Pi
 *  keeps one `sessionManager` when it switches sessions), on `globalThis` as `conversions` is. A
 *  change to its shape takes a new key (`.v2`). */
const PLACEMENTS_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-placements.v1");
const placementsGlobal = globalThis as typeof globalThis & {
  [PLACEMENTS_KEY]?: WeakMap<object, Map<string, Placement>>;
};
const placements =
  placementsGlobal[PLACEMENTS_KEY] ?? new WeakMap<object, Map<string, Placement>>();
placementsGlobal[PLACEMENTS_KEY] = placements;

/** Whether `next` shows something `shown` did not: an item, or an answer or comment a person
 *  changed. Without both reads' words (a list an older copy of this extension placed), any change
 *  in the text counts. */
function showsNew(shown: CarriedAnswers, next: CarriedAnswers): boolean {
  if (shown.words === undefined || next.words === undefined) return shown.text !== next.text;
  const before = new Set(shown.words);
  return next.words.some((word) => !before.has(word));
}

/**
 * `messages` with the list inserted where it belongs. A list that shows something its session's
 * last list did not goes after the last message, where the model reads it as what arrived last,
 * and the session's list stays right after that message on every later request while it shows
 * nothing new: the model reads it as part of the conversation, and its bytes sit at the same place
 * in each request, so the cached prefix of each request runs through it. A session with no list
 * placed yet, or whose list's message left the conversation (a compaction summarized it), gets it
 * after the last message again. While a compaction may be converting the conversation
 * (`compacting`) the list goes after the last message and places nothing: a remote compaction
 * keeps only as many messages of its whole conversion as its cut's conversion holds, so a list
 * before the cut would push one out.
 */
function placeList(
  messages: readonly unknown[],
  carried: CarriedAnswers,
  request: SessionAnswersRequest,
  compacting: boolean
): unknown[] {
  const list = userTextMessage(carried.text);
  if (compacting) return [...messages, list];
  const sessions = placements.get(request.session) ?? new Map<string, Placement>();
  placements.set(request.session, sessions);
  const placed = sessions.get(request.id);
  if (placed !== undefined && !showsNew(placed.carried, carried)) {
    const index = messages.findLastIndex((message) => messageKey(message) === placed.after);
    if (index >= 0) {
      sessions.set(request.id, { after: placed.after, carried });
      return [...messages.slice(0, index + 1), list, ...messages.slice(index + 1)];
    }
  }
  sessions.set(request.id, { after: messageKey(messages.at(-1)), carried });
  return [...messages, list];
}

export interface SessionAnswersRequest {
  /** The session's branch entries (`sessionManager.getBranch`). */
  readonly branch: () => readonly unknown[];
  /** The session the request is for (its `sessionManager`): the key of its last conversion. */
  readonly session: object;
  /** The session's id (`sessionManager.getSessionId`): with `session`, the key of its list's
   *  place. */
  readonly id: string;
  /** Whether Oh My Pi is compacting the session now: the live `AgentSession`'s `isCompacting`, or
   *  its `compactionSpeculation` reading `"running"`. */
  readonly compacting: () => boolean;
  /** The list to carry, or undefined for none. */
  readonly read: () => Promise<CarriedAnswers | undefined>;
}

/**
 * The messages one main-loop provider request carries, with what people wrote to the session
 * (`request.read`'s text) as a separate user message where `placeList` puts it: after the last
 * message when it shows something new, else where the session's list already sat. Undefined when
 * `read` gives no text or the request carries no list, in which case `read` is not called:
 *
 * - another copy of this extension already decided this request: it placed the list (and passed
 *   on the array it returned), or it read and found no text, or the request is one of those below;
 * - a side turn, whose last message must stay its prompt and which must not wait on a read;
 * - the handoff prompt, whose last message must stay that prompt;
 * - a conversion of the conversation up to a remote compaction's cut: it ends at a message the
 *   branch holds a later one after (`branchPosition`);
 * - a remote compaction's whole conversion when it keeps no tail: then it converts the same
 *   messages as its cut, both ending at the branch's last message, so the second of the two is a
 *   repeat of the conversion before it, over the same branch, while the session compacts. With no
 *   list on that one, the compaction keeps no list whatever the first carried:
 *     const ve = (await this.convertMessagesToLlm(Se, ce, pe)).length;
 *     const Re = await this.agent.buildSideRequestContext(await this.convertMessagesToLlm([...Se, ...le], ce, pe));
 *     for (let Ue = 0;Le < Re.messages.length && Ue < ve; Le++) ...
 *     return { ...Re, messages: Re.messages.slice(0, Le) };
 *   A repeat right after a cut is the main loop's next request beside a speculative compaction
 *   with a tail, whose whole conversion it matches, and keeps its list.
 *
 * It is never at the head, so a change in the list costs only the bytes after it: Oh My Pi marks a
 * message a `context` handler adds as per-call, and its Anthropic placement puts no cache anchor on
 * a per-call message, so every byte before the list is the cached prefix of the request before it,
 * and a list that keeps its place and its bytes is inside that prefix too. It is never merged into
 * a message of the conversation, whose bytes and cache anchors are the conversation's.
 */
export async function withSessionAnswers(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> {
  if (decided.has(messages)) return undefined;
  decided.add(messages);
  if (endsWithSessionAnswers(messages)) return placedLegacy(messages, request);
  if (
    isSideTurn(messages) ||
    hostText(messages.at(-1), "user")?.startsWith(HANDOFF_PROMPT) === true
  ) {
    return undefined;
  }
  const position = branchPosition(messages, request.branch());
  const previous = conversions.get(request.session);
  if (position.at === "before") {
    conversions.set(request.session, "cut");
    return undefined;
  }
  // What the request converted, for telling a repeat of it apart: the branch's last message and
  // each message's role and timestamp.
  const key =
    position.at === "last" ? [position.tip, ...messages.map(messageKey)].join("\n") : undefined;
  conversions.set(request.session, { key, afterCut: previous === "cut" });
  if (
    key !== undefined &&
    typeof previous === "object" &&
    previous.key === key &&
    !previous.afterCut &&
    request.compacting()
  ) {
    return undefined;
  }
  const carried = await request.read();
  if (carried === undefined) return undefined;
  const placed = placeList(messages, carried, request, previous === "cut" || request.compacting());
  decided.add(placed);
  return placed;
}

/**
 * The request an older copy of this extension, which puts the list after the last message of
 * every request, already gave its list: that list moved where `placeList` puts it, with no read of
 * this copy's own. The older copy decided which requests carry one.
 */
function placedLegacy(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): unknown[] | undefined {
  const last = messages.at(-1);
  const text = leadingText(last, "user");
  // The list a copy adds is stamped 0 (`userTextMessage`); a person's own message quoting the
  // marker is theirs, never moved.
  const added =
    typeof last === "object" && last !== null && "timestamp" in last && last.timestamp === 0;
  if (text === undefined || !added) return undefined;
  const recorded = conversions.get(request.session);
  const compacting = (typeof recorded === "object" && recorded.afterCut) || request.compacting();
  const placed = placeList(messages.slice(0, -1), { text }, request, compacting);
  decided.add(placed);
  return placed;
}
