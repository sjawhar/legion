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
import type { AgentStreamPublisher, AgentStreamSettingsState } from "@legion/pi-shared/agent-stream"
import type {
  ModAppendMessage,
  ModCommandInfo,
  ModConfigRow,
  ModContentBlock,
  ModEffort,
  ModEvent,
} from "../hooks/mod-protocol"

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
  /** Forget the conversation's timestamps and the effort read off it: the session id moved, and
   *  the publisher was reset with it. */
  readonly reset: () => void
}

/** The text a row's content holds, blocks flattened: a tool result is shown as text alone, and a
 *  person's prompt reaches the view as one part. A block with no text of its own is named rather
 *  than dropped, as the publisher names one. */
function flatten(value: unknown): string {
  if (typeof value === "string") return value
  if (!Array.isArray(value)) return ""
  const pieces: string[] = []
  for (const item of value as readonly unknown[]) {
    if (typeof item === "string") {
      pieces.push(item)
      continue
    }
    if (typeof item !== "object" || item === null) continue
    const block = item as Record<string, unknown>
    const text = block["text"]
    const type = block["type"]
    if (typeof text === "string") pieces.push(text)
    else if (typeof type === "string") pieces.push(`[${type}]`)
  }
  return pieces.join("\n")
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

/** Under which role a row of the conversation is shown. Claude Code names the role on the rows
 *  that have one and leaves it off the rest, whose `type` says it instead; anything else is the
 *  session talking to the model, which the view shows on the person's side. */
function roleOf(message: ModAppendMessage): "assistant" | "user" {
  if (message.role !== undefined) return message.role
  return message.type === "assistant" ? "assistant" : "user"
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
  return options.map((option) => `${CLAUDE_PROVIDER}/${option}`)
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
  const { publisher, now } = deps
  /** The timestamp each `(turnId, index)` step was first seen at, which is its ring slot. */
  const stepAt = new Map<string, number>()
  /** The last timestamp handed to a step, so the next one is strictly past it. */
  let allocated = 0
  let observed: ObservedEffort | undefined

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

  const record = (subject: string, message: ClaudeStreamMessage, streaming: boolean): void => {
    publisher.record(subject, message, streaming)
  }

  /** One `turn.step` snapshot as the assistant message of that step. */
  const recordStep = (subject: string, event: Extract<ModEvent, { kind: "step" }>): void => {
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
      record(
        subject,
        {
          content: [{ text: flatten(message.content), type: "text" }],
          role: "user",
          tag: uuid,
          timestamp: now(),
        },
        false,
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
            content: flatten(block["content"]),
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
    record(
      subject,
      {
        content: message.content.map(partOf),
        role: roleOf(message),
        tag: uuid,
        timestamp: now(),
      },
      false,
    )
  }

  return {
    record(subject: string, event: ModEvent): void {
      if (event.kind === "step") recordStep(subject, event)
      else if (event.kind === "append") recordAppend(subject, event)
      // Every other event says something about the session rather than its conversation: its id,
      // its model, a turn's boundaries. The server reads those; the view shows no message for
      // them.
    },
    settings(
      model: string | undefined,
      rows: readonly ModConfigRow[] | undefined,
    ): AgentStreamSettingsState {
      if (rows === undefined) return { reported: false }
      return {
        models: listedModels(rows),
        reported: true,
        // Claude Code reports the effort on each request's `turn.step` and nowhere else, so
        // before this session's first request there is a selector and no level to show, and a
        // step that named none is a model with no thinking to adjust. A step of another model is
        // no answer about this one.
        thinking:
          observed !== undefined && observed.model === model
            ? observed.effort === undefined
              ? null
              : { level: String(observed.effort), levels: CLAUDE_EFFORT_LEVELS }
            : { level: null, levels: CLAUDE_EFFORT_LEVELS },
        ...(model === undefined ? {} : { model: `${CLAUDE_PROVIDER}/${model}` }),
      }
    },
    reset(): void {
      stepAt.clear()
      observed = undefined
    },
  }
}
