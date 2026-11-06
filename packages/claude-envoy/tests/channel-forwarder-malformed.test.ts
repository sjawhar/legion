import { expect, test } from "bun:test"
import { connect } from "nats"
import { type ChannelForwarder, createChannelForwarder } from "../src/channel-forwarder"
import { FakeNatsServer } from "./fake-nats-server"

const THREAD = "notifications.github.acme-org.example-repo.issue.3.>"
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"

test("passes an unidentifiable envelope to the shared renderer instead of dropping it", async () => {
  // The broker binds in its constructor and `connect` opens a real connection, so both are built
  // inside the try: a throw past either leaves the finally to close it, rather than a bound port
  // and a nats.js reconnect loop that keeps `bun test` from exiting.
  let stopBroker: (() => Promise<void>) | undefined
  let forwarder: ChannelForwarder | undefined
  try {
    const broker = new FakeNatsServer()
    stopBroker = () => broker.stop()
    const connection = await connect({ servers: broker.url })
    const delivered = Promise.withResolvers<string>()
    forwarder = createChannelForwarder(connection, {
      deliver: async (message) => {
        delivered.resolve(new TextDecoder().decode(message.data))
        return true
      },
    })

    forwarder.follow(THREAD)
    await broker.until(() => broker.subscribed.includes(THREAD))
    broker.deliver(COMMENT, "not an Envoy envelope")

    expect(await delivered.promise).toBe("not an Envoy envelope")
  } finally {
    await forwarder?.close()
    await stopBroker?.()
  }
})
