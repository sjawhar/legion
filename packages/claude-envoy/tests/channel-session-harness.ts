/**
 * The fakes every channel-session test file drives `startChannelSession` with: a
 * NATS connection, an MCP notifier, an Envoy listener client that records its
 * calls, and the scratch state directory and session options that bind them.
 * Shared so the session's concerns can live in their own files
 * (`envoy-channel-server`, `envoy-channel-handoff`, `envoy-channel-registry`) as
 * the rest of this package's tests do, rather than one file per entry point.
 */
import { randomBytes } from "node:crypto"
import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import type {
  EnvoyClient,
  ReplayInput,
  ReplayResult,
  SubscribeResult,
} from "@legion/envoy-client/transport"
import type {
  ChannelBrokerMessage,
  ChannelConnectionStatus,
  ChannelForwarderConnection,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import {
  CHANNEL_CAPABILITIES,
  type ChannelNotification,
  type ChannelNotifier,
  type ChannelSessionOptions,
} from "../src/envoy-channel-server"
import type { SessionIdentity } from "../src/session-identity"

interface Queue {
  readonly subject: string
  readonly messages: ChannelBrokerMessage[]
  waiter: PromiseWithResolvers<ChannelBrokerMessage | null> | undefined
  /** Set by unsubscribe: the iterator ends at its next pull, even when a message was mid-delivery. */
  closed: boolean
}

export class FakeNats implements ChannelForwarderConnection {
  readonly published: Array<{ readonly subject: string; readonly data: Uint8Array }> = []
  readonly order: string[] = []
  readonly unsubscribed: string[] = []
  readonly subscriptions = new Map<string, Queue>()
  closed = false
  private readonly statuses: ChannelConnectionStatus[] = []
  private statusWaiter: PromiseWithResolvers<void> | undefined

  constructor(readonly calls: string[] = []) {}

  subscribe(subject: string): ChannelTopicSubscription {
    this.calls.push(`nats.subscribe ${subject}`)
    const queue: Queue = { subject, messages: [], waiter: undefined, closed: false }
    this.subscriptions.set(subject, queue)
    return {
      unsubscribe: () => {
        this.calls.push(`nats.unsubscribe ${subject}`)
        this.unsubscribed.push(subject)
        this.subscriptions.delete(subject)
        queue.closed = true
        queue.waiter?.resolve(null)
      },
      async *[Symbol.asyncIterator]() {
        for (;;) {
          if (queue.closed) return
          const message = queue.messages.shift()
          if (message !== undefined) {
            yield message
            continue
          }
          const waiter = Promise.withResolvers<ChannelBrokerMessage | null>()
          queue.waiter = waiter
          const next = await waiter.promise
          queue.waiter = undefined
          if (next === null) return
          yield next
        }
      },
    }
  }

  publish(subject: string, data: Uint8Array): void {
    this.order.push("receipt")
    this.published.push({ subject, data })
  }

  /** When set, a flush never settles, as on a stalled connection. */
  stallFlush = false

  flush(): Promise<void> {
    return this.stallFlush ? new Promise<void>(() => undefined) : Promise.resolve()
  }

  isClosed(): boolean {
    return this.closed
  }

  drain(): Promise<void> {
    return this.close()
  }

  close(): Promise<void> {
    this.closed = true
    this.statusWaiter?.resolve()
    return Promise.resolve()
  }

  /** nats.js reports a status event: `reconnect`, say. */
  emitStatus(type: string): void {
    this.statuses.push({ type })
    this.statusWaiter?.resolve()
  }

  async *status(): AsyncIterable<ChannelConnectionStatus> {
    while (!this.closed) {
      const next = this.statuses.shift()
      if (next !== undefined) {
        yield next
        continue
      }
      this.statusWaiter = Promise.withResolvers<void>()
      await this.statusWaiter.promise
    }
  }

  emit(subject: string, raw: string, reply?: string): void {
    this.emitOn(subject, subject, raw, reply)
  }

  /** A message published on `subject` reaches the subscription `pattern`, which matches it. */
  emitOn(pattern: string, subject: string, raw: string, reply?: string): void {
    const queue = this.subscriptions.get(pattern)
    if (queue === undefined) throw new Error(`no subscription for ${pattern}`)
    const message = {
      subject,
      data: new TextEncoder().encode(raw),
      ...(reply === undefined ? {} : { reply }),
    }
    if (queue.waiter === undefined) {
      queue.messages.push(message)
      return
    }
    const waiter = queue.waiter
    queue.waiter = undefined
    waiter.resolve(message)
  }

  /**
   * The owning listener relays a stored message to `sessionID`'s relay subject (consumer
   * delivery), with the relay headers and a reply inbox of its own.
   */
  emitRelay(
    sessionID: string,
    frame: {
      readonly subject: string
      readonly seq: number
      readonly raw: string
      readonly reply: string
      readonly delivered?: number
      readonly storedAt?: string
      readonly streamID?: string
      readonly gap?: boolean
    },
  ): void {
    const subject = deliverSubject(sessionID)
    const queue = this.subscriptions.get(subject)
    if (queue === undefined) throw new Error(`no subscription for ${subject}`)
    const headers = new Map<string, string>([
      [RelayHeader.subject, frame.subject],
      [RelayHeader.streamSeq, String(frame.seq)],
      [RelayHeader.streamID, frame.streamID ?? "stream-1"],
      [RelayHeader.delivered, String(frame.delivered ?? 1)],
      [RelayHeader.storedAt, frame.storedAt ?? new Date().toISOString()],
      ...(frame.gap === true ? [[RelayHeader.gap, "true"] as [string, string]] : []),
    ])
    const message: ChannelBrokerMessage = {
      subject,
      data: new TextEncoder().encode(frame.raw),
      reply: frame.reply,
      headers: { get: (name) => headers.get(name) },
    }
    if (queue.waiter === undefined) {
      queue.messages.push(message)
      return
    }
    const waiter = queue.waiter
    queue.waiter = undefined
    waiter.resolve(message)
  }

  /** The bodies published on `subject` so far, decoded: a relay frame's replies on its inbox. */
  repliesOn(subject: string): string[] {
    return this.published
      .filter((entry) => entry.subject === subject)
      .map((entry) => new TextDecoder().decode(entry.data))
  }
}

export class FakeNotifier implements ChannelNotifier {
  readonly notifications: ChannelNotification[] = []

  constructor(readonly order: string[] = []) {}

  notification(notification: ChannelNotification): Promise<void> {
    this.order.push("notification")
    this.notifications.push(notification)
    return Promise.resolve()
  }
}

export type FakeClient = Pick<
  EnvoyClient,
  | "subscribe"
  | "unsubscribe"
  | "unregisterSession"
  | "setRole"
  | "getRole"
  | "getInterest"
  | "replay"
>

/** A listener whose stream holds nothing: every subject answers no message, its cursor at 0. */
export async function emptyReplay(input: ReplayInput): Promise<ReplayResult> {
  return {
    stream: { created: "2026-10-20T00:00:00Z", first_seq: 1, last_seq: 0 },
    subjects: input.subjects.map(({ subject }) => ({
      subject,
      messages: [],
      next_after: 0,
      more: false,
    })),
  }
}

export function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

/**
 * Polls `condition` every 5 ms until it holds; fails after 5 s naming `what`, followed by what
 * `state` reports at that moment (an `observedClient`'s `describe`, the test's own fakes).
 * Real time on purpose: the awaited conditions are filesystem writes the
 * server's own heartbeat performs, which no fake clock can advance. The
 * deadline is read from the monotonic clock, which `setSystemTime` leaves alone.
 *
 * The package's `test` script gives each test 60 s (`--timeout=60000`), above the longest chain of
 * waits one test makes: a wait that succeeds can take up to its 5 s, so a stuck wait names its step
 * only once every wait before it has had its time. The longest is eleven, counting the waits of
 * `boot` and `reconnect` (envoy-channel-replay.test.ts, "a stored frame the channel never takes is
 * named in one notice…"): 11 × 5 s, and 5 s more covers startup and shutdown. A test that chains
 * more raises it. Under bun's default of 5 s its own timeout fired first, and a stuck wait never
 * said which step it was.
 */
export async function waitFor(
  condition: () => Promise<boolean>,
  what: string,
  state?: () => string,
): Promise<void> {
  const deadline = performance.now() + 5_000
  while (!(await condition())) {
    if (performance.now() > deadline) {
      throw new Error(`timed out waiting for ${what}${state === undefined ? "" : ` (${state()})`}`)
    }
    await Bun.sleep(5)
  }
}

/**
 * `client` with a record of what the session asked of it, for a `waitFor` that times out to
 * report: how each role claim ended, the holder the last `getRole` answered, the calls still
 * awaited, and when the last registration began. The server keeps whether a heartbeat tick is in
 * flight to itself; from here a tick shows as its calls, since every tick starts by registering,
 * so a call still awaited is the tick stuck on it, and none awaited with no registration for
 * many heartbeats is a tick stuck between calls, or no tick starting at all.
 */
export function observedClient(client: FakeClient): {
  readonly client: FakeClient
  describe(): string
} {
  const claims: string[] = []
  let answeredHolder: string | undefined
  const awaited = new Set<{ readonly call: string; readonly at: number }>()
  let lastRegistration: number | undefined
  const observe = async <T>(call: string, run: () => Promise<T>): Promise<T> => {
    const entry = { call, at: Date.now() }
    awaited.add(entry)
    try {
      return await run()
    } finally {
      awaited.delete(entry)
    }
  }
  return {
    client: {
      subscribe: (input) => {
        lastRegistration = Date.now()
        return observe(`subscribe ${input.sessionID}`, () => client.subscribe(input))
      },
      unsubscribe: (input) =>
        observe(`unsubscribe ${input.sessionID}`, () => client.unsubscribe(input)),
      unregisterSession: (input) =>
        observe(`unregister ${input.sessionID}`, () => client.unregisterSession(input)),
      setRole: (input) =>
        observe(`setRole ${input.sessionID} ${input.role}`, async () => {
          try {
            const result = await client.setRole(input)
            claims.push(result.claimed ? "claimed" : "refused")
            return result
          } catch (error) {
            claims.push(`threw ${error instanceof Error ? error.message : String(error)}`)
            throw error
          }
        }),
      getRole: (role) =>
        observe(`getRole ${role}`, async () => {
          const info = await client.getRole(role)
          answeredHolder = info.holder
          return info
        }),
      getInterest: (sessionID) =>
        observe(`getInterest ${sessionID}`, () => client.getInterest(sessionID)),
      replay: (...args) => observe("replay", () => client.replay(...args)),
    },
    describe: () =>
      [
        `claims [${claims.join(", ")}]`,
        `getRole last answered ${answeredHolder === undefined ? "never" : `holder "${answeredHolder}"`}`,
        `calls awaited [${[...awaited].map(({ call, at }) => `${call} for ${Date.now() - at} ms`).join(", ")}]`,
        `last registration ${lastRegistration === undefined ? "never" : `${Date.now() - lastRegistration} ms ago`}`,
      ].join("; "),
  }
}

export async function scratchState(): Promise<string> {
  return mkdtemp(join(tmpdir(), "claude-envoy-state-"))
}

/** A registration answer with no interest and no consumer delivery, as a listener of an earlier
 *  release leaves it (`parseSubscribeAnswer` fills the core mode in). */
export function noInterest(): SubscribeResult {
  return {
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [directSubject],
    delivery: { mode: "core", reason: "not_answered" },
  }
}

/** A client that records every call it sees, in order, into `calls`; its stream holds nothing. */
export function recordingClient(calls: string[], overrides: Partial<FakeClient> = {}): FakeClient {
  return {
    subscribe: async (input) => {
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      return noInterest()
    },
    unsubscribe: async (input) => {
      calls.push(`unsubscribe ${input.sessionID} ${input.topics.join(",")}`)
    },
    unregisterSession: async ({ sessionID }) => {
      calls.push(`unregister ${sessionID}`)
    },
    setRole: async (input) => {
      calls.push(
        `setRole ${input.sessionID} ${input.role}${input.soft ? " soft" : ""}${input.previousSessionID === undefined ? "" : ` previous=${input.previousSessionID}`}`,
      )
      return { claimed: true, interest: noInterest() }
    },
    getRole: async (role) => ({ role, holder: "ses_claude", last_seen: 1 }),
    getInterest: async (sessionID) => {
      calls.push("getInterest")
      return {
        ...noInterest(),
        session_id: sessionID,
        topics: [`notifications.agent.${sessionID}`],
      }
    },
    replay: emptyReplay,
    ...overrides,
  }
}

/** Options binding the fakes, with the envoy.json the test's environment picks as they are built:
 * `isolatePaneEnvironment`'s scratch file in a file that calls it. Each call mints its own client
 * instance id, as each channel-server process does; a test that asserts it reads it back from the
 * registration the session sent. */
export function sessionOptions(
  identity: SessionIdentity,
  stateDirectory: string,
  extra: Partial<ChannelSessionOptions> = {},
): ChannelSessionOptions {
  return {
    identity,
    connection: new FakeNats(),
    notifier: new FakeNotifier(),
    capabilities: CHANNEL_CAPABILITIES,
    client: recordingClient([]),
    heartbeatMs: 60_000,
    configFile: sessionConfigFile("claude", process.env),
    stateDirectory,
    clientInstanceID: randomBytes(16).toString("hex"),
    pluginVersion: "source",
    ...extra,
  }
}

export const directSubject = "notifications.agent.ses_claude"
export const deliveryRaw = JSON.stringify({
  event_id: "evt-42",
  dedupe_key: "delivery-42",
  source: "agent",
  source_session: "ses_sender",
  topic: directSubject,
  issued_at: 1_760_000_000_000,
  urgency: "high",
  payload_summary: "Use the channel",
})
