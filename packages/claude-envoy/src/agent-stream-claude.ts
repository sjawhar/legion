/**
 * Claude Code's session, as the Agents page's live view reads it (INFRA-1026).
 *
 * The mod (`../hooks/mod.ts`) sends what happens in the session over the link's socket as
 * `ModEvent`s; this turns each one into a call on the `AgentStreamPublisher` every Legion host
 * shares (`@legion/pi-shared/agent-stream`), and builds the two frames that describe the session
 * rather than its conversation: its model and thinking level, and its slash commands.
 *
 * It owns one piece of state the publisher cannot own for it. The publisher derives a message's
 * ring slot from the message itself — an assistant message's slot is its timestamp — so every
 * snapshot of one streaming step has to carry the one timestamp that step was first seen at, and
 * two steps must never carry the same one. The allocation below does both: a step's timestamp is
 * kept under `(turnId, index)` for as long as the session lasts, and each new one is at least one
 * millisecond past the last handed out, so two steps that start inside one millisecond still land
 * in two slots.
 *
 * Nothing here opens a socket or knows about NATS: a subject is a string its caller passes in.
 */
import { type AgentStreamCommand, isTypableCommandName } from "@legion/contracts"
import {
  type AgentStreamPublisher,
  type AgentStreamSettingsState,
  modelReference,
} from "@legion/pi-shared/agent-stream"
import type {
  ModAppendMessage,
  ModCommandInfo,
  ModConfigRow,
  ModContentBlock,
  ModEffort,
  ModEvent,
} from "../hooks/mod-protocol"
import { claudeEchoedCommandLine, claudeRowText, isCommandBookkeeping } from "./claude-rows"

/** The provider every model of Claude Code's is named under, so a frame carries the same
 *  `provider/id` reference an Oh My Pi session's does (`anthropic/claude-sonnet-5`). */
export const CLAUDE_PROVIDER = "anthropic"

/**
 * What a reasoning part says when the model's thinking arrived with no text.
 *
 * Claude Code sends the thinking block's signature and no words unless the session asked for
 * summarized thinking, so publishing the block as it stands would show the viewer an empty
 * reasoning box. Measured on 2.1.310: every `thinking` block of a plain session carries
 * `"thinking": ""`, and a session started with `--thinking-display summarized` carries the
 * summary. A terminal session turns the summaries on with the `showThinkingSummaries` setting; a
 * Legion pane gets them from the launch flag the daemon passes, which no reader of this view can
 * change, so the setting is what the sentence names.
 *
 * One sentence, published once per step however many empty blocks the step holds: a browser test
 * and the Dispatch documentation both quote it.
 */
export const CLAUDE_THINKING_HIDDEN =
  "Claude Code hides what the model thought here: it sends the thinking text only when the terminal session turns on the showThinkingSummaries setting."

/**
 * The effort levels `/effort` offers, in the order Claude Code lists them (`ModEffort`). Claude
 * Code reports a request's effort only on its `turn.step`, so this list is what a viewer can pick
 * from and the step's own `effort` is which one is in force.
 */
export const CLAUDE_EFFORT_LEVELS = [
  "low",
  "medium",
  "high",
  "xhigh",
  "max",
] as const satisfies readonly ModEffort[]

/** One content part as the publisher reads one (`conversationParts` in
 *  `@legion/pi-shared/agent-stream`): a `text`, a `thinking`, a `toolCall`, or any other `type`,
 *  which the publisher renders as the literal `[<type>]`. */
interface ClaudeStreamPart {
  readonly type: string
  readonly text?: string
  readonly thinking?: string
  readonly id?: string
  readonly name?: string
  readonly arguments?: unknown
}

/** One message as the publisher reads one (`readMessage`): `role` and `timestamp` are what it
 *  needs to keep the message at all, and the rest is what each role carries. */
interface ClaudeStreamMessage {
  readonly role: "assistant" | "user" | "toolResult"
  readonly timestamp: number
  readonly content: string | readonly ClaudeStreamPart[]
  readonly provider?: string
  readonly model?: string
  readonly tag?: string
  readonly toolCallId?: string
  readonly toolName?: string
  readonly isError?: boolean
}

/** The effort the last `turn.step` of the session ran under, and the model it ran on. Claude Code
 *  names no effort for a model that has none, so the absent `effort` is the fact that there is no
 *  thinking level to show. */
interface ObservedEffort {
  readonly model: string
  readonly effort: ModEffort | undefined
}

export interface ClaudeAgentStreamDeps {
  readonly publisher: AgentStreamPublisher
  /** The clock a step's timestamp is allocated from, and every other message is stamped with. */
  readonly now: () => number
  /**
   * Whether this session is a Legion pane rather than a terminal one. A pane is seeded with one
   * command before anything else happens in it (`PANE_SEEDED_COMMAND`), which is the daemon
   * naming the pane rather than a person writing to the agent, so the view draws neither the
   * prompt that carried it nor the echo Claude Code wrote of it.
   */
  readonly isPane: boolean
  /**
   * The Dispatch message a row of the conversation is, where it is one
   * (`./user-turn-state`). A person's own message would otherwise reach the page twice - as the
   * message Dispatch stored and as the row the session took it as - and the row's tag is how the
   * page knows the two are one message (`conversation.ts` in the Agents page). A row of the
   * session's own keeps its own id as its tag, as every row did before.
   */
  readonly tagFor?: (row: string) => string | undefined
}

export interface ClaudeAgentStream {
  /** Record what `event` says on `subject`, which is where the publisher sends its frames. An
   *  event that is no message of the conversation records nothing. */
  readonly record: (subject: string, event: ModEvent) => void
  /**
   * The settings frame's state for the session's `model` (`$.session.model()`'s answer) and the
   * `/config` rows `$.config.list()` answered with. Unreported until the rows have been read:
   * they are the only list of the models a pick may name.
   */
  readonly settings: (
    model: string | undefined,
    rows: readonly ModConfigRow[] | undefined,
  ) => AgentStreamSettingsState
  /**
   * The effort a person's pick put in force. Claude Code reports an effort only on a request's
   * own `turn.step`, so until the session's next turn its report is the level before the pick;
   * the pick itself is what set the effort the mod puts on every later request that names an
   * effort of its own (`../hooks/mod`, `turn.step`), so from the moment it lands the settings
   * frame says so and keeps saying so until another pick changes it or a step reports that the
   * request ran at another level, or at none - which is what a model with no effort to set does.
   */
  readonly pickedEffort: (effort: ModEffort) => void
  /** Forget the conversation's timestamps and the effort read off it: the session id moved, and
   *  the publisher was reset with it. */
  readonly reset: () => void
}

/** One block of a row as a part the publisher reads. A kind it does not know crosses as its type
 *  alone, which the viewer shows as `[image]` and the like. */
function partOf(block: ModContentBlock): ClaudeStreamPart {
  const text = block["text"]
  if (block.type === "text" && typeof text === "string") return { type: "text", text }
  const thinking = block["thinking"]
  if (block.type === "thinking" && typeof thinking === "string") {
    return { type: "thinking", thinking }
  }
  const id = block["id"]
  if (block.type === "tool_use" && typeof id === "string") {
    const name = block["name"]
    return {
      arguments: block["input"],
      id,
      name: typeof name === "string" ? name : "tool",
      type: "toolCall",
    }
  }
  return { type: block.type }
}

/** Whether a part puts anything in front of a reader: text or thinking with characters in it, a
 *  tool call, or a kind the publisher names in brackets (`[image]`). An empty text part is the
 *  one that shows nothing, since `conversationParts` keeps it as the empty string it is. */
function shows(part: ClaudeStreamPart): boolean {
  if (part.type === "text") return part.text !== ""
  if (part.type === "thinking") return part.thinking !== ""
  return true
}

/** Under which role a row of the conversation is shown. Claude Code names the role on the rows
 *  that have one and leaves it off the rest, whose `type` says it instead; anything else is the
 *  session talking to the model, which the view shows on the person's side. */
function roleOf(message: ModAppendMessage): "assistant" | "user" {
  if (message.role !== undefined) return message.role
  return message.type === "assistant" ? "assistant" : "user"
}

/** The command the daemon's bridge seeds a fresh Legion pane with before anything else
 *  (`packages/daemon/internal/claudebridge/bridge.go:210-213`), which names the pane. */
const PANE_SEEDED_COMMAND = "/rename"

/** Whether a line is the command a pane is seeded with, with or without the title it names. */
function isPaneSeededRename(line: string): boolean {
  return line === PANE_SEEDED_COMMAND || line.startsWith(`${PANE_SEEDED_COMMAND} `)
}

/**
 * A streamed tool call's arguments as the publisher should render them. They arrive as JSON text
 * a piece at a time, so a snapshot taken mid-block does not parse: the parsed value is handed
 * over once there is one, and until then the text as it arrived, which is what the viewer shows
 * while the call is still being written.
 */
function callArguments(args: string): unknown {
  if (args === "") return undefined
  try {
    return JSON.parse(args)
  } catch {
    return args
  }
}

/**
 * The models a person's pick may send back, from the `/config` rows.
 *
 * The `model` row's `options` are carried and its `value` is not. Measured on 2.1.310: the value
 * is the model's own name while the session names one (`claude-sonnet-4-5`), and the row's label
 * for the default while it does not (`Default (recommended)`), which is no name any pick can
 * send. The options are the row's own choices, so every one of them is a value `/config` takes —
 * a shorter list of picks that all run beats a longer one holding a pick that cannot.
 */
function listedModels(rows: readonly ModConfigRow[]): string[] {
  const row = rows.find((candidate) => candidate.key === "model")
  const options = row?.options ?? []
  // A nameless option is no reference a pick could send back, and `modelReference` is the rule
  // for what a `provider/id` is.
  return options.flatMap((option) => modelReference(CLAUDE_PROVIDER, option) ?? [])
}

/**
 * Claude Code's slash commands as the viewer offers them: the name without its slash, and the
 * source mapped onto the four the frame knows. A name a person could not type is dropped
 * (`isTypableCommandName`), and nothing is marked `terminalOnly`: Claude Code says of no command
 * whether running it opens a dialog, so marking any would be a guess.
 */
export function claudeAgentStreamCommands(
  commands: readonly ModCommandInfo[],
): AgentStreamCommand[] {
  const listed: AgentStreamCommand[] = []
  for (const command of commands) {
    const name = command.name.startsWith("/") ? command.name.slice(1) : command.name
    if (!isTypableCommandName(name)) continue
    listed.push({
      name,
      source:
        command.source === "builtin"
          ? "builtin"
          : command.source === "plugin"
            ? "extension"
            : "prompt",
      ...(command.description === "" ? {} : { description: command.description }),
    })
  }
  return listed
}

export function createClaudeAgentStream(deps: ClaudeAgentStreamDeps): ClaudeAgentStream {
  const { isPane, publisher, now } = deps
  /** The timestamp each `(turnId, index)` step was first seen at, which is its ring slot. */
  const stepAt = new Map<string, number>()
  /** The last timestamp handed to a step, so the next one is strictly past it. */
  let allocated = 0
  let observed: ObservedEffort | undefined
  /** The effort the last pick put in force, which is the effort the mod puts on every later
   *  request that names an effort of its own, until a step disagrees or another pick changes it. */
  let picked: ModEffort | undefined

  /** The timestamp this step keeps for every snapshot of it. */
  const timestampFor = (turnId: string, index: number): number => {
    const key = `${turnId}\u0000${index}`
    const kept = stepAt.get(key)
    if (kept !== undefined) return kept
    const at = Math.max(now(), allocated + 1)
    allocated = at
    stepAt.set(key, at)
    return at
  }

  const record = (
    subject: string,
    message: ClaudeStreamMessage,
    streaming: boolean,
    /** The Dispatch message this row delivered, where it is one. */
    dispatchMessageId?: string,
  ): void => {
    publisher.record(subject, message, streaming, dispatchMessageId)
  }

  /** One `turn.step` snapshot as the assistant message of that step. */
  const recordStep = (subject: string, event: Extract<ModEvent, { kind: "step" }>): void => {
    // The step reports the effort its own request carried, which is the truth about that
    // request: a pick stands only while the steps agree with it. A step whose own effort is
    // another level, or none at all, ends the pick and leaves the step itself to answer -
    // measured live on 2.1.310, a pick of `low` with `claude-haiku-4-5` in force left the header
    // reading `low` while the session's next request carried no effort at all (INFRA-1026).
    if (event.effort !== picked) picked = undefined
    observed = { model: event.model, effort: event.effort }
    const content: ClaudeStreamPart[] = []
    let hiddenThinkingShown = false
    for (const part of event.parts) {
      if (part.type === "text") {
        content.push({ text: part.text, type: "text" })
      } else if (part.type === "reasoning") {
        if (part.text !== "") {
          content.push({ thinking: part.text, type: "thinking" })
        } else if (!hiddenThinkingShown) {
          hiddenThinkingShown = true
          content.push({ thinking: CLAUDE_THINKING_HIDDEN, type: "thinking" })
        }
      } else {
        content.push({
          arguments: callArguments(part.args),
          id: part.id,
          name: part.name,
          type: "toolCall",
        })
      }
    }
    record(
      subject,
      {
        content,
        model: event.model,
        provider: CLAUDE_PROVIDER,
        role: "assistant",
        timestamp: timestampFor(event.turnId, event.index),
      },
      !event.done,
    )
  }

  /** One row of the conversation, by the door it came in. */
  const recordAppend = (subject: string, event: Extract<ModEvent, { kind: "append" }>): void => {
    const { door, message, origin, uuid } = event
    // The step that produced the answer already carried it, and the publisher would key this row
    // into a slot of its own: publishing both shows the viewer the same answer twice.
    if (door === "response") return
    if (door === "prompt") {
      // A prompt this plugin submitted without `asUser` is not a person's message: Claude Code
      // frames it as "The claude-envoy plugin sent a message:" and the only one the server sends
      // is the nudge at a row a turn never read (`./mod-session`, `unreadRowPrompt`).
      // Measured on 2.1.310: a submit with `asUser` carries `asUser: true` on the row's origin
      // and one without it carries no such field, which is what tells the two apart.
      if (origin.kind === "plugin" && origin.asUser !== true) return
      // The command a pane is seeded with arrives by this door, written to the session's input
      // by the daemon's bridge before anything else happens in the pane: it names the pane, and
      // no person sent it.
      if (isPane && isPaneSeededRename(claudeRowText(message.content))) return
      // Each block crosses as the part it is, as it does by every other door: the person's text
      // stays a part of its own and a picture becomes the `[image]` part the publisher names it
      // (`conversationParts` in `@legion/pi-shared/agent-stream`). Flattened into one
      // "Look at this\n[image]" part the row says what no stored message says, and the Agents
      // page shows the person's message twice (INFRA-1026).
      const content = message.content.map(partOf)
      if (!content.some(shows)) return
      record(
        subject,
        {
          content,
          role: "user",
          tag: uuid,
          timestamp: now(),
        },
        false,
        deps.tagFor?.(uuid),
      )
      return
    }
    if (door === "tool-result") {
      for (const block of message.content) {
        const toolCallId = block["tool_use_id"]
        // A tool result's slot is its call's id; a block naming none joins no call, and the
        // publisher would drop it.
        if (typeof toolCallId !== "string") continue
        record(
          subject,
          {
            content: claudeRowText(block["content"]),
            isError: block["is_error"] === true,
            role: "toolResult",
            timestamp: now(),
            toolCallId,
            ...(origin.tool === undefined ? {} : { toolName: origin.tool }),
          },
          false,
        )
      }
      return
    }
    if (door === "command") {
      // A command a person ran writes three rows by this door and only one of them is a message
      // (`./claude-rows`, `COMMAND_BOOKKEEPING_TAGS`): the echo of what was typed, which the
      // view draws as the plain command line the person would have typed - `/clear`,
      // `/effort low` - rather than as the tags Claude Code wrote it in. A row that is neither
      // the echo nor the bookkeeping goes the way every other row does, below.
      const line = claudeEchoedCommandLine(message.content)
      if (line !== undefined) {
        if (isPane && isPaneSeededRename(line)) return
        record(
          subject,
          {
            content: [{ text: line, type: "text" }],
            role: "user",
            tag: uuid,
            timestamp: now(),
          },
          false,
          deps.tagFor?.(uuid),
        )
        return
      }
      if (isCommandBookkeeping(message.content)) return
    }
    const content = message.content.map(partOf)
    // A row that shows a reader nothing is the session's bookkeeping, not its conversation, so
    // nothing is recorded for it: the publisher would key it into a slot of its own and the view
    // would draw an empty bubble. Claude Code's engine writes one such row per turn, by the
    // `notice` door (`type: "system"`, `name: "turn_duration"`, no content blocks, measured on
    // 2.1.310), which is one empty bubble per turn of the conversation. A tool result with no
    // output is a result rather than a message and left alone: it went out above, by its call's
    // id.
    if (!content.some(shows)) return
    record(
      subject,
      {
        content,
        role: roleOf(message),
        tag: uuid,
        timestamp: now(),
      },
      false,
      deps.tagFor?.(uuid),
    )
  }

  return {
    record(subject: string, event: ModEvent): void {
      if (event.kind === "step") recordStep(subject, event)
      else if (event.kind === "append") recordAppend(subject, event)
      // Every other event says something about the session rather than its conversation: its id,
      // its model, a turn's boundaries, a request going out. The server reads those; the view
      // shows no message for them.
    },
    pickedEffort(effort: ModEffort): void {
      picked = effort
    },
    settings(
      model: string | undefined,
      rows: readonly ModConfigRow[] | undefined,
    ): AgentStreamSettingsState {
      if (rows === undefined) return { reported: false }
      const reference = modelReference(CLAUDE_PROVIDER, model)
      return {
        models: listedModels(rows),
        reported: true,
        // Claude Code reports the effort on each request's `turn.step` and nowhere else, so a
        // pick is the only other thing that knows one: it set the effort the mod puts on every
        // later request that names an effort of its own, so from the moment it lands it is the
        // level in force, and it survives a model pick whose later step reports that same
        // effort. A step that reports another level, or none at all - a model Claude Code names
        // no effort for takes none whatever the pick set - ends the pick and answers for itself
        // (`recordStep`). Before this session's first request and before any pick there is a
        // selector and no level to show; a step that named no effort is a model with no thinking
        // to adjust. A step of another model is no answer about this one.
        thinking:
          picked !== undefined
            ? { level: String(picked), levels: CLAUDE_EFFORT_LEVELS }
            : observed !== undefined && observed.model === model
              ? observed.effort === undefined
                ? null
                : { level: String(observed.effort), levels: CLAUDE_EFFORT_LEVELS }
              : { level: null, levels: CLAUDE_EFFORT_LEVELS },
        // Absent when the session has no model selected, which is `modelReference`'s own answer
        // for a name that is empty (`AgentStreamSettingsReported.model`).
        ...(reference === undefined ? {} : { model: reference }),
      }
    },
    reset(): void {
      stepAt.clear()
      observed = undefined
      picked = undefined
    },
  }
}
