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
 * Whether the request's last message is the list of people's answers as `withSessionAnswers`
 * appends it: a user message whose first text block starts with the list's marker or the failure
 * line's. Only that one position is checked, as `withDispatchFirst` checks only its own: every
 * copy of this extension appends there, and a tool result, delivered message or reply quoting the
 * marker earlier in the conversation is conversation, which must not switch the list off.
 */
function endsWithSessionAnswers(messages: readonly unknown[]): boolean {
  const text = leadingText(messages.at(-1), "user");
  return text !== undefined && isSessionAnswersText(text);
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
function entryMessage(
  entry: unknown
):
  | { readonly role?: unknown; readonly timestamp?: unknown; readonly stopReason?: unknown }
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

/**
 * Where the request's last message sits on the session's branch, matched by role and timestamp.
 * Oh My Pi's Anthropic remote compaction (`buildLiveProviderContext`) converts the conversation up
 * to its cut alone, counts the result, and keeps that many messages of the whole conversation's
 * conversion: a list on the first would make it keep one message past the cut. A main-loop request
 * ends at a message not yet written, or at the branch's last; a failed or aborted reply, which a
 * retry drops from the context but not from the branch, does not count as later.
 */
function branchPosition(messages: readonly unknown[], entries: readonly unknown[]): BranchPosition {
  const last = messages.at(-1);
  if (typeof last !== "object" || last === null || !("timestamp" in last) || !("role" in last)) {
    return { at: "absent" };
  }
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
      message.role === "assistant" &&
      (message.stopReason === "error" || message.stopReason === "aborted");
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

export interface SessionAnswersRequest {
  /** The session's branch entries (`sessionManager.getBranch`). */
  readonly branch: () => readonly unknown[];
  /** The session the request is for (its `sessionManager`): the key of its last conversion. */
  readonly session: object;
  /** Whether Oh My Pi is compacting the session now: the live `AgentSession`'s `isCompacting`, or
   *  its `compactionSpeculation` reading `"running"`. */
  readonly compacting: () => boolean;
  /** The list to carry, or undefined for none. */
  readonly read: () => Promise<string | undefined>;
}

/**
 * The messages one main-loop provider request carries, with what people wrote to the session
 * (`request.read`'s text) appended as a separate user message after the last one; undefined when
 * `read` gives no text or the request carries no list, in which case `read` is not called:
 *
 * - another copy of this extension already decided this request: it appended the list, or it
 *   read and found no text, or the request is one of those below;
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
 * It sits at the tail, never at the head, so a change in the list costs only its own tokens:
 * Oh My Pi marks a message a `context` handler adds as per-call, and its Anthropic placement puts
 * no cache anchor on a per-call message, so every byte before the list is the cached prefix of the
 * request before it. It is never merged into the last message, which is the conversation's and
 * carries the request's cache anchor.
 */
export async function withSessionAnswers(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> {
  if (decided.has(messages) || endsWithSessionAnswers(messages)) return undefined;
  decided.add(messages);
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
    position.at === "last"
      ? [
          position.tip,
          ...messages.map((message) =>
            typeof message === "object" && message !== null
              ? `${"role" in message ? String(message.role) : ""}@${"timestamp" in message ? String(message.timestamp) : ""}`
              : ""
          ),
        ].join("\n")
      : undefined;
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
  const text = await request.read();
  if (text === undefined) return undefined;
  return [...messages, userTextMessage(text)];
}
