/**
 * The mod's own hooks (`../hooks/mod.ts`), driven the way Claude Code drives them: `register`
 * hands its hooks over, `session.start` gives the link a `$` of this file's own, and the server on
 * the far side of the socket is the real one (`../src/mod-link.ts`), so a `set-effort` is in force
 * here exactly as the live view puts it in force.
 *
 * The module holds one link per load, as a mod loaded into a session does, so the whole file
 * shares one session: it is started once and every test drives steps through it.
 */
import { afterAll, beforeAll, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { request as httpRequest } from "node:http"
import { tmpdir } from "node:os"
import { join } from "node:path"
import type {
  EngineInterface,
  HookStream,
  HttpInit,
  HttpResponse,
  On,
  SessionStartInput,
  SessionStartResult,
  TurnStepChunk,
  TurnStepInput,
  TurnStepResult,
} from "claude-code"
import { register } from "../hooks/mod"
import { MOD_MIN_CLAUDE_VERSION, type ModEvent } from "../hooks/mod-protocol"
import { type ModLink, startModLink } from "../src/mod-link"

/** The hook `on("session.start", ...)` takes, as `On` declares it. */
type SessionStartHook = (
  $: EngineInterface,
  e: SessionStartInput,
  next: (e: SessionStartInput) => Promise<SessionStartResult>,
) => Promise<SessionStartResult>

/** The hook `on("turn.step", ...)` takes, as `On` declares it. */
type StepHook = (
  $: EngineInterface,
  e: TurnStepInput,
  next: (e: TurnStepInput) => HookStream<TurnStepChunk, TurnStepResult>,
) => AsyncGenerator<TurnStepChunk, TurnStepResult | undefined>

/** The session this file drives, once it has started. */
interface Session {
  readonly server: ModLink
  readonly engine: EngineInterface
  readonly step: StepHook
}

/** Short on purpose: the socket is `<TMPDIR>/claude-envoy-<id>.sock` and that whole path has 100
 *  bytes to fit in (`MOD_SOCKET_PATH_MAX_BYTES`). */
const SESSION_ID = "mod-step"

/** A model Claude Code names an effort for: measured on 2.1.310, a request of it before any pick
 *  carried `output_config.effort: medium`. */
const MODEL_WITH_EFFORT = "claude-opus-5-5"

/** A model Claude Code names no effort for: measured on 2.1.310, its requests carried
 *  `thinking=None` while a pick of `low` was in force. */
const MODEL_WITHOUT_EFFORT = "claude-haiku-4-5"

let session: Session | undefined
let scratch: string | undefined
/** True once the tests are over: the mod's waits end at once from then on, so its poll loop spends
 *  its reconnect backoff instantly on the socket this file has unlinked instead of sleeping 31 s
 *  into the next test file. */
let over = false

/** One request over the socket, as the host's own `$.http.fetch({ socketPath })` makes it. */
function fetchOverSocket(url: string, init: HttpInit = {}): Promise<HttpResponse> {
  const { promise, resolve, reject } = Promise.withResolvers<HttpResponse>()
  const target = new URL(url)
  const sent = httpRequest(
    {
      socketPath: init.socketPath,
      path: target.pathname,
      method: init.method ?? "GET",
      headers: { host: target.host, ...(init.headers ?? {}) },
    },
    (response) => {
      let text = ""
      response.setEncoding("utf8")
      response.on("data", (chunk: string) => {
        text += chunk
      })
      response.on("end", () => {
        const status = response.statusCode ?? 0
        const headers: Record<string, string> = {}
        for (const [name, value] of Object.entries(response.headers)) {
          if (typeof value === "string") headers[name] = value
        }
        resolve({ status, ok: status >= 200 && status < 300, headers, text })
      })
    },
  )
  sent.on("error", reject)
  sent.end(init.body)
  return promise
}

/** The `$` the hooks are given: what the mod calls on it over a step, and nothing else. A call no
 *  test makes rejects rather than answering something made up. */
function engineOn(directory: string): EngineInterface {
  const unused = (call: string): Promise<never> =>
    Promise.reject(new Error(`$.${call} is no call of this test`))
  return {
    session: {
      id: async () => SESSION_ID,
      version: async () => ({ version: MOD_MIN_CLAUDE_VERSION }),
      model: async () => MODEL_WITH_EFFORT,
      append: () => unused("session.append"),
    },
    prompt: { submit: () => unused("prompt.submit") },
    command: { list: () => unused("command.list"), run: () => unused("command.run") },
    config: { list: () => unused("config.list"), set: () => unused("config.set") },
    model: { fork: () => unused("model.fork") },
    http: { fetch: (url, init) => fetchOverSocket(url, init) },
    env: { get: async (name) => (name === "TMPDIR" ? directory : undefined) },
    clock: {
      sleep: async (ms) => {
        if (!over) await Bun.sleep(ms)
      },
    },
  }
}

/** The session, or the reason there is none to drive. */
function started(): Session {
  if (session === undefined) throw new Error("the session did not start")
  return session
}

/** The engine beneath the hook: it keeps the request it was handed and streams one text chunk. */
function beneath(
  seen: TurnStepInput[],
): (e: TurnStepInput) => HookStream<TurnStepChunk, TurnStepResult> {
  return (e: TurnStepInput): HookStream<TurnStepChunk, TurnStepResult> => {
    seen.push(e)
    const result: TurnStepResult = { turnId: e.turnId, index: e.index, stopReason: "end_turn" }
    const stream = (async function* (): AsyncGenerator<TurnStepChunk, TurnStepResult> {
      yield { kind: "text", index: 0, text: "ready" }
      return result
    })()
    return Object.assign(stream, { result: Promise.resolve(result) })
  }
}

/** Run one step through the hook as Claude Code runs it - the stream read to its end - and answer
 *  with the request that reached the engine beneath. */
async function runStep(input: TurnStepInput): Promise<TurnStepInput> {
  const seen: TurnStepInput[] = []
  const { engine, step } = started()
  const chunks: TurnStepChunk[] = []
  for await (const chunk of step(engine, input, beneath(seen))) chunks.push(chunk)
  expect(chunks).toHaveLength(1)
  const requested = seen[0]
  if (requested === undefined) throw new Error("the hook made no request")
  return requested
}

/** The step snapshot the server reads once the step is whole, with everything sent before it -
 *  the hello, the model, the step's own `step-request` and its streaming snapshots - passed over. */
async function finishedStep(): Promise<Extract<ModEvent, { kind: "step" }>> {
  const { server } = started()
  for (;;) {
    const next = await server.events.next()
    if (next.done === true) throw new Error("the link ended before the step was whole")
    if (next.value.kind === "step" && next.value.done) return next.value
  }
}

beforeAll(async () => {
  const directory = await mkdtemp(join(tmpdir(), "mod-"))
  scratch = directory
  const server = await startModLink({ sessionId: SESSION_ID, env: { TMPDIR: directory } })
  const hooks = new Map<string, unknown>()
  const collect: On = (pattern: string, ...rest: unknown[]): void => {
    hooks.set(pattern, rest[rest.length - 1])
  }
  register(collect)
  const engine = engineOn(directory)
  const start = hooks.get("session.start") as SessionStartHook
  await start(engine, { cwd: directory, isInteractive: true }, async (e) => ({ cwd: e.cwd }))
  session = { server, engine, step: hooks.get("turn.step") as StepHook }
  // The mod probes the socket off the hook, so the link is up a moment after `session.start`
  // returns; the server says when, rather than this file guessing a delay.
  while (!server.connected()) await Bun.sleep(5)
})

afterAll(async () => {
  over = true
  await session?.server.close()
  session = undefined
  if (scratch !== undefined) await rm(scratch, { recursive: true, force: true })
})

// Measured live on 2.1.310: after a pick of `low` the requests of `claude-haiku-4-5` carried
// `thinking=None`, the model taking no effort whatever the mod set, while the mod went on
// reporting `low` on every step and the live view's header read `low` for a session thinking at no
// level at all. The step reports the request that ran, so the pick is left off a request Claude
// Code names no effort for, and off its step (INFRA-1026).
test("a step of a model with no effort of its own runs and reports none, pick or no pick", async () => {
  const { server } = started()
  expect(await server.request("set-effort", { effort: "low" })).toEqual({ effort: "low" })

  const requested = await runStep({
    turnId: "turn-1",
    index: 0,
    model: MODEL_WITHOUT_EFFORT,
    messageCount: 2,
  })

  // The step is read off the link before anything is asserted: a test that left its own
  // snapshots unread would hand them to the next one, whose failure would then name this step.
  const sent = await finishedStep()
  expect(requested.effort).toBeUndefined()
  expect(sent.model).toBe(MODEL_WITHOUT_EFFORT)
  expect(sent.effort).toBeUndefined()
})

// The other half of the same rule, and what makes a thinking pick work at all: a model Claude Code
// names an effort for takes the picked one in place of it, and the step says so.
test("a step of a model with an effort of its own runs and reports the picked one", async () => {
  const { server } = started()
  expect(await server.request("set-effort", { effort: "low" })).toEqual({ effort: "low" })

  const requested = await runStep({
    turnId: "turn-2",
    index: 0,
    model: MODEL_WITH_EFFORT,
    effort: "max",
    messageCount: 4,
  })

  const sent = await finishedStep()
  expect(requested.effort).toBe("low")
  expect(sent.model).toBe(MODEL_WITH_EFFORT)
  expect(sent.effort).toBe("low")
})
