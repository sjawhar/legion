import { randomUUID } from "node:crypto"
import { chmod, lstat, mkdir, stat, unlink } from "node:fs/promises"
import {
  createServer,
  request as httpRequest,
  type IncomingMessage,
  type Server,
  type ServerResponse,
} from "node:http"
import { dirname } from "node:path"
import {
  MOD_HEALTH_MARKER,
  MOD_INSTANCE_HEADER,
  MOD_POLL_HOLD_MS,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_STATUS_TAKEN,
  MOD_URL_BASE,
  type ModAckBody,
  type ModCallArguments,
  type ModCallName,
  type ModCallResults,
  type ModEvent,
  type ModHealthBody,
  type ModRequest,
  type ModRequestsBody,
  type ModResult,
  type ModResultValue,
  resolveModSocketPath,
} from "../hooks/mod-protocol"

/** How long a `request()` waits for the mod's answer when the caller names no bound. A `$` call
 *  of the mod's is the engine's own work, and a `session.message` can wait for a turn already
 *  running before the engine takes its prompt — 8.5 s, 11.2 s and 12.4 s in the submits
 *  recorded on 2.1.310 — so the caller of one passes a bound of its own
 *  (`./mod-session.ts`, `SHOW_TIMEOUT_MS`). */
const MOD_REQUEST_TIMEOUT_MS = 30_000

/** How long the bind's probe waits for a server that may hold the path. Long enough for a live
 *  server to answer a request it serves out of memory, short enough not to stall the MCP server's
 *  start on a socket file nothing is behind. */
const MOD_PROBE_TIMEOUT_MS = 1_000

/** The most events held for an owner that is not reading them. One session's events are small and
 *  the live view (`./agent-stream-runner`) consumes them as they arrive; past this the oldest go,
 *  since the newest frame is the one a live view needs. */
const MOD_EVENT_BUFFER = 2_048

/** The `Host` of every request on the socket: over a Unix socket the host names nothing, and both
 *  sides use the link's own name so a stray request is recognisable. */
const MOD_HOST = new URL(MOD_URL_BASE).host

/** The link to this session's Claude Code mod: what it said, and what it can be asked to do. */
export interface ModLink {
  /** The socket this link is bound to. */
  readonly socketPath: string
  /** Everything the mod has sent, in the order it sent it. One consumer: each event goes to the
   *  first `next()` that asks for it. Events sent before the owner starts reading are held, the
   *  oldest dropped past 2048; the iterator ends when the link closes. */
  readonly events: AsyncIterableIterator<ModEvent>
  /** Ask the mod to make one call, and resolve with its answer. Rejects with the mod's own error
   *  when the call failed, when `timeoutMs` passes with no answer, and when the link closes
   *  first. A request whose own bound is up leaves the queue with it, so a mod that connects an
   *  hour later is handed no work nobody is waiting for (INFRA-1026 review).
   *
   *  Nothing here reports a request handed to a mod. A hand-out is a write into a poll's
   *  response, which a SIGSTOPped process never reads, so it proves nothing about the session
   *  (INFRA-1026 review, round eleven, repro `R-A`). What the session did with a message is the
   *  row the mod answers with, and a request the mod never answered is one its caller leaves
   *  unanswered too: Envoy sends the frame again, and the mod writes the message once
   *  (`../hooks/mod.ts`, `writeOnce`). */
  readonly request: <N extends ModCallName>(
    call: N,
    args: ModCallArguments[N],
    options?: { readonly timeoutMs?: number },
  ) => Promise<ModCallResults[N]>
  /** The session id the mod last reported: its boot id, then whatever a `/clear` moved it to.
   *  Undefined until the mod says hello. */
  readonly sessionId: () => string | undefined
  /** Called whenever that id changes, the first report included. */
  readonly onSessionId: (listener: (sessionId: string) => void) => void
  /** Whether a mod is talking to this link now: true while a poll is held, and for one hold
   *  after the mod's last poll or event, so a mod that stopped stops counting within about ten
   *  seconds. A session's delivery modes are read off this (`sessionCapabilities`), and the
   *  mod's first event - its hello - is as good a proof that it is there as its first poll. */
  readonly connected: () => boolean
  /** Called when a poll or an event arrives while this link did not count as connected: the
   *  rising edge of `connected()`. The mod sends its hello once per Claude Code process, so a
   *  server Claude Code starts again inside that process hears nothing else from a mod that is
   *  already there, and neither does a listener added after the hello landed (INFRA-1026
   *  review). There is no falling edge: `connected()` goes false on its own, about ten seconds
   *  after the mod's last poll. */
  readonly onConnected: (listener: () => void) => void
  /** Stop serving, unlink the socket, end `events` and reject every unanswered request. */
  readonly close: () => Promise<void>
}

/** What `startModLink` needs: whose session this is, and the environment the socket path is
 *  resolved against (the same rule the mod resolves it by, in `../hooks/mod-protocol`). */
export interface ModLinkOptions {
  readonly sessionId: string
  readonly env: Readonly<Record<string, string | undefined>>
  /** Where a refused second mod is reported; this process's stderr when the caller names none. */
  readonly warn?: (text: string) => void
}

/** One poll of the mod's, held open until there is work for it or the hold is up. */
interface HeldPoll {
  readonly answer: (requests: readonly ModRequest[]) => void
  readonly timer: ReturnType<typeof setTimeout>
}

/** One request made of the mod and not yet answered. */
interface PendingRequest {
  readonly call: ModCallName
  readonly settle: PromiseWithResolvers<ModResultValue>
  readonly timer: ReturnType<typeof setTimeout>
}

/** A one-consumer stream of what the mod sent. */
interface ModEventStream {
  readonly iterator: AsyncIterableIterator<ModEvent>
  readonly push: (event: ModEvent) => void
  readonly end: () => void
}

/** Whether a live link answers `GET /healthz` at `socketPath` with its marker: the one way to
 *  tell a socket another server owns from one a dead server left behind. */
function probeHealth(socketPath: string, timeoutMs: number): Promise<boolean> {
  const { promise, resolve } = Promise.withResolvers<boolean>()
  const probe = httpRequest(
    {
      socketPath,
      path: MOD_ROUTE_HEALTH,
      method: "GET",
      headers: { host: MOD_HOST },
      timeout: timeoutMs,
    },
    (response: IncomingMessage) => {
      let body = ""
      response.setEncoding("utf8")
      response.on("data", (chunk: string) => {
        body += chunk
      })
      response.on("end", () => {
        if (response.statusCode !== 200) {
          resolve(false)
          return
        }
        let parsed: unknown
        try {
          parsed = JSON.parse(body)
        } catch {
          resolve(false)
          return
        }
        if (typeof parsed !== "object" || parsed === null || !("link" in parsed)) {
          resolve(false)
          return
        }
        resolve(parsed.link === MOD_HEALTH_MARKER)
      })
    },
  )
  probe.on("error", () => resolve(false))
  probe.on("timeout", () => {
    probe.destroy()
    resolve(false)
  })
  probe.end()
  return promise
}

/** The events of a `POST /events` body, or undefined when the body is not one. The mod is this
 *  socket's only writer, so an item that carries a `kind` is the event it says it is. */
function readEvents(body: unknown): readonly ModEvent[] | undefined {
  if (typeof body !== "object" || body === null || !("events" in body)) return undefined
  const events = body.events
  if (!Array.isArray(events)) return undefined
  return events.filter(
    (event: unknown): event is ModEvent =>
      typeof event === "object" &&
      event !== null &&
      "kind" in event &&
      typeof event.kind === "string",
  )
}

/** The results of a `POST /results` body, or undefined when the body is not one. */
function readResults(body: unknown): readonly ModResult[] | undefined {
  if (typeof body !== "object" || body === null || !("results" in body)) return undefined
  const results = body.results
  if (!Array.isArray(results)) return undefined
  return results.filter(
    (result: unknown): result is ModResult =>
      typeof result === "object" &&
      result !== null &&
      "id" in result &&
      typeof result.id === "string" &&
      "ok" in result &&
      typeof result.ok === "boolean",
  )
}

/** A stream whose `push` buffers while nobody reads and whose `end` closes the iterator for
 *  good: the owner reads the mod's events from it at its own pace. */
function createEventStream(limit: number): ModEventStream {
  const buffered: ModEvent[] = []
  let waiting: ((result: IteratorResult<ModEvent>) => void) | undefined
  let ended = false
  const iterator: AsyncIterableIterator<ModEvent> = {
    [Symbol.asyncIterator]() {
      return iterator
    },
    next(): Promise<IteratorResult<ModEvent>> {
      const next = buffered.shift()
      if (next !== undefined) return Promise.resolve({ value: next, done: false })
      if (ended) return Promise.resolve({ value: undefined, done: true })
      const { promise, resolve } = Promise.withResolvers<IteratorResult<ModEvent>>()
      waiting = resolve
      return promise
    },
  }
  return {
    iterator,
    push(event: ModEvent): void {
      if (ended) return
      const settle = waiting
      if (settle !== undefined) {
        waiting = undefined
        settle({ value: event, done: false })
        return
      }
      buffered.push(event)
      if (buffered.length > limit) buffered.splice(0, buffered.length - limit)
    },
    end(): void {
      if (ended) return
      ended = true
      const settle = waiting
      if (settle !== undefined) {
        waiting = undefined
        settle({ value: undefined, done: true })
      }
    },
  }
}

/** Whether something is at `path` at all. */
async function exists(path: string): Promise<boolean> {
  try {
    await stat(path)
    return true
  } catch {
    return false
  }
}

/** The directory the socket sits in, made when it is missing and checked when it is not: a
 *  directory of this user's own, `0700`, and no symbolic link to one elsewhere. Anyone who can
 *  write this directory can bind the socket before this server does and then read the
 *  conversation and submit prompts as the person, so a directory that is open to anyone else is
 *  refused rather than tightened: whatever left it open may have left something in it too
 *  (INFRA-1026 review). `mkdir` can only drop bits of the `0700` it is given, so a directory
 *  this server made is never the one that is refused. */
async function privateDirectory(path: string): Promise<void> {
  await mkdir(path, { recursive: true, mode: 0o700 })
  const directory = await lstat(path)
  if (!directory.isDirectory()) {
    throw new Error(`${path} is not a directory of its own, so the socket has no private home`)
  }
  // Windows has no `getuid`, and no Unix socket for the mod either; a host without one has no
  // owner to compare and the mode below is the whole check.
  const user = process.getuid?.()
  if (user !== undefined && directory.uid !== user) {
    throw new Error(`${path} belongs to uid ${directory.uid}, not to this user (uid ${user})`)
  }
  if ((directory.mode & 0o077) !== 0) {
    throw new Error(
      `${path} is mode ${(directory.mode & 0o777).toString(8)}, which lets another user into this session's socket; chmod 700 it`,
    )
  }
}

/** Serve `socketPath`, inside a directory only this user can use, with a stale socket unlinked;
 *  refuses a path a live link already answers. */
async function bind(server: Server, socketPath: string): Promise<void> {
  await privateDirectory(dirname(socketPath))
  if (await exists(socketPath)) {
    if (await probeHealth(socketPath, MOD_PROBE_TIMEOUT_MS)) {
      throw new Error(`another claude-envoy server already serves this session at ${socketPath}`)
    }
    await unlink(socketPath).catch(() => undefined)
  }
  const { promise, resolve, reject } = Promise.withResolvers<void>()
  server.once("error", reject)
  server.listen(socketPath, () => {
    server.removeListener("error", reject)
    resolve()
  })
  await promise
  await chmod(socketPath, 0o600)
}

/**
 * Bind this session's mod socket and serve the four routes the mod speaks (`MOD_ROUTE_*` in
 * `../hooks/mod-protocol`): its events in, its poll for work and its answers, and the liveness
 * probe both the mod and the next server use.
 *
 * The path is the one the mod computes from the same rule, so neither side guesses, and it sits
 * in a directory of this user's own (`privateDirectory`). A socket that is already there is
 * probed: a live link answering the marker owns this session and the bind is refused; anything
 * else is a socket a dead server left, unlinked and bound over. The bound socket is the user's
 * alone (`0600`).
 *
 * One link serves one mod: the first mod instance to name itself in a post holds it, and every
 * other is refused (`MOD_STATUS_TAKEN`), so two Claude Code processes that booted on one session
 * id do not share a live view or a person's Send (INFRA-1026 review).
 *
 * Rejects when the session names no socket path or no private directory for it, when another
 * server owns it, and when the bind itself fails. Nothing here stops the MCP server: the caller
 * says on stderr what it could not do and carries on.
 */
export async function startModLink(options: ModLinkOptions): Promise<ModLink> {
  const resolved = resolveModSocketPath(options.sessionId, options.env)
  if (resolved.path === undefined) {
    throw new Error(`no socket path for this session: ${resolved.refusal}`)
  }
  const socketPath = resolved.path

  const stream = createEventStream(MOD_EVENT_BUFFER)
  const queued: ModRequest[] = []
  const held = new Set<HeldPoll>()
  const pending = new Map<string, PendingRequest>()
  const sessionIdListeners: ((sessionId: string) => void)[] = []
  const connectedListeners: (() => void)[] = []
  const warn =
    options.warn ??
    ((text: string): void => {
      process.stderr.write(`claude-envoy: ${text}\n`)
    })
  /** The mod instance this link serves: the first one to name itself in a post. */
  let instance: string | undefined
  let reportedSessionId: string | undefined
  let lastPollAt = 0
  let closed = false

  const isConnected = (): boolean =>
    !closed && (held.size > 0 || Date.now() - lastPollAt < MOD_POLL_HOLD_MS * 2)

  /** One post of the mod's arrived. A post that lands while the link counts as disconnected is
   *  the rising edge `onConnected` reports, and the only word a server hears from a mod that
   *  said its hello to an earlier server in the same Claude Code process. */
  const heard = (): void => {
    const before = isConnected()
    lastPollAt = Date.now()
    if (!before) for (const listener of connectedListeners) listener()
  }

  const reportSessionId = (sessionId: string): void => {
    if (sessionId === reportedSessionId) return
    reportedSessionId = sessionId
    for (const listener of sessionIdListeners) listener(sessionId)
  }

  /** Hand the work waiting to the poll that is held, if one is. */
  const wake = (): void => {
    const [poll] = held
    if (poll === undefined || queued.length === 0) return
    held.delete(poll)
    clearTimeout(poll.timer)
    poll.answer(queued.splice(0, queued.length))
  }

  const answerJson = (response: ServerResponse, status: number, body: unknown): void => {
    const text = JSON.stringify(body)
    response.writeHead(status, {
      "content-type": "application/json",
      "content-length": Buffer.byteLength(text),
    })
    response.end(text)
  }

  const readBody = (incoming: IncomingMessage): Promise<string> => {
    const { promise, resolve, reject } = Promise.withResolvers<string>()
    let body = ""
    incoming.setEncoding("utf8")
    incoming.on("data", (chunk: string) => {
      body += chunk
    })
    incoming.on("end", () => resolve(body))
    incoming.on("error", reject)
    return promise
  }

  const serve = async (incoming: IncomingMessage, response: ServerResponse): Promise<void> => {
    const route = (incoming.url ?? "").split("?")[0]
    if (route === MOD_ROUTE_HEALTH && incoming.method === "GET") {
      answerJson(response, 200, { link: MOD_HEALTH_MARKER } satisfies ModHealthBody)
      return
    }
    if (
      incoming.method !== "POST" ||
      (route !== MOD_ROUTE_EVENTS && route !== MOD_ROUTE_RESULTS && route !== MOD_ROUTE_REQUESTS)
    ) {
      answerJson(response, 404, { error: `no route ${incoming.method ?? ""} ${route ?? ""}` })
      return
    }
    const named = incoming.headers[MOD_INSTANCE_HEADER]
    const mod = typeof named === "string" ? named : undefined
    if (mod !== undefined) {
      instance ??= mod
      if (instance !== mod) {
        warn(
          `a second Claude Code mod (${mod}) posted to a link mod ${instance} holds, so two processes booted on one session id; it is refused and stops (${socketPath})`,
        )
        answerJson(response, MOD_STATUS_TAKEN, {
          error: `this session's link belongs to mod ${instance}`,
        })
        return
      }
    }
    const text = await readBody(incoming)
    let body: unknown
    try {
      body = text.length === 0 ? {} : JSON.parse(text)
    } catch {
      answerJson(response, 400, { error: "the body is not JSON" })
      return
    }
    if (route === MOD_ROUTE_EVENTS) {
      heard()
      const events = readEvents(body)
      if (events === undefined) {
        answerJson(response, 400, { error: "the body carries no events" })
        return
      }
      for (const event of events) {
        if (event.kind === "hello" || event.kind === "session-id") reportSessionId(event.sessionId)
        stream.push(event)
      }
      answerJson(response, 200, { ok: true } satisfies ModAckBody)
      return
    }
    if (route === MOD_ROUTE_RESULTS) {
      const results = readResults(body)
      if (results === undefined) {
        answerJson(response, 400, { error: "the body carries no results" })
        return
      }
      for (const result of results) {
        const waiting = pending.get(result.id)
        if (waiting === undefined) continue
        pending.delete(result.id)
        clearTimeout(waiting.timer)
        if (result.ok) waiting.settle.resolve(result.value)
        else waiting.settle.reject(new Error(`the mod could not ${waiting.call}: ${result.error}`))
      }
      answerJson(response, 200, { ok: true } satisfies ModAckBody)
      return
    }
    heard()
    if (queued.length > 0 || closed) {
      answerJson(response, 200, {
        requests: queued.splice(0, queued.length),
      } satisfies ModRequestsBody)
      return
    }
    // Held, not blocked: this handler returns and the server goes on serving, so a `GET /healthz`
    // on another connection is answered while the mod waits here.
    const poll: HeldPoll = {
      answer: (requests) => answerJson(response, 200, { requests } satisfies ModRequestsBody),
      timer: setTimeout(() => {
        held.delete(poll)
        answerJson(response, 200, { requests: [] } satisfies ModRequestsBody)
      }, MOD_POLL_HOLD_MS),
    }
    poll.timer.unref()
    held.add(poll)
    incoming.on("close", () => {
      if (held.delete(poll)) clearTimeout(poll.timer)
    })
  }

  const server = createServer((incoming, response) => {
    void serve(incoming, response).catch(() => {
      if (response.headersSent) response.end()
      else answerJson(response, 500, { error: "the mod link failed" })
    })
  })
  await bind(server, socketPath)

  const close = async (): Promise<void> => {
    if (closed) return
    closed = true
    for (const waiting of pending.values()) {
      clearTimeout(waiting.timer)
      waiting.settle.reject(new Error("the mod link closed before the mod answered"))
    }
    pending.clear()
    // The queue goes with them: every request it held has just been rejected, and a mod that
    // posts one last poll as this server goes down is handed none of it. A request no mod took
    // leaves the queue when its own bound is up or when the link drops, and never sits there
    // waiting for a mod that polls an hour later (INFRA-1026 review).
    queued.length = 0
    for (const poll of held) {
      clearTimeout(poll.timer)
      poll.answer([])
    }
    held.clear()
    stream.end()
    server.closeAllConnections()
    const { promise, resolve } = Promise.withResolvers<void>()
    server.close(() => resolve())
    await promise
    await unlink(socketPath).catch(() => undefined)
  }

  return {
    socketPath,
    events: stream.iterator,
    request<N extends ModCallName>(
      call: N,
      args: ModCallArguments[N],
      requestOptions?: { readonly timeoutMs?: number },
    ): Promise<ModCallResults[N]> {
      if (closed) return Promise.reject(new Error("the mod link is closed"))
      const timeoutMs = requestOptions?.timeoutMs ?? MOD_REQUEST_TIMEOUT_MS
      const id = randomUUID()
      const settle = Promise.withResolvers<ModResultValue>()
      // One arm of ModRequest per call name, a correlation the compiler cannot follow through `N`.
      const request = { id, call, args } as ModRequest
      const timer = setTimeout(() => {
        pending.delete(id)
        // Out of the queue with it. Queued is where a request waits while no mod is polling,
        // and a mod that connects an hour later would otherwise run work whose caller gave up
        // long ago; in a session whose mod never connects the queue would grow for the life of
        // the process (INFRA-1026 review).
        const waiting = queued.indexOf(request)
        if (waiting !== -1) queued.splice(waiting, 1)
        settle.reject(new Error(`the mod did not answer ${call} within ${timeoutMs} ms`))
      }, timeoutMs)
      timer.unref()
      pending.set(id, { call, settle, timer })
      queued.push(request)
      wake()
      // The mod answers the call it was asked for, so the value is that call's own.
      return settle.promise as Promise<ModCallResults[N]>
    },
    sessionId: () => reportedSessionId,
    onSessionId(listener: (sessionId: string) => void): void {
      sessionIdListeners.push(listener)
    },
    connected: isConnected,
    onConnected(listener: () => void): void {
      connectedListeners.push(listener)
    },
    close,
  }
}
