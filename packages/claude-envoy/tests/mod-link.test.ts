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
import { CardNotShown, type ModSession, modSession } from "../src/mod-session"

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
// left: a mod that connected later ran it, so an old `session.append` or `prompt.submit` the
// listener had already sent again reached the agent twice (INFRA-1026 review).
test("a request that timed out is not handed to a mod that polls afterwards", async () => {
  const dir = await directory()
  const link = await start(dir, "s15")
  const gone = link.request(
    "session.append",
    { message: { type: "user", content: [{ type: "text", text: "a message from Dispatch" }] } },
    { timeoutMs: 20 },
  )
  expect(await rejection(gone)).toBe("the mod did not answer session.append within 20 ms")

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

// Whether a mod took the request is the one thing that separates a `$.prompt.submit` which
// entered the session from one which never did: both leave the call pending, since a submit
// answers only at the end of the turn it starts, and a stopped Claude Code writes no row either
// (INFRA-1026, R12; `../src/mod-session`, `CardNotShown`). So the report is made when a mod is
// handed the request and at no other time: a caller that was never called knows the work is
// still in this process.
test("the link reports a request only when a polling mod is handed it", async () => {
  const dir = await directory()
  const link = await start(dir, "s17")
  let handed = 0
  const answered = link.request("session.model", undefined, {
    handed: () => {
      handed += 1
    },
  })
  // Nothing is polling, so the request is still in this process and nothing is reported.
  expect(handed).toBe(0)

  const [request] = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(handed).toBe(1)
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: request?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")
  // Once, for the one hand-out: the answer that follows is no second report.
  expect(handed).toBe(1)
})

test("a request handed straight to a held poll is reported at once", async () => {
  const dir = await directory()
  const link = await start(dir, "s18")
  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)

  let handed = 0
  const answered = link.request("session.model", undefined, {
    handed: () => {
      handed += 1
    },
  })
  expect(handed).toBe(1)
  const [request] = polled(await poll)
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: request?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")
})

// A request dropped from the queue unpolled reached no Claude Code at all, and the caller is
// never told otherwise: that is what has the listener's resend of a card go to the session
// (INFRA-1026, check k7's second clause).
test("a request that timed out unpolled is never reported handed", async () => {
  const dir = await directory()
  const link = await start(dir, "s19")
  let handed = 0
  const gone = link.request("session.model", undefined, {
    timeoutMs: 20,
    handed: () => {
      handed += 1
    },
  })
  expect(await rejection(gone)).toBe("the mod did not answer session.model within 20 ms")
  expect(handed).toBe(0)

  // Nor by the poll that comes afterwards, which is handed nothing.
  const answered = link.request("session.model", undefined)
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests).toHaveLength(1)
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: requests[0]?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")
  expect(handed).toBe(0)
})

// A caller whose own wait is up takes its request back out of the queue, rather than leaving it
// for a mod that polls an hour later: a `$.prompt.submit` has a day to answer in
// (`SUBMIT_TIMEOUT_MS`), and the card it carries has long since been sent again by then
// (INFRA-1026 review, round ten).
test("a withdrawn request leaves the queue, and a mod that polls later is handed nothing", async () => {
  const dir = await directory()
  const link = await start(dir, "s20")
  const withdrawal = new AbortController()
  let handed = 0
  const gone = link.request("session.model", undefined, {
    signal: withdrawal.signal,
    handed: () => {
      handed += 1
    },
  })
  withdrawal.abort()
  expect(await rejection(gone)).toBe("session.model was withdrawn before a mod took it")

  const answered = link.request("session.append", {
    message: { type: "user", content: [{ text: "still waited for", type: "text" }] },
  })
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map((request) => request.call)).toEqual(["session.append"])
  expect(handed).toBe(0)
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: requests[0]?.id ?? "", ok: true, value: { uuid: "row-1" } }],
  })
  expect(await answered).toEqual({ uuid: "row-1" })
})

// A request a mod already holds is the mod's to answer: the work left this process, and the
// caller giving up on its own wait does not take it back (`../src/mod-session`, `show`, which
// withdraws only what no mod was handed).
test("a request a mod already holds is not withdrawn", async () => {
  const dir = await directory()
  const link = await start(dir, "s21")
  const poll = call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {})
  await untilPolled(link)
  const withdrawal = new AbortController()
  const answered = link.request("session.model", undefined, { signal: withdrawal.signal })
  const [request] = polled(await poll)

  withdrawal.abort()
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: request?.id ?? "", ok: true, value: "claude-opus-5" }],
  })
  expect(await answered).toBe("claude-opus-5")
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
  const answered = next.request("session.append", {
    message: { type: "user", content: [{ text: "the new session's own", type: "text" }] },
  })
  const requests = polled(await call(next.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map((request) => request.call)).toEqual(["session.append"])
  await call(next.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: [{ id: requests[0]?.id ?? "", ok: true, value: { uuid: "row-1" } }],
  })
  expect(await answered).toEqual({ uuid: "row-1" })
})

// What a card does over this link, end to end (`../src/mod-session`, `show`): the session's idle
// path is the one that decides whether a card is submitted again, and it decides it on what this
// link reports, so these drive the real one rather than a stand-in for it.

/** The card the session is shown, as the server renders one, and a second of its own. */
const CARD = "Dispatch message from alice: the migration is approved."
const SECOND_CARD = "Dispatch message from bob: the rollout is paused."
const BLOCKS = [{ text: CARD, type: "text" }]

/** The session the router drives over `link`, with the live view's own pump beside it: it reads
 *  the mod's events and hands each one over (`startAgentStream`, `../src/agent-stream-runner`),
 *  which is how the row Claude Code writes for a prompt reaches the session. */
function sessionOn(link: ModLink, promptRowMs: number): ModSession {
  const session = modSession({ link, promptRowMs, warn: () => undefined })
  void (async () => {
    for await (const event of link.events) session.observe(event)
  })()
  return session
}

/** The row Claude Code writes for a prompt this plugin submitted, as 2.1.310 writes it: by the
 *  `prompt` door, under the plugin's origin, with Claude Code's own line in front of the text
 *  (`./fake-mod`, which sends the same row for a submit a fake mod took). */
function promptRowEvent(text: string): ModEvent {
  return {
    door: "prompt",
    kind: "append",
    message: {
      content: [{ text: `The claude-envoy plugin sent a message:\n${text}`, type: "text" }],
      role: "user",
      type: "user",
    },
    origin: { kind: "plugin", name: "claude-envoy" },
    uuid: "prompt-row-1",
  }
}

/** How `show` refused the card, as `./mod-session.test.ts` reads it. */
async function refusal(showing: Promise<void>): Promise<CardNotShown> {
  const error = await showing.then(
    () => undefined,
    (reason: unknown) => reason,
  )
  if (!(error instanceof CardNotShown)) {
    throw new Error(`show did not refuse the card: ${String(error)}`)
  }
  return error
}

// R12 (INFRA-1026), one time: a mod that is not polling - stopped, reconnecting after a server
// restart, or never started at all - leaves the card's submit in this link's queue, where the
// frame goes unanswered and Envoy sends the card again. The submit nobody took must be gone by
// then: left in the queue, the mod that polls next would be handed both it and the resend's, and
// the one card would reach the model twice.
test("a card no mod took is submitted once, by its resend, when the mod finally polls", async () => {
  const dir = await directory()
  const link = await start(dir, "s23")
  const session = sessionOn(link, 300)

  const refused = await refusal(session.show({ content: BLOCKS, text: CARD }))
  expect(refused.entered).toBe(false)

  // The frame nothing answered comes again, and the session is idle still.
  const showing = session.show({ content: BLOCKS, text: CARD })
  // The mod polls at last: the resend's submit is the one piece of work there is.
  const requests = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(requests.map((request) => request.call)).toEqual(["prompt.submit"])

  // Claude Code writes the prompt's row, which is what answers the card's frame.
  await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, { events: [promptRowEvent(CARD)] })
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

// A submit answers only at the end of the turn it starts, so a card that arrives while one is
// pending is a row of that turn: a second prompt would leave this process a whole turn later,
// and the row Claude Code writes for it could not arrive inside the bound the frame waits
// (INFRA-1026 review, round ten; `../src/mod-session`, `inTurn`).
test("a card that arrives while a submit is pending is a row, not a second prompt", async () => {
  const dir = await directory()
  const link = await start(dir, "s24")
  const session = sessionOn(link, 2_000)

  const first = session.show({ content: BLOCKS, text: CARD })
  const submits = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(submits.map((request) => request.call)).toEqual(["prompt.submit"])
  // The mod took it and is running the turn: it answers at that turn's end, and nothing here
  // answers it. The row proves the session has the card.
  await call(link.socketPath, "POST", MOD_ROUTE_EVENTS, { events: [promptRowEvent(CARD)] })
  await first

  const second = session.show({ content: BLOCKS, text: SECOND_CARD })
  const third = session.show({ content: BLOCKS, text: `${SECOND_CARD} again` })
  const rows = polled(await call(link.socketPath, "POST", MOD_ROUTE_REQUESTS, {}))
  expect(rows.map((request) => request.call)).toEqual(["session.append", "session.append"])
  await call(link.socketPath, "POST", MOD_ROUTE_RESULTS, {
    results: rows.map((request, at) => ({
      id: request.id,
      ok: true,
      value: { uuid: `row-${at}` },
    })),
  })
  await second
  await third
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
    const refused = await refusal(session.show({ content: BLOCKS, text }))
    expect(refused.entered).toBe(false)
  }

  // Nor does the queue hold them: the mod that starts now is held with nothing to do, and the
  // first work it is handed is the call made after it. A queue that had kept the three would
  // answer this poll with them instead, and submit cards Envoy has long since sent again.
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
