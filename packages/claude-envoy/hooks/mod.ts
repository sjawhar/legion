/**
 * The Claude Code mod this plugin loads through the `modules` key of `./hooks.json`: Claude Code
 * runs it from source, in an environment of its own, and it sees every hook of the session.
 *
 * It carries the session to this plugin's MCP server over the Unix socket both sides name the
 * same way (`./mod-protocol`): events one way, the server's requests the other. The server makes
 * the Agents page's live view out of those events (`../src/agent-stream-runner.ts`) and routes a
 * person's message back through those requests (`../src/user-turn-router.ts`); nothing of either
 * is here.
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
  MOD_INSTANCE_HEADER,
  MOD_MIN_CLAUDE_VERSION,
  MOD_PLUGIN_NAME,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_STATUS_TAKEN,
  MOD_URL_BASE,
  type ModAppendDoor,
  type ModAppendMessage,
  type ModAppendOrigin,
  type ModCallArguments,
  type ModCallResults,
  type ModContentBlock,
  type ModEffort,
  type ModEvent,
  type ModEventsBody,
  type ModRequest,
  type ModResult,
  type ModResultsBody,
  type ModResultValue,
  type ModStepPart,
  type ModStopReason,
  type ModTurnReason,
  type ModUnreadMessage,
  modVersionAtLeast,
  resolveModSocketPath,
  unreadRowPrompt,
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
 * How long the mod waits for the row Claude Code writes for a prompt it submitted.
 *
 * The row is there before the call answers: in all six `$.prompt.submit` calls recorded on
 * 2.1.310 the prompt's own `prompt`-door row landed 15 ms to 26 ms before the call resolved
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:144-145`,
 * `logs/mod3-q2-310.jsonl:51-52,86-87`, `logs/mod3-q1q3q5.jsonl:236-239`). So this is a guard
 * on a wait that is normally over before it starts, at about eighty times the largest gap.
 */
const PROMPT_ROW_MS = 2_000

/**
 * How long a prompt the engine took has to start its turn before the mod stops holding the rows
 * handed over meanwhile for it.
 *
 * `turn.start` followed the submit's resolution by 8 ms to 11 ms in the same six submits, so a
 * prompt that has started no turn by now starts none: its rows are pointed at rather than left
 * waiting for a `turn.complete` that never comes (INFRA-1026 review, round eleven, on the rule
 * at `../src/mod-session.ts:577` before this change).
 */
const TURN_START_MS = 2_000

/**
 * The most Envoy events the mod remembers the row of, in `$.store`.
 *
 * Each record is a key of some seventy bytes and a small object, so the whole of it is about
 * 30 KiB, against the 4 MiB of compact JSON `set` refuses past. The bound is what keeps a
 * session of months off that wall and off the cost of a big store: measured on 2.1.310, a
 * 3.9 MiB store cost `get` 7–20 ms, `set` 44 ms and `keys()` up to 16 ms, against about 1 ms
 * each on an empty one (`/legion/scratch/tmp/infra-1026-store/README.md`).
 */
const CARD_RECORD_MAX = 256

/** What every key of that record starts with, so the eviction pass reads this mod's own keys and
 *  leaves anything else this plugin may store alone. */
const CARD_KEY_PREFIX = "message:"

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
  /** `CLAUDE_CONFIG_DIR`, Claude Code's own configuration directory, the second base. */
  readonly configDirectory: () => Promise<string | undefined>
  /** `HOME`, whose `.claude` is that configuration directory when the variable names none. */
  readonly homeDirectory: () => Promise<string | undefined>
  readonly append: (args: SessionAppendArgs) => Promise<SessionAppendResult>
  readonly submit: (args: PromptSubmitArgs) => Promise<PromptSubmitResult>
  readonly runCommand: (args: CommandRunArgs) => Promise<CommandRunResult>
  readonly listCommands: () => Promise<CommandInfo[]>
  readonly fork: (request: ModelForkRequest) => Promise<ModelForkResult>
  readonly setConfig: (args: ConfigSetArgs) => Promise<ConfigSetResult>
  readonly listConfig: () => Promise<ConfigRow[]>
  /** `$.store.get(key)`: this plugin's own key-value store, kept between sessions. */
  readonly storeGet: (key: string) => Promise<unknown>
  readonly storeSet: (key: string, value: unknown) => Promise<void>
  readonly storeDelete: (key: string) => Promise<void>
  /** `$.store.keys()`, in insertion order (measured on 2.1.310). */
  readonly storeKeys: () => Promise<string[]>
}

/** What the hooks below drive: the session's half of the link. The server's half is `ModLink` in
 *  `../src/mod-link.ts`; the two meet on the socket alone. */
interface SessionLink {
  /** Name the socket and start the poll loop, once, at the first `session.start`. */
  readonly begin: (host: ModHost, interactive: boolean) => Promise<void>
  /** Queue one event and start it on its way; never throws. */
  readonly send: (event: ModEvent) => void
  /** A turn started: reported, and taken as the turn a message handed over now is a row of. */
  readonly turnStarted: (turnId: string) => void
  /** A turn ended: reported, and every row of this mod's that no request of that turn read is
   *  pointed at by one prompt (`unreadRowPrompt`). */
  readonly turnEnded: (turnId: string, reason: ModTurnReason) => void
  /** A request of the turn is going out, counted at the top of the `turn.step` hook: a row
   *  stored before this moment is in this request, and one stored after it is in none. */
  readonly stepRequested: () => void
  /** One row of the conversation as the `session.append` hook handed it: stamped where this mod
   *  wrote it, and sent. */
  readonly row: (row: {
    readonly uuid: string
    readonly door: ModAppendDoor
    readonly origin: ModAppendOrigin
    readonly message: ModAppendMessage
  }) => void
  /** Watch for the id a `/clear` moves the session to and send it when it arrives. */
  readonly followSessionId: () => Promise<void>
  /** The effort the server last asked for, for the next `turn.step` that names one of its own;
   *  null for none. */
  readonly effortNow: () => ModEffort | null
}

/** One row this mod wrote into a turn, watched until a request reads it or the turn ends. */
interface WatchedRow {
  /** The turn the row was stored into, and undefined for a row stored while the prompt that
   *  starts that turn was still in flight: the turn adopts it when it reports itself. */
  turnId?: string
  /**
   * The requests this session had made when the row was stored: one made after it is the
   * request that read it.
   *
   * Counted once the append has resolved rather than before it: a request that went out while
   * the row was being stored cannot be proven to carry it, and a row quoted though the model
   * had read it costs a repetition, where a row counted as read that never was costs the
   * message (INFRA-1026 review, repro `R-B`).
   */
  readonly requests: number
  readonly message: ModUnreadMessage
}

/** One prompt this mod submitted whose row has not arrived: the text the row carries, what the
 *  row is to the server, and what is woken by it. */
interface AwaitedRow {
  readonly text: string
  readonly wrote: "message" | "pointer"
  readonly seen: (uuid: string) => void
}

/** What this mod recorded one Envoy event's write as, in `$.store`. Values round-trip as
 *  `JSON.parse(JSON.stringify(value))` (measured on 2.1.310), so this is plain JSON data. */
interface RecordedWrite {
  readonly uuid: string
  readonly as: "row" | "prompt"
}

/** `value` as this mod recorded a write, or undefined where it is anything else: the store is
 *  this plugin's own, and a value it did not write is no record of one. */
function recordedWrite(value: unknown): RecordedWrite | undefined {
  if (typeof value !== "object" || value === null) return undefined
  if (!("uuid" in value) || !("as" in value)) return undefined
  const { uuid, as } = value
  if (typeof uuid !== "string" || (as !== "row" && as !== "prompt")) return undefined
  return { uuid, as }
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
  /** This Claude Code process's mod, minted here: the link serves the first instance that names
   *  itself and refuses every other, so two processes that booted on one session id (`claude
   *  -c`, and `claude --resume <id>` in a second terminal) no longer share one server, one live
   *  view and one person's Send (INFRA-1026 review). The time and the random part together are
   *  as distinct as two processes of one session need: they rule out the one other instance this
   *  socket can see. */
  const instance = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
  let host: ModHost | undefined
  let socketPath: string | undefined
  /** True while the last probe or post succeeded. */
  let connected = false
  /** True once the link gave up for good, or decided never to start. */
  let finished = false
  let started = false
  /** The id the socket is named for, which a `/clear` does not move. */
  let currentSessionId: string | undefined
  /** The effort the server last asked for, put on every later `turn.step` that names an effort
   *  of its own; null for none. */
  let effort: ModEffort | null = null
  const queued: ModEvent[] = []
  let flushing = false
  /** The turn running now, as `turn.start` and `turn.complete` report it. Read in the same tick
   *  as the write that follows it, which is what makes "a row of this turn, or the prompt of a
   *  new one" a decision and not a guess (INFRA-1026 review, round eleven). */
  let turn: string | undefined
  /**
   * True from the moment this mod hands the engine a prompt until the turn that prompt starts
   * has reported itself, or until the mod gives up on one (`TURN_START_MS`).
   *
   * A message handed over meanwhile is a row of that turn rather than a second prompt: the
   * engine takes one prompt at a time, so a second would sit behind this turn, and the row the
   * turn is about to read says the same thing sooner.
   */
  let prompting = false
  /** Bumped whenever a turn starts or this mod stops waiting for one, so the guard armed for
   *  one prompt never fires against a later one. */
  let promptEpoch = 0
  /** How many requests this session has made, counted at the top of every `turn.step`. */
  let requests = 0
  /** The rows this mod wrote that no request has read, by the uuid Claude Code stored each
   *  under. A row leaves it when its turn ends, which is also when the prompt pointing at it
   *  goes out, so this holds the rows of the turn running and nothing else. */
  const watched = new Map<string, WatchedRow>()
  /** The prompts this mod submitted whose row has not arrived, oldest first. Each entry is
   *  dropped by the wait that put it there, so nothing else bounds this. */
  const awaitedRows: AwaitedRow[] = []
  /** The write running for each Envoy event id, so a copy of that event which arrives while the
   *  first is still running waits for it and then finds what it recorded (`writeOnce`). */
  const writing = new Map<string, Promise<void>>()

  const sleep = async (ms: number): Promise<void> => {
    try {
      await host?.sleep(ms)
    } catch {
      // A refused wait is the host's business; the caller's loop carries on.
    }
  }

  /** The response body of a request that succeeded, or undefined for any failure at all: a socket
   *  that has gone, a refused request, a body that never arrived. A post the server refuses
   *  because another mod holds the link ends this one for good: that server belongs to the
   *  process that got there first, and there is no second socket to try. */
  const fetch = async (route: string, body: string | undefined): Promise<string | undefined> => {
    if (host === undefined || socketPath === undefined) return undefined
    try {
      const response = await host.fetch(
        `${MOD_URL_BASE}${route}`,
        body === undefined
          ? { socketPath }
          : {
              method: "POST",
              headers: { "content-type": "application/json", [MOD_INSTANCE_HEADER]: instance },
              body,
              socketPath,
            },
      )
      if (response.status === MOD_STATUS_TAKEN) {
        finished = true
        connected = false
        // Nothing will read these again: `send` refuses every later event and `flush` posts only
        // while connected.
        queued.length = 0
        return undefined
      }
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

  /** The key `$.store` holds one Envoy event's write under: this session's id with the event
   *  id. A `/clear` moves the id, and the conversation it moves to is one the card is not in,
   *  so its key is a key of its own. */
  const recordKey = (eventId: string): string =>
    `${CARD_KEY_PREFIX}${currentSessionId ?? ""}:${eventId}`

  /** Record that `eventId`'s message is this row, and drop the oldest records past
   *  `CARD_RECORD_MAX`. `keys()` answers in insertion order, so the oldest are the first of
   *  them (the issue's store spike). A store that refuses the write leaves no record and the
   *  message stands: a later copy of that event would then be written a second time, which is
   *  the one case this rule misses and the one the crash case already covers
   *  (`docs/site/src/content/docs/dispatch/envoy.md`). */
  const remember = async (
    engine: ModHost,
    eventId: string,
    write: RecordedWrite,
  ): Promise<void> => {
    try {
      await engine.storeSet(recordKey(eventId), write)
      const keys = (await engine.storeKeys()).filter((key) => key.startsWith(CARD_KEY_PREFIX))
      for (const old of keys.slice(0, keys.length - CARD_RECORD_MAX)) {
        await engine.storeDelete(old)
      }
    } catch {
      // The record is a saving, not the write itself; a store that refuses it costs nothing
      // here and there is nowhere in a session to report it to.
    }
  }

  /** Wait for the row Claude Code writes for the prompt carrying `text`, and stamp that row
   *  `wrote` for the server. Registered before the submit goes out, because the row reaches
   *  this mod's own `session.append` hook before the call resolves (`PROMPT_ROW_MS`). */
  const awaitRow = (
    text: string,
    wrote: "message" | "pointer",
  ): { readonly uuid: () => Promise<string | undefined>; readonly drop: () => void } => {
    const settle = Promise.withResolvers<string | undefined>()
    const waiting: AwaitedRow = { text, wrote, seen: settle.resolve }
    awaitedRows.push(waiting)
    const drop = (): void => {
      const at = awaitedRows.indexOf(waiting)
      if (at !== -1) awaitedRows.splice(at, 1)
    }
    return {
      drop,
      uuid: async (): Promise<string | undefined> => {
        try {
          return await Promise.race([settle.promise, sleep(PROMPT_ROW_MS).then(() => undefined)])
        } finally {
          drop()
        }
      },
    }
  }

  /** One row of the `prompt` door, claimed for the prompt of this mod's it is the row of, if
   *  any: the oldest whose text this row carries, since Claude Code writes its own framing in
   *  front of a submit that named no `asUser`. Every other prompt row is a person's own. */
  const claimRow = (
    uuid: string,
    content: readonly ModContentBlock[],
  ): AwaitedRow["wrote"] | undefined => {
    const text = content
      .map((block) => (typeof block["text"] === "string" ? block["text"] : ""))
      .join("\n")
    const at = awaitedRows.findIndex((waiting) => text.includes(waiting.text))
    const waiting = awaitedRows[at]
    if (waiting === undefined) return undefined
    awaitedRows.splice(at, 1)
    waiting.seen(uuid)
    return waiting.wrote
  }

  /** Stop waiting for the turn a prompt was to start: it started none, so every row handed over
   *  while it was in flight is read by nothing and the model is pointed at it. A submit the
   *  engine dropped, one it refused, and one whose turn never came all end here (INFRA-1026
   *  review, repro `R-C`). */
  const stopPrompting = (): void => {
    prompting = false
    promptEpoch += 1
    const unread: ModUnreadMessage[] = []
    for (const [uuid, entry] of watched) {
      if (entry.turnId !== undefined) continue
      watched.delete(uuid)
      unread.push(entry.message)
    }
    if (unread.length > 0) pointAt(unread)
  }

  /** Give the prompt the engine has just taken `TURN_START_MS` to start its turn. */
  const guardTurnStart = (): void => {
    const epoch = promptEpoch
    void (async () => {
      await sleep(TURN_START_MS)
      if (promptEpoch === epoch && prompting) stopPrompting()
    })()
  }

  /**
   * Hand the engine one prompt and answer with the row Claude Code wrote for it.
   *
   * `$.prompt.submit` resolves once the engine has taken the prompt, not at the end of the turn
   * it starts: in all six calls recorded on 2.1.310 the prompt's own row landed 15 ms to 26 ms
   * before the call resolved and `turn.start` 8 ms to 11 ms after it, and the long resolutions
   * — 8.5 s, 11.2 s and 12.4 s — are the wait for a turn that was already running, which ended
   * just before (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:136-147`,
   * `logs/mod3-q2-310.jsonl:72-89`, `logs/mod3-q1q3q5.jsonl:163-179`). So the row is there by
   * the time this answers, and what the caller waits for is the prompt entering, never a turn.
   */
  const submitPrompt = async (
    engine: ModHost,
    text: string,
    asUser: boolean,
    wrote: "message" | "pointer",
  ): Promise<{ readonly uuid?: string; readonly drop?: string }> => {
    // Set before the call and in the same tick as the check that chose this branch: a message
    // handed over from here on is a row of the turn this prompt starts.
    prompting = true
    const row = awaitRow(text, wrote)
    let answer: PromptSubmitResult
    try {
      answer = await engine.submit({ text, ...(asUser ? { asUser: true as const } : {}) })
    } catch (error) {
      row.drop()
      stopPrompting()
      throw error
    }
    if (answer.drop !== undefined) {
      row.drop()
      stopPrompting()
      return { drop: answer.drop }
    }
    guardTurnStart()
    const uuid = await row.uuid()
    return uuid === undefined ? {} : { uuid }
  }

  /** Submit the one prompt that points the model at `unread`, quoting each message in full
   *  (`unreadRowPrompt`). Its own row is stamped `pointer`, so the live view draws nothing of
   *  it: it quotes rows that view has already drawn. */
  const pointAt = (unread: readonly ModUnreadMessage[]): void => {
    const engine = host
    if (engine === undefined) return
    void submitPrompt(engine, unreadRowPrompt(unread), false, "pointer").catch(() => undefined)
  }

  /** Write one message into the session as the session stands now: a row of the turn running,
   *  or the prompt of a new turn. */
  const writeMessage = async (
    engine: ModHost,
    args: ModCallArguments["session.message"],
  ): Promise<ModCallResults["session.message"]> => {
    if (turn === undefined && !prompting) {
      // Nothing is running: an appended row would be read at the turn's next request, and an
      // idle session makes none, so the message is the prompt itself and nothing else is
      // stored for it — a row beside a prompt quoting it put the one message in front of the
      // model twice (INFRA-1026, handoff v6).
      const sent = await submitPrompt(
        engine,
        args.message.text,
        args.message.kind === "person",
        "message",
      )
      if (sent.drop !== undefined) return { deny: sent.drop, as: "prompt" }
      if (sent.uuid === undefined) {
        return {
          deny: `the session wrote no row for the prompt within ${PROMPT_ROW_MS} ms`,
          as: "prompt",
        }
      }
      return { uuid: sent.uuid, as: "prompt" }
    }
    const started = turn
    const answer = await engine.append({ message: { type: "user", content: args.content } })
    if (answer.deny !== undefined) return { deny: answer.deny, as: "row" }
    // Claude Code does not re-enter this mod's own `session.append` hook for a row this mod
    // appended, so the row reaches the server's mapper nowhere else and a card delivered into
    // a running turn was drawn nowhere at all (INFRA-1026, handoff v6). Measured in the issue's
    // spike: two `$.session.append` calls resolved
    // `{message: {type: "user", role: "user", isMeta: true, content}, uuid}` and neither uuid
    // ever arrived at a `session.append` hook
    // (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:36,64`). So the mod reports
    // the row itself, out of what the call answered (`SessionAppendResult`,
    // `./claude-mod.d.ts`).
    //
    // The door and the origin are the engine's to set and the result carries neither: a
    // plugin's own append comes in by `note`, under the calling plugin. `wrote` is what the
    // server reads it by, and it is on the event rather than on this call's answer because the
    // two cross (`./mod-protocol`, `ModEvent`).
    send({
      kind: "append",
      uuid: answer.uuid,
      door: "note",
      origin: { kind: "plugin", name: MOD_PLUGIN_NAME },
      message: answer.message,
      wrote: "message",
    })
    watched.set(answer.uuid, {
      message: args.message,
      requests,
      ...(turn === undefined ? {} : { turnId: turn }),
    })
    // The turn ended while the row was being stored, with no other starting: no request will
    // read it, so the model is pointed at it now rather than at a `turn.complete` that has been
    // and gone.
    if (started !== undefined && turn === undefined && !prompting) {
      watched.delete(answer.uuid)
      pointAt([args.message])
    }
    return { uuid: answer.uuid, as: "row" }
  }

  /**
   * Write the message of `eventId` once, however many times Envoy sends it.
   *
   * The store has no compare-and-set: two `get`s of an unset key both answered undefined, 0 ms
   * and 2 s apart, in one session and across two (`/legion/scratch/tmp/infra-1026-store`). So a
   * copy that arrives while the first write is still running waits for it here, inside the one
   * process, and reads the record that write left; a copy that arrives after this process is
   * gone — the engine restarted, `claude --resume <id>` — finds that record in the store, which
   * `set` had written to disk before it resolved and which survived a `kill -9` taken at that
   * moment (same spike).
   */
  const writeOnce = (
    engine: ModHost,
    eventId: string,
    args: ModCallArguments["session.message"],
  ): Promise<ModCallResults["session.message"]> => {
    const settle = Promise.withResolvers<ModCallResults["session.message"]>()
    const behind = writing.get(eventId)
    const run = (behind === undefined ? Promise.resolve() : behind.catch(() => undefined)).then(
      async () => {
        let written: ModCallResults["session.message"]
        try {
          const recorded = recordedWrite(
            await engine.storeGet(recordKey(eventId)).catch(() => undefined),
          )
          // A store that refuses the read answers nothing, so the message is written again:
          // at-least-once is this path's rule, and a message lost is the failure that matters.
          written = recorded ?? (await writeMessage(engine, args))
          if (recorded === undefined && written.deny === undefined) {
            await remember(engine, eventId, { as: written.as, uuid: written.uuid })
          }
        } catch (error) {
          settle.reject(error)
          return
        }
        settle.resolve(written)
      },
    )
    writing.set(eventId, run)
    void run.finally(() => {
      if (writing.get(eventId) === run) writing.delete(eventId)
    })
    return settle.promise
  }

  /** Make the call a request names and answer with its own value. */
  const invoke = async (engine: ModHost, request: ModRequest): Promise<ModResultValue> => {
    switch (request.call) {
      case "session.message": {
        const { eventId } = request.args
        return eventId === undefined
          ? await writeMessage(engine, request.args)
          : await writeOnce(engine, eventId, request.args)
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
      // Each request runs on its own: `session.message` serialises what has to be serialised
      // inside itself — the write for one Envoy event id, and the choice between a row and a
      // prompt, which is made in one tick (`writeOnce`, `writeMessage`). A chain here would
      // instead hold a call behind a `$.command.run` waiting on a dialog at the terminal.
      for (const request of requests) void answer(request).catch(() => undefined)
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
        CLAUDE_CONFIG_DIR: await engine.configDirectory(),
        HOME: await engine.homeDirectory(),
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

  /** A turn started: it reads every row stored for it, including the ones stored while the
   *  prompt that started it was still in flight. */
  const turnStarted = (turnId: string): void => {
    turn = turnId
    prompting = false
    promptEpoch += 1
    for (const entry of watched.values()) entry.turnId ??= turnId
    send({ kind: "turn-start", turnId })
  }

  /** The turn ended: every row of this mod's that no request of it read is carried by one
   *  prompt of its own, whatever went unread (`unreadRowPrompt`). A second prompt would sit
   *  behind the turn this one starts, so the one prompt names them all. */
  const turnEnded = (turnId: string, reason: ModTurnReason): void => {
    if (turn === turnId) turn = undefined
    const unread: ModUnreadMessage[] = []
    for (const [uuid, entry] of watched) {
      if (entry.turnId !== turnId) continue
      watched.delete(uuid)
      if (entry.requests === requests) unread.push(entry.message)
    }
    send({ kind: "turn-complete", turnId, stopReason: reason })
    if (unread.length > 0) pointAt(unread)
  }

  /** One row of the conversation, stamped where this mod wrote it. */
  const row = (arrived: {
    readonly uuid: string
    readonly door: ModAppendDoor
    readonly origin: ModAppendOrigin
    readonly message: ModAppendMessage
  }): void => {
    const wrote =
      arrived.door === "prompt" ? claimRow(arrived.uuid, arrived.message.content) : undefined
    send({ kind: "append", ...arrived, ...(wrote === undefined ? {} : { wrote }) })
  }

  return {
    begin,
    send,
    turnStarted,
    turnEnded,
    stepRequested: (): void => {
      requests += 1
    },
    row,
    followSessionId,
    effortNow: (): ModEffort | null => effort,
  }
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
    link.row({ uuid: e.uuid, door: e.door, origin: e.origin, message: e.message })
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
        configDirectory: () => $.env.get("CLAUDE_CONFIG_DIR"),
        homeDirectory: () => $.env.get("HOME"),
        append: (args) => $.session.append(args),
        submit: (args) => $.prompt.submit(args),
        runCommand: (args) => $.command.run(args),
        listCommands: () => $.command.list(),
        fork: (request) => $.model.fork(request),
        setConfig: (args) => $.config.set(args),
        listConfig: () => $.config.list(),
        storeGet: (key) => $.store.get(key),
        storeSet: (key, value) => $.store.set(key, value),
        storeDelete: (key) => $.store.delete(key),
        storeKeys: () => $.store.keys(),
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
    link.turnStarted(e.turnId)
    return await next(e)
  })

  on("turn.complete", async (_, e, next) => {
    link.turnEnded(e.turnId, e.reason)
    return await next(e)
  })

  on("turn.step", async function* (_, e, next) {
    // The effort the request really carries. Claude Code names the effort of the model the
    // request is for, and names none for a model that takes none: measured on 2.1.310, a request
    // of `claude-opus-5-5` before any pick carried `output_config.effort: medium`, while with a
    // pick of `low` in force a request of `claude-haiku-4-5` carried `thinking=None` and dropped
    // the effort set here (the stack's model-request log, 2026-11-05). So the server's pick rides
    // on an effort Claude Code named and never fills in an absent one, and the step reports what
    // went out rather than what was set: a step that echoed a pick its own request did not carry
    // held the live view's header on a level nothing was thinking at (INFRA-1026).
    const set = link.effortNow()
    const request: TurnStepInput =
      set === null || e.effort === undefined ? e : { ...e, effort: set }
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
    // Before the request: the rows stored by now are the ones it carries, and a row stored
    // after this moment is in no request of this step. Counted here rather than reported,
    // because the one reader of that count is this mod (`SessionLink.stepRequested`).
    link.stepRequested()
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
