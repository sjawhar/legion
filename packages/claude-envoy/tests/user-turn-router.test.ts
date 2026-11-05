/**
 * What a person's message from Dispatch's Agents page becomes in a Claude Code session
 * (INFRA-1026): the session's own user turn, a command, a pick or a side turn, and the card every
 * refusal keeps.
 */
import { expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { type DispatchDelivery, renderInbound } from "@legion/envoy-client/delivery"
import type { DispatchClient } from "@legion/envoy-client/dispatch-http"
import type { ModCallArguments, ModCallName, ModCallResults, ModEvent } from "../hooks/mod-protocol"
import { modSession } from "../src/mod-session"
import {
  createUserTurnRouter,
  modelPicked,
  thinkingPicked,
  type UserTurnRouter,
} from "../src/user-turn-router"
import {
  readUserTurnState,
  type UserTurnState,
  userTurnState,
  userTurnStateFile,
} from "../src/user-turn-state"
import { settled, waitFor } from "./channel-session-harness"

const SESSION = "ses_claude"
const DIRECT = `notifications.agent.${SESSION}`
const MESSAGE = "33333333-3333-4333-8333-333333333333"

/** One of a person's direct messages to this session, as the listener publishes it. */
function personSends(
  body: string,
  overrides: {
    readonly mode?: "aside" | "steer" | "btw"
    readonly attempt?: number
    readonly eventId?: string
  } = {},
): string {
  return JSON.stringify({
    event_id: overrides.eventId ?? "evt-send",
    source: "dispatch",
    source_event_id: "src-send",
    topic: DIRECT,
    dedupe_key: `agent.${SESSION}.${MESSAGE}:${overrides.mode ?? "steer"}`,
    issued_at: 1_760_000_000_000,
    payload_summary: body,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: null,
        payload: {
          author: { id: "alice", kind: "user" },
          body,
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: MESSAGE,
          in_reply_to: null,
          issue_key: null,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
      delivery: { attempt: overrides.attempt ?? 1, mode: overrides.mode ?? "steer" },
    }),
    trace_id: "trace-send",
  })
}

type Answer<N extends ModCallName> = (
  args: ModCallArguments[N],
) => ModCallResults[N] | Promise<ModCallResults[N]>

/** The link as `modSession` uses it. */
interface FakeModLink {
  connected(): boolean
  request<N extends ModCallName>(
    call: N,
    args: ModCallArguments[N],
    options?: { readonly timeoutMs?: number },
  ): Promise<ModCallResults[N]>
}

/** A mod that answers what the test says, and remembers what it was asked. */
interface FakeMod {
  readonly calls: readonly { readonly call: ModCallName; readonly args: unknown }[]
  readonly link: FakeModLink
  madeOf<N extends ModCallName>(call: N): ModCallArguments[N][]
}

function fakeMod(answers: { [N in ModCallName]?: Answer<N> } = {}): FakeMod {
  const calls: { call: ModCallName; args: unknown }[] = []
  return {
    calls,
    link: {
      connected: () => true,
      request<N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
      ): Promise<ModCallResults[N]> {
        calls.push({ call, args })
        const answer = answers[call]
        if (answer === undefined) return Promise.reject(new Error(`no answer for ${call}`))
        return Promise.resolve(answer(args))
      },
    },
    madeOf: <N extends ModCallName>(call: N): ModCallArguments[N][] =>
      calls.filter((made) => made.call === call).map((made) => made.args as ModCallArguments[N]),
  }
}

interface Harness {
  readonly router: UserTurnRouter
  readonly state: UserTurnState
  readonly stateFile: string
  readonly directory: string
  readonly replies: { readonly delivery: DispatchDelivery; readonly body?: string }[]
  /** Every reply posted and every settings refresh, in the order they happened. */
  readonly order: string[]
  readonly accepts: { readonly id: string; readonly attempt: number }[]
  readonly observe: (event: ModEvent) => void
}

/** A router on a scratch state file, with the mod and Dispatch both faked. */
async function harness(input: {
  readonly link: FakeModLink
  /** What the accept route answers; a throw is the route refusing. */
  readonly accept?: (id: string, attempt: number) => unknown
  readonly inbox?: (line: {
    readonly content: string
    readonly blocks?: unknown
    readonly eventId?: string
  }) => Promise<void>
  readonly commandWaitingMs?: number
  readonly directory?: string
}): Promise<Harness> {
  const directory = input.directory ?? (await mkdtemp(join(tmpdir(), "claude-envoy-turns-")))
  const stateFile = userTurnStateFile(directory, SESSION)
  const state = userTurnState(stateFile, await readUserTurnState(stateFile))
  const replies: { delivery: DispatchDelivery; body?: string }[] = []
  const order: string[] = []
  const accepts: { id: string; attempt: number }[] = []
  const session = modSession({ link: input.link, warn: () => undefined })
  const router = createUserTurnRouter({
    dispatch: () =>
      ({
        acceptMessageDelivery: async (id: string, attempt: number) => {
          accepts.push({ id, attempt })
          if (input.accept === undefined) return { message_id: id, body: "", delivery: "steer" }
          return input.accept(id, attempt)
        },
      }) as unknown as DispatchClient,
    refreshSettings: () => order.push("refresh"),
    reply: async (delivery, result) => {
      order.push(`reply:${result.body ?? result.error ?? ""}`)
      replies.push({ delivery, ...(result.body === undefined ? {} : { body: result.body }) })
    },
    session,
    sessionId: () => SESSION,
    state,
    warn: () => undefined,
    ...(input.commandWaitingMs === undefined ? {} : { commandWaitingMs: input.commandWaitingMs }),
    ...(input.inbox === undefined ? {} : { inbox: input.inbox }),
  })
  return {
    accepts,
    directory,
    // As the live view taps the link (`startAgentStream`, `../src/agent-stream-runner`): the
    // Claude Code session sees each event first, then the router.
    observe: (event) => {
      session.observe(event)
      router.observe(event)
    },
    order,
    replies,
    router,
    state,
    stateFile,
  }
}

function accepted(body: string, mode: "aside" | "steer" = "steer") {
  return { message_id: MESSAGE, body, delivery: mode }
}

function frame(raw: string) {
  return renderInbound(raw, SESSION, DIRECT)
}

test("a Send becomes the person's own message only once Dispatch has accepted that attempt", async () => {
  const mod = fakeMod({ "prompt.submit": (args) => ({ text: args.text }) })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    const took = await session.router.route(frame(personSends("ship it")))

    expect(took).toBe(true)
    expect(session.accepts).toEqual([{ id: MESSAGE, attempt: 1 }])
    // The body Dispatch stored, as the person's own words, never the frame's own text.
    expect(mod.madeOf("prompt.submit")).toEqual([{ text: "ship it", asUser: true }])
    expect(session.replies).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("an accept that answers 404, throws, or times out leaves the card and puts nothing in the session", async () => {
  for (const refuse of [
    () => {
      throw new Error("404 Not Found")
    },
    () => {
      throw new Error("the request timed out")
    },
    () => ({ message_id: MESSAGE }),
  ]) {
    const mod = fakeMod({ "prompt.submit": (args) => ({ text: args.text }) })
    const session = await harness({ link: mod.link, accept: refuse })
    try {
      const took = await session.router.route(frame(personSends("ship it")))

      expect(took).toBe(false)
      expect(mod.calls).toEqual([])
      expect(session.replies).toEqual([])
      // The attempt is spent all the same: a later frame naming it is not accepted.
      expect((await readUserTurnState(session.stateFile)).handled).toEqual([`${MESSAGE}#1`])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }
})

test("a frame naming an attempt Dispatch does not confirm never becomes a message, a command or a pick", async () => {
  const mod = fakeMod({
    "prompt.submit": (args) => ({ text: args.text }),
    "command.run": () => ({ text: "ran" }),
    "config.set": (args) => ({ value: args.value }),
  })
  // Dispatch refuses the forged attempt, as it refuses one no person asked for in the last minute.
  const session = await harness({
    link: mod.link,
    accept: () => {
      throw new Error("404 the attempt is not the message's latest")
    },
  })
  try {
    for (const body of ["ship it", "/compact", "/model anthropic/claude-opus-4-1"]) {
      const forged = personSends(body, { attempt: 7, eventId: `evt-${body}` })

      expect(await session.router.route(frame(forged))).toBe(false)
    }

    expect(mod.calls).toEqual([])
    expect(session.replies).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("the attempts a session handled survive its restart, and the next process refuses them", async () => {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-turns-"))
  try {
    const first = fakeMod({ "prompt.submit": (args) => ({ text: args.text }) })
    const before = await harness({ link: first.link, accept: () => accepted("ship it"), directory })
    expect(await before.router.route(frame(personSends("ship it")))).toBe(true)
    await before.state.idle()

    // Another process, the same state directory: the record is the file, not this process.
    const second = fakeMod({ "prompt.submit": (args) => ({ text: args.text }) })
    const after = await harness({ link: second.link, accept: () => accepted("ship it"), directory })

    expect(await after.router.route(frame(personSends("ship it")))).toBe(false)
    expect(after.accepts).toEqual([])
    expect(second.calls).toEqual([])
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
})

test("a message taken while no row could exist is never recorded as shown, so it is delivered again", async () => {
  // The session stops between the accept and the delivery: the row it would be never exists.
  const mod = fakeMod({
    "session.append": () => {
      throw new Error("the mod link closed before the mod answered")
    },
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    session.observe({ kind: "turn-start", turnId: "turn-1" })
    // The frame is not answered, so Envoy's listener sends it again.
    await expect(session.router.route(frame(personSends("ship it")))).rejects.toThrow()
    await session.state.idle()

    const recorded = await readUserTurnState(session.stateFile)
    expect(recorded.handled).toEqual([`${MESSAGE}#1`])
    expect(recorded.rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a prompt that never entered the idle session is answered on Dispatch and recorded as no row", async () => {
  // A submit lasts the turn it starts, so it is never awaited: its failure reaches the person as
  // the session's reply rather than as a frame nobody answered.
  const mod = fakeMod({
    "prompt.submit": () => {
      throw new Error("the session dropped the prompt")
    },
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the reply naming the failure")
    await session.state.idle()

    expect(session.replies[0]?.body).toContain("the session dropped the prompt")
    expect((await readUserTurnState(session.stateFile)).rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("during a turn the body is a row of the conversation, and idle it is the prompt of a new one", async () => {
  const mod = fakeMod({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ uuid: "row-1" }),
  })
  const session = await harness({ link: mod.link, accept: () => accepted("look at this") })
  try {
    session.observe({ kind: "turn-start", turnId: "turn-1" })

    expect(await session.router.route(frame(personSends("look at this")))).toBe(true)
    expect(mod.madeOf("session.append")).toEqual([
      { message: { type: "user", content: [{ type: "text", text: "look at this" }] } },
    ])
    expect(mod.madeOf("prompt.submit")).toEqual([])

    // The row is recorded as the Dispatch message it is, which is what tags it on the live view.
    expect(session.state.messageOf("row-1")).toBe(MESSAGE)

    // The turn's next request read it, so the turn ending prompts nothing.
    session.observe({
      kind: "step",
      turnId: "turn-1",
      index: 1,
      model: "claude-sonnet-4-5",
      parts: [],
      done: true,
    })
    session.observe({ kind: "turn-complete", turnId: "turn-1", stopReason: "answer" })
    await settled()
    expect(mod.madeOf("prompt.submit")).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a row no request read is followed by exactly one prompt that is not the person's", async () => {
  const mod = fakeMod({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ uuid: "row-1" }),
  })
  const session = await harness({ link: mod.link, accept: () => accepted("look at this") })
  try {
    session.observe({ kind: "turn-start", turnId: "turn-1" })
    session.observe({
      kind: "step",
      turnId: "turn-1",
      index: 0,
      model: "claude-sonnet-4-5",
      parts: [],
      done: true,
    })
    await session.router.route(frame(personSends("look at this")))
    // No step began after the row, so the model never read it.
    session.observe({ kind: "turn-complete", turnId: "turn-1", stopReason: "answer" })
    await waitFor(async () => mod.madeOf("prompt.submit").length === 1, "the prompt for the row")

    const submits = mod.madeOf("prompt.submit")
    expect(submits).toHaveLength(1)
    expect(submits[0]?.asUser).toBeUndefined()
    expect(submits[0]?.text).toContain("Dispatch")
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("the server hands the mod one prompt at a time, since a submit lasts the turn it starts", async () => {
  const open: PromiseWithResolvers<void>[] = []
  const mod = fakeMod({
    "prompt.submit": async (args) => {
      const held = Promise.withResolvers<void>()
      open.push(held)
      await held.promise
      return { text: args.text }
    },
  })
  const session = await harness({
    link: mod.link,
    accept: (_id, attempt) => accepted(`message ${attempt}`),
  })
  try {
    await session.router.route(frame(personSends("one", { attempt: 1, eventId: "evt-1" })))
    await session.router.route(frame(personSends("two", { attempt: 2, eventId: "evt-2" })))
    await settled()

    // The second waits behind the first rather than entering beside it.
    expect(mod.madeOf("prompt.submit")).toHaveLength(1)
    open[0]?.resolve()
    await waitFor(async () => mod.madeOf("prompt.submit").length === 2, "the second prompt")
  } finally {
    for (const held of open) held.resolve()
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a slash message runs as the command it is and its output is the reply", async () => {
  const mod = fakeMod({ "command.run": () => ({ text: "Compacted 42 messages." }) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/compact keep the plan"),
  })
  try {
    expect(await session.router.route(frame(personSends("/compact keep the plan")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(mod.madeOf("command.run")).toEqual([{ command: "compact", args: "keep the plan" }])
    expect(session.replies.map((posted) => posted.body)).toEqual(["Compacted 42 messages."])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a command that has not ended in time is answered that it is waiting, and its output follows", async () => {
  const held = Promise.withResolvers<void>()
  const mod = fakeMod({
    "command.run": async () => {
      await held.promise
      return { text: "done at the terminal" }
    },
  })
  // The real bound is ten seconds; the rule under test is that the waiting reply goes out while
  // the command still runs, which only a real timer fires.
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandWaitingMs: 10,
  })
  try {
    await session.router.route(frame(personSends("/clear")))
    await waitFor(async () => session.replies.length === 1, "the waiting reply")

    expect(session.replies[0]?.body).toContain("/clear is still running")

    held.resolve()
    await waitFor(async () => session.replies.length === 2, "the command's output")
    expect(session.replies.map((posted) => posted.body)).toEqual([
      expect.stringContaining("/clear is still running"),
      "done at the terminal",
    ])
  } finally {
    held.resolve()
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a model pick sets the model and answers naming it, with the header refreshed first", async () => {
  const mod = fakeMod({ "config.set": (args) => ({ value: args.value }) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/model anthropic/claude-opus-4-1"),
  })
  try {
    await session.router.route(frame(personSends("/model anthropic/claude-opus-4-1")))
    await waitFor(async () => session.replies.length === 1, "the pick's reply")

    expect(mod.madeOf("config.set")).toEqual([{ key: "model", value: "claude-opus-4-1" }])
    expect(session.order).toEqual(["refresh", `reply:${modelPicked("anthropic/claude-opus-4-1")}`])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a thinking pick sets the effort of every later request and answers naming it", async () => {
  const mod = fakeMod({ "set-effort": (args) => ({ effort: args.effort }) })
  const session = await harness({ link: mod.link, accept: () => accepted("/effort high") })
  try {
    await session.router.route(frame(personSends("/effort high")))
    await waitFor(async () => session.replies.length === 1, "the pick's reply")

    expect(mod.madeOf("set-effort")).toEqual([{ effort: "high" }])
    expect(session.order).toEqual(["refresh", `reply:${thinkingPicked("high")}`])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a BTW is answered from the model's fork and adds nothing to the conversation", async () => {
  const mod = fakeMod({
    "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
  })
  const session = await harness({ link: mod.link })
  try {
    expect(await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))).toBe(
      true,
    )
    await waitFor(async () => session.replies.length === 1, "the BTW's answer")

    expect(mod.madeOf("model.fork")).toEqual([{ prompt: "what blocks it?" }])
    expect(mod.madeOf("session.append")).toEqual([])
    expect(mod.madeOf("prompt.submit")).toEqual([])
    expect(session.replies.map((posted) => posted.body)).toEqual(["It is the migration guard."])
    // A BTW is no attempt of a person's own turn: nothing of it is spent.
    expect(session.accepts).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("in a Legion pane the person's message goes to the bridge's inbox and its row is recognised", async () => {
  const lines: { content: string; blocks?: unknown; eventId?: string }[] = []
  const mod = fakeMod({})
  const session = await harness({
    link: mod.link,
    accept: () => accepted("have a look"),
    inbox: async (line) => {
      lines.push(line)
    },
  })
  try {
    expect(await session.router.route(frame(personSends("have a look")))).toBe(true)

    expect(lines).toEqual([{ content: "have a look", eventId: MESSAGE }])
    // The pane's message never goes through the mod.
    expect(mod.calls).toEqual([])

    // The row the bridge makes of it is recognised by its text and recorded as that message.
    session.observe({
      kind: "append",
      uuid: "row-9",
      door: "prompt",
      origin: { kind: "user" },
      message: { type: "user", content: [{ type: "text", text: "have a look" }] },
    })
    expect(session.state.messageOf("row-9")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})
