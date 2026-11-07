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
  commandPrintedNothing,
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
  readonly replies: {
    readonly delivery: DispatchDelivery
    readonly body?: string
    /** Whether the answer asked Dispatch to post it although the attempt is already answered
     *  (`?follow_up=true`), which every answer after the first needs. */
    readonly followUp?: boolean
  }[]
  /** Every pick handed to the live view, every reply posted and every settings refresh, in the
   *  order they happened. */
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
  /** How long a run whose echo row has been claimed waits for the output row behind it; short,
   *  so a test that drives that wait does not sit through the real bound. */
  readonly commandOutputMs?: number
  /** How long after its own answer a run may still claim an echo row; short, for the same
   *  reason. */
  readonly commandEchoMs?: number
  readonly directory?: string
}): Promise<Harness> {
  const directory = input.directory ?? (await mkdtemp(join(tmpdir(), "claude-envoy-turns-")))
  const stateFile = userTurnStateFile(directory, SESSION)
  const state = userTurnState(stateFile, await readUserTurnState(stateFile))
  const replies: { delivery: DispatchDelivery; body?: string }[] = []
  const order: string[] = []
  const accepts: { id: string; attempt: number }[] = []
  const session = modSession({
    link: input.link,
    warn: () => undefined,
    ...(input.commandEchoMs === undefined ? {} : { commandEchoMs: input.commandEchoMs }),
    ...(input.commandOutputMs === undefined ? {} : { commandOutputMs: input.commandOutputMs }),
  })
  const router = createUserTurnRouter({
    dispatch: () =>
      ({
        acceptMessageDelivery: async (id: string, attempt: number) => {
          accepts.push({ id, attempt })
          if (input.accept === undefined) return { message_id: id, body: "", delivery: "steer" }
          return input.accept(id, attempt)
        },
      }) as unknown as DispatchClient,
    effortPicked: (effort) => order.push(`picked:${effort}`),
    refreshSettings: () => order.push("refresh"),
    reply: async (delivery, result) => {
      order.push(`reply:${result.body ?? result.error ?? ""}`)
      // `followUp` is the field an answer after the first needs; until the router carries one
      // the read is simply absent, which is what the test below pins.
      const asked = "followUp" in result ? result.followUp : undefined
      replies.push({
        delivery,
        ...(result.body === undefined ? {} : { body: result.body }),
        ...(asked === undefined ? {} : { followUp: asked }),
      })
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

/** One row of the `command` door, as the mod reports it. */
function commandRow(text: string, uuid: string): ModEvent {
  return {
    door: "command",
    kind: "append",
    message: { content: [{ text, type: "text" }], role: "user", type: "user" },
    origin: { kind: "engine" },
    uuid,
  }
}

/** The echo row Claude Code writes of a slash command it ran, as 2.1.310 records it: one text
 *  block naming the command, the message it carries and its arguments, each tag on its own line
 *  and every one after the first indented
 *  (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`). */
function commandEcho(name: string, args: string): ModEvent {
  const text = [
    `<command-name>/${name}</command-name>`,
    `            <command-message>${name}</command-message>`,
    `            <command-args>${args}</command-args>`,
  ].join("\n")
  return commandRow(text, `row-echo-${name}`)
}

/** The output row Claude Code writes for a command it ran: one text block wrapped in
 *  `<local-command-stdout>`, which is where most commands put everything they printed. */
function commandOutput(text: string): ModEvent {
  return commandRow(`<local-command-stdout>${text}</local-command-stdout>`, "row-stdout")
}

test("a Send becomes the person's own message only once Dispatch has accepted that attempt", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "prompt", uuid: "prompt-row-1" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    const took = await session.router.route(frame(personSends("ship it")))

    expect(took).toBe(true)
    expect(session.accepts).toEqual([{ id: MESSAGE, attempt: 1 }])
    // The body Dispatch stored, as the person's own words, never the frame's own text.
    expect(mod.madeOf("session.message")).toEqual([
      {
        content: [{ type: "text", text: "ship it" }],
        message: { kind: "person", messageId: MESSAGE, text: "ship it" },
      },
    ])
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
    const mod = fakeMod({ "session.message": () => ({ as: "prompt", uuid: "prompt-row-1" }) })
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
    "session.message": () => ({ as: "prompt", uuid: "prompt-row-1" }),
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
    const first = fakeMod({ "session.message": () => ({ as: "prompt", uuid: "prompt-row-1" }) })
    const before = await harness({ link: first.link, accept: () => accepted("ship it"), directory })
    expect(await before.router.route(frame(personSends("ship it")))).toBe(true)
    await before.state.idle()

    // Another process, the same state directory: the record is the file, not this process.
    const second = fakeMod({ "session.message": () => ({ as: "prompt", uuid: "prompt-row-2" }) })
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
    "session.message": () => {
      throw new Error("the mod link closed before the mod answered")
    },
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
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
  // A prompt the session refused is the person's to be told about: their own words were
  // dropped, and Dispatch is where they are waiting. Anything else that went wrong leaves the
  // frame unanswered instead (`../src/mod-session`, `deliver`).
  const mod = fakeMod({
    "session.message": () => ({ as: "prompt", deny: "a UserPromptSubmit hook blocked it" }),
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the reply naming the failure")
    await session.state.idle()

    expect(session.replies[0]?.body).toContain("a UserPromptSubmit hook blocked it")
    expect((await readUserTurnState(session.stateFile)).rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})
test("the body goes to the mod as the person's own message, and its row is recorded", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "row", uuid: "row-1" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("look at this") })
  try {
    expect(await session.router.route(frame(personSends("look at this")))).toBe(true)

    // One call, carrying the person's own words and the Dispatch message they are: what that
    // becomes in the session — a row of the turn running, or the prompt of a new turn — and
    // what points the model at a row no request read are the mod's (`../hooks/mod.ts`).
    expect(mod.madeOf("session.message")).toEqual([
      {
        content: [{ type: "text", text: "look at this" }],
        message: { kind: "person", messageId: MESSAGE, text: "look at this" },
      },
    ])
    // The row the mod answered with is recorded as that Dispatch message, which is what tags it
    // on the live view.
    expect(session.state.messageOf("row-1")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A message the mod took as the prompt of a new turn answers with that prompt's own row, which
// is no row of this message's: the router recognises it by its text instead, and records
// nothing here (`../src/mod-session`, `deliver`).
test("a body the mod took as a prompt is recorded as no row of its own", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "prompt", uuid: "prompt-row-1" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(true)
    await session.state.idle()

    expect((await readUserTurnState(session.stateFile)).rows).toEqual([])
  } finally {
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

// Measured on 2.1.310 in a terminal session: `$.command.run("context")` resolves
// `{context: ["## Context Usage\n\n**Model:** …"]}` with no `text` field at all
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`). Answering `text ?? ""`
// left the person who sent `/context` from the live view with no reply at all (INFRA-1026).
test("a command whose result puts its output in `context` reaches the person as the reply", async () => {
  const table = "## Context Usage\n\n**Model:** claude-haiku-4-5  \n**Tokens:** 32k / 200k (16%)"
  const mod = fakeMod({ "command.run": () => ({ context: [table] }) })
  const session = await harness({ link: mod.link, accept: () => accepted("/context") })
  try {
    expect(await session.router.route(frame(personSends("/context")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(mod.madeOf("command.run")).toEqual([{ command: "context" }])
    expect(session.replies.map((posted) => posted.body)).toEqual([table])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// Most commands resolve with nothing and put everything they printed in the row they wrote:
// `/effort low` on 2.1.310 resolved `{}` and wrote its `<local-command-stdout>` row 158 ms
// later (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`). The row and the
// result reach this process on two separate posts, so the row is waited for a bounded moment
// rather than answered empty at once.
test("a command whose result carries no output is answered from the row Claude Code wrote", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    session.observe(commandEcho("cost", ""))
    session.observe(commandOutput("Total cost:            $0.0993"))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual(["Total cost:            $0.0993"])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// `typedInputReply` answers undefined for an empty output, which posted nothing: a person who
// sent a command that printed nothing was left with silence (INFRA-1026).
test("a command that printed nothing is answered saying so rather than with silence", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/clear")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    // An output row that arrives empty is itself the answer that the command printed nothing.
    session.observe(commandEcho("clear", ""))
    session.observe(commandOutput(""))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual([commandPrintedNothing("/clear")])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A command does not run under the name the person sent it with: measured live on 2.1.310, a
// `/cost` from Dispatch was run and echoed by Claude Code as `/usage`, and a
// `/model anthropic/haiku` as `/model haiku` (INFRA-1026). So what ties the rows to the run is
// that they arrived just after its call answered, never the command the rows name.
test("a command Claude Code runs under another name is still answered from that run's output", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")
    expect(mod.madeOf("command.run")).toEqual([{ command: "cost" }])

    session.observe(commandEcho("usage", ""))
    session.observe(commandOutput("Total cost:            $0.0993"))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual(["Total cost:            $0.0993"])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A run whose command opened a dialog at the terminal has written no row of its own while the
// person answers it, and the rows that arrive meanwhile are the person's own typing. Measured
// on live Claude Code 2.1.310 (INFRA-1026): a Dispatch `/cost` asked at 08:09:56.390 was still
// unanswered when the person typed `/config` at that terminal 36 seconds later, and the rule
// before this one answered their `/cost` with `Config dialog dismissed` - the other command's
// output, posted to Dispatch as the reply to theirs.
test("a command a person typed at the terminal is not the answer to one still waiting", async () => {
  const dialog = Promise.withResolvers<ModCallResults["command.run"]>()
  const mod = fakeMod({ "command.run": () => dialog.promise })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandEchoMs: 100,
    commandOutputMs: 100,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    // What the person typed at their own terminal while the dialog waited for them.
    session.observe(commandEcho("config", ""))
    session.observe(commandOutput("Config dialog dismissed"))
    await settled()
    expect(session.replies).toEqual([])

    // Their `/cost` answers for itself once the dialog closes, and it printed nothing.
    dialog.resolve({})
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual([commandPrintedNothing("/cost")])
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

// Dispatch settles a delivery attempt with the first reply it takes, and answers a second POST to
// the same attempt by handing the stored reply back and posting nothing, unless the request asks
// for a follow-up (`?follow_up=true`; `packages/envoy/internal/dispatch/api/message_delivery.go`,
// and `DispatchClient.messageReply`'s own `followUp` option). So the answer that carries a slow
// command's output - the one the waiting reply promised "follows in another reply" - has to ask
// for it, or the person is left with the waiting reply and never gets the output. Driven live on
// a Claude Code 2.1.310 pane: `/context` inside a turn got the waiting reply, the mod's POST of
// the table was answered 200 with nothing inserted, and the conversation holds one reply
// (INFRA-1026).
test("the answer that follows a waiting reply asks Dispatch to post it", async () => {
  const held = Promise.withResolvers<void>()
  const mod = fakeMod({
    "command.run": async () => {
      await held.promise
      return { text: "done at the terminal" }
    },
  })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandWaitingMs: 10,
  })
  try {
    await session.router.route(frame(personSends("/clear")))
    await waitFor(async () => session.replies.length === 1, "the waiting reply")
    held.resolve()
    await waitFor(async () => session.replies.length === 2, "the command's output")

    // The first answer settles the attempt, so it asks for nothing; the second is the one
    // Dispatch drops without the ask.
    expect(session.replies.map((posted) => posted.followUp === true)).toEqual([false, true])
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
    // The live view is told the level the pick put in force before the reply naming it goes
    // out: Claude Code reports an effort only on a request of its own, and an idle session
    // makes none.
    expect(session.order).toEqual(["picked:high", "refresh", `reply:${thinkingPicked("high")}`])
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
    expect(mod.madeOf("session.message")).toEqual([])
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

// A person's message that embeds a picture reaches the session as its text and the picture
// beside it, and the row written back for it holds the text alone - measured live on a Legion
// pane (Claude Code 2.1.310), with the picture's markdown gone from it, the picture having become
// a block of its own. Read against the stored body as it stands, that row is recognised as no
// message at all, so nothing records which Dispatch message it is and the Agents page draws its
// stored copy beside it (INFRA-1026).
test("in a Legion pane the row of a message embedding a picture is recognised without its markdown", async () => {
  const body =
    "What colour is this picture? ![blue](dispatch://agent/ses_claude/artifact/i3-blue96-png@v1)"
  const lines: { content: string; blocks?: unknown; eventId?: string }[] = []
  const mod = fakeMod({})
  const session = await harness({
    link: mod.link,
    accept: () => accepted(body),
    inbox: async (line) => {
      lines.push(line)
    },
  })
  try {
    expect(await session.router.route(frame(personSends(body)))).toBe(true)
    expect(lines.map((line) => line.content)).toEqual([body])

    session.observe({
      kind: "append",
      uuid: "row-picture",
      door: "prompt",
      origin: { kind: "user" },
      message: { type: "user", content: [{ type: "text", text: "What colour is this picture? " }] },
    })
    expect(session.state.messageOf("row-picture")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})
