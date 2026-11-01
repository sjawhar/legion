import { afterEach, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { createServer, type Server, type Socket } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { createInboxNotifier } from "../src/legion-inbox"

const scratch: string[] = []
const servers: Server[] = []
afterEach(async () => {
  for (const server of servers.splice(0)) server.close()
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

/** A stand-in for the bridge's inbox (`claudebridge/inbox.go`): it records each line and answers
 * it with `answer`. */
async function inbox(answer: (delivery: { id: string; content: string }) => object): Promise<{
  readonly socketPath: string
  readonly lines: { id: string; content: string }[]
}> {
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  const socketPath = join(dir, "inbox.sock")
  const lines: { id: string; content: string }[] = []
  const server = createServer((socket: Socket) => {
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const delivery = JSON.parse(buffered.slice(0, end))
        buffered = buffered.slice(end + 1)
        lines.push(delivery)
        socket.write(`${JSON.stringify(answer(delivery))}\n`)
      }
    })
  })
  servers.push(server)
  await new Promise<void>((listening) => server.listen(socketPath, listening))
  return { socketPath, lines }
}

function notification(content: string, eventID?: string) {
  return {
    method: "notifications/claude/channel" as const,
    params: { content, meta: eventID === undefined ? {} : { event_id: eventID } },
  }
}

test("writes each delivery as one line keyed by its event id and resolves on the bridge's ok", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  const notifier = createInboxNotifier(socketPath)
  await notifier.notification(notification("envoy:\n  id: evt-1", "evt-1"))
  await notifier.notification(notification("envoy:\n  id: evt-2", "evt-2"))
  expect(lines).toEqual([
    { id: "evt-1", content: "envoy:\n  id: evt-1" },
    { id: "evt-2", content: "envoy:\n  id: evt-2" },
  ])
})

test("a delivery without an event id still gets an id of its own", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  await createInboxNotifier(socketPath).notification(notification("plain"))
  expect(lines).toHaveLength(1)
  expect(lines[0]?.id).not.toBe("")
})

test("rejects a delivery the bridge refuses, naming its error", async () => {
  const { socketPath } = await inbox(({ id }) => ({ id, error: "the bridge is stopping" }))
  await expect(
    createInboxNotifier(socketPath).notification(notification("x", "evt-3")),
  ).rejects.toThrow("the Legion inbox refused delivery evt-3: the bridge is stopping")
})

test("rejects when no bridge listens at the socket", async () => {
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  await expect(
    createInboxNotifier(join(dir, "absent.sock")).notification(notification("x", "evt-4")),
  ).rejects.toThrow("the Legion inbox")
})
