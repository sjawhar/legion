import { expect, test } from "bun:test"
import { agentSubject } from "@legion/contracts"
import {
  createThreadForwarder,
  type ForwardedMessage,
  type ForwarderConnection,
  type TopicSubscription,
} from "../src/thread-forwarder"

interface Queue {
  readonly pattern: string
  readonly buffer: ForwardedMessage[]
  waiter: ((message: ForwardedMessage | null) => void) | null
  closed: boolean
}

/** An in-memory NATS stand-in: exact and `>`-suffixed subject matching, per-subscription async queues. */
class FakeNats implements ForwarderConnection {
  readonly published: ForwardedMessage[] = []
  readonly unsubscribed: string[] = []
  drained = false
  private readonly queues: Queue[] = []
  private publishWaiters: Array<() => void> = []

  subscribe(pattern: string): TopicSubscription {
    const queue: Queue = { pattern, buffer: [], waiter: null, closed: false }
    this.queues.push(queue)
    const next = (): Promise<ForwardedMessage | null> => {
      const buffered = queue.buffer.shift()
      if (buffered) return Promise.resolve(buffered)
      if (queue.closed) return Promise.resolve(null)
      const { promise, resolve } = Promise.withResolvers<ForwardedMessage | null>()
      queue.waiter = resolve
      return promise
    }
    return {
      unsubscribe: () => {
        this.unsubscribed.push(pattern)
        queue.closed = true
        queue.waiter?.(null)
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
    this.published.push({ subject, data })
    const waiters = this.publishWaiters
    this.publishWaiters = []
    for (const wake of waiters) wake()
  }

  drain(): Promise<void> {
    this.drained = true
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
        waiter(message)
      } else {
        queue.buffer.push(message)
      }
    }
  }
}

function matches(pattern: string, subject: string): boolean {
  if (pattern.endsWith(".>")) return subject.startsWith(pattern.slice(0, -1))
  return pattern === subject
}

const decode = (data: Uint8Array): string => new TextDecoder().decode(data)

test("republishes thread-topic envelopes verbatim on the session's agent subject", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.3.>")
  const envelope = '{"payload_summary":"Sami answered: option A","topic":"…"}'

  // when
  const published = nats.nextPublish()
  nats.emit("notifications.github.acme-org.example-repo.issue.3.comment", envelope)
  await published

  // then
  expect(nats.published.map((m) => [m.subject, decode(m.data)])).toEqual([
    [agentSubject("ses_claude"), envelope],
  ])
})

test("following the same topic twice opens one subscription", () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.3.>")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.3.>")
  expect(forwarder.topics()).toEqual(["notifications.github.acme-org.example-repo.issue.3.>"])
})

test("unfollowing a topic stops forwarding it; unfollowing nothing stops everything", async () => {
  // given
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.3.>")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.4.>")

  // when
  await forwarder.unfollow(["notifications.github.acme-org.example-repo.issue.3.>"])
  const published = nats.nextPublish()
  nats.emit("notifications.github.acme-org.example-repo.issue.3.comment", "late")
  nats.emit("notifications.github.acme-org.example-repo.issue.4.comment", "still-followed")
  await published

  // then
  expect(nats.unsubscribed).toEqual(["notifications.github.acme-org.example-repo.issue.3.>"])
  expect(nats.published.map((m) => decode(m.data))).toEqual(["still-followed"])
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

  // when: the self-addressed message is queued first, so it is handled before the one we await
  const published = nats.nextPublish()
  nats.emit(agentSubject("ses_claude"), "already here")
  nats.emit(agentSubject("ses_other"), "peer traffic")
  await published

  // then
  expect(nats.published.map((m) => decode(m.data))).toEqual(["peer traffic"])
})

test("close unsubscribes everything and drains the connection", async () => {
  const nats = new FakeNats()
  const forwarder = createThreadForwarder(nats, "ses_claude")
  forwarder.follow("notifications.github.acme-org.example-repo.issue.3.>")

  await forwarder.close()

  expect(nats.unsubscribed).toEqual(["notifications.github.acme-org.example-repo.issue.3.>"])
  expect(nats.drained).toBe(true)
  expect(forwarder.topics()).toEqual([])
})
