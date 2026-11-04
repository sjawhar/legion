import { afterEach, expect, test } from "bun:test"
import { mkdtemp, rename, rm, stat } from "node:fs/promises"
import { request as httpRequest } from "node:http"
import { createServer, type Server } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  MOD_HEALTH_MARKER,
  MOD_POLL_HOLD_MS,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  type ModEvent,
  type ModRequest,
  type ModResult,
} from "../hooks/mod-protocol"
import { type ModLink, startModLink } from "../src/mod-link"

const scratch: string[] = []
const links: ModLink[] = []
afterEach(async () => {
  for (const link of links.splice(0)) await link.close()
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

/** A scratch directory to put sockets in, short enough that the path stays under the bound. */
async function directory(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "mod-"))
  scratch.push(dir)
  return dir
}

/** A link on `sessionId`, closed after the test. */
async function start(dir: string, sessionId: string): Promise<ModLink> {
  const link = await startModLink({ sessionId, env: { TMPDIR: dir } })
  links.push(link)
  return link
}

interface Answer {
  readonly status: number
  readonly body: unknown
}

/** One request over the socket, as the mod's `$.http.fetch({ socketPath })` makes it: a fresh
 *  connection each time, as the host's own client does while another request is in flight. */
function call(socketPath: string, method: string, path: string, body?: unknown): Promise<Answer> {
  const { promise, resolve, reject } = Promise.withResolvers<Answer>()
  const text = body === undefined ? undefined : JSON.stringify(body)
  const sent = httpRequest(
    {
      socketPath,
      path,
      method,
      headers:
        text === undefined
          ? { host: "claude-envoy" }
          : { host: "claude-envoy", "content-type": "application/json" },
    },
    (response) => {
      let answer = ""
      response.setEncoding("utf8")
      response.on("data", (chunk: string) => {
        answer += chunk
      })
      response.on("end", () => {
        try {
          resolve({ status: response.statusCode ?? 0, body: JSON.parse(answer) })
        } catch {
          resolve({ status: response.statusCode ?? 0, body: answer })
        }
      })
    },
  )
  sent.on("error", reject)
  sent.end(text)
  return promise
}

/** The requests a poll answered with. */
function polled(answer: Answer): readonly ModRequest[] {
  const { body } = answer
  if (typeof body !== "object" || body === null || !("requests" in body)) return []
  const { requests } = body
  return Array.isArray(requests) ? (requests as ModRequest[]) : []
}

/** The next `count` events the owner reads. */
async function readEvents(link: ModLink, count: number): Promise<ModEvent[]> {
  const read: ModEvent[] = []
  while (read.length < count) {
    const next = await link.events.next()
    if (next.done === true) break
    read.push(next.value)
  }
  return read
}

/** Wait until the link has a poll in hand, which `connected()` reports: the server's own signal,
 *  rather than a delay long enough to hope the poll arrived. */
async function untilPolled(link: ModLink): Promise<void> {
  while (!link.connected()) await Bun.sleep(5)
}

/** A socket file with nothing behind it, as a server killed before it could unlink one leaves: a
 *  listener is bound elsewhere and its socket renamed onto `path`, so the close that follows
 *  unlinks the name it was given and leaves this one standing. */
async function staleSocket(path: string): Promise<void> {
  const server: Server = createServer()
  const bound = `${path}.bound`
  const listening = Promise.withResolvers<void>()
  server.listen(bound, () => listening.resolve())
  await listening.promise
  await rename(bound, path)
  const closed = Promise.withResolvers<void>()
  server.close(() => closed.resolve())
  await closed.promise
}

/** The message a request rejects with, read as the request is made: a rejection nothing is
 *  listening for yet fails the test run on its own. */
function rejection(answered: Promise<unknown>): Promise<string> {
  return answered.then(
    (value) => `resolved with ${JSON.stringify(value)}`,
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
}

test("binds the path the mod computes, answers the liveness marker, and keeps the socket private", async () => {
  const dir = await directory()
  const link = await start(dir, "s1")
  expect(link.socketPath).toBe(join(dir, "claude-envoy-s1.sock"))
  expect(await call(link.socketPath, "GET", MOD_ROUTE_HEALTH)).toEqual({
    status: 200,
    body: { link: MOD_HEALTH_MARKER },
  })
  expect((await stat(link.socketPath)).mode & 0o777).toBe(0o600)
})

test("takes over a socket a dead server left behind", async () => {
  const dir = await directory()
  const path = join(dir, "claude-envoy-s2.sock")
  await staleSocket(path)
  expect((await stat(path)).isSocket()).toBe(true)
  const link = await start(dir, "s2")
  expect(link.socketPath).toBe(path)
  expect(await call(path, "GET", MOD_ROUTE_HEALTH)).toEqual({
    status: 200,
    body: { link: MOD_HEALTH_MARKER },
  })
})

test("refuses a socket a live link already answers, and leaves that link serving", async () => {
  const dir = await directory()
  const first = await start(dir, "s3")
  await expect(startModLink({ sessionId: "s3", env: { TMPDIR: dir } })).rejects.toThrow(
    `another claude-envoy server already serves this session at ${first.socketPath}`,
  )
  expect(await call(first.socketPath, "GET", MOD_ROUTE_HEALTH)).toEqual({
    status: 200,
    body: { link: MOD_HEALTH_MARKER },
  })
})

test("a request is handed to the next poll and resolves with the value the mod answers", async () => {
  const dir = await directory()
  const link = await start(dir, "s4")
  const answered = link.request("session.model", undefined)
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests).toHaveLength(1)
  const [request] = requests
  expect(request?.call).toBe("session.model")
  expect(request?.args).toBeUndefined()
  const result: ModResult = { id: request?.id ?? "", ok: true, value: "claude-opus-5" }
  expect(await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, { results: [result] })).toEqual({
    status: 200,
    body: { ok: true },
  })
  expect(await answered).toBe("claude-opus-5")
})

test("a request the mod could not make rejects with the mod's own words", async () => {
  const dir = await directory()
  const link = await start(dir, "s5")
  const answered = rejection(link.request("command.run", { command: "nope" }))
  const [request] = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(request?.args).toEqual({ command: "nope" })
  const result: ModResult = { id: request?.id ?? "", ok: false, error: "unknown command nope" }
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, { results: [result] })
  expect(await answered).toBe("the mod could not command.run: unknown command nope")
})

test("a request still unanswered when the link closes rejects", async () => {
  const dir = await directory()
  const link = await start(dir, "s6")
  const answered = rejection(link.request("config.list", undefined))
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests).toHaveLength(1)
  await link.close()
  expect(await answered).toBe("the mod link closed before the mod answered")
})

// The hold is wall-clock behaviour of the running server: the point of the test is that a real
// poll over a real socket comes back on its own, so the clock here is the one under test.
test("a poll with nothing to do answers an empty list once the hold is up, rather than hanging", async () => {
  const dir = await directory()
  const link = await start(dir, "s7")
  const startedAt = Date.now()
  const answer = await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  const held = Date.now() - startedAt
  expect(answer).toEqual({ status: 200, body: { requests: [] } })
  expect(held).toBeGreaterThanOrEqual(MOD_POLL_HOLD_MS - 250)
  expect(held).toBeLessThan(MOD_POLL_HOLD_MS * 2)
})

test("a request made while a poll is held is answered at once, not after the hold", async () => {
  const dir = await directory()
  const link = await start(dir, "s8")
  const startedAt = Date.now()
  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  const answered = link.request("command.list", undefined)
  const requests = polled(await poll)
  expect(Date.now() - startedAt).toBeLessThan(MOD_POLL_HOLD_MS - 1_000)
  expect(requests).toHaveLength(1)
  const [request] = requests
  expect(request?.call).toBe("command.list")
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: request?.id ?? "", ok: true, value: [] }],
  })
  expect(await answered).toEqual([])
})

test("the liveness probe is answered while a poll is held", async () => {
  const dir = await directory()
  const link = await start(dir, "s9")
  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  expect(await call(link.socketPath, "GET", MOD_ROUTE_HEALTH)).toEqual({
    status: 200,
    body: { link: MOD_HEALTH_MARKER },
  })
  expect(link.connected()).toBe(true)
  expect(polled(await poll)).toEqual([])
})

test("events reach the owner in the order the mod posted them", async () => {
  const dir = await directory()
  const link = await start(dir, "s10")
  const first: ModEvent[] = [
    { kind: "hello", sessionId: "s10", claudeVersion: "2.1.310", interactive: true },
    { kind: "turn-start", turnId: "t1" },
  ]
  const second: ModEvent[] = [
    {
      kind: "step",
      turnId: "t1",
      index: 0,
      model: "claude-opus-5",
      parts: [{ type: "text", text: "hi" }],
      done: false,
    },
    { kind: "turn-complete", turnId: "t1", stopReason: "answer" },
  ]
  expect(await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, { events: first })).toEqual({
    status: 200,
    body: { ok: true },
  })
  await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, { events: second })
  expect(await readEvents(link, 4)).toEqual([...first, ...second])
})

test("the owner reads the session id the mod reports and is told when a clear moves it", async () => {
  const dir = await directory()
  const link = await start(dir, "s11")
  const moves: string[] = []
  link.onSessionId((sessionId) => moves.push(sessionId))
  expect(link.sessionId()).toBeUndefined()
  await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, {
    events: [{ kind: "hello", sessionId: "s11", claudeVersion: "2.1.310", interactive: false }],
  })
  expect(link.sessionId()).toBe("s11")
  await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, {
    events: [{ kind: "session-id", sessionId: "after-clear" }],
  })
  expect(link.sessionId()).toBe("after-clear")
  expect(moves).toEqual(["s11", "after-clear"])
})

test("a link nothing has polled is not connected, and a closed one serves nothing at all", async () => {
  const dir = await directory()
  const link = await start(dir, "s12")
  expect(link.connected()).toBe(false)
  await link.close()
  expect(link.connected()).toBe(false)
  expect(await stat(link.socketPath).catch(() => undefined)).toBeUndefined()
  expect(await rejection(link.request("session.model", undefined))).toBe("the mod link is closed")
  // A second close is the shutdown path's: `stop()` and the `finally` both call it.
  await link.close()
})

test("a body that is not JSON is refused, and a route the mod does not speak is not found", async () => {
  const dir = await directory()
  const link = await start(dir, "s13")
  const broken = await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, "{ not json")
  expect(broken.status).toBe(400)
  const empty = await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, { nothing: true })
  expect(empty.status).toBe(400)
  expect((await call(link.socketPath, "GET", "/elsewhere")).status).toBe(404)
  expect((await call(link.socketPath, "POST", "/elsewhere", {})).status).toBe(404)
})
