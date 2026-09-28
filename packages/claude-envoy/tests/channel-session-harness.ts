/**
 * The fakes every channel-session test file drives `startChannelSession` with: a
 * NATS connection, an MCP notifier, an Envoy listener client that records its
 * calls, and the scratch state directory and session options that bind them.
 * Shared so the session's concerns can live in their own files
 * (`envoy-channel-server`, `envoy-channel-handoff`, `envoy-channel-registry`) as
 * the rest of this package's tests do, rather than one file per entry point.
 */
import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import type {
  ChannelBrokerMessage,
  ChannelForwarderConnection,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import type {
  ChannelNotification,
  ChannelNotifier,
  ChannelSessionOptions,
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
    this.closed = true
    return Promise.resolve()
  }

  close(): Promise<void> {
    this.closed = true
    return Promise.resolve()
  }

  emit(subject: string, raw: string, reply?: string): void {
    const queue = this.subscriptions.get(subject)
    if (queue === undefined) throw new Error(`no subscription for ${subject}`)
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
  "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
>

export function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

/**
 * Polls `condition` every 5 ms until it holds; fails naming `what` after 5 s.
 * Real time on purpose: the awaited conditions are filesystem writes the
 * server's own heartbeat performs, which no fake clock can advance.
 */
export async function waitFor(condition: () => Promise<boolean>, what: string): Promise<void> {
  const deadline = Date.now() + 5_000
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`)
    await Bun.sleep(5)
  }
}

export async function scratchState(): Promise<string> {
  return mkdtemp(join(tmpdir(), "claude-envoy-state-"))
}

export function noInterest(): Interest {
  return {
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [directSubject],
  }
}

/** A client that records every call it sees, in order, into `calls`. */
export function recordingClient(calls: string[], overrides: Partial<FakeClient> = {}): FakeClient {
  return {
    subscribe: async (input) => {
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      return noInterest()
    },
    unsubscribe: async (input) => {
      calls.push(`unsubscribe ${input.sessionID} ${input.topics.join(",")}`)
    },
    unregisterSession: async (sessionID) => {
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
    ...overrides,
  }
}

export function sessionOptions(
  identity: SessionIdentity,
  stateDirectory: string,
  extra: Partial<ChannelSessionOptions> = {},
): ChannelSessionOptions {
  return {
    identity,
    connection: new FakeNats(),
    notifier: new FakeNotifier(),
    client: recordingClient([]),
    heartbeatMs: 60_000,
    stateDirectory,
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
