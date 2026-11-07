import { afterEach, expect, test } from "bun:test"
import { chmod, mkdir, mkdtemp, rename, rm, stat } from "node:fs/promises"
import { request as httpRequest } from "node:http"
import { createServer, type Server } from "node:net"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import {
  MOD_HEALTH_MARKER,
  MOD_INSTANCE_HEADER,
  MOD_POLL_HOLD_MS,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_STATUS_TAKEN,
  type ModEvent,
  type ModRequest,
  type ModResult,
  modSocketPath,
} from "../hooks/mod-protocol"
import { type ModLink, startModLink } from "../src/mod-link"
import { type ModSession, modSession } from "../src/mod-session"

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

/** A link on `sessionId`, closed after the test; `warn` takes what it says about a second mod. */
async function start(
  dir: string,
  sessionId: string,
  warn?: (text: string) => void,
): Promise<ModLink> {
  const link = await startModLink({
    sessionId,
    env: { XDG_RUNTIME_DIR: dir },
    ...(warn === undefined ? {} : { warn }),
  })
  links.push(link)
  return link
}

interface Answer {
  readonly status: number
  readonly body: unknown
}

/** One request over the socket, as the mod's `$.http.fetch({ socketPath })` makes it: a fresh
 *  connection each time, as the host's own client does while another request is in flight.
 *  `instance` is the mod instance it is sent as; a call that names none claims nothing. */
function call(
  socketPath: string,
  method: string,
  path: string,
  body?: unknown,
  instance?: string,
): Promise<Answer> {
  const { promise, resolve, reject } = Promise.withResolvers<Answer>()
  const text = body === undefined ? undefined : JSON.stringify(body)
  const sent = httpRequest(
    {
      socketPath,
      path,
      method,
      headers: {
        host: "claude-envoy",
        ...(text === undefined ? {} : { "content-type": "application/json" }),
        ...(instance === undefined ? {} : { [MOD_INSTANCE_HEADER]: instance }),
      },
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
  expect(link.socketPath.startsWith(join(dir, "claude-envoy/"))).toBe(true)
  expect(modSocketPath("s1", { XDG_RUNTIME_DIR: dir })).toBe(link.socketPath)
  expect(await call(link.socketPath, "GET", MOD_ROUTE_HEALTH)).toEqual({
    status: 200,
    body: { link: MOD_HEALTH_MARKER },
  })
  expect((await stat(link.socketPath)).mode & 0o777).toBe(0o600)
  // The socket is only as private as the directory it sits in: another user who can write that
  // directory binds the path first and reads the conversation (INFRA-1026 review).
  expect((await stat(dirname(link.socketPath))).mode & 0o777).toBe(0o700)
})

test("refuses a socket directory another user could use, rather than binding in it", async () => {
  const dir = await directory()
  const path = modSocketPath("open", { XDG_RUNTIME_DIR: dir })
  if (path === undefined) throw new Error("the session names no mod socket")
  await mkdir(dirname(path), { recursive: true, mode: 0o700 })
  await chmod(dirname(path), 0o755)
  await expect(startModLink({ sessionId: "open", env: { XDG_RUNTIME_DIR: dir } })).rejects.toThrow(
    `${dirname(path)} is mode 755`,
  )
  expect(await stat(path).catch(() => undefined)).toBeUndefined()
})

test("takes over a socket a dead server left behind", async () => {
  const dir = await directory()
  const path = modSocketPath("s2", { XDG_RUNTIME_DIR: dir })
  if (path === undefined) throw new Error("the session names no mod socket")
  await mkdir(dirname(path), { recursive: true, mode: 0o700 })
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
  await expect(startModLink({ sessionId: "s3", env: { XDG_RUNTIME_DIR: dir } })).rejects.toThrow(
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

// Two Claude Code processes can boot on one session id - `claude -c`, and `claude --resume <id>`
// in a second terminal - and one server serving both would put two conversations in one live
// view and hand a person's Send to whichever mod polled first (INFRA-1026 review).
test("one link serves one mod: a second instance is refused and said so, and the first goes on", async () => {
  const dir = await directory()
  const refusals: string[] = []
  const link = await start(dir, "s14", (text) => refusals.push(text))
  const hello = await call(
    link.socketPath,
    "POST",
    MOD_ROUTE_EVENTS,
    { events: [{ kind: "hello", sessionId: "s14", claudeVersion: "2.1.310", interactive: true }] },
    "mod-one",
  )
  expect(hello.status).toBe(200)

  const second = await call(
    link.socketPath,
    "POST",
    MOD_ROUTE_EVENTS,
    {
      events: [
        { kind: "hello", sessionId: "s14-older", claudeVersion: "2.1.310", interactive: true },
      ],
    },
    "mod-two",
  )
  expect(second.status).toBe(MOD_STATUS_TAKEN)
  const refusedPoll = await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}, "mod-two")
  expect(refusedPoll.status).toBe(MOD_STATUS_TAKEN)
  // The live view stays with the process that got here first, and the refusal is on the record.
  expect(link.sessionId()).toBe("s14")
  expect(refusals.join("\n")).toContain("mod-two")

  const answered = link.request("session.model", undefined)
  const [request] = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}, "mod-one"))
  await call(
    link.socketPath,
    "POST",
    MOD_ROUTE_RESULTS,
    { results: [{ id: request?.id ?? "", ok: true, value: "claude-opus-5" }] },
    "mod-one",
  )
  expect(await answered).toBe("claude-opus-5")
})

// A request waits in the queue while no mod polls, which is exactly where one that timed out was
// left: a mod that connected later ran it, so an old `session.message` the listener had already
// sent again reached the agent twice (INFRA-1026 review).
test("a request that timed out is not handed to a mod that polls afterwards", async () => {
  const dir = await directory()
  const link = await start(dir, "s15")
  const gone = link.request(
    "session.message",
    {
      content: [{ type: "text", text: "a message from Dispatch" }],
      message: { kind: "card", text: "a message from Dispatch" },
    },
    { timeoutMs: 20 },
  )
  expect(await rejection(gone)).toBe("the mod did not answer session.message within 20 ms")

  const answered = link.request("session.model", undefined)
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map(({ call: name }) => name)).toEqual(["session.model"])
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: requests[0]?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")
})

// The mod says hello once per Claude Code process, so a server started again inside that process
// hears only the poll, and a session that waited for a hello advertised nothing for up to a
// heartbeat (INFRA-1026 review).
test("the link reports the first poll of a mod that never said hello, and only the first", async () => {
  const dir = await directory()
  const link = await start(dir, "s16")
  let rises = 0
  link.onConnected(() => {
    rises += 1
  })
  expect(link.connected()).toBe(false)

  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  expect(rises).toBe(1)
  const answered = link.request("session.model", undefined)
  const [request] = polled(await poll)
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: request?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")

  // A poll that arrives while the link already counts as connected is no rising edge of its own.
  const answeredAgain = link.request("session.model", undefined)
  const [again] = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: again?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answeredAgain).toBe("claude-opus-5")
  expect(rises).toBe(1)
})

// The link drops what its queue holds when it closes: a mod of the next server to bind this
// socket is handed nothing of the session that went (INFRA-1026 review, round ten).
test("the queue goes with the link, so nothing of it waits for a later mod", async () => {
  const dir = await directory()
  const link = await start(dir, "s22")
  const gone = rejection(link.request("session.model", undefined))
  await link.close()
  expect(await gone).toBe("the mod link closed before the mod answered")

  const next = await start(dir, "s22")
  const answered = next.request("session.message", {
    content: [{ text: "the new session's own", type: "text" }],
    message: { kind: "card", text: "the new session's own" },
  })
  const requests = polled(await call(next.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map((request) => request.call)).toEqual(["session.message"])
  await call(next.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: requests[0]?.id ?? "", ok: true, value: { as: "row", uuid: "row-1" } }],
  })
  expect(await answered).toEqual({ as: "row", uuid: "row-1" })
})

// What a card does over this link, end to end (`../src/mod-session`, `show`): the frame is
// answered on the row the mod answers with and on nothing else, so these drive the real link
// rather than a stand-in for it.

/** The card the session is shown, as the server renders one, and a second of its own. */
const CARD = "Dispatch message from alice: the migration is approved."
const SECOND_CARD = "Dispatch message from bob: the rollout is paused."
const BLOCKS = [{ text: CARD, type: "text" }]

/** The session the router drives over `link`, with the live view's own pump beside it: it reads
 *  the mod's events and hands each one over (`startAgentStream`, `../src/agent-stream-runner`). */
function sessionOn(link: ModLink, showMs: number): ModSession {
  const session = modSession({ link, showMs, warn: () => undefined })
  void (async () => {
    for await (const event of link.events) session.observe(event)
  })()
  return session
}

/** What the mod answers one `session.message` with: the row the message became. */
function wrote(request: ModRequest, uuid: string): ModResult {
  return { id: request.id, ok: true, value: { as: "prompt", uuid } }
}

// Repro `R-A` (INFRA-1026 review, round eleven): a mod that is not polling - stopped,
// reconnecting after a server restart, or never started at all - answers nothing, so the card's
// frame goes unanswered and Envoy sends the card again. Nothing of the first attempt may be left
// for a later poll: the mod that polls next would be handed both it and the resend's, and the
// one message would reach the engine twice over two event ids it could not tell apart.
test("a card no mod answered is written once, by its resend, when the mod finally polls", async () => {
  const dir = await directory()
  const link = await start(dir, "s23")
  const session = sessionOn(link, 300)

  await expect(session.show({ content: BLOCKS, eventId: "evt-1", text: CARD })).rejects.toThrow(
    "the mod did not answer session.message within 300 ms",
  )

  // The frame nothing answered comes again, and this time a mod is there.
  const showing = session.show({ content: BLOCKS, eventId: "evt-1", text: CARD })
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map((request) => request.call)).toEqual(["session.message"])
  const asked = requests[0]
  if (asked === undefined) throw new Error("the resend was handed over as no request")
  // The id the mod writes once by, carried on both copies (`../hooks/mod.ts`, `writeOnce`).
  expect((asked.args as { eventId?: string }).eventId).toBe("evt-1")
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [wrote(asked, "prompt-row-1")],
  })
  await showing

  // Nothing of the first attempt is left for a later poll: the mod's next one is held, and the
  // first work it is handed is the call made after it.
  const next = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  const model = link.request("session.model", undefined)
  const held = polled(await next)
  expect(held.map((request) => request.call)).toEqual(["session.model"])
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: held[0]?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await model).toBe("claude-opus-5")
})

// A session whose mod never connects - Claude Code before 2.1.310, or a mod that gave up its
// connect backoff - takes nothing, and every frame must come again: a card answered as delivered
// here would be a message lost, where R12 allows only a message shown twice (INFRA-1026, check
// k7's second clause).
test("no card is answered as delivered while no mod has ever connected", async () => {
  const dir = await directory()
  const link = await start(dir, "s25")
  const session = sessionOn(link, 150)

  for (const text of [CARD, SECOND_CARD, CARD]) {
    await expect(session.show({ content: BLOCKS, text })).rejects.toThrow(
      "the mod did not answer session.message within 150 ms",
    )
  }

  // Nor does the queue hold them: the mod that starts now is held with nothing to do, and the
  // first work it is handed is the call made after it. A queue that had kept the three would
  // answer this poll with them instead, and write cards Envoy has long since sent again.
  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  const model = link.request("session.model", undefined)
  const held = polled(await poll)
  expect(held.map((request) => request.call)).toEqual(["session.model"])
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: held[0]?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await model).toBe("claude-opus-5")
})
