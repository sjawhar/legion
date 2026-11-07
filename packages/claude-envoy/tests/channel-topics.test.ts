import { expect, test } from "bun:test"
import { connect } from "nats"
import { createChannelTopics, deliveryIdentity } from "../src/channel-topics"
import { FakeNatsServer } from "./fake-nats-server"

const THREAD = "notifications.github.acme-org.example-repo.issue.3.>"
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"

// A delivery's identity fields parse one by one: a field the shared rule refuses identifies
// nothing, and its valid sibling still recognises a repeat of the same event. What is done with
// the identity is the ledger's and the dedupe's (`@legion/envoy-client/delivery-ledger`); this is
// what this package reads out of a frame for them.
test("keeps each valid dedupe identity when a sibling identity field is empty", () => {
  const identityOf = (identity: Record<string, string>) => {
    const parsed = deliveryIdentity(
      JSON.stringify({ source: "github", topic: COMMENT, issued_at: 1, ...identity }),
    )
    return { event_id: parsed?.event_id, dedupe_key: parsed?.dedupe_key }
  }

  expect(identityOf({ event_id: "evt-valid-event", dedupe_key: "" })).toEqual({
    event_id: "evt-valid-event",
    dedupe_key: undefined,
  })
  expect(
    identityOf({ event_id: "", dedupe_key: "dispatch.valid-key", source: "dispatch" }),
  ).toEqual({ event_id: undefined, dedupe_key: "dispatch.valid-key" })
  // Neither field names an event: the frame identifies nothing, so no copy of it is a repeat.
  expect(identityOf({ event_id: "" })).toEqual({ event_id: undefined, dedupe_key: undefined })
  expect(identityOf({ event_id: "", dedupe_key: "", source: "dispatch" })).toEqual({
    event_id: undefined,
    dedupe_key: undefined,
  })
  expect(identityOf({})).toEqual({ event_id: undefined, dedupe_key: undefined })
  // A frame that is not JSON identifies nothing either, and is not an error.
  expect(deliveryIdentity("not an Envoy envelope")).toBeUndefined()
})

test("subscribes nothing for a followed topic, and closes the connection at shutdown", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const followed = createChannelTopics(connection)

  try {
    followed.follow(THREAD)
    // The session receives only what its own consumer's relay pushes, so a followed topic is a
    // registration entry and nothing more: this process opens no subscription for it.
    expect(followed.topics()).toEqual([
      "notifications.github.acme-org.example-repo.issue.3",
      THREAD,
    ])
    expect(broker.subscribed).toEqual([])

    await followed.close()
    expect(followed.topics()).toEqual([])
    expect(connection.isClosed()).toBe(true)
  } finally {
    await broker.stop()
  }
})

test("unfollow drops a topic from topics() at once", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const followed = createChannelTopics(connection)

  try {
    followed.follow(COMMENT)
    followed.unfollow([COMMENT])
    expect(followed.topics()).toEqual([])
  } finally {
    await followed.close()
    await broker.stop()
  }
})
