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
import {
  type ModAppendOrigin,
  type ModCallName,
  type ModCallResults,
  type ModCommandInfo,
  type ModConfigRow,
  type ModContentBlock,
  type ModEvent,
  unreadRowPrompt,
} from "../hooks/mod-protocol"
import {
  CLAUDE_EFFORT_LEVELS,
  CLAUDE_THINKING_HIDDEN,
  type ClaudeAgentStream,
  claudeAgentStreamCommands,
  createClaudeAgentStream,
} from "../src/agent-stream-claude"
import { modSession } from "../src/mod-session"

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

/** What a test gives the mapper besides the events: the deps the server wires from the session
 *  it runs beside (`../src/agent-stream-runner`), each absent by default. */
interface HarnessOptions {
  readonly tagFor?: (row: string) => string | undefined
  readonly isPane?: boolean
  readonly runsCommandRow?: (row: string) => boolean
  readonly runsUnreadPrompt?: (row: string) => boolean
  readonly appendedRow?: (row: string) => boolean
}

/** A publisher a viewer is already watching, with a clock the test moves itself: a real one would
 *  make a step's allocated timestamp unpredictable, and the slot is the timestamp. */
function harness(options: HarnessOptions = {}): Harness {
  let clock = 1_000
  const now = (): number => clock
  const published: AgentStreamFrame[] = []
  const publisher = new AgentStreamPublisher({
    now,
    publish: (_subject, payload) => published.push(JSON.parse(payload) as AgentStreamFrame),
  })
  const stream = createClaudeAgentStream({
    isPane: options.isPane === true,
    now,
    publisher,
    ...(options.appendedRow === undefined ? {} : { appendedRow: options.appendedRow }),
    ...(options.runsCommandRow === undefined ? {} : { runsCommandRow: options.runsCommandRow }),
    ...(options.runsUnreadPrompt === undefined
      ? {}
      : { runsUnreadPrompt: options.runsUnreadPrompt }),
    ...(options.tagFor === undefined ? {} : { tagFor: options.tagFor }),
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

/** The echo row Claude Code writes of a slash command, as 2.1.310 records it: one text block
 *  naming the command, the message it carries and its arguments, newline-separated, with every
 *  tag after the first indented (the transcripts `COMMAND_BOOKKEEPING_TAGS` names in
 *  `../src/claude-rows`).
 *
 *  `origin` says nothing about who ran it, and the default is what a terminal writes: measured
 *  on live Claude Code 2.1.310 (INFRA-1026), a `/config` a person typed and the `/usage` echo
 *  of a run this session made both carried `{"kind":"engine"}`, a run's echo in a Legion pane
 *  carried `{"kind":"plugin"}`, and the pane's own seeded command carried
 *  `{"kind":"unclassified"}`. The row Claude Code writes carries `type: "system"` and
 *  `name: "local_command"` with it. */
function commandEcho(
  name: string,
  args: string,
  uuid: string,
  origin: ModAppendOrigin = { kind: "engine" },
): ModEvent {
  const text = [
    `<command-name>/${name}</command-name>`,
    `            <command-message>${name}</command-message>`,
    `            <command-args>${args}</command-args>`,
  ].join("\n")
  return appendRow("command", [{ text, type: "text" }], {
    message: {
      content: [{ text, type: "text" }],
      name: "local_command",
      role: "user",
      type: "system",
    },
    origin,
    uuid,
  })
}

/** The output row Claude Code writes behind a command's echo: one text block wrapped in
 *  `<local-command-stdout>`, `type: "system"`, `name: "local_command"`, measured on live
 *  Claude Code 2.1.310 beside the echo above (INFRA-1026). */
function commandStdout(
  text: string,
  uuid: string,
  origin: ModAppendOrigin = { kind: "engine" },
): ModEvent {
  const wrapped = `<local-command-stdout>${text}</local-command-stdout>`
  return appendRow("command", [{ text: wrapped, type: "text" }], {
    message: {
      content: [{ text: wrapped, type: "text" }],
      name: "local_command",
      role: "user",
      type: "system",
    },
    origin,
    uuid,
  })
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
  const live = harness({
    // The session claims the row of the prompt it submitted at the row that turn never read
    // (`../src/mod-session`, `runsUnreadPrompt`), which is how this view knows the row from a
    // person's own prompt.
    runsUnreadPrompt: (row) => row === "row-nudge",
    tagFor: (row) => (row === "row-sent" ? "msg-7" : undefined),
  })
  // What Claude Code puts in front of the text a plugin submitted without `asUser`, measured on
  // 2.1.310. Such a prompt quotes rows of the conversation this view has already drawn, so
  // nothing is drawn for the prompt itself (`unreadRowPrompt`, `../hooks/mod-protocol`).
  const framed = (text: string): string => `The claude-envoy plugin sent a message:\n${text}`
  const nudge = unreadRowPrompt([{ kind: "person", messageId: "msg-7", text: "ship it" }])
  live.feed(
    // Measured on 2.1.310: a submit with `asUser` carries it on the row's origin, and the
    // plugin's own prompt is framed as the plugin speaking.
    appendRow("prompt", [{ text: "ship it", type: "text" }], {
      origin: { asUser: true, kind: "plugin", name: "claude-envoy" },
      uuid: "row-sent",
    }),
    appendRow("prompt", [{ text: framed(nudge), type: "text" }], {
      origin: { kind: "plugin", name: "claude-envoy" },
      uuid: "row-nudge",
    }),
  )

  const kept = messages(live.replay())
  expect(kept.map(textOf)).toEqual(["ship it"])
  // Tagged with the Dispatch message it is, so the page shows it once rather than beside the
  // copy Dispatch stored.
  expect(kept[0]?.dispatchMessageId).toBe("msg-7")
})

// An Envoy card reaches the conversation in one of two shapes, and the view draws each of them
// once.
//
// During a turn it is the row `$.session.append` writes: by the `note` door, `isMeta: true`,
// which the engine sets on a row a plugin appends (`SessionAppendArgs`,
// `../hooks/claude-mod.d.ts`), reported by the mod itself because Claude Code does not re-enter
// a mod's own `session.append` hook for it (`../hooks/mod.ts`), under a `plugin` origin with no
// name - the call's answer carries no origin, so the mod names the kind alone. Measured on
// 2.1.310
// (`/legion/scratch/tmp/t-term-uz/config/projects/-legion-scratch-tmp-t-term-uz-cwd/9bdfebc9-06e5-496a-ad93-06105d0af932.jsonl:167`,
// the HAZEL card delivered while a turn ran). That row is all such a card ever becomes - no
// prompt of its own carries it - so a view that drew nothing of an `isMeta` row showed the
// model answering a message the viewer never saw (INFRA-1026 review). The session says which
// `isMeta` row is its own by the uuid its append answered with (`appendedRow`).
//
// While the session is idle it is the prompt itself, submitted without `asUser` and stored
// nowhere else (`../src/mod-session`, `show`), which reaches the view as a `prompt`-door row
// under Claude Code's own framing. Drawn, because it is the card; the prompt at rows a turn
// never read is the one such prompt that is not, and the session claims that row by its uuid
// too (`runsUnreadPrompt`, the test above).
test("a card shows in both states, appended during a turn and submitted while idle", () => {
  const live = harness({
    appendedRow: (row) => row === "row-card",
    runsUnreadPrompt: (row) => row === "row-unread-prompt",
  })
  const appended = "Dispatch message from alice: the migration is approved."
  const submitted = "Dispatch message from bob: the rollback is done."
  live.feed(
    appendRow("note", [{ text: appended, type: "text" }], {
      message: {
        content: [{ text: appended, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "plugin" },
      uuid: "row-card",
    }),
    appendRow(
      "prompt",
      [{ text: `The claude-envoy plugin sent a message:\n${submitted}`, type: "text" }],
      { origin: { kind: "plugin", name: "claude-envoy" }, uuid: "row-card-prompt" },
    ),
    // The prompt at a row a turn never read quotes rows this view has already drawn, so it
    // draws nothing, though it comes by the same door under the same origin.
    appendRow(
      "prompt",
      [
        {
          text: `The claude-envoy plugin sent a message:\n${unreadRowPrompt([
            { kind: "card", text: appended },
          ])}`,
          type: "text",
        },
      ],
      { origin: { kind: "plugin", name: "claude-envoy" }, uuid: "row-unread-prompt" },
    ),
  )

  const kept = messages(live.replay())
  expect(kept.every((message) => message.role === "user")).toBe(true)
  expect(kept.map(textOf)).toEqual([
    appended,
    `The claude-envoy plugin sent a message:\n${submitted}`,
  ])
})

// A card's text is whatever Envoy delivered, and a card can carry the words the prompt at
// unread rows is built from: a review of this plugin's code, or a message quoting one, holds
// them (`../hooks/mod-protocol`, `unreadRowPrompt`). The rule before this one read those words off
// the row, so such a card reached the model and was drawn for nobody, and the prompt's wording
// was a contract with this mapper that no test of the wording could keep (INFRA-1026 review,
// round ten). The session says which prompt row is its own prompt at unread rows by the uuid
// Claude Code wrote the row under, and claims no other.
test("a card carrying the unread prompt's own words is drawn for the person", async () => {
  // The mod answers with the row the prompt became, which is what the card's frame rests on.
  const session = modSession({
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "session.message"
          ? (Promise.resolve({ as: "prompt", uuid: "row-card-prompt" }) as Promise<
              ModCallResults[N]
            >)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  // As the server wires the two (`../src/envoy-channel-server`): the session takes each event,
  // then the view records it, and the view asks the session whose row it is.
  const live = harness({ runsUnreadPrompt: (row) => session.runsUnreadPrompt(row) })
  const card = [
    "Dispatch message from carol: the reviewer asked why a message sent",
    unreadRowPrompt([{ kind: "card", text: "the deploy is done" }]),
  ].join("\n\n")
  const framed = `The claude-envoy plugin sent a message:\n${card}`

  // The session is idle, so the card is the prompt of a turn and reaches the conversation
  // nowhere else (`../src/mod-session`, `show`).
  const showing = session.show({ content: [{ text: card, type: "text" }], text: card })
  const row = appendRow("prompt", [{ text: framed, type: "text" }], {
    origin: { kind: "plugin", name: "claude-envoy" },
    uuid: "row-card-prompt",
    // The stamp the mod puts on the row it wrote for one message, which is not the stamp it
    // puts on the prompt at unread rows (`../hooks/mod-protocol`, `ModEvent`'s `wrote`).
    wrote: "message",
  })
  session.observe(row)
  live.feed(row)
  await showing

  expect(session.runsUnreadPrompt("row-card-prompt")).toBe(false)
  expect(messages(live.replay()).map(textOf)).toEqual([framed])
})

// Another installed mod writes model-facing context of its own with `$.session.append`, and
// that row reaches this plugin's hook marked `isMeta` under a `plugin` origin naming that mod:
// `{"kind":"plugin","name":"spike-mod2"}` and `{"kind":"plugin","name":"spike-mod3p"}` in the
// 2.1.310 recordings of the issue's spike (`/legion/scratch/tmp/infra-1026-spike/logs`). The
// rule before this one kept every `isMeta` row whose origin was a plugin's, so that mod's
// context was drawn as the person's own words (INFRA-1026 review, round ten). This session's
// own appended row names this plugin instead: the mod reports it under
// `{"kind":"plugin","name":"claude-envoy"}`, the origin Claude Code itself stamps on a row of
// this plugin's (`../hooks/mod.ts`), and the session claims that row's uuid by that name,
// which is what tells the two apart - nothing else on either row does.
test("another plugin's meta row is not the person's, and this session's appended card is", async () => {
  const session = modSession({
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "session.message"
          ? (Promise.resolve({ as: "row", uuid: "row-card" }) as Promise<ModCallResults[N]>)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  const live = harness({ appendedRow: (row) => session.appendedRow(row) })
  const feed = (event: ModEvent): void => {
    session.observe(event)
    live.feed(event)
  }
  const card = "Dispatch message from alice: the migration is approved."
  const theirs = "<system-reminder>The repository's branch is main.</system-reminder>"

  // A turn is running, so the card is a row of the conversation rather than a prompt.
  feed({ kind: "turn-start", turnId: "turn-1" })
  await session.show({ content: [{ text: card, type: "text" }], text: card })
  // What the mod reports of the row it appended: the `note` door, `isMeta`, and a `plugin`
  // origin naming this plugin (`../hooks/mod.ts`).
  feed(
    appendRow("note", [{ text: card, type: "text" }], {
      message: {
        content: [{ text: card, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "plugin", name: "claude-envoy" },
      uuid: "row-card",
      wrote: "message",
    }),
  )
  // Another mod's own append, as the engine hands it to this mod's hook.
  feed(
    appendRow("note", [{ text: theirs, type: "text" }], {
      message: {
        content: [{ text: theirs, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "plugin", name: "spike-mod3p" },
      uuid: "row-theirs",
    }),
  )

  expect(messages(live.replay()).map(textOf)).toEqual([card])
})

// The order of the live stack, which the test above cannot show: the row's event and the result
// of the call that wrote the row cross — the mod holds an event behind a post already in flight
// while a result goes out on a fetch of its own (`../hooks/mod.ts`, `flush`) — so the server can
// read the row while that call is still unanswered, and a claim made from its answer is not
// there yet. Driven live on real Claude
// Code 2.1.310 against this stack, a card delivered into a running turn reached the model and
// was drawn nowhere in the view at all (INFRA-1026 k2, session
// 1cdb849c-cfa4-458f-91a9-6f2c09653222). The session claims the row as it handles the row's
// own event, and the server hands it every event before the view records it
// (`../src/envoy-channel-server`), so the claim is in place when the mapper asks.
test("a card appended during a turn is drawn when its row arrives before the append answers", async () => {
  const appending = Promise.withResolvers<ModCallResults["session.message"]>()
  const session = modSession({
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "session.message"
          ? (appending.promise as unknown as Promise<ModCallResults[N]>)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  const live = harness({ appendedRow: (row) => session.appendedRow(row) })
  const feed = (event: ModEvent): void => {
    session.observe(event)
    live.feed(event)
  }
  const card = "Dispatch message from alice: the migration is approved."

  // A turn is running, so the card is a row of the conversation rather than a prompt.
  feed({ kind: "turn-start", turnId: "turn-1" })
  const showing = session.show({ content: [{ text: card, type: "text" }], text: card })
  feed(
    appendRow("note", [{ text: card, type: "text" }], {
      message: {
        content: [{ text: card, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "plugin", name: "claude-envoy" },
      uuid: "row-card",
      wrote: "message",
    }),
  )

  // Drawn out of the event alone: the call has answered nothing yet.
  expect(messages(live.replay()).map(textOf)).toEqual([card])
  appending.resolve({ as: "row", uuid: "row-card" })
  await showing
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

// One command a person ran writes three rows by the `command` door: the caveat saying the
// command's output went to the person rather than to the model, the echo of what was typed, and
// the output itself (measured on 2.1.310 with this plugin's own mod running `/usage`). Recorded
// as they stand, the caveat and the output draw a person the raw tags and Claude Code's
// `/context` table inside the agent's conversation (INFRA-1026).
test("a command's caveat row and its output record nothing", () => {
  const live = harness()
  const caveat =
    "<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"
  const stdout = "<local-command-stdout>Total cost:            $0.0993</local-command-stdout>"
  live.feed(
    appendRow("command", [{ text: caveat, type: "text" }], {
      message: {
        content: [{ text: caveat, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "plugin", name: "claude-envoy" },
      uuid: "row-caveat",
    }),
    // The output row's own type is `user` in some runs and `system` in others, so the tag its
    // text starts with is the only thing that says it is the command's output.
    appendRow("command", [{ text: stdout, type: "text" }], {
      message: {
        content: [{ text: stdout, type: "text" }],
        name: "local_command",
        type: "system",
      },
      uuid: "row-stdout",
    }),
  )

  expect(messages(live.replay())).toEqual([])
})

test("a command a person ran shows as the plain command line they typed", () => {
  const live = harness()
  live.feed(commandEcho("usage", "", "row-usage"), commandEcho("effort", "low", "row-effort"))

  const kept = messages(live.replay())
  expect(kept.map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/usage"],
    ["user", "/effort low"],
  ])
  expect(kept[0]?.parts).toEqual([{ text: "/usage", type: "text" }])
})

// A person's `/` message from Dispatch runs as `$.command.run`, and the row Claude Code writes
// of it echoes the command it ran rather than the one they sent: `/cost` ran as `/usage`, and
// `/model anthropic/haiku` as `/model haiku`, both measured live on 2.1.310 (INFRA-1026).
// Drawn, that row is a second message the person never wrote, beside Dispatch's stored copy of
// the one they did send, which is what the Agents page shows next to this stream. The rows of a
// run arrive after the call that asked for it has answered
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:35-42`: the result at t=…345207, the
// echo at …345297), so the run claims the first echo row inside a second of its own answer,
// rather than one by the command it names.
test("the echo of a command this session ran shows nothing, and one typed at the terminal shows", async () => {
  const ran = Promise.withResolvers<ModCallResults["command.run"]>()
  const session = modSession({
    commandOutputMs: 500,
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "command.run"
          ? (ran.promise as unknown as Promise<ModCallResults[N]>)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  const live = harness({ runsCommandRow: (row) => session.runsCommandRow(row) })
  // As the runner wires the two (`../src/agent-stream-runner`): the session takes each event,
  // then the view records it.
  const feed = (event: ModEvent): void => {
    session.observe(event)
    live.feed(event)
  }

  // The person sent `/cost`, and the result carries its own text: the run is answered before
  // any row of it has arrived.
  const printed = session.runCommand("cost", "")
  ran.resolve({ text: "Total cost:            $0.0993" })
  expect(await printed).toBe("Total cost:            $0.0993")

  // What Claude Code ran it as, echoed after the call answered, and a `/config` the person
  // typed at the session's own terminal in the same moment.
  feed(commandEcho("usage", "", "row-usage", { kind: "engine" }))
  feed(commandEcho("config", "", "row-config"))

  expect(messages(live.replay()).map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/config"],
  ])
})

// The rule before this one told a person's own terminal command from the echo of a run this
// session asked for by `origin.kind`, taking `unclassified` for the person's. Driven on a real
// Claude Code 2.1.310 terminal session, that value never appears: a `/config` the person typed
// while a Dispatch `/cost` was outstanding carried `{"kind":"engine"}`, the same as the `/usage`
// echo of the run itself, and so did a `/config` typed at an idle terminal and a plugin-run
// `/context` echo (read at the mod event, the one place the value exists, with one logging line
// added to a copy of the built plugin; the view's consequences reproduced on the untouched
// build). So that rule hid the person's command and drew the run's echo in its place, which is
// the defect it set out to remove, and the person's `/cost` reply carried the other command's
// output. Whatever tells the two apart, the person's own command must reach the view
// (INFRA-1026).
test("a command typed at the terminal shows although its origin is the run's own", async () => {
  const ran = Promise.withResolvers<ModCallResults["command.run"]>()
  const session = modSession({
    commandOutputMs: 500,
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "command.run"
          ? (ran.promise as unknown as Promise<ModCallResults[N]>)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  const live = harness({ runsCommandRow: (row) => session.runsCommandRow(row) })
  const feed = (event: ModEvent): void => {
    session.observe(event)
    live.feed(event)
  }

  const printed = session.runCommand("cost", "")
  ran.resolve({ text: "Total cost:            $0.0993" })
  expect(await printed).toBe("Total cost:            $0.0993")

  // Both rows carry the origin a 2.1.310 terminal really writes.
  feed(commandEcho("usage", "", "row-usage", { kind: "engine" }))
  feed(commandEcho("config", "", "row-config", { kind: "engine" }))

  expect(messages(live.replay()).map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/config"],
  ])
})

// A run whose `$.command.run` has not been answered has written no row of its own yet, so a row
// that arrives while it waits belongs to whoever typed it. Measured on live Claude Code 2.1.310
// (INFRA-1026): a Dispatch `/cost` asked at 08:09:56.390 opened a dialog at the terminal and
// had written no row 36 seconds later, when the person typed `/config` there - and the rule
// before this one handed that run the person's echo and output rows, so their command vanished
// from the view.
test("a command typed at the terminal shows while a command this session ran is still waiting", () => {
  const waiting = Promise.withResolvers<ModCallResults["command.run"]>()
  const session = modSession({
    commandOutputMs: 500,
    link: {
      connected: () => true,
      request: <N extends ModCallName>(call: N): Promise<ModCallResults[N]> =>
        call === "command.run"
          ? (waiting.promise as unknown as Promise<ModCallResults[N]>)
          : Promise.reject(new Error(`no answer for ${call}`)),
    },
    warn: () => undefined,
  })
  const live = harness({ runsCommandRow: (row) => session.runsCommandRow(row) })
  // As the runner wires the two (`../src/agent-stream-runner`): the session takes each event,
  // then the view records it.
  const feed = (event: ModEvent): void => {
    session.observe(event)
    live.feed(event)
  }

  // The person's `/cost` is still waiting on a dialog at the terminal.
  void session.runCommand("cost", "")
  // What they typed there meanwhile, and what it printed.
  feed(commandEcho("config", "", "row-config"))
  feed(commandStdout("Set model to sonnet", "row-config-stdout"))

  expect(messages(live.replay()).map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/config"],
  ])
})

// A `/rename` typed at a terminal leaves three rows, measured on live Claude Code 2.1.310
// (INFRA-1026) at 08:29:23: the echo by the `command` door (`{"kind":"engine"}`,
// `type: "system"`, `name: "local_command"`) at .018, its `<local-command-stdout>` row at .048,
// and at .070 a row by the `note` door - `{"kind":"engine"}`, `type: "user"`, `isMeta: true` -
// whose whole text is the `<system-reminder>` naming the session. Drawn, that reminder is
// model-facing XML under the person's name: the live view showed it as a message of theirs
// beside the command they typed.
test("a terminal rename shows the command line alone", () => {
  const live = harness()
  const reminder =
    '<system-reminder>\nThe user named this session "m1 rename probe". This may indicate the session\'s focus or intent.\n</system-reminder>'
  live.feed(
    commandEcho("rename", "m1 rename probe", "row-rename"),
    commandStdout("Session renamed to: m1 rename probe", "row-rename-stdout"),
    appendRow("note", [{ text: reminder, type: "text" }], {
      message: {
        content: [{ text: reminder, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "engine" },
      uuid: "row-rename-reminder",
    }),
  )

  expect(messages(live.replay()).map((message) => [message.role, textOf(message)])).toEqual([
    ["user", "/rename m1 rename probe"],
  ])
})

// A Legion pane is named before anything else happens in it: the daemon's bridge types
// `/rename <title>` into the session's input
// (`packages/daemon/internal/claudebridge/bridge.go`, lines 210-213), and Claude Code runs it
// as the command it is. Measured on live 2.1.310 in a pane (INFRA-1026): the caveat row
// (`{"kind":"unclassified"}`, `isMeta: true`), the echo (`{"kind":"unclassified"}`) and the
// `<local-command-stdout>` row (`{"kind":"engine"}`), and no prompt row at all. The pane being
// named is no message of a person's, and nothing of it is drawn.
test("a pane's seeded rename draws nothing", () => {
  const caveat =
    "<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"
  const pane = harness({ isPane: true })
  pane.feed(
    appendRow("command", [{ text: caveat, type: "text" }], {
      message: {
        content: [{ text: caveat, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "unclassified" },
      uuid: "row-pane-caveat",
    }),
    commandEcho("rename", "legion/INFRA-1026 worker", "row-pane-rename", {
      kind: "unclassified",
    }),
    commandStdout("Session renamed to: legion/INFRA-1026 worker", "row-pane-stdout"),
  )

  expect(messages(pane.replay())).toEqual([])
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

// A person's message that carries a picture reaches the session as the person's own text and the
// picture beside it. The page hides Dispatch's stored copy of that message only where the row the
// session streamed says what the person wrote, so the person's text has to stay a part of its
// own, as it does by every other door: flattened with the picture into one
// "Look at this\n[image]" part it matches no stored message, and the message shows twice
// (INFRA-1026).
test("a person's message with a picture keeps the person's text as a part of its own", () => {
  const live = harness({ tagFor: () => "m1" })
  live.feed(
    appendRow(
      "prompt",
      [
        { text: "Look at this", type: "text" },
        { source: { data: "…", type: "base64" }, type: "image" },
      ],
      { origin: { asUser: true, kind: "plugin" }, uuid: "row-picture" },
    ),
  )

  const kept = messages(live.replay())
  expect(kept).toHaveLength(1)
  expect(kept[0]?.dispatchMessageId).toBe("m1")
  expect(kept[0]?.parts).toEqual([
    { text: "Look at this", type: "text" },
    { text: "[image]", type: "text" },
  ])
})

// Claude Code writes a picture's bytes under the session's own images directory and records a row
// of its own naming where they went (`../src/claude-rows`, `isPictureAttachmentRow`). Measured on
// 2.1.310 in a Legion pane, where a picture sent from the Agents page wrote
// `type: "user"`, `isMeta: true`, `turnCompanion: true` and that one text block beside the
// person's row
// (`/legion/scratch/tmp/i3-k5-pane/config/projects/-legion-scratch-tmp-i3-k5-pane-cwd/f376a3a9-6e75-461b-806b-22dea4e83ddf.jsonl:123`).
// Recorded as it stands, it draws a second user row for the one message the person sent
// (INFRA-1026).
test("Claude Code's note of where it put a picture's bytes records nothing", () => {
  const live = harness({ tagFor: () => "m1" })
  const source =
    "[Image: source: /legion/scratch/tmp/i3-k5-pane/tmp/claude-1000/-legion-scratch-tmp-i3-k5-pane-cwd/f376a3a9-6e75-461b-806b-22dea4e83ddf/images/1.png]"
  live.feed(
    appendRow(
      "prompt",
      [
        { text: "What colour is this picture?", type: "text" },
        { source: { data: "…", type: "base64" }, type: "image" },
      ],
      { origin: { asUser: true, kind: "plugin" }, uuid: "row-picture" },
    ),
    appendRow("prompt", [{ text: source, type: "text" }], {
      message: {
        content: [{ text: source, type: "text" }],
        isMeta: true,
        role: "user",
        type: "user",
      },
      origin: { kind: "user" },
      uuid: "row-source",
    }),
  )

  // The person's message, and nothing of where the bytes went.
  expect(messages(live.replay()).map((message) => message.parts)).toEqual([
    [
      { text: "What colour is this picture?", type: "text" },
      { text: "[image]", type: "text" },
    ],
  ])
})

// A line a person types at the terminal while a turn is running reaches the view by the
// `delivery` door, inside framing Claude Code wrote for the model (`../src/claude-rows`,
// `claudeMidTurnMessage`). Measured on 2.1.310, where typing one sentence mid-turn wrote
// `origin.kind: "composer"`, `type: "attachment"`, `name: "queued_command"` and that one text
// block
// (`/legion/scratch/tmp/t-term-h1/config/projects/-legion-scratch-tmp-t-term-h1-cwd/b6411755-7568-4608-93a5-1db3c839e038.jsonl:70`).
// Recorded as it stands, it draws the tags and Claude Code's note to the model where the person
// wrote one sentence (INFRA-1026).
test("a line typed while a turn runs shows the person's words and none of the wrapper", () => {
  const live = harness({ tagFor: () => "m1" })
  const typed = "TYPED-AT-TERMINAL-h1: say the word CEDAR when you can."
  const wrapped = [
    "<system-reminder>",
    "The user sent a new message while you were working:",
    typed,
    "",
    "This is how Claude Code surfaces messages the user sends mid-turn — within the running turn, often alongside the next tool result, rather than as a separate conversation turn. Address the message above as you continue this turn.",
    "</system-reminder>",
  ].join("\n")
  live.feed(
    appendRow("delivery", [{ text: wrapped, type: "text" }], {
      message: {
        content: [{ text: wrapped, type: "text" }],
        name: "queued_command",
        role: "user",
        type: "attachment",
      },
      origin: { kind: "composer" },
      uuid: "row-typed",
    }),
  )

  const kept = messages(live.replay())
  expect(kept).toHaveLength(1)
  expect(kept[0]?.role).toBe("user")
  expect(kept[0]?.parts).toEqual([{ text: typed, type: "text" }])
})

// A `<system-reminder>` of any other kind is not that wrapper, and the view draws it as it draws
// any other text row. Measured on 2.1.310: a peer session's message comes by the same `delivery`
// door (`origin.kind: "peer"`), carrying an announcement of its own
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:223`). This pins the behaviour
// the mid-turn reader leaves alone (INFRA-1026).
test("a system reminder of another kind still crosses whole", () => {
  const live = harness()
  const peer =
    "<system-reminder>\nAnother Claude session sent a message while you were working:\nping\n</system-reminder>"
  live.feed(
    appendRow("delivery", [{ text: peer, type: "text" }], {
      message: { content: [{ text: peer, type: "text" }], role: "user", type: "attachment" },
      origin: { kind: "peer" },
      uuid: "row-peer",
    }),
  )

  expect(messages(live.replay()).map((message) => message.parts)).toEqual([
    [{ text: peer, type: "text" }],
  ])
})

test("a row with nothing to show records nothing, and the rows around it still publish", () => {
  const live = harness()
  live.feed(
    appendRow("prompt", [{ text: "is 391 prime?", type: "text" }], { uuid: "row-prompt" }),
    // What Claude Code's engine writes after every turn: a system row by the `notice` door,
    // named `turn_duration`, carrying no content blocks (measured on 2.1.310). Published, it is
    // an empty bubble per turn.
    appendRow("notice", [], {
      message: { content: [], name: "turn_duration", type: "system" },
      origin: { kind: "engine" },
      uuid: "row-duration",
    }),
    // A row whose only block is an empty text block shows as little, and goes the same way.
    appendRow("notice", [{ text: "", type: "text" }], {
      message: { content: [{ text: "", type: "text" }], type: "system" },
      origin: { kind: "engine" },
      uuid: "row-blank",
    }),
    // A notice that carries text is a row of the conversation, and still reaches the viewer.
    appendRow("notice", [{ text: "the Stop hook was skipped", type: "text" }], {
      message: {
        content: [{ text: "the Stop hook was skipped", type: "text" }],
        name: "informational",
        type: "system",
      },
      origin: { kind: "engine" },
      uuid: "row-notice",
    }),
    // A tool result with no output is a result, not a message: it keeps its own path, where its
    // call's id is its slot.
    appendRow("tool-result", [{ content: [], tool_use_id: "toolu_1", type: "tool_result" }], {
      origin: { kind: "tool", tool: "Bash" },
      uuid: "row-result",
    }),
  )

  const replayed = live.replay()
  expect(messages(replayed).map(textOf)).toEqual(["is 391 prime?", "the Stop hook was skipped"])
  expect(toolResults(replayed).map((result) => [result.toolCallId, result.output])).toEqual([
    ["toolu_1", ""],
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

test("a thinking pick is the level the header shows at once, until a step reports it back", async () => {
  const recorded = await fixture("claude-2.1.310-turn")
  const live = harness()
  const rows = recorded.answers["config.list"]

  // Before the session's first request and before any pick there is no level to show.
  const before = live.stream.settings("claude-opus-5-5", rows)
  if (!before.reported) throw new Error("the settings were not reported")
  expect(before.thinking).toEqual({ level: null, levels: CLAUDE_EFFORT_LEVELS })

  // The session has run: the recorded turn's steps ran at max.
  for (const event of recorded.events) live.feed(event)
  const ran = live.stream.settings("claude-opus-5-5", rows)
  if (!ran.reported) throw new Error("the settings were not reported")
  expect(ran.thinking).toEqual({ level: "max", levels: CLAUDE_EFFORT_LEVELS })

  // A person picks low. Claude Code reports an effort only on a request of its own and the
  // session is idle, so the pick is what the header shows, from now rather than from the next
  // turn.
  live.stream.pickedEffort("low")
  const picked = live.stream.settings("claude-opus-5-5", rows)
  if (!picked.reported) throw new Error("the settings were not reported")
  expect(picked.thinking).toEqual({ level: "low", levels: CLAUDE_EFFORT_LEVELS })

  // The next turn's step reports the level the pick set, and the step is the fact from there on.
  live.feed(
    step({
      done: true,
      effort: "low",
      index: 0,
      model: "claude-opus-5-5",
      parts: [{ text: "ready", type: "text" }],
      turnId: "turn-2",
    }),
  )
  const reported = live.stream.settings("claude-opus-5-5", rows)
  if (!reported.reported) throw new Error("the settings were not reported")
  expect(reported.thinking).toEqual({ level: "low", levels: CLAUDE_EFFORT_LEVELS })
})

// The mod puts the effort it was last told to set on every later request that names an effort of
// its own (`../hooks/mod`, `turn.step`), and only another pick changes it. So a person who picks
// a model after picking a level is still thinking at that level until a step of that model says
// otherwise, and the header says so: falling back to the model's default would tell the person a
// level the requests do not carry (INFRA-1026).
test("a model pick keeps the effort the mod puts on every later request in the header", async () => {
  const recorded = await fixture("claude-2.1.310-turn")
  const live = harness()
  const rows = recorded.answers["config.list"]

  // The session ran at max on claude-opus-5-5; a person picked low, and the next step reported it.
  for (const event of recorded.events) live.feed(event)
  live.stream.pickedEffort("low")
  live.feed(
    step({
      done: true,
      effort: "low",
      index: 0,
      model: "claude-opus-5-5",
      parts: [{ text: "ready", type: "text" }],
      turnId: "turn-2",
    }),
  )

  // The person now picks another model. No step of that model has run, and the mod still sets
  // low on a request that names an effort of its own, so low is the level in force.
  const switched = live.stream.settings("claude-sonnet-4-5", rows)
  if (!switched.reported) throw new Error("the settings were not reported")
  expect(switched.thinking).toEqual({ level: "low", levels: CLAUDE_EFFORT_LEVELS })
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

// Measured live on 2.1.310 and rejected by the tester: after a thinking pick of `low` with
// `claude-haiku-4-5` in force the header read `low` while the session's next request carried no
// effort at all. A step tells the truth about the request it ran, so a step whose own effort is
// not the picked one - another level, or none at all - ends the pick (INFRA-1026).
test("a step of a model that reports no effort ends a thinking pick", async () => {
  const recorded = await fixture("claude-2.1.310-no-effort")
  const live = harness()
  const rows = recorded.answers["config.list"]
  const model = recorded.answers["session.model"]

  // The pick is the level in force from the moment it lands, the session being idle.
  live.stream.pickedEffort("low")
  const picked = live.stream.settings(model, rows)
  if (!picked.reported) throw new Error("the settings were not reported")
  expect(picked.thinking).toEqual({ level: "low", levels: CLAUDE_EFFORT_LEVELS })

  // The recorded turn's steps name no effort at all, which is a model with no thinking to
  // adjust: the pick is over, and the header has no level to show.
  for (const event of recorded.events) live.feed(event)
  const reported = live.stream.settings(model, rows)
  if (!reported.reported) throw new Error("the settings were not reported")
  expect(reported.thinking).toBeNull()
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
