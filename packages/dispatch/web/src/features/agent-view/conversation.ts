import type { ThreadMessageLike } from "@assistant-ui/react";
import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_TRUNCATION_SUFFIX,
  type AgentStreamCommand,
  type AgentStreamFrame,
  type AgentStreamMessage,
  type AgentStreamSettings,
  type AgentStreamSettingsReported,
  type AgentStreamToolResult,
  comparableLines,
  isTypableCommandName,
  pictureSyntax,
  trimmedLines,
} from "@legion/contracts";
import { dispatchMetadata, type KeyedMessage } from "./dispatch-marks";

/**
 * The live conversation a session is streaming, assembled from frames that arrive in whatever
 * order the session's own concurrent handlers produced them.
 *
 * Every frame is a whole snapshot of one message, so applying them is last-writer-wins by the
 * session's monotonic `seq`: a frame that lost a race is dropped rather than overwriting a newer
 * snapshot of the same message. Order in the thread is the host's own message timestamp, never
 * arrival order.
 */
export interface AgentConversation {
  readonly messages: readonly AgentStreamMessage[];
  /** Tool results keyed by tool call id, which is how they find their call. */
  readonly results: Readonly<Record<string, AgentStreamToolResult>>;
  /** The slash commands the session takes from Dispatch, as its newest `commands` frame lists
   *  them; undefined until one arrives, which a session that cannot take typed input never sends. */
  readonly commands: readonly AgentStreamCommand[] | undefined;
  /** The model and thinking level the session runs now, as its newest `settings` frame reports
   *  them (INFRA-195); absent until one arrives, which a plugin older than the frame never sends. */
  readonly settings?: AgentStreamSettings;
  /** The highest `seq` already applied per message id, per tool call id, to the command list and
   *  to the settings. */
  readonly applied: Readonly<Record<string, number>>;
}

export const EMPTY_CONVERSATION: AgentConversation = {
  applied: {},
  commands: undefined,
  messages: [],
  results: {},
};

/** Where a command's `source` may come from, as the contract lists them. */
const COMMAND_SOURCES: Record<AgentStreamCommand["source"], true> = {
  builtin: true,
  extension: true,
  prompt: true,
  skill: true,
};

/** Whether one listed command has the contract's shape. The list is the bus's like any frame, and
 *  the composer renders every field of it. A name a person could not type is no command the
 *  session lists (`isTypableCommandName`, which pi-envoy's `setCommands` applies too). */
function isCommand(command: unknown): command is AgentStreamCommand {
  if (typeof command !== "object" || command === null) return false;
  if (
    !("name" in command) ||
    typeof command.name !== "string" ||
    !isTypableCommandName(command.name)
  ) {
    return false;
  }
  if (
    !("source" in command) ||
    typeof command.source !== "string" ||
    !Object.hasOwn(COMMAND_SOURCES, command.source)
  ) {
    return false;
  }
  if (
    "description" in command &&
    command.description !== undefined &&
    (typeof command.description !== "string" ||
      command.description.length >
        AGENT_STREAM_LIMITS.commandDescriptionChars + AGENT_STREAM_TRUNCATION_SUFFIX.length)
  ) {
    return false;
  }
  return (
    command.name.length <= AGENT_STREAM_LIMITS.commandNameChars &&
    (!("terminalOnly" in command) ||
      command.terminalOnly === undefined ||
      command.terminalOnly === true)
  );
}

/** Whether every value is a string: the settings frame's lists, which a pick sends verbatim. */
function isStringList(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === "string");
}

/** Whether a `settings` frame has the contract's shape, every field of it. The header renders
 *  each field and a pick sends a listed model as typed input, so a frame with any field of the
 *  wrong type is dropped whole and the header keeps what the session reported before. */
function isSettings(frame: Record<string, unknown>): boolean {
  if (frame.reported === false) return true;
  if (frame.reported !== true) return false;
  if (frame.model !== undefined && typeof frame.model !== "string") return false;
  if (!isStringList(frame.models)) return false;
  const { thinking } = frame;
  if (thinking === null) return true;
  if (typeof thinking !== "object") return false;
  return (
    "level" in thinking &&
    (thinking.level === null || typeof thinking.level === "string") &&
    "levels" in thinking &&
    isStringList(thinking.levels)
  );
}

/**
 * Whether a frame is one this build can render. Frames arrive as JSON off a bus any client can
 * publish on, and from a publisher that ships separately from this dashboard, so a shape this
 * code does not expect is a live possibility rather than a theoretical one. assistant-ui throws
 * on a tool-call or reasoning part attached to a user message and on any unknown role, and that
 * throw takes the whole page to the route's error screen — on every load, because the session's
 * ring serves the same frame again. A frame that does not check out is dropped instead.
 */
export function isRenderableFrame(value: unknown): value is AgentStreamFrame {
  if (typeof value !== "object" || value === null) return false;
  // Read as the frame it claims to be; every field below is checked before anything trusts it.
  const frame = value as AgentStreamFrame;
  if (frame.v !== 1 || typeof frame.seq !== "number") return false;
  if (frame.kind === "tool-result") {
    const { result } = frame;
    return (
      typeof result?.toolCallId === "string" &&
      typeof result.toolName === "string" &&
      typeof result.output === "string" &&
      typeof result.at === "number"
    );
  }
  if (frame.kind === "commands") {
    // The whole list or nothing: a list missing one entry would tell the person a command they
    // could send does not exist.
    return (
      Array.isArray(frame.commands) &&
      frame.commands.length <= AGENT_STREAM_LIMITS.commands &&
      frame.commands.every(isCommand)
    );
  }
  if (frame.kind === "settings") {
    return isSettings(frame as unknown as Record<string, unknown>);
  }
  if (frame.kind !== "message") return false;
  const { message } = frame;
  if (message === undefined || message === null) return false;
  if (typeof message.id !== "string" || typeof message.at !== "number") return false;
  if (message.role !== "user" && message.role !== "assistant") return false;
  if (message.model !== undefined && typeof message.model !== "string") return false;
  if (!Array.isArray(message.parts)) return false;
  return message.parts.every((part) => {
    // A JSON null or primitive in the list is no part at all.
    if (typeof part !== "object" || part === null) return false;
    if (part.type === "text") return typeof part.text === "string";
    // Text is the only part a user message may carry. assistant-ui throws
    // "Unsupported user message part type: reasoning" on the others, exactly as it does for a
    // tool call, and the throw takes the thread down with it.
    if (part.type === "reasoning") {
      return message.role === "assistant" && typeof part.text === "string";
    }
    if (part.type !== "tool-call") return false;
    return (
      message.role === "assistant" &&
      typeof part.toolCallId === "string" &&
      typeof part.toolName === "string" &&
      typeof part.argsText === "string"
    );
  });
}

export function applyFrame(state: AgentConversation, frame: unknown): AgentConversation {
  if (!isRenderableFrame(frame)) return state;
  if (frame.kind === "tool-result") {
    const key = `t:${frame.result.toolCallId}`;
    if ((state.applied[key] ?? 0) >= frame.seq) return state;
    return {
      ...state,
      applied: { ...state.applied, [key]: frame.seq },
      results: { ...state.results, [frame.result.toolCallId]: frame.result },
    };
  }
  if (frame.kind === "commands") {
    // One list per session, last-writer-wins by `seq` like a message's snapshots.
    const key = "commands";
    if ((state.applied[key] ?? 0) >= frame.seq) return state;
    return {
      ...state,
      applied: { ...state.applied, [key]: frame.seq },
      commands: frame.commands,
    };
  }
  if (frame.kind === "settings") {
    // One state per session, last-writer-wins by `seq` as the command list is.
    const key = "settings";
    if ((state.applied[key] ?? 0) >= frame.seq) return state;
    return {
      ...state,
      applied: { ...state.applied, [key]: frame.seq },
      settings: frame,
    };
  }
  const key = `m:${frame.message.id}`;
  if ((state.applied[key] ?? 0) >= frame.seq) return state;
  // A settled message is final, whatever sequence number a later snapshot carries. The host
  // hands the extension a message's last few `message_update`s after its `message_end`, so the
  // late streaming snapshot is the one with the higher number; letting it win would leave a
  // finished turn rendered as still streaming.
  const settled = state.messages.find(
    (message) => message.id === frame.message.id && !message.streaming
  );
  if (settled !== undefined && frame.message.streaming) {
    return { ...state, applied: { ...state.applied, [key]: frame.seq } };
  }
  const others = state.messages.filter((message) => message.id !== frame.message.id);
  const messages = [...others, frame.message].sort((left, right) =>
    left.at === right.at ? left.id.localeCompare(right.id) : left.at - right.at
  );
  return {
    ...state,
    applied: { ...state.applied, [key]: frame.seq },
    messages,
  };
}

export function applyFrames(
  state: AgentConversation,
  frames: readonly unknown[]
): AgentConversation {
  return frames.reduce(applyFrame, state);
}

/** One content part of a message assistant-ui renders. */
type ThreadPart = Exclude<ThreadMessageLike["content"], string>[number];

/**
 * The conversation as assistant-ui renders it: a tool call carries its own result.
 *
 * A user message emits text and nothing else. assistant-ui throws on any other part type there
 * ("Unsupported user message part type: reasoning"), and that throw happens where the runtime is
 * built — above the thread, so no boundary around the thread can catch it and the composer goes
 * with the transcript. `isRenderableFrame` already drops such a frame; this is the second guard,
 * because a part type a future publisher adds would otherwise reach the library through a frame
 * this build did not know to refuse.
 */
export function toThreadMessages(state: AgentConversation): KeyedMessage[] {
  return state.messages.map((message) => ({
    content: message.parts.flatMap<ThreadPart>((part) => {
      if (part.type === "text") return [{ text: part.text, type: "text" as const }];
      if (message.role !== "assistant") return [];
      if (part.type === "reasoning") return [{ text: part.text, type: "reasoning" as const }];
      const result = state.results[part.toolCallId];
      return [
        {
          argsText: part.argsText,
          isError: result?.isError ?? false,
          result: result?.output,
          toolCallId: part.toolCallId,
          toolName: part.toolName,
          type: "tool-call" as const,
        },
      ];
    }),
    createdAt: new Date(message.at),
    id: message.id,
    role: message.role,
    // A status belongs to an assistant message alone: assistant-ui refuses one on a user
    // message, and a whole page would fail to render rather than one turn.
    ...(message.role === "assistant"
      ? {
          status: message.streaming
            ? ({ type: "running" } as const)
            : ({ reason: "stop", type: "complete" } as const),
        }
      : {}),
    // The model that produced this assistant turn, read back by AgentThread's renderer
    // (LEGION-548); absent when the publishing host reported none.
    ...(message.role === "assistant" && message.model !== undefined
      ? { metadata: dispatchMetadata({ model: message.model }) }
      : {}),
  }));
}

/**
 * The model the session is currently running, for the live view's header: the most recent
 * assistant turn's own report, whatever it is. A session whose client never reports one shows
 * nothing here rather than a guess. `state.messages` is kept sorted ascending by `at`
 * (`applyFrame`), so walking from the end finds the newest assistant entry first;
 * `Array.prototype.findLast` would read the same way but needs ES2023, past this package's ES2022
 * `lib`.
 */
export function currentModel(state: AgentConversation): string | undefined {
  for (let index = state.messages.length - 1; index >= 0; index -= 1) {
    const message = state.messages[index];
    if (message?.role === "assistant") return message.model;
  }
  return undefined;
}

/** The slash commands the composer completes: the session's newest list, or undefined for a
 *  session that sent none, which is one whose host cannot run typed input. */
export function sessionCommands(
  state: AgentConversation
): readonly AgentStreamCommand[] | undefined {
  return state.commands;
}

/** What the live view's header shows of the session's model and thinking level. */
export type SessionSettingsView =
  | {
      /** Nothing the session reported: the header shows the last turn's model, as it always
       *  did, and offers no picker. */
      readonly reported: false;
      readonly model: string | undefined;
      readonly notice: string | undefined;
    }
  | {
      readonly reported: true;
      readonly settings: AgentStreamSettingsReported;
      /** Whether the session runs the built-in `/model` from Dispatch, which is what a pick sends. */
      readonly modelPicker: boolean;
      /** Whether the session runs the built-in `/effort` from Dispatch. */
      readonly thinkingPicker: boolean;
      readonly notice: string | undefined;
    };

/**
 * What the header shows of the session's model and thinking level, and why (INFRA-195). It never
 * shows a value the session did not report: the settings are the newest `settings` frame alone,
 * and a session that sent none shows the last turn's model, as the header did before the frame.
 * A picker is offered only where the session lists the built-in a pick runs (`model`, `effort`)
 * and runs it from Dispatch, since a pick is a person's Send of `/model <id>` or `/effort <level>`.
 *
 * A session that is not answering the live view (an older plugin, one started without its plugin,
 * or one no longer running) gets no notice, since the empty transcript's own text already says
 * why; nor does one whose replay has not arrived yet, since the replay is where the frame would
 * be.
 */
export function sessionSettingsView(
  state: AgentConversation,
  responding: boolean | undefined,
  replays: number
): SessionSettingsView {
  const lastTurn = currentModel(state);
  if (responding === false) return { model: lastTurn, notice: undefined, reported: false };
  const { settings } = state;
  if (settings === undefined) {
    return {
      model: lastTurn,
      notice:
        replays === 0
          ? undefined
          : "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.",
      reported: false,
    };
  }
  if (!settings.reported) {
    return {
      model: lastTurn,
      notice: "This session's Oh My Pi does not tell Dispatch its model and thinking level.",
      reported: false,
    };
  }
  const runnable = new Set(
    (state.commands ?? [])
      .filter((command) => command.source === "builtin" && command.terminalOnly !== true)
      .map((command) => command.name)
  );
  const modelPicker = runnable.has("model");
  const thinkingPicker = runnable.has("effort");
  return {
    modelPicker,
    notice:
      modelPicker && thinkingPicker
        ? undefined
        : "This session switches its model and thinking level only at its own terminal.",
    reported: true,
    settings,
    thinkingPicker,
  };
}

/**
 * The part the stream writes for a picture a person's message carried. `conversationParts`
 * (`@legion/pi-shared/agent-stream`) writes `[${part.type}]` for a block it renders no other
 * way, and what a person's message reaches the session as is text blocks and image blocks
 * alone (`ContentBlock` in `@legion/pi-shared/pi-types`), so `[image]` is the only one of
 * these the publisher writes for such a message.
 *
 * A person writes a bracketed word too - `[ack]`, `[done]`, `[LGTM]` - and the bus is open to
 * any client, so no other bracketed word reads as the stream naming a block: a row that put
 * `[approved-by-me]` after the text of a body that embeds a picture would otherwise hide the
 * person's stored copy and show that word under their name. Even this literal reads as the
 * stream's only where it follows every line of the person's own body and a picture that body
 * embeds is left to account for it (`turnSaysBody`), which is where the publisher writes one,
 * one part per block and after the person's text.
 */
const PICTURE_PLACEHOLDER = "[image]";

/** One streamed user message a person's Dispatch message became: the Dispatch message it says it
 *  delivered, and what the session sent, its text parts joined in the order they arrived. */
export interface DispatchTurn {
  readonly dispatchMessageId: string;
  readonly text: string;
}

/**
 * The pictures a body embeds, each by the markdown that wrote it and how many times it wrote
 * it: what a streamed row may carry of a person's own pictures and no more (`rowLines`).
 */
function bodyPictures(body: string): Map<string, number> {
  const written = new Map<string, number>();
  for (const [markdown] of body.matchAll(pictureSyntax())) {
    written.set(markdown, (written.get(markdown) ?? 0) + 1);
  }
  return written;
}

/**
 * A streamed row's lines as they are compared with the body's, the pictures it carries spent
 * against `spare` (the body's own, by `bodyPictures`); undefined for a row carrying a picture
 * the body does not hold, or holding one more time than the body wrote it.
 *
 * This is the page's own claim-checking, not the shared rule (INFRA-1026). The bus is open to
 * any client, and the row is rendered as markdown under the person's name, so a row stripped of
 * picture markdown the way the body is would let a frame add `![any caption](dispatch://…@v1)`
 * to the words the person wrote, still "say" the body, hide the stored copy, and show a picture
 * and a caption the person never sent. Each picture the row carries must therefore be one the
 * body wrote, verbatim and address and caption alike, and may be spent only as many times as
 * the body wrote it. A row that carries the markdown as the person wrote it - which is how a
 * terminal session's row records it - spends exactly the body's own and says its body still.
 *
 * The body's pictures are spent once, and what is left is compared as text by the line step
 * `comparableLines` shares (`trimmedLines` in `@legion/contracts`), its strip of the pictures
 * left out. Taking a picture out joins the text on either side of it, and the join may write
 * picture markdown that neither the body nor the row held before it. A second strip would take
 * that one out too, which is how
 * `look at this ![I approve deleting prod](dispatch://agent/a1/artifact/x<the body's own
 * picture>@v1)` said a body of `look at this <that picture>` and drew its caption under the
 * person's name (INFRA-1026). Nothing honest is lost by comparing the leftover whole: for a row
 * that is the body verbatim, both sides have had each of the body's pictures taken out exactly
 * once, so both end with the same text.
 */
function rowLines(text: string, spare: Map<string, number>): string[] | undefined {
  let ownPictures = true;
  const written = text.replace(pictureSyntax(), (markdown) => {
    const left = spare.get(markdown) ?? 0;
    if (left === 0) {
      ownPictures = false;
      return markdown;
    }
    spare.set(markdown, left - 1);
    return "";
  });
  if (!ownPictures) return undefined;
  return trimmedLines(written);
}

/**
 * Whether a streamed turn says exactly what the person wrote, so Dispatch's stored copy of that
 * message gives way to the streamed row (INFRA-1026). The turn's lines are read against the
 * body's own lines in order: the body's side is the shared rule (`comparableLines` in
 * `@legion/contracts`, which the Claude Code plugin tags its rows by), the row's side the
 * body's own pictures spent against what it carries and what is left read by the shared rule's
 * own line step (`trimmedLines`), never stripped of pictures a second time (`rowLines`). Each
 * line must be the body line that comes next, and only once every one of them is matched may a
 * line be the picture placeholder instead, at most as many of those as the body embeds
 * pictures. The bus is open to any client, so a streamed row is a claim: the only placeholder
 * the publisher writes for a person's own message is the one an image block becomes, one per
 * picture and after the person's text (`PICTURE_PLACEHOLDER`), so a row that puts a bracketed
 * word of its own anywhere, or the placeholder before the person's lines or more often than
 * the body has pictures, says words the person never wrote and hides nothing. A message
 * carrying a picture, which reaches the session as the person's text and the picture beside
 * it, says its body still: the `[image]` is the stream's, the markdown the body held is no
 * line of the body's side, and the picture it stood as is what accounts for that line. A body
 * that is the bracketed word `[ack]` says itself, picture or no picture, because the body has
 * that line of its own.
 */
export function turnSaysBody(turn: DispatchTurn, body: string): boolean {
  const lines = comparableLines(body);
  const spare = bodyPictures(body);
  let pictures = 0;
  for (const count of spare.values()) pictures += count;
  const said = rowLines(turn.text, spare);
  if (said === undefined) return false;
  let next = 0;
  for (const line of said) {
    if (line === lines[next]) {
      next += 1;
      continue;
    }
    if (next < lines.length || pictures === 0 || line !== PICTURE_PLACEHOLDER) return false;
    pictures -= 1;
  }
  return next === lines.length;
}

/**
 * The Dispatch message each streamed user message delivered, by streamed message id: a person's
 * direct message the session took as its own user turn is tagged with its Dispatch id, and the
 * view stops showing Dispatch's stored copy of it once the turn says what the person wrote
 * (`turnSaysBody`). A turn the session left untagged is not in the map, so its stored copy keeps
 * showing beside it (`packages/pi-envoy/AGENTS.md` says which turns go untagged). The bus is open
 * to any client, so a tag counts only as the publisher's contract puts it, a string on a user
 * message; any other is ignored rather than taking the message with it, since a message shown
 * twice beats one lost. The text is read from the frame's own parts, which on a user message are
 * text alone (`isRenderableFrame`), every one of them: which of its lines the person wrote and
 * which the stream named is for `turnSaysBody` to tell, against the stored body.
 */
export function dispatchTurns(state: AgentConversation): Map<string, DispatchTurn> {
  const turns = new Map<string, DispatchTurn>();
  for (const message of state.messages) {
    if (message.role === "user" && typeof message.dispatchMessageId === "string") {
      turns.set(message.id, {
        dispatchMessageId: message.dispatchMessageId,
        text: message.parts.flatMap((part) => (part.type === "text" ? [part.text] : [])).join("\n"),
      });
    }
  }
  return turns;
}
