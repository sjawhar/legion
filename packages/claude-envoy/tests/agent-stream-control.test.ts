/**
 * The live view's control subject on a Claude Code session (`startAgentStream`,
 * `../src/envoy-channel-server`): what a viewer's `replay` is answered with, what a `watch` arms,
 * what a message that is neither does, and where both subjects go after a `/clear`.
 *
 * The mod is a fake here: the events it sends and the answers its `$` calls give are the two
 * things this exercises, and the real one is driven over a socket by `./mod-link.test.ts`.
 */
import { expect, test } from "bun:test"
import {
  AGENT_STREAM_PROTOCOL,
  type AgentStreamFrame,
  type AgentStreamReplay,
  agentStreamControlSubject,
  agentStreamFramesSubject,
} from "@legion/contracts"
import type {
  ModCallArguments,
  ModCallName,
  ModCallResults,
  ModEvent,
} from "../hooks/mod-protocol"
import { type AgentStreamRunner, startAgentStream } from "../src/envoy-channel-server"
import type { ModLink } from "../src/mod-link"
import { FakeNats } from "./channel-session-harness"

const SESSION = "0c3b6f6e-0000-4000-8000-000000000001"
const CLEARED = "0c3b6f6e-0000-4000-8000-000000000002"

const MODEL_ROW = {
  isLocked: false,
  key: "model",
  kind: "choice",
  label: "Model",
  options: ["default", "sonnet", "opus"],
  provider: { plugin: "engine", tier: "core" },
  value: "Default (recommended)",
} as const

/** Let the pump's iterator, the link's reader and the mod's answers run. Nothing under test
 *  waits on a timer here — every hand-off is a promise — so draining the microtask queue is the
 *  whole wait, and the suite never pays the clock for it. */
async function settle(): Promise<void> {
  for (let pass = 0; pass < 50; pass += 1) await Promise.resolve()
}

interface LinkHarness {
  readonly link: ModLink
  readonly push: (...events: readonly ModEvent[]) => void
  readonly moveSession: (sessionId: string) => void
  readonly calls: ModCallName[]
  readonly close: () => void
}

function fakeLink(): LinkHarness {
  const queued: ModEvent[] = []
  const listeners: ((sessionId: string) => void)[] = []
  const calls: ModCallName[] = []
  let waiter: PromiseWithResolvers<void> | undefined
  let ended = false
  let sessionId: string | undefined = SESSION

  const events = (async function* (): AsyncGenerator<ModEvent> {
    for (;;) {
      const event = queued.shift()
      if (event !== undefined) {
        yield event
        continue
      }
      if (ended) return
      waiter = Promise.withResolvers<void>()
      await waiter.promise
    }
  })()

  const answers: { [N in ModCallName]?: ModCallResults[N] } = {
    "command.list": [
      { description: "Compact the conversation", name: "compact", source: "builtin" },
      { description: "What Envoy knows", name: "claude-envoy:envoy", source: "plugin" },
    ],
    "config.list": [MODEL_ROW],
    "session.model": "claude-sonnet-5",
  }

  const link: ModLink = {
    close: async () => {
      ended = true
      waiter?.resolve()
    },
    connected: () => !ended,
    events,
    onSessionId: (listener) => void listeners.push(listener),
    request: <N extends ModCallName>(
      call: N,
      _args: ModCallArguments[N],
    ): Promise<ModCallResults[N]> => {
      calls.push(call)
      const answer = answers[call]
      if (answer === undefined) return Promise.reject(new Error(`no answer for ${call}`))
      return Promise.resolve(answer)
    },
    sessionId: () => sessionId,
    socketPath: "/scratch/claude-envoy-test.sock",
  }

  return {
    calls,
    close: () => {
      ended = true
      waiter?.resolve()
    },
    link,
    moveSession: (next) => {
      sessionId = next
      for (const listener of listeners) listener(next)
    },
    push: (...events_) => {
      queued.push(...events_)
      waiter?.resolve()
    },
  }
}

interface Running {
  readonly nats: FakeNats
  readonly mod: LinkHarness
  readonly runner: AgentStreamRunner
  readonly warnings: string[]
  /** Publish one control message and answer it on `reply`, then let the pump run. */
  readonly control: (session: string, body: unknown, reply?: string) => Promise<void>
  /** The replay answered on `reply`, or undefined when nothing was. */
  readonly answered: (reply: string) => AgentStreamReplay | undefined
  readonly frames: (session: string) => AgentStreamFrame[]
}

function running(): Running {
  const nats = new FakeNats()
  const mod = fakeLink()
  const warnings: string[] = []
  const runner = startAgentStream({
    connection: nats,
    followsSessionId: true,
    link: mod.link,
    sessionID: SESSION,
    warn: (text) => void warnings.push(text),
  })
  return {
    answered: (reply) => {
      const sent = nats.published.filter((message) => message.subject === reply).at(-1)
      return sent === undefined
        ? undefined
        : (JSON.parse(new TextDecoder().decode(sent.data)) as AgentStreamReplay)
    },
    control: async (session, body, reply) => {
      nats.emit(agentStreamControlSubject(session), JSON.stringify(body), reply)
      await settle()
    },
    frames: (session) =>
      nats.published
        .filter((message) => message.subject === agentStreamFramesSubject(session))
        .map((message) => JSON.parse(new TextDecoder().decode(message.data)) as AgentStreamFrame),
    mod,
    nats,
    runner,
    warnings,
  }
}

const replayRequest = { type: "replay", v: AGENT_STREAM_PROTOCOL } as const
const watchRequest = { type: "watch", v: AGENT_STREAM_PROTOCOL } as const

function prompt(uuid: string, text: string): ModEvent {
  return {
    door: "prompt",
    kind: "append",
    message: { content: [{ text, type: "text" }], role: "user", type: "user" },
    origin: { kind: "unclassified" },
    uuid,
  }
}

test("the session takes its control subject as it starts", () => {
  const live = running()
  expect([...live.nats.subscriptions.keys()]).toEqual([agentStreamControlSubject(SESSION)])
  live.runner.stop()
})

test("a replay before any viewer attached answers an empty history", async () => {
  const live = running()
  live.mod.push(prompt("row-1", "what is 391?"))
  await settle()

  await live.control(SESSION, replayRequest, "_INBOX.first")
  expect(live.answered("_INBOX.first")).toEqual({
    frames: [],
    session_id: SESSION,
    v: AGENT_STREAM_PROTOCOL,
  })
  live.runner.stop()
})

test("a watch arms the session, and the replay after it holds the conversation", async () => {
  const live = running()
  live.mod.push(prompt("row-1", "what is 391?"))
  await settle()

  await live.control(SESSION, watchRequest)
  await live.control(SESSION, replayRequest, "_INBOX.second")

  const replay = live.answered("_INBOX.second")
  expect(replay?.session_id).toBe(SESSION)
  const conversation = (replay?.frames ?? []).flatMap((frame) =>
    frame.kind === "message" ? [frame.message] : [],
  )
  expect(conversation).toHaveLength(1)
  expect(conversation[0]?.parts).toEqual([{ text: "what is 391?", type: "text" }])
  live.runner.stop()
})

test("an armed session publishes the model it runs and the commands it takes", async () => {
  const live = running()
  live.mod.push({ claudeVersion: "2.1.310", interactive: true, kind: "hello", sessionId: SESSION })
  await settle()
  await live.control(SESSION, watchRequest)

  const published = live.frames(SESSION)
  const settings = published.findLast((frame) => frame.kind === "settings")
  expect(settings?.kind === "settings" && settings.reported ? settings.model : undefined).toBe(
    "anthropic/claude-sonnet-5",
  )
  expect(
    settings?.kind === "settings" && settings.reported ? settings.models : undefined,
  ).toEqual(["anthropic/default", "anthropic/sonnet", "anthropic/opus"])
  const commands = published.findLast((frame) => frame.kind === "commands")
  expect(commands?.kind === "commands" ? commands.commands : undefined).toEqual([
    { description: "Compact the conversation", name: "compact", source: "builtin" },
    { description: "What Envoy knows", name: "claude-envoy:envoy", source: "extension" },
  ])
  expect(live.warnings).toEqual([])
  live.runner.stop()
})

test("a control message that is neither a replay nor a watch is ignored", async () => {
  const live = running()
  await live.control(SESSION, { type: "poke", v: AGENT_STREAM_PROTOCOL }, "_INBOX.poke")
  await live.control(SESSION, null, "_INBOX.null")
  live.nats.emit(agentStreamControlSubject(SESSION), "not json at all", "_INBOX.broken")
  await settle()

  expect(live.answered("_INBOX.poke")).toBeUndefined()
  expect(live.answered("_INBOX.null")).toBeUndefined()
  expect(live.answered("_INBOX.broken")).toBeUndefined()
  // The iterator survived all three, so a replay after them is still answered.
  await live.control(SESSION, replayRequest, "_INBOX.after")
  expect(live.answered("_INBOX.after")?.session_id).toBe(SESSION)
  live.runner.stop()
})

test("a clear moves the control subject, and the new id replays nothing from before it", async () => {
  const live = running()
  await live.control(SESSION, watchRequest)
  live.mod.push(prompt("row-1", "before the clear"))
  await settle()

  live.mod.moveSession(CLEARED)
  await settle()

  expect(live.nats.unsubscribed).toContain(agentStreamControlSubject(SESSION))
  expect([...live.nats.subscriptions.keys()]).toEqual([agentStreamControlSubject(CLEARED)])

  // The new conversation's own viewer arms it, and sees nothing of the one before.
  await live.control(CLEARED, watchRequest)
  await live.control(CLEARED, replayRequest, "_INBOX.cleared")
  const replay = live.answered("_INBOX.cleared")
  expect(replay?.session_id).toBe(CLEARED)
  expect(
    (replay?.frames ?? []).filter((frame) => frame.kind === "message" || frame.kind === "tool-result"),
  ).toEqual([])

  // The next turn is published under the new id.
  live.mod.push(prompt("row-2", "after the clear"))
  await settle()
  const conversation = live
    .frames(CLEARED)
    .flatMap((frame) => (frame.kind === "message" ? [frame.message] : []))
  expect(conversation.map((message) => message.parts)).toEqual([
    [{ text: "after the clear", type: "text" }],
  ])
  live.runner.stop()
})

test("a stopped runner answers nothing more", async () => {
  const live = running()
  await live.control(SESSION, watchRequest)
  live.runner.stop()
  expect(live.nats.unsubscribed).toContain(agentStreamControlSubject(SESSION))
  expect(live.nats.subscriptions.size).toBe(0)
})
