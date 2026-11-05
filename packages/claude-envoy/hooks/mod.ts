/**
 * The Claude Code mod this plugin loads through the `modules` key of `./hooks.json`: Claude Code
 * runs it from source, in an environment of its own, and it sees every hook of the session.
 *
 * It carries the session to this plugin's MCP server over the Unix socket both sides name the
 * same way (`./mod-protocol`): events one way, the server's requests the other. A later task
 * makes the Agents page's live view out of those events and routes a person's message back
 * through those requests; nothing of either is here.
 *
 * Three rules hold everywhere below:
 *
 *   - A hook always calls `next(e)` and returns its result. Every call on the engine and every
 *     post is wrapped, so a throw of the link's can never end a turn or a hook.
 *   - A mod that finds no server is silent: it connects over a bounded backoff and then stops for
 *     good, having changed nothing about the session. That is the release window, where a person
 *     has this mod beside the previously built server. It also has nowhere to complain to: the
 *     only surfaces a mod can write are the person's screen and the model's context, and this
 *     change adds to neither. The server says on its stderr what it could not do.
 *   - `$` is never passed, returned or read: Claude Code scans this module's source before it
 *     loads it and refuses one that spells `$` as anything but `$.noun.method(...)` at a call
 *     site (`claude plugin validate`). So the link below is handed a `ModHost`, one closure per
 *     call, built in the `session.start` hook where `$` is in scope.
 *
 * It imports nothing but `./mod-protocol`, which imports nothing at all: a mod can import neither
 * a `node:` builtin nor an npm package, and `$.http.fetch` is its only way out.
 */
import type {
  CommandInfo,
  CommandRunArgs,
  CommandRunResult,
  ConfigRow,
  ConfigSetArgs,
  ConfigSetResult,
  HttpInit,
  HttpResponse,
  ModelForkRequest,
  ModelForkResult,
  On,
  PromptSubmitArgs,
  PromptSubmitResult,
  SessionAppendArgs,
  SessionAppendDoor,
  SessionAppendHook,
  SessionAppendResult,
  SessionVersion,
  TurnStepChunk,
  TurnStepInput,
  TurnStepResult,
} from "claude-code"
import {
  MOD_APPEND_DOORS,
  MOD_HEALTH_MARKER,
  MOD_MIN_CLAUDE_VERSION,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_URL_BASE,
  type ModAppendDoor,
  type ModEffort,
  type ModEvent,
  type ModEventsBody,
  type ModRequest,
  type ModResult,
  type ModResultsBody,
  type ModResultValue,
  type ModStepPart,
  type ModStopReason,
  modVersionAtLeast,
  resolveModSocketPath,
} from "./mod-protocol"

/** Whether a row's door is one of the conversation's that this mod watches: the eight it
 *  registers for, which is every door but the ones the host hides from a mod a person installed
 *  and `queue`, which is no row of the conversation. */
function isWatchedDoor(door: SessionAppendDoor): door is ModAppendDoor {
  const watched: readonly string[] = MOD_APPEND_DOORS
  return watched.includes(door)
}

/** How long to wait before each further attempt at `GET /healthz`, in milliseconds: eleven probes
 *  over about 31 seconds, growing. The server binds its socket while the MCP server starts, which
 *  is after Claude Code has run this mod's `session.start`, so the first probes normally miss. */
const CONNECT_DELAYS_MS = [250, 500, 1_000, 2_000, 3_000, 4_000, 5_000, 5_000, 5_000, 5_000]

/** The shortest gap between two snapshots of one `turn.step` while it streams. */
const STEP_SNAPSHOT_MS = 100

/** The shortest a poll pass may take when it answered nothing, so a server that answers at once
 *  without holding (a wrong one, or one that is refusing) cannot spin this loop. */
const POLL_FLOOR_MS = 250

/** How long to watch for the new session id after `session.end`, in tries and their gap: it is
 *  there within about 100 ms of the hook, and the process lives on only after a `/clear`. */
const SESSION_ID_TRIES = 10
const SESSION_ID_WAIT_MS = 30

/** The most events held while no server is connected. The live view's own frames are a later
 *  task's and are rebuilt from the session, so the oldest of an overflow is the one to drop. */
const EVENT_QUEUE_MAX = 2048

/**
 * Everything the link does to the session, one closure per call on `$`.
 *
 * It exists because `$` may not leave the hook it was given to: the host reads this module's
 * source and refuses a `$` that is passed, bound, spread, returned or read. Each closure spells
 * its call in full at its own call site, which is what the host asks for, and what it then lists
 * as the module's calls and the variables it reads.
 */
interface ModHost {
  readonly fetch: (url: string, init: HttpInit) => Promise<HttpResponse>
  readonly sleep: (ms: number) => Promise<void>
  readonly sessionId: () => Promise<string>
  readonly version: () => Promise<SessionVersion>
  readonly model: () => Promise<string>
  /** `XDG_RUNTIME_DIR`, the first base the socket path is looked for in. */
  readonly runtimeDirectory: () => Promise<string | undefined>
  /** `TMPDIR`, the second. */
  readonly tmpDirectory: () => Promise<string | undefined>
  readonly append: (args: SessionAppendArgs) => Promise<SessionAppendResult>
  readonly submit: (args: PromptSubmitArgs) => Promise<PromptSubmitResult>
  readonly runCommand: (args: CommandRunArgs) => Promise<CommandRunResult>
  readonly listCommands: () => Promise<CommandInfo[]>
  readonly fork: (request: ModelForkRequest) => Promise<ModelForkResult>
  readonly setConfig: (args: ConfigSetArgs) => Promise<ConfigSetResult>
  readonly listConfig: () => Promise<ConfigRow[]>
}

/** What the hooks below drive: the session's half of the link. The server's half is `ModLink` in
 *  `../src/mod-link.ts`; the two meet on the socket alone. */
interface SessionLink {
  /** Name the socket and start the poll loop, once, at the first `session.start`. */
  readonly begin: (host: ModHost, interactive: boolean) => Promise<void>
  /** Queue one event and start it on its way; never throws. */
  readonly send: (event: ModEvent) => void
  /** Watch for the id a `/clear` moves the session to and send it when it arrives. */
  readonly followSessionId: () => Promise<void>
  /** The effort the server last asked for, for the next `turn.step`; null for none. */
  readonly effortNow: () => ModEffort | null
}

/** `error`'s text, bounded: it crosses as one JSON string into a result the server rejects with. */
function errorText(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  return message.slice(0, 500)
}

/** The requests one `POST /requests` answered with. The only writer of this socket is the server
 *  that bound it, so an item that is shaped like a request at all is the request it says it is;
 *  anything else, and a body that is not an answer at all, is nothing to do. */
function readRequests(text: string): readonly ModRequest[] {
  let body: unknown
  try {
    body = JSON.parse(text)
  } catch {
    return []
  }
  if (typeof body !== "object" || body === null || !("requests" in body)) return []
  const requests = body.requests
  if (!Array.isArray(requests)) return []
  return requests.filter((request: unknown): request is ModRequest => {
    if (typeof request !== "object" || request === null) return false
    if (!("id" in request) || !("call" in request)) return false
    return typeof request.id === "string" && typeof request.call === "string"
  })
}

/** The link to the server, all of it closure state: one mod instance drives one session. */
function createLink(): SessionLink {
  let host: ModHost | undefined
  let socketPath: string | undefined
  /** True while the last probe or post succeeded. */
  let connected = false
  /** True once the link gave up for good, or decided never to start. */
  let finished = false
  let started = false
  /** The id the socket is named for, which a `/clear` does not move. */
  let currentSessionId: string | undefined
  /** The effort the server last asked for, put on every later `turn.step`; null for none. */
  let effort: ModEffort | null = null
  const queued: ModEvent[] = []
  let flushing = false
  /** `$.prompt.submit` resolves only when the turn it starts ends, so a second call would sit
   *  behind the first inside the engine: they run one after another, off the poll loop. */
  let prompts: Promise<void> = Promise.resolve()

  const sleep = async (ms: number): Promise<void> => {
    try {
      await host?.sleep(ms)
    } catch {
      // A refused wait is the host's business; the caller's loop carries on.
    }
  }

  /** The response body of a request that succeeded, or undefined for any failure at all: a socket
   *  that has gone, a refused request, a body that never arrived. */
  const fetch = async (route: string, body: string | undefined): Promise<string | undefined> => {
    if (host === undefined || socketPath === undefined) return undefined
    try {
      const response = await host.fetch(
        `${MOD_URL_BASE}${route}`,
        body === undefined
          ? { socketPath }
          : {
              method: "POST",
              headers: { "content-type": "application/json" },
              body,
              socketPath,
            },
      )
      return response.ok ? response.text : undefined
    } catch {
      return undefined
    }
  }

  /** Whether a server of ours answers the socket now. */
  const probe = async (): Promise<boolean> => {
    const text = await fetch(MOD_ROUTE_HEALTH, undefined)
    if (text === undefined) return false
    let body: unknown
    try {
      body = JSON.parse(text)
    } catch {
      return false
    }
    if (typeof body !== "object" || body === null || !("link" in body)) return false
    return body.link === MOD_HEALTH_MARKER
  }

  /** Probe until a server answers or the backoff is spent; false means stop for good. */
  const connect = async (): Promise<boolean> => {
    for (const delay of CONNECT_DELAYS_MS) {
      if (await probe()) {
        connected = true
        return true
      }
      await sleep(delay)
    }
    connected = await probe()
    return connected
  }

  /** Post everything queued, oldest first, one batch at a time so no event overtakes another. A
   *  batch that fails is dropped and the link counts as lost: the poll loop reconnects. */
  const flush = async (): Promise<void> => {
    if (flushing) return
    flushing = true
    try {
      while (connected && queued.length > 0) {
        const events = queued.splice(0, queued.length)
        const body = { events } satisfies ModEventsBody
        if ((await fetch(MOD_ROUTE_EVENTS, JSON.stringify(body))) === undefined) {
          connected = false
          return
        }
      }
    } finally {
      flushing = false
    }
  }

  /** Queue one event and start it on its way. Never throws: every hook calls this. */
  const send = (event: ModEvent): void => {
    if (finished) return
    queued.push(event)
    if (queued.length > EVENT_QUEUE_MAX) queued.splice(0, queued.length - EVENT_QUEUE_MAX)
    void flush().catch(() => undefined)
  }

  /** Make the call a request names and answer with its own value. */
  const invoke = async (engine: ModHost, request: ModRequest): Promise<ModResultValue> => {
    switch (request.call) {
      case "session.append": {
        const answer = await engine.append(request.args)
        return answer.deny === undefined ? { uuid: answer.uuid } : { deny: answer.deny }
      }
      case "prompt.submit": {
        const answer = await engine.submit(request.args)
        return answer.drop === undefined ? { text: answer.text } : { drop: answer.drop }
      }
      case "command.run":
        return await engine.runCommand(request.args)
      case "model.fork":
        return await engine.fork(request.args)
      case "config.set": {
        const answer = await engine.setConfig(request.args)
        return answer.deny === undefined ? { value: answer.value } : { deny: answer.deny }
      }
      case "session.model":
        return await engine.model()
      case "config.list":
        return await engine.listConfig()
      case "command.list":
        return await engine.listCommands()
      case "set-effort":
        effort = request.args.effort
        return { effort }
    }
  }

  /** Run one request and post its result. A call that throws answers the throw, not nothing. */
  const answer = async (request: ModRequest): Promise<void> => {
    let result: ModResult
    try {
      if (host === undefined) throw new Error("the session is gone")
      result = { id: request.id, ok: true, value: await invoke(host, request) }
    } catch (error) {
      result = { id: request.id, ok: false, error: errorText(error) }
    }
    const body = { results: [result] } satisfies ModResultsBody
    await fetch(MOD_ROUTE_RESULTS, JSON.stringify(body))
  }

  /** Connect, then poll for work until the server goes for good. */
  const pump = async (): Promise<void> => {
    while (!finished) {
      if (!connected && !(await connect())) {
        finished = true
        // Dropped rather than kept: `send` refuses every later event now, and `flush` only posts
        // while connected, so nothing will ever read these again. They are not free to hold - a
        // queued `step` carries its whole accumulated text, so a step that streamed through the
        // backoff leaves one full copy of itself per snapshot pinned for the session's life.
        queued.length = 0
        return
      }
      const pollStartedAt = Date.now()
      await flush()
      const text = await fetch(MOD_ROUTE_REQUESTS, "{}")
      if (text === undefined) {
        // The server stopped, or refused: back to the bounded backoff rather than spinning.
        connected = false
        continue
      }
      const requests = readRequests(text)
      for (const request of requests) {
        if (request.call === "prompt.submit") prompts = prompts.then(() => answer(request))
        else void answer(request).catch(() => undefined)
      }
      if (requests.length === 0 && Date.now() - pollStartedAt < POLL_FLOOR_MS) {
        await sleep(POLL_FLOOR_MS)
      }
    }
  }

  /** Read the session's model and send it; a failure costs nothing. */
  const sendModel = async (engine: ModHost): Promise<void> => {
    try {
      send({ kind: "model", model: await engine.model() })
    } catch {
      // The model is one event, not a reason to fail a hook.
    }
  }

  /** Watch for the id a `/clear` moves the session to, which arrives shortly after the hook. */
  const followSessionId = async (): Promise<void> => {
    for (let attempt = 0; attempt < SESSION_ID_TRIES; attempt += 1) {
      await sleep(SESSION_ID_WAIT_MS)
      let id: string
      try {
        if (host === undefined) return
        id = await host.sessionId()
      } catch {
        return
      }
      if (id !== currentSessionId) {
        currentSessionId = id
        send({ kind: "session-id", sessionId: id })
        return
      }
    }
  }

  /** Name the socket, refuse a Claude Code too old for the hooks and calls this mod is written
   *  against, and start the poll loop. Never throws, and never waits on the connect: a session
   *  does not start more slowly because a server is not up yet. */
  const begin = async (engine: ModHost, interactive: boolean): Promise<void> => {
    if (started) return
    started = true
    host = engine
    try {
      const version = await engine.version()
      if (!modVersionAtLeast(version.version, MOD_MIN_CLAUDE_VERSION)) {
        finished = true
        return
      }
      // The boot id: `$.session.id()` at the first `session.start` is the id Claude Code gave the
      // MCP server as CLAUDE_CODE_SESSION_ID, and a later `/clear` does not move the socket.
      const sessionId = await engine.sessionId()
      currentSessionId = sessionId
      const resolved = resolveModSocketPath(sessionId, {
        XDG_RUNTIME_DIR: await engine.runtimeDirectory(),
        TMPDIR: await engine.tmpDirectory(),
      })
      if (resolved.path === undefined) {
        finished = true
        return
      }
      socketPath = resolved.path
      send({ kind: "hello", sessionId, claudeVersion: version.version, interactive })
      void sendModel(engine).catch(() => undefined)
      void pump().catch(() => undefined)
    } catch {
      finished = true
    }
  }

  return { begin, send, followSessionId, effortNow: (): ModEffort | null => effort }
}

/** The link's own accumulation of one `turn.step`: the parts as they stream, and which part each
 *  content block of the response is building. */
interface StepParts {
  readonly parts: ModStepPart[]
  readonly blocks: Map<number, number>
}

/** Add one text or thinking piece to the part its block is building, opening one if it is new. */
function appendText(
  step: StepParts,
  index: number,
  kind: "text" | "reasoning",
  text: string,
): void {
  const at = step.blocks.get(index)
  const existing = at === undefined ? undefined : step.parts[at]
  const merged = existing !== undefined && existing.type === kind ? existing.text + text : text
  const part: ModStepPart =
    kind === "text" ? { type: "text", text: merged } : { type: "reasoning", text: merged }
  if (at !== undefined && existing !== undefined && existing.type === kind) step.parts[at] = part
  else {
    step.blocks.set(index, step.parts.length)
    step.parts.push(part)
  }
}

/** Fold one streamed chunk into the step's parts. The engine's own items and the stop carry
 *  nothing the live view shows, so they only pass through. */
function accumulate(step: StepParts, chunk: TurnStepChunk): void {
  if (chunk.kind === "text") appendText(step, chunk.index, "text", chunk.text)
  else if (chunk.kind === "thinking") appendText(step, chunk.index, "reasoning", chunk.text)
  else if (chunk.kind === "tool") {
    step.blocks.set(chunk.index, step.parts.length)
    step.parts.push({ type: "tool-call", id: chunk.id, name: chunk.name, args: "" })
  } else if (chunk.kind === "input") {
    const at = step.blocks.get(chunk.index)
    const existing = at === undefined ? undefined : step.parts[at]
    if (at !== undefined && existing !== undefined && existing.type === "tool-call") {
      step.parts[at] = { ...existing, args: existing.args + chunk.json }
    }
  }
}

/** This session's link, one per loaded module, so the hook the `session.append` registrations
 *  share can be declared here: the host resolves a named hook only against this file's own
 *  top-level functions. */
const link = createLink()

/** One row of the conversation, by whichever door it came in. The door is read off the row
 *  rather than the registration, so the eight registrations share this one hook. */
const appended: SessionAppendHook = async (_, e, next) => {
  if (isWatchedDoor(e.door)) {
    link.send({ kind: "append", uuid: e.uuid, door: e.door, origin: e.origin, message: e.message })
  }
  return await next(e)
}

/** The hooks this mod registers, and nothing else. In particular no `classic.*` hook: on a
 *  machine with managed settings Claude Code's built-in guard writes a notice into what the model
 *  reads for every hook it skips, and this change adds nothing to what the agent reads. For the
 *  same reason `session.append` is registered for the doors of the conversation alone. */
export function register(on: On): void {
  on("session.start", async ($, e, next) => {
    const result = await next(e)
    // The one place `$` is spelled: every call the link makes is a closure of its own, built
    // here, so `$` itself never leaves this hook.
    await link.begin(
      {
        fetch: (url, init) => $.http.fetch(url, init),
        sleep: (ms) => $.clock.sleep(ms),
        sessionId: () => $.session.id(),
        version: () => $.session.version(),
        model: () => $.session.model(),
        runtimeDirectory: () => $.env.get("XDG_RUNTIME_DIR"),
        tmpDirectory: () => $.env.get("TMPDIR"),
        append: (args) => $.session.append(args),
        submit: (args) => $.prompt.submit(args),
        runCommand: (args) => $.command.run(args),
        listCommands: () => $.command.list(),
        fork: (request) => $.model.fork(request),
        setConfig: (args) => $.config.set(args),
        listConfig: () => $.config.list(),
      },
      e.isInteractive,
    )
    return result
  })

  on("session.end", async (_, e, next) => {
    link.send({ kind: "session-end", reason: e.reason })
    const result = await next(e)
    void link.followSessionId().catch(() => undefined)
    return result
  })

  on("turn.start", async (_, e, next) => {
    link.send({ kind: "turn-start", turnId: e.turnId })
    return await next(e)
  })

  on("turn.complete", async (_, e, next) => {
    link.send({ kind: "turn-complete", turnId: e.turnId, stopReason: e.reason })
    return await next(e)
  })

  on("turn.step", async function* (_, e, next) {
    const effort = link.effortNow()
    const request: TurnStepInput = effort === null ? e : { ...e, effort }
    const step: StepParts = { parts: [], blocks: new Map() }
    const snapshot = (done: boolean, stopReason?: ModStopReason): void => {
      link.send({
        kind: "step",
        turnId: request.turnId,
        index: request.index,
        model: request.model,
        ...(request.effort === undefined ? {} : { effort: request.effort }),
        parts: [...step.parts],
        done,
        ...(stopReason === undefined ? {} : { stopReason }),
      })
    }
    const stream = next(request)
    let snapshotAt = 0
    for await (const chunk of stream) {
      accumulate(step, chunk)
      const now = Date.now()
      if (now - snapshotAt >= STEP_SNAPSHOT_MS) {
        snapshotAt = now
        snapshot(false)
      }
      yield chunk
    }
    let result: TurnStepResult | undefined
    try {
      result = await stream.result
    } catch {
      result = undefined
    }
    if (result === undefined) {
      // The step failed beneath: say it is over and leave the result to the chain.
      snapshot(true)
      return
    }
    snapshot(true, result.stopReason)
    return result
  })

  // One registration per door, each door spelled where it is registered: the host reads the
  // matchers off this source (`claude plugin validate` prints them), and a door it cannot read
  // is a door it cannot attest this mod stays away from.
  on("session.append", { door: "prompt" }, appended)
  on("session.append", { door: "command" }, appended)
  on("session.append", { door: "response" }, appended)
  on("session.append", { door: "tool-result" }, appended)
  on("session.append", { door: "tool-message" }, appended)
  on("session.append", { door: "delivery" }, appended)
  on("session.append", { door: "note" }, appended)
  on("session.append", { door: "notice" }, appended)
}
