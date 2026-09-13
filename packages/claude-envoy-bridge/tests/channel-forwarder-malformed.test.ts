import { expect, test } from "bun:test"
import { connect } from "nats"
import { createChannelForwarder } from "../src/channel-forwarder"
import { FakeNatsServer } from "./fake-nats-server"

const THREAD = "notifications.github.acme-org.example-repo.issue.3.>"
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"

test("passes an unidentifiable envelope to the shared renderer instead of dropping it", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const delivered = Promise.withResolvers<string>()
  const forwarder = createChannelForwarder(connection, {
    deliver: async (message) => delivered.resolve(new TextDecoder().decode(message.data)),
  })

  try {
    forwarder.follow(THREAD)
    await broker.until(() => broker.subscribed.includes(THREAD))
    broker.deliver(COMMENT, "not an Envoy envelope")

    expect(await delivered.promise).toBe("not an Envoy envelope")
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})
