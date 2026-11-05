/**
 * What a viewer of the Agents page sees of a Claude Code session: the mapper from the mod's
 * events onto the shared publisher (`../src/agent-stream-claude`), driven here by the events a
 * real Claude Code 2.1.310 session sent (`./fixtures`, recorded by the link's own harness) and by
 * the few shapes a recording cannot reach.
 *
 * Every assertion is over the replay a viewer would be answered with, never over which function
 * called which.
 */

import { expect, test } from "bun:test"
import { join } from "node:path"
import {
  AGENT_STREAM_LIMITS,
  type AgentStreamFrame,
  type AgentStreamMessage,
  type AgentStreamReplay,
  type AgentStreamToolResult,
  agentStreamFramesSubject,
} from "@legion/contracts"
import { AgentStreamPublisher } from "@legion/pi-shared/agent-stream"
import type { ModCommandInfo, ModConfigRow, ModContentBlock, ModEvent } from "../hooks/mod-protocol"
import {
  CLAUDE_EFFORT_LEVELS,
  CLAUDE_THINKING_HIDDEN,
  type ClaudeAgentStream,
  claudeAgentStreamCommands,
  createClaudeAgentStream,
} from "../src/agent-stream-claude"

const SESSION = "0c3b6f6e-0000-4000-8000-000000000001"
const SUBJECT = agentStreamFramesSubject(SESSION)

/** One recorded session: every `ModEvent` its mod sent, and what its `$` calls answered. */
interface Fixture {
  readonly sessionId: string
  readonly answers: {
    readonly "session.model": string
    readonly "config.list": readonly ModConfigRow[]
    readonly "command.list": readonly ModCommandInfo[]
  }
  readonly events: readonly ModEvent[]
}

async function fixture(name: string): Promise<Fixture> {
  const body: unknown = await Bun.file(join(import.meta.dir, "fixtures", `${name}.json`)).json()
  return body as Fixture
}

interface Harness {
  readonly stream: ClaudeAgentStream
  readonly publisher: AgentStreamPublisher
  /** Every frame the publisher put on the wire, in order. */
  readonly published: AgentStreamFrame[]
  /** Move the clock on, so the next message is stamped later than the last. */
  readonly tick: (ms: number) => void
  readonly feed: (...events: readonly ModEvent[]) => void
  readonly replay: () => AgentStreamReplay
}

/** A publisher a viewer is already watching, with a clock the test moves itself: a real one would
 *  make a step's allocated timestamp unpredictable, and the slot is the timestamp. */
function harness(startAt = 1_000, tagFor?: (row: string) => string | undefined): Harness {
  let clock = startAt
  const now = (): number => clock
  const published: AgentStreamFrame[] = []
  const publisher = new AgentStreamPublisher({
    now,
    publish: (_subject, payload) => published.push(JSON.parse(payload) as AgentStreamFrame),
  })
  const stream = createClaudeAgentStream({
    now,
    publisher,
    ...(tagFor === undefined ? {} : { tagFor }),
  })
  publisher.noteViewer()
  return {
    feed: (...events) => {
      for (const event of events) stream.record(SUBJECT, event)
    },
    published,
    publisher,
    replay: () => publisher.replay(SESSION),
    stream,
    tick: (ms) => {
      clock += ms
    },
  }
}

function messages(replay: AgentStreamReplay): AgentStreamMessage[] {
  return replay.frames.flatMap((frame) => (frame.kind === "message" ? [frame.message] : []))
}

function toolResults(replay: AgentStreamReplay): AgentStreamToolResult[] {
  return replay.frames.flatMap((frame) => (frame.kind === "tool-result" ? [frame.result] : []))
}

function textOf(message: AgentStreamMessage): string {
  return message.parts.flatMap((part) => (part.type === "text" ? [part.text] : [])).join("")
}

function reasoningOf(message: AgentStreamMessage): string[] {
  return message.parts.flatMap((part) => (part.type === "reasoning" ? [part.text] : []))
}

/** One `turn.step` snapshot, with the fields a test does not care about filled in. */
function step(
  overrides: Partial<Extract<ModEvent, { kind: "step" }>> & {
    readonly parts: Extract<ModEvent, { kind: "step" }>["parts"]
  },
): ModEvent {
  return {
    done: false,
    index: 0,
    kind: "step",
    model: "claude-sonnet-5",
    turnId: "turn-1",
    ...overrides,
  }
}

function appendRow(
  door: Extract<ModEvent, { kind: "append" }>["door"],
  content: readonly ModContentBlock[],
  overrides: Partial<Extract<ModEvent, { kind: "append" }>> = {},
): ModEvent {
  return {
    door,
    kind: "append",
    message: { content, role: "user", type: "user" },
    origin: { kind: "unclassified" },
    uuid: "row-1",
    ...overrides,
  }
}

test("a streaming step's snapshots stay one message, and its text grows", () => {
  const live = harness()
  live.feed(step({ parts: [{ text: "He", type: "text" }] }))
  live.tick(200)
  live.feed(step({ parts: [{ text: "Hello th", type: "text" }] }))
  live.tick(200)
  live.feed(step({ done: true, parts: [{ text: "Hello there.", type: "text" }] }))

  const kept = messages(live.replay())
  expect(kept).toHaveLength(1)
  expect(textOf(kept[0] as AgentStreamMessage)).toBe("Hello there.")
  expect(kept[0]?.streaming).toBe(false)
  expect(kept[0]?.model).toBe("anthropic/claude-sonnet-5")
  // Three frames on the wire, all of them the same message: the viewer replaces it in place.
  const ids = new Set(
    live.published.flatMap((frame) => (frame.kind === "message" ? [frame.message.id] : [])),
  )
  expect(ids.size).toBe(1)
  expect(live.published.filter((frame) => frame.kind === "message")).toHaveLength(3)
})

test("two steps of one turn are two messages, and two steps in one millisecond do not collide", () => {
  const live = harness()
  // The clock never moves: both steps are first seen in the same millisecond.
  live.feed(
    step({ done: true, index: 0, parts: [{ text: "first", type: "text" }] }),
    step({ done: true, index: 1, parts: [{ text: "second", type: "text" }] }),
  )

  const kept = messages(live.replay())
  expect(kept.map(textOf)).toEqual(["first", "second"])
  expect(new Set(kept.map((message) => message.id)).size).toBe(2)
  expect(kept.map((message) => message.at)).toEqual([1_000, 1_001])
})

test("a step keeps its own timestamp when another step is allocated between its snapshots", () => {
  const live = harness()
  live.feed(step({ index: 0, parts: [{ text: "a", type: "text" }] }))
  live.tick(500)
  live.feed(step({ index: 1, parts: [{ text: "b", type: "text" }] }))
  live.tick(500)
  live.feed(step({ done: true, index: 0, parts: [{ text: "a and more", type: "text" }] }))

  const kept = messages(live.replay())
  expect(kept).toHaveLength(2)
  expect(kept.map(textOf)).toEqual(["a and more", "b"])
})

test("a response row publishes nothing, so the step's answer is the only copy", () => {
  const live = harness()
  live.feed(step({ done: true, parts: [{ text: "PAPAYA", type: "text" }] }))
  live.tick(10)
  live.feed(
    appendRow("response", [{ text: "PAPAYA", type: "text" }], {
      message: {
        content: [{ text: "PAPAYA", type: "text" }],
        role: "assistant",
        type: "assistant",
      },
      origin: { kind: "model", model: "claude-sonnet-5" },
      uuid: "row-response",
    }),
  )

  const kept = messages(live.replay())
  expect(kept).toHaveLength(1)
  expect(textOf(kept[0] as AgentStreamMessage)).toBe("PAPAYA")
  expect(kept[0]?.role).toBe("assistant")
})

test("two prompts stamped in one millisecond keep separate slots", () => {
  const live = harness()
  live.feed(
    appendRow("prompt", [{ text: "first ask", type: "text" }], { uuid: "row-a" }),
    appendRow("prompt", [{ text: "second ask", type: "text" }], { uuid: "row-b" }),
  )

  const kept = messages(live.replay())
  expect(kept.map(textOf)).toEqual(["first ask", "second ask"])
  expect(kept.every((message) => message.role === "user")).toBe(true)
})

test("a person's message shows as their own, and the prompt pointing at a row it left shows nothing", () => {
  const live = harness(1_000, (row) => (row === "row-sent" ? "msg-7" : undefined))
  live.feed(
    // Measured on 2.1.310: a submit with `asUser` carries it on the row's origin, and the
    // plugin's own prompt is framed as the plugin speaking.
    appendRow("prompt", [{ text: "ship it", type: "text" }], {
      origin: { asUser: true, kind: "plugin", name: "claude-envoy" },
      uuid: "row-sent",
    }),
    appendRow(
      "prompt",
      [{ text: "The claude-envoy plugin sent a message:\nRead the message above.", type: "text" }],
      { origin: { kind: "plugin", name: "claude-envoy" }, uuid: "row-nudge" },
    ),
  )

  const kept = messages(live.replay())
  expect(kept.map(textOf)).toEqual(["ship it"])
  // Tagged with the Dispatch message it is, so the page shows it once rather than beside the
  // copy Dispatch stored.
  expect(kept[0]?.dispatchMessageId).toBe("msg-7")
})

test("a command row keeps the row's own role and a note row an assistant one", () => {
  const live = harness()
  live.feed(
    appendRow("command", [{ text: "/compact", type: "text" }], { uuid: "row-command" }),
    appendRow("note", [{ text: "the session compacted", type: "text" }], {
      message: {
        content: [{ text: "the session compacted", type: "text" }],
        role: "assistant",
        type: "assistant",
      },
      uuid: "row-note",
    }),
  )

  expect(messages(live.replay()).map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/compact"],
    ["assistant", "the session compacted"],
  ])
})

test("an unknown content part renders as its type in brackets", () => {
  const live = harness()
  live.feed(
    appendRow("notice", [
      { text: "see this", type: "text" },
      { source: { data: "…", type: "base64" }, type: "image" },
    ]),
  )

  const kept = messages(live.replay())
  expect(kept[0]?.parts).toEqual([
    { text: "see this", type: "text" },
    { text: "[image]", type: "text" },
  ])
})

test("a thinking block with no text publishes one fixed sentence, once per step", () => {
  const live = harness()
  live.feed(
    step({
      done: true,
      parts: [
        { text: "", type: "reasoning" },
        { text: "", type: "reasoning" },
        { text: "done", type: "text" },
      ],
    }),
  )

  const kept = messages(live.replay())
  expect(reasoningOf(kept[0] as AgentStreamMessage)).toEqual([CLAUDE_THINKING_HIDDEN])
  expect(CLAUDE_THINKING_HIDDEN).toContain("showThinkingSummaries")
  // One sentence: one full stop, at the end.
  expect(CLAUDE_THINKING_HIDDEN.split(". ")).toHaveLength(1)
  expect(CLAUDE_THINKING_HIDDEN.endsWith(".")).toBe(true)
})

test("a thinking block with text publishes the text", () => {
  const live = harness()
  live.feed(step({ done: true, parts: [{ text: "I will check 17 × 23.", type: "reasoning" }] }))

  expect(reasoningOf(messages(live.replay())[0] as AgentStreamMessage)).toEqual([
    "I will check 17 × 23.",
  ])
})

test("a real 2.1.310 turn replays as the prompt, three steps and two tool results", async () => {
  const recorded = await fixture("claude-2.1.310-turn")
  const live = harness()
  for (const event of recorded.events) {
    live.feed(event)
    live.tick(1)
  }
  const replay = live.replay()
  const kept = messages(replay)

  // The prompt, then one assistant message per step of the turn. The three `response` rows the
  // session also appended added nothing.
  expect(kept.map((message) => message.role)).toEqual([
    "user",
    "assistant",
    "assistant",
    "assistant",
  ])
  expect(textOf(kept[0] as AgentStreamMessage)).toContain("is 391 prime?")
  expect(textOf(kept[3] as AgentStreamMessage)).toBe(
    "No, 391 is not prime, because it equals 17 × 23.",
  )
  // Claude Code sent both thinking blocks without text, so each step that thought says why once.
  expect(reasoningOf(kept[1] as AgentStreamMessage)).toEqual([CLAUDE_THINKING_HIDDEN])
  expect(reasoningOf(kept[3] as AgentStreamMessage)).toEqual([CLAUDE_THINKING_HIDDEN])

  // Each tool result joins the call the step streamed, by the id the call carried.
  const calls = kept.flatMap((message) =>
    message.parts.flatMap((part) => (part.type === "tool-call" ? [part] : [])),
  )
  const results = toolResults(replay)
  expect(calls.map((call) => call.toolName)).toEqual(["Read", "Read"])
  expect(results.map((result) => result.toolCallId)).toEqual(calls.map((call) => call.toolCallId))
  expect(results[0]?.isError).toBe(false)
  expect(results[0]?.output).toContain("hello from present.txt")
  expect(results[1]?.isError).toBe(true)
  expect(results[1]?.output).toContain("File does not exist.")
  expect(results.map((result) => result.toolName)).toEqual(["Read", "Read"])
  // The call's arguments are shown as JSON the viewer can read, not as an escaped string.
  expect(calls[0]?.argsText).toContain('"file_path"')
})

test("a real 2.1.310 turn with thinking summaries replays the summaries themselves", async () => {
  const recorded = await fixture("claude-2.1.310-thinking-summaries")
  const live = harness()
  for (const event of recorded.events) {
    live.feed(event)
    live.tick(1)
  }

  const thought = messages(live.replay()).flatMap(reasoningOf)
  expect(thought.length).toBeGreaterThan(0)
  expect(thought).not.toContain(CLAUDE_THINKING_HIDDEN)
  expect(thought[0]).toContain("present.txt")
})

test("a partly streamed tool call shows the arguments as they arrived", () => {
  const live = harness()
  live.feed(
    step({
      parts: [{ args: '{"file_', id: "toolu_1", name: "Read", type: "tool-call" }],
    }),
  )

  const parts = messages(live.replay())[0]?.parts ?? []
  expect(parts[0]?.type).toBe("tool-call")
  expect(parts[0]?.type === "tool-call" ? parts[0].argsText : "").toContain("file_")
})

test("the settings frame reports the models a pick can send and the level the step named", async () => {
  const recorded = await fixture("claude-2.1.310-turn")
  const live = harness()
  const rows = recorded.answers["config.list"]

  // Nothing read yet: the session cannot say what it runs.
  expect(live.stream.settings(undefined, undefined)).toEqual({ reported: false })

  // Read, and before the session's first request: a selector, and no level in force.
  const before = live.stream.settings("claude-opus-5-5", rows)
  expect(before.reported).toBe(true)
  if (!before.reported) throw new Error("the settings were not reported")
  expect(before.model).toBe("anthropic/claude-opus-5-5")
  expect(before.models).toEqual([
    "anthropic/default",
    "anthropic/sonnet",
    "anthropic/opus",
    "anthropic/haiku",
    "anthropic/fable",
    "anthropic/best",
    "anthropic/sonnet[1m]",
    "anthropic/opus[1m]",
    "anthropic/fable[1m]",
    "anthropic/opusplan",
  ])
  expect(before.thinking).toEqual({ level: null, levels: CLAUDE_EFFORT_LEVELS })

  // After the first request, the step's own effort is the level.
  for (const event of recorded.events) live.feed(event)
  const after = live.stream.settings("claude-opus-5-5", rows)
  if (!after.reported) throw new Error("the settings were not reported")
  expect(after.thinking).toEqual({ level: "max", levels: CLAUDE_EFFORT_LEVELS })
})

test("a model that reports no effort has no thinking to adjust", async () => {
  const recorded = await fixture("claude-2.1.310-no-effort")
  const live = harness()
  for (const event of recorded.events) live.feed(event)

  const settings = live.stream.settings(
    recorded.answers["session.model"],
    recorded.answers["config.list"],
  )
  if (!settings.reported) throw new Error("the settings were not reported")
  expect(settings.model).toBe("anthropic/claude-sonnet-4-5")
  expect(settings.thinking).toBeNull()
})

test("a step of another model answers nothing about the one the session runs now", async () => {
  const recorded = await fixture("claude-2.1.310-no-effort")
  const live = harness()
  for (const event of recorded.events) live.feed(event)

  const settings = live.stream.settings("claude-opus-5-5", recorded.answers["config.list"])
  if (!settings.reported) throw new Error("the settings were not reported")
  expect(settings.thinking).toEqual({ level: null, levels: CLAUDE_EFFORT_LEVELS })
})

test("the commands frame maps each source, drops an untypable name and marks nothing terminal", async () => {
  const recorded = await fixture("claude-2.1.310-no-effort")
  const listed = claudeAgentStreamCommands([
    ...recorded.answers["command.list"],
    { description: "a name nobody can type", name: "two words", source: "user" },
    { description: "an MCP prompt", name: "server:prompt", source: "mcp" },
    { description: "a leading slash", name: "/slashed", source: "user" },
  ])

  expect(listed.some((command) => command.name === "two words")).toBe(false)
  expect(listed.some((command) => command.terminalOnly === true)).toBe(false)
  expect(listed.find((command) => command.name === "slashed")?.source).toBe("prompt")
  expect(listed.find((command) => command.name === "server:prompt")?.source).toBe("prompt")
  // The recording's own list: 52 of Claude Code's own and 7 this plugin registers.
  expect(listed.find((command) => command.name === "compact")?.source).toBe("builtin")
  expect(listed.find((command) => command.name === "claude-envoy:dispatch")?.source).toBe(
    "extension",
  )
  expect(listed).toHaveLength(recorded.answers["command.list"].length + 2)
})

test("a conversation past the history limit keeps the newest messages", () => {
  const live = harness()
  const over = AGENT_STREAM_LIMITS.historyMessages + 20
  for (let index = 0; index < over; index += 1) {
    live.feed(step({ done: true, index, parts: [{ text: `step ${index}`, type: "text" }] }))
    live.tick(1)
  }

  const kept = messages(live.replay())
  expect(kept).toHaveLength(AGENT_STREAM_LIMITS.historyMessages)
  expect(textOf(kept[0] as AgentStreamMessage)).toBe(
    `step ${over - AGENT_STREAM_LIMITS.historyMessages}`,
  )
  expect(textOf(kept[kept.length - 1] as AgentStreamMessage)).toBe(`step ${over - 1}`)
})

test("a conversation past the byte budget answers inside it, newest first", () => {
  const live = harness()
  const wide = "x".repeat(AGENT_STREAM_LIMITS.partChars)
  for (let index = 0; index < 60; index += 1) {
    live.feed(step({ done: true, index, parts: [{ text: `${index}:${wide}`, type: "text" }] }))
    live.tick(1)
  }

  const replay = live.replay()
  const bytes = new TextEncoder().encode(JSON.stringify(replay.frames)).length
  expect(bytes).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.historyBytes)
  const kept = messages(replay)
  expect(kept.length).toBeLessThan(60)
  expect(textOf(kept[kept.length - 1] as AgentStreamMessage).startsWith("59:")).toBe(true)
})

test("a reset forgets the conversation and the level the session was running at", () => {
  const live = harness()
  live.feed(step({ done: true, effort: "high", parts: [{ text: "before", type: "text" }] }))
  const rows: readonly ModConfigRow[] = [
    {
      isLocked: false,
      key: "model",
      kind: "choice",
      label: "Model",
      options: ["sonnet", "opus"],
      provider: { plugin: "engine", tier: "core" },
      value: "Default (recommended)",
    },
  ]
  const before = live.stream.settings("claude-sonnet-5", rows)
  if (!before.reported) throw new Error("the settings were not reported")
  expect(before.thinking).toEqual({ level: "high", levels: CLAUDE_EFFORT_LEVELS })

  live.publisher.reset()
  live.stream.reset()
  live.publisher.noteViewer()
  expect(messages(live.replay())).toEqual([])
  const after = live.stream.settings("claude-sonnet-5", rows)
  if (!after.reported) throw new Error("the settings were not reported")
  expect(after.thinking).toEqual({ level: null, levels: CLAUDE_EFFORT_LEVELS })
})
