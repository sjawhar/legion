import { expect, spyOn, test } from "bun:test"
import { agentSubject } from "@legion/contracts"
import { connect } from "nats"
import {
  createThreadForwarder,
  type ForwardedMessage,
  type ForwarderConnection,
  type TopicSubscription,
} from "../src/thread-forwarder"
import { FakeNatsServer } from "./fake-nats-server"

interface Queue {
  readonly pattern: string
  readonly buffer: ForwardedMessage[]
  waiter: PromiseWithResolvers<ForwardedMessage | null> | null
  closed: boolean
  failure: Error | null
}

/** An in-memory NATS stand-in: exact and `>`-suffixed subject matching, per-subscription async queues. */
class FakeNats implements ForwarderConnection {
  readonly published: ForwardedMessage[] = []
  readonly unsubscribed: string[] = []
  drained = false
  closed = false
  /** When set, `drain()` never resolves — the broker is unreachable and the flush waits forever. */
  drainHangs = false
  /** Thrown by the next `publish()` call, once. */
  nextPublishError: Error | null = null
  private readonly queues: Queue[] = []
  private publishWaiters: Array<() => void> = []

  subscribe(pattern: string): TopicSubscription {
    const queue: Queue = { pattern, buffer: [], waiter: null, closed: false, failure: null }
    this.queues.push(queue)
    const next = (): Promise<ForwardedMessage | null> => {
      const buffered = queue.buffer.shift()
      if (buffered) return Promise.resolve(buffered)
      if (queue.failure) return Promise.reject(queue.failure)
      if (queue.closed) return Promise.resolve(null)
      queue.waiter = Promise.withResolvers<ForwardedMessage | null>()
      return queue.waiter.promise
    }
    return {
      unsubscribe: () => {
        this.unsubscribed.push(pattern)
        queue.closed = true
        queue.waiter?.resolve(null)
        queue.waiter = null
      },
      async *[Symbol.asyncIterator]() {
        for (;;) {
          const message = await next()
          if (message === null) return
          yield message
        }
      },
    }
  }

  publish(subject: string, data: Uint8Array): void {
    if (this.nextPublishError) {
      const error = this.nextPublishError
      this.nextPublishError = null
      throw error
    }
    this.published.push({ subject, data })
    const waiters = this.publishWaiters
    this.publishWaiters = []
    for (const wake of waiters) wake()
  }

  isClosed(): boolean {
    return this.closed
  }

  drain(): Promise<void> {
    if (this.drainHangs) return Promise.withResolvers<void>().promise
    this.drained = true
    this.closed = true
    return Promise.resolve()
  }

  close(): Promise<void> {
    this.closed = true
    return Promise.resolve()
  }

  /** Resolves when the forwarder next publishes; the signal tests await instead of a timer. */
  nextPublish(): Promise<void> {
    const { promise, resolve } = Promise.withResolvers<void>()
    this.publishWaiters.push(resolve)
    return promise
  }

  /** Deliver a message to every open subscription whose pattern matches. */
  emit(subject: string, text: string): void {
    const message = { subject, data: new TextEncoder().encode(text) }
    for (const queue of this.queues) {
      if (queue.closed || !matches(queue.pattern, subject)) continue
      if (queue.waiter) {
        const waiter = queue.waiter
        queue.waiter = null
        waiter.resolve(message)
      } else {
        queue.buffer.push(message)
      }
    }
  }

  /** End every subscription on `pattern` with an error, the way nats.js reports a permissions violation. */
  fail(pattern: string, error: Error): void {
    for (const queue of this.queues) {
      if (queue.pattern !== pattern || queue.closed) continue
      queue.failure = error
      queue.waiter?.reject(error)
      queue.waiter = null
    }
  }
}

function matches(pattern: string, subject: string): boolean {
  if (pattern.endsWith(".>")) return subject.startsWith(pattern.slice(0, -1))
  return pattern === subject
}

const decode = (data: Uint8Array): string => new TextDecoder().decode(data)

/** The fields of an Envoy envelope the forwarder reads; the rest is opaque to it. */
const envelope = (dedupeKey: string, summary = "Sami answered: option A"): string =>
  JSON.stringify({ dedupe_key: dedupeKey, payload_summary: summary })

/**
 * Every pending forward step is a chain of microtasks (the fake resolves
 * queues synchronously), so one macrotask boundary lets all of them finish;
 * afterwards `published` is final for everything emitted so far.
 */
function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

function captureStderr(): { readonly lines: string[]; restore(): void } {
  const lines: string[] = []
  const spy = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    lines.push(String(chunk))
    return true
  })
  return { lines, restore: () => spy.mockRestore() }
}

const THREAD = "notifications.github.acme-org.example-repo.issue.3.>"
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"

test("republishes thread-topic envelopes verbatim on the session's agent subject", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  const reply = envelope("github.comment.1")

  // when
  const published = nats.nextPublish()
  nats.emit(COMMENT, reply)
  await published

  // then
  expect(nats.published.map((m) => [m.subject, decode(m.data)])).toEqual([
    [agentSubject("ses_claude"), reply],
  ])
})

test("records and skips a dispatch echo for the originating session", async () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  const echo = JSON.stringify({
    dedupe_key: "github.dispatch.1",
    payload_summary: "Keep the conversation open?",
    payload: JSON.stringify({ dispatch_session: "ses_claude" }),
  })
  const laterCopy = JSON.stringify({
    dedupe_key: "github.dispatch.1",
    payload_summary: "Keep the conversation open?",
    payload: JSON.stringify({ dispatch_session: "ses_other" }),
  })

  nats.emit(COMMENT, echo)
  nats.emit(COMMENT, laterCopy)
  await settled()

  expect(nats.published).toEqual([])
})

test("following the same topic twice opens one subscription", () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  forwarder.follow(THREAD)
  expect(forwarder.topics()).toEqual([THREAD])
})

test("republishes an envelope once however often it arrives", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)

  // when: the broker redelivers the same envelope, then a new one follows
  nats.emit(COMMENT, envelope("github.comment.1"))
  nats.emit(COMMENT, envelope("github.comment.1"))
  nats.emit(COMMENT, envelope("github.comment.2"))
  await settled()

  // then
  expect(nats.published.map((m) => decode(m.data))).toEqual([
    envelope("github.comment.1"),
    envelope("github.comment.2"),
  ])
})

test("an envelope matched by two followed patterns reaches the inbox once", async () => {
  // given: a repo-wide subscription and a thread on that repo, both covering the comment
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.github.acme-org.example-repo.>")
  forwarder.follow(THREAD)

  // when
  nats.emit(COMMENT, envelope("github.comment.1"))
  await settled()

  // then
  expect(nats.published.map((m) => [m.subject, decode(m.data)])).toEqual([
    [agentSubject("ses_claude"), envelope("github.comment.1")],
  ])
})

test("falls back to event_id when an envelope has no dedupe_key", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  const byEventId = JSON.stringify({ event_id: "evt-1", payload_summary: "hello" })

  // when
  nats.emit(COMMENT, byEventId)
  nats.emit(COMMENT, byEventId)
  await settled()

  // then
  expect(nats.published.map((m) => decode(m.data))).toEqual([byEventId])
})

test("drops a message that carries no dedupe_key or event_id and says so on stderr", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  const stderr = captureStderr()
  try {
    // when: not JSON, then JSON without an identity, then a real envelope
    nats.emit(COMMENT, "not an envelope")
    nats.emit(COMMENT, JSON.stringify({ payload_summary: "no identity" }))
    nats.emit(COMMENT, envelope("github.comment.1"))
    await settled()

    // then
    expect(nats.published.map((m) => decode(m.data))).toEqual([envelope("github.comment.1")])
    expect(stderr.lines).toEqual([
      `envoy-mcp: dropped a message on ${COMMENT}: no dedupe_key or event_id\n`,
      `envoy-mcp: dropped a message on ${COMMENT}: no dedupe_key or event_id\n`,
    ])
  } finally {
    stderr.restore()
  }
})

test("unfollowing a topic stops forwarding it; unfollowing nothing stops everything", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  forwarder.follow("notifications.github.acme-org.example-repo.issue.4.>")

  // when
  await forwarder.unfollow([THREAD])
  const published = nats.nextPublish()
  nats.emit(COMMENT, envelope("late"))
  nats.emit(
    "notifications.github.acme-org.example-repo.issue.4.comment",
    envelope("still-followed"),
  )
  await published

  // then
  expect(nats.unsubscribed).toEqual([THREAD])
  expect(nats.published.map((m) => decode(m.data))).toEqual([envelope("still-followed")])
  expect(forwarder.topics()).toEqual(["notifications.github.acme-org.example-repo.issue.4.>"])

  // when
  await forwarder.unfollow([])

  // then
  expect(forwarder.topics()).toEqual([])
  expect(nats.unsubscribed).toHaveLength(2)
})

test("never republishes the agent subject to itself", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.agent.>")

  // when
  nats.emit(agentSubject("ses_claude"), envelope("already here"))
  nats.emit(agentSubject("ses_other"), envelope("peer traffic"))
  await settled()

  // then
  expect(nats.published.map((m) => decode(m.data))).toEqual([envelope("peer traffic")])
})

test("an envelope first seen on the inbox is not relayed back when a peer echoes it", async () => {
  // given: two sessions following notifications.agent.> cover each other's inbox; this is one of them
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.agent.>")

  // when: a message lands on this inbox, the peer republishes it on its own, and a fresh one follows
  nats.emit(agentSubject("ses_claude"), envelope("direct-1"))
  nats.emit(agentSubject("ses_peer"), envelope("direct-1"))
  nats.emit(agentSubject("ses_peer"), envelope("direct-2"))
  await settled()

  // then: the peer's copy is not relayed back, so the pair cannot ping-pong; new traffic still flows
  expect(nats.published.map((m) => [m.subject, decode(m.data)])).toEqual([
    [agentSubject("ses_claude"), envelope("direct-2")],
  ])
})

test("a message that cannot be republished is reported and does not stop the topic", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  nats.nextPublishError = new Error("CONNECTION_DRAINING")
  const stderr = captureStderr()
  try {
    // when
    nats.emit(COMMENT, envelope("github.comment.1"))
    nats.emit(COMMENT, envelope("github.comment.2"))
    await settled()

    // then
    expect(nats.published.map((m) => decode(m.data))).toEqual([envelope("github.comment.2")])
    expect(stderr.lines).toEqual([
      `envoy-mcp: could not forward a message on ${COMMENT} to ${agentSubject("ses_claude")} — CONNECTION_DRAINING\n`,
    ])
  } finally {
    stderr.restore()
  }
})

test("a subscription that fails is reported; other topics keep flowing and close still resolves", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  forwarder.follow("notifications.github.acme-org.example-repo.issue.4.>")
  const stderr = captureStderr()
  try {
    // when
    nats.fail(THREAD, new Error("PERMISSIONS_VIOLATION"))
    const published = nats.nextPublish()
    nats.emit("notifications.github.acme-org.example-repo.issue.4.comment", envelope("other"))
    await published
    await forwarder.close()

    // then
    expect(nats.published.map((m) => decode(m.data))).toEqual([envelope("other")])
    expect(stderr.lines).toEqual([
      `envoy-mcp: forwarding ${THREAD} stopped — PERMISSIONS_VIOLATION\n`,
    ])
    expect(nats.drained).toBe(true)
  } finally {
    stderr.restore()
  }
})

test("close unsubscribes everything and drains the connection", async () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)

  await forwarder.close()

  expect(nats.unsubscribed).toEqual([THREAD])
  expect(nats.drained).toBe(true)
  expect(forwarder.topics()).toEqual([])
  expect(forwarder.isClosed()).toBe(true)
})

test("close does not wait forever for a drain the broker cannot answer", async () => {
  // given: the broker is unreachable, so nats.js's flush would never see its PONG
  const nats = new FakeNats()
  nats.drainHangs = true
  const forwarder = createThreadForwarder(nats, "ses_claude", { drainTimeoutMs: 10 })
  forwarder.follow(THREAD)

  // when
  await forwarder.close()

  // then: the connection is closed outright, ending the reconnect loop that would keep the process alive
  expect(nats.unsubscribed).toEqual([THREAD])
  expect(nats.drained).toBe(false)
  expect(nats.closed).toBe(true)
})

test("close on a connection nats.js already closed neither drains nor rejects", async () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow(THREAD)
  nats.closed = true

  await forwarder.close()

  expect(nats.drained).toBe(false)
  expect(forwarder.isClosed()).toBe(true)
  expect(forwarder.topics()).toEqual([])
})

test("with the real nats client, close resolves while the broker is down and leaves the connection closed", async () => {
  // given: the connection is up, then the broker goes away, so nats.js is in its reconnect loop
  const broker = new FakeNatsServer()
  const connection = await connect({
    servers: broker.url,
    name: "claude-envoy-mcp-ses_claude",
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  })
  const forwarder = createThreadForwarder(connection, "ses_claude", { drainTimeoutMs: 50 })
  forwarder.follow(THREAD)
  await broker.until(() => broker.subscribed.includes(THREAD))
  const statuses = connection.status()
  broker.vanish()
  for await (const status of statuses) if (status.type === "disconnect") break

  // when
  await forwarder.close()

  // then: the drain could not complete (no PONG will come), so the connection was closed outright
  expect(connection.isClosed()).toBe(true)
  expect(forwarder.isClosed()).toBe(true)
})
