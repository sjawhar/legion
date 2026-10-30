import { afterEach, expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { SessionAnswer } from "@legion/contracts"
import {
  renderSessionAnswers,
  SESSION_ANSWERS_DEADLINE_MS,
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers"

// The answers hook as Claude Code runs it: the committed bundle (hooks.json names
// `dist/session-hook.js answers`), its whole environment given, its input on stdin. One stand-in
// plays Dispatch, whose answers route each case sets to answer with a list or to hang.
const packageRoot = resolve(import.meta.dir, "..")
const bundle = join(packageRoot, "dist", "session-hook.js")
const SESSION = "11111111-2222-4333-8444-555555555555"

const cleanup: (() => Promise<void>)[] = []
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step()
})

type Route =
  | { readonly kind: "list"; readonly items: readonly SessionAnswer[] }
  | { readonly kind: "hang" }

/** The stand-in Dispatch: its answers route answers as `route.kind` says at the time of each
 *  request; every request is recorded with its bearer. */
function standinDispatch(): {
  readonly base: string
  readonly requests: string[]
  route: Route
} {
  const state = {
    base: "",
    requests: [] as string[],
    route: { kind: "list", items: [] } as Route,
  }
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const url = new URL(request.url)
      state.requests.push(`${request.headers.get("authorization")} ${url.pathname}`)
      if (url.pathname !== `/api/v1/agents/${SESSION}/answers`) {
        return new Response("not found", { status: 404 })
      }
      const { route } = state
      if (route.kind === "hang") return new Promise<Response>(() => {})
      return Response.json({
        session_id: SESSION,
        since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
        total: route.items.length,
        items: route.items,
      })
    },
  })
  cleanup.push(async () => {
    await server.stop(true)
  })
  state.base = `http://127.0.0.1:${server.port}`
  return state
}

/** A person's answer to the session's ask, written `minutesAgo` minutes ago. */
function answer(id: string, text: string, minutesAgo: number): SessionAnswer {
  return {
    kind: "answer",
    relation: "answered_your_ask",
    id,
    at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
    author: { kind: "user", id: "alice@example.com" },
    text,
    selected: ["The second way"],
    ask: { id, question: "Which way should the fix go?" },
    owner: { issue: { key: "LEGION-1", title: "The work" } },
    ref: `dispatch://LEGION-1/ask/${id}`,
    href: `/issues/LEGION-1#ask-${id}`,
  }
}

/** A scratch HOME, Dispatch state directory, Claude config directory and TMPDIR for one case. */
async function scratch(): Promise<{
  readonly home: string
  readonly state: string
  readonly config: string
  readonly tmp: string
}> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-answers-"))
  cleanup.push(() => rm(root, { recursive: true, force: true }))
  const home = join(root, "home")
  const state = join(root, "state")
  const config = join(root, "claude")
  const tmp = join(root, "tmp")
  for (const directory of [home, state, config, tmp]) await mkdir(directory, { recursive: true })
  return { home, state, config, tmp }
}

/** Waits, 15 s at most, until the session's refresher (the process an answers hook starts to run
 *  a `!` token command) has finished: what it read written after `since` (epoch ms), and its lock
 *  gone. */
async function refresherFinished(state: string, since: number): Promise<void> {
  const directory = join(state, "sessions", SESSION)
  const until = Date.now() + 15_000
  while (Date.now() < until) {
    const outcome = await readFile(join(directory, "answers-refresh.json"), "utf8").then(
      (text): { finished_at: number } => JSON.parse(text),
      () => undefined,
    )
    if (
      outcome !== undefined &&
      outcome.finished_at >= since &&
      !existsSync(join(directory, "answers-refresh.lock"))
    ) {
      return
    }
    // The refresher is another process, detached from the hook, that tells nobody it ended: its
    // files are the only signal, so the wait polls them on the real clock.
    await Bun.sleep(50)
  }
  throw new Error("the refresher did not finish within 15 s")
}

/** Waits, 15 s at most, until `check` holds: for a process that tells nobody what it did. */
async function eventually(check: () => boolean, what: string): Promise<void> {
  const until = Date.now() + 15_000
  while (!check()) {
    if (Date.now() >= until) throw new Error(`${what} did not happen within 15 s`)
    // Another process's state (a file, a process table entry) is the only signal: polled on the
    // real clock, as `refresherFinished` polls.
    await Bun.sleep(20)
  }
}

/** A `--preload` for a hook that writes `signal` once its process has started, then holds the
 *  hook's code until a refresher has finished after that start (its outcome written, its lock
 *  gone), 15 s at most. The hook then finds that outcome at its first look, and starts a refresher
 *  of its own, so where a refresher's race with the hook's deadline would decide the case, this
 *  decides it. */
async function preloadAfterRefresher(home: string, state: string, signal: string): Promise<string> {
  const directory = join(state, "sessions", SESSION)
  const outcome = JSON.stringify(join(directory, "answers-refresh.json"))
  const lock = JSON.stringify(join(directory, "answers-refresh.lock"))
  const preload = join(home, "after-refresher.ts")
  await writeFile(
    preload,
    [
      `import { existsSync, readFileSync, writeFileSync } from "node:fs"`,
      `const startedAt = Date.now() - performance.now()`,
      `writeFileSync(${JSON.stringify(signal)}, "")`,
      `while (performance.now() < 15_000) {`,
      `  let finishedAt = 0`,
      `  try {`,
      `    finishedAt = JSON.parse(readFileSync(${outcome}, "utf8")).finished_at`,
      `  } catch {}`,
      `  if (finishedAt >= startedAt && !existsSync(${lock})) break`,
      `  Bun.sleepSync(20)`,
      `}`,
      "",
    ].join("\n"),
  )
  return preload
}

/** What the hook measured of itself, written to `ENVOY_ANSWERS_TIMING_FILE`, in ms after its
 *  process started: when its code began, when it printed (null: it failed before), and when it
 *  exited. */
interface HookTiming {
  readonly began: number
  readonly printed: number | null
  readonly ended: number
}

interface HookRun {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
  readonly timing: HookTiming
}

async function runHook(
  env: Record<string, string>,
  input: Record<string, unknown>,
  options: { readonly preload?: string } = {},
): Promise<HookRun> {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-answers-timing-"))
  cleanup.push(() => rm(directory, { recursive: true, force: true }))
  const timingFile = join(directory, "timing.json")
  const preload = options.preload === undefined ? [] : ["--preload", options.preload]
  const run = Bun.spawn(["bun", ...preload, bundle, "answers"], {
    env: {
      PATH: process.env["PATH"] ?? "",
      CLAUDE_PLUGIN_ROOT: packageRoot,
      ENVOY_ANSWERS_TIMING_FILE: timingFile,
      ...env,
    },
    stdin: new TextEncoder().encode(JSON.stringify(input)),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    run.exited,
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
  ])
  const timing: HookTiming = JSON.parse(await readFile(timingFile, "utf8"))
  return { exitCode, stdout, stderr, timing }
}

/** The hook's print deadline as the design states it, in ms after its process started: 800 ms,
 *  or `ANSWERS_LEAST_WAIT_MS` (150 ms) after its code began when bun's start-up took longer. */
function printDeadline(run: HookRun): number {
  return Math.max(SESSION_ANSWERS_DEADLINE_MS, run.timing.began + 150)
}

/** The reason a hook whose refresher had not finished by its print deadline names. */
function notFinishedReason(run: HookRun): string {
  return `a "!" command in envoy.json had not finished ${Math.round(printDeadline(run))} ms after the hook started`
}

/** How far past its deadline the hook may print, for what the design does not bound: the time the
 *  host does not run it. Unloaded, the hook prints within 15 ms of its deadline; on a 2-core pod
 *  with four busy loops it printed up to 650 ms past it, a stall with no wait of its own (a read
 *  Dispatch had answered). A hook waiting on what the design says it must not (a token command of
 *  seconds, a gate never opened, Dispatch's 10 s read timeout) overruns by more than this; the
 *  exact budgets it waited are checked from what it printed (`notFinishedReason`,
 *  `expectReadBudget`), which no scheduling changes. */
const DEADLINE_SLACK_MS = 1_000

/** Holds the hook to the wait the design bounds, measured by the hook itself from its process's
 *  start, so bun's start-up (which no code of the hook's controls) never decides the check: it
 *  prints by its deadline and exits `ANSWERS_SETTLE_MS` (50 ms) after, give or take
 *  `DEADLINE_SLACK_MS`. No wall time from the spawn is held: on a loaded host the start-up alone
 *  can take seconds. */
function expectWithinDeadline(run: HookRun): void {
  const deadline = printDeadline(run)
  expect(run.timing.printed).not.toBeNull()
  expect(run.timing.printed as number).toBeLessThanOrEqual(deadline + DEADLINE_SLACK_MS)
  expect(run.timing.ended).toBeLessThanOrEqual(deadline + 50 + DEADLINE_SLACK_MS)
}

/** Holds a read that did not answer to the budget the design gives it, `waitedMs` as the hook's
 *  line names it: what was left of the deadline when the read started, never less than 150 ms
 *  (`ANSWERS_LEAST_WAIT_MS`), so at most what was left when the hook's code began. */
function expectReadBudget(run: HookRun, waitedMs: number): void {
  expect(waitedMs).toBeGreaterThanOrEqual(150)
  expect(waitedMs).toBeLessThanOrEqual(
    Math.max(150, Math.round(printDeadline(run) - run.timing.began)),
  )
}

/** A read the hook gave up on, as its line names it: Dispatch's answer missed the hook's wait. */
const READ_MISSED = /^Dispatch did not answer within \d+ ms$/

/** The same read, held to the least wait the design gives it, 150 ms (`ANSWERS_LEAST_WAIT_MS`):
 *  a hook whose start-up spent the whole deadline still waits that long, so a line naming a
 *  shorter wait, "within 0 ms" above all, fails the test. */
const READ_MISSED_LEAST_WAIT = /^Dispatch did not answer within (1[5-9]\d|[2-9]\d\d|\d{4,}) ms$/

/** A refresher the hook gave up on, as its line names it for `run`'s own deadline. */
function refresherMissed(run: HookRun): RegExp {
  return new RegExp(`^${notFinishedReason(run).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`)
}

/** Which of the outcomes the design allows a hook whose start-up may have spent its budget
 *  `context` is: `fresh` (the list Dispatch holds, `items`), `cached` (that list as a hook read it
 *  before, its second line naming a `reason` this read failed), or `failure` (the failure line
 *  naming a `reason`, no list having been read). Anything else fails the test. On an unloaded host
 *  the hook gets the fresh list; on a loaded one its 150 ms read, or the refresher, can miss. */
function allowedOutcome(
  context: string,
  items: readonly SessionAnswer[],
  reason: RegExp,
): "fresh" | "cached" | "failure" {
  const fresh = renderSessionAnswers(items, { now: Date.now(), total: items.length }) as string
  if (context === fresh) return "fresh"
  const lines = context.split("\n")
  const header = /^As read at \S+; this read failed: (.*?)\.?$/.exec(lines[1] ?? "")
  if (header !== null && [lines[0], ...lines.slice(2)].join("\n") === fresh) {
    expect(header[1]).toMatch(reason)
    return "cached"
  }
  const prefix = sessionAnswersFailureLine("\u0000").split("\u0000")
  expect(context.startsWith(prefix[0] as string) && context.endsWith(prefix[1] as string)).toBe(
    true,
  )
  expect(context.slice((prefix[0] as string).length, -(prefix[1] as string).length)).toMatch(reason)
  return "failure"
}

function sessionStart(source: string, cwd: string): Record<string, unknown> {
  return { session_id: SESSION, source, cwd, hook_event_name: "SessionStart" }
}

/** The hook's additionalContext, checked to be under the event that ran it. */
function contextOf(run: HookRun, event: string): string {
  expect(run.exitCode).toBe(0)
  expect({ stdout: run.stdout === "" ? "" : "printed", stderr: run.stderr }).toMatchObject({
    stdout: "printed",
  })
  const output = JSON.parse(run.stdout)
  expect(output.hookSpecificOutput.hookEventName).toBe(event)
  const context: string = output.hookSpecificOutput.additionalContext
  // Claude Code keeps an additionalContext string whole only up to 10,000 characters.
  expect(context.length).toBeLessThan(10_000)
  return context
}

/** Runs `startup` hooks, four at most, until one has left what Dispatch answers in `answers.json`
 *  for the hooks after it, and gives back what was saved. Each run is held to what the design
 *  allows a hook reading a list that adds nothing: it prints nothing, its read having landed on
 *  an empty list, or it prints the failure line naming the wait its read was given. A hook whose
 *  read missed that wait also exits soon after printing, which the read it left behind need not
 *  beat, so on a loaded host one run does not always fill the cache. */
async function fillAnswersCache(
  env: Record<string, string>,
  home: string,
  state: string,
): Promise<{ readonly items: readonly SessionAnswer[] }> {
  const file = join(state, "sessions", SESSION, "answers.json")
  const attempts = 4
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const run = await runHook(env, sessionStart("startup", home))
    expect(run.exitCode).toBe(0)
    if (run.stdout !== "") {
      expect(allowedOutcome(contextOf(run, "SessionStart"), [], READ_MISSED_LEAST_WAIT)).toBe(
        "failure",
      )
    }
    const saved = await readFile(file, "utf8")
      .then((text): { items: SessionAnswer[] } => JSON.parse(text))
      .catch(() => undefined)
    if (saved !== undefined) return saved
  }
  throw new Error(`no hook left what Dispatch answered in ${file} within ${attempts} runs`)
}

test("puts the list into context at every SessionStart source and at SubagentStart, read for the session the input names", async () => {
  const dispatch = standinDispatch()
  const items = [
    answer("ask-2", "Go the second way; keep the cache.", 5),
    answer("ask-1", "Not yet.", 60),
  ]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }
  const expected = renderSessionAnswers(items, { now: Date.now(), total: items.length })
  expect(expected?.startsWith(SESSION_ANSWERS_MARKER)).toBe(true)

  // Each hook carries the list Dispatch holds, or, when its start-up left the read only 150 ms
  // and Dispatch's answer missed it, the list a hook read before or the failure line, each naming
  // the missed read.
  const outcomes: string[] = []
  for (const source of ["startup", "resume", "clear", "compact", "fork"]) {
    const run = await runHook(env, sessionStart(source, home))
    outcomes.push(allowedOutcome(contextOf(run, "SessionStart"), items, READ_MISSED))
  }
  const subagent = await runHook(env, {
    session_id: SESSION,
    cwd: home,
    hook_event_name: "SubagentStart",
    agent_id: "agent-1",
    agent_type: "general-purpose",
  })
  outcomes.push(allowedOutcome(contextOf(subagent, "SubagentStart"), items, READ_MISSED))
  // Every read was for the session the input names, with the configured bearer.
  expect(dispatch.requests.length).toBeGreaterThanOrEqual(1)
  expect(dispatch.requests.length).toBeLessThanOrEqual(6)
  for (const request of dispatch.requests) {
    expect(request).toBe(`Bearer hook-token /api/v1/agents/${SESSION}/answers`)
  }
  // A read that answered left the list for the next hook process.
  if (outcomes.includes("fresh")) {
    const file = join(state, "sessions", SESSION, "answers.json")
    expect(JSON.parse(await readFile(file, "utf8")).items).toEqual(items)
  }
}, 20_000)

test("with Dispatch hanging, answers by the hook's deadline with the list a hook read before", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-3", "Read before Dispatch hung.", 2)]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }
  contextOf(await runHook(env, sessionStart("startup", home)), "SessionStart")
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))

  dispatch.route = { kind: "hang" }
  const run = await runHook(env, sessionStart("compact", home))
  const context = contextOf(run, "SessionStart")
  // The header's second line names when the list was read and why this read failed.
  const waited =
    /^.*\nAs read at .*; this read failed: Dispatch did not answer within (\d+) ms\.\n/.exec(
      context,
    )
  expect(waited).not.toBeNull()
  expect(context).toBe(
    renderSessionAnswers(items, {
      now: Date.now(),
      total: 1,
      cached: { readAt: cache.read_at, reason: `Dispatch did not answer within ${waited?.[1]} ms` },
    }) as string,
  )
  expect(context).toContain("Read before Dispatch hung.")
  expectWithinDeadline(run)
  expectReadBudget(run, Number(waited?.[1]))
  expect(dispatch.requests).toHaveLength(2)
}, 20_000)

test("with Dispatch hanging after a read gave an empty list, the failure line names the wait rather than nothing", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [] }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }
  // The hang the case is about starts only once the empty list Dispatch holds is in the cache:
  // a hook whose read missed its wait prints the failure line and saves nothing, so the read is
  // repeated until it lands.
  const cache = await fillAnswersCache(env, home, state)
  expect(cache.items).toEqual([])

  dispatch.route = { kind: "hang" }
  const context = contextOf(await runHook(env, sessionStart("resume", home)), "SessionStart")
  const waited =
    /^\[Dispatch: people's answers from the last 72 hours could not be read \(Dispatch did not answer within (\d+) ms\); run `dispatch answers`\.\]$/.exec(
      context,
    )
  expect(waited).not.toBeNull()
}, 20_000)

test("with Dispatch hanging and no list ever read, the failure line names the wait", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "hang" }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }

  const run = await runHook(env, sessionStart("startup", home))
  const context = contextOf(run, "SessionStart")
  const waited =
    /^\[Dispatch: people's answers from the last 72 hours could not be read \(Dispatch did not answer within (\d+) ms\); run `dispatch answers`\.\]$/.exec(
      context,
    )
  expect(waited).not.toBeNull()
  expect(context).toBe(
    sessionAnswersFailureLine(`Dispatch did not answer within ${waited?.[1]} ms`),
  )
  expectWithinDeadline(run)
  expectReadBudget(run, Number(waited?.[1]))
}, 20_000)

test("a list past the cap stays under 8,000 characters with the left-out line", async () => {
  const dispatch = standinDispatch()
  const items = Array.from({ length: 12 }, (_, index) =>
    answer(`ask-${index}`, `${index}: ${"word ".repeat(180)}`, index + 1),
  )
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }

  // A hook whose read missed its wait carries the failure line (no list read yet) or the list a
  // hook read before; whichever list a hook shows, fresh or read before, keeps to the cap.
  let shown: string | undefined
  for (const source of ["startup", "resume", "clear"]) {
    const context = contextOf(await runHook(env, sessionStart(source, home)), "SessionStart")
    if (allowedOutcome(context, items, READ_MISSED) !== "failure") {
      shown = context
      break
    }
  }
  expect(shown).toBeDefined()
  expect((shown as string).length).toBeLessThan(8_000)
  expect(shown).toMatch(
    /\n\n\d+ older items left out to stay under 8,000 characters: run `dispatch answers` to read every one in full\.$/,
  )
}, 20_000)

test("adds nothing when the list is empty or Dispatch is not configured", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [] }
  const { home, state } = await scratch()

  const empty = await runHook(
    {
      HOME: home,
      DISPATCH_STATE_DIR: state,
      DISPATCH_URL: dispatch.base,
      DISPATCH_TOKEN: "hook-token",
    },
    sessionStart("startup", home),
  )
  // An empty list adds nothing; a read that missed the hook's wait, with no list read before,
  // says so rather than looking like nothing.
  if (empty.stdout !== "") {
    expect(allowedOutcome(contextOf(empty, "SessionStart"), [], READ_MISSED)).toBe("failure")
  }
  expect(empty.exitCode).toBe(0)
  const off = await runHook(
    { HOME: home, DISPATCH_STATE_DIR: state },
    sessionStart("startup", home),
  )
  expect(off).toMatchObject({ exitCode: 0, stdout: "", stderr: "" })
  // The empty list came from the one read. A read that missed the wait (the hook printed the
  // failure line) may end with the hook's process before it reaches Dispatch. Dispatch off sends
  // nothing.
  const expectedRequests = empty.stdout === "" ? [1] : [0, 1]
  expect(expectedRequests).toContain(dispatch.requests.length)
}, 20_000)

test("a `!` token command runs outside the hook: a quick one gives the bearer, a slow one cannot hold the hook", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-4", "Token from a command.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const bearer = `Bearer command-token /api/v1/agents/${SESSION}/answers`

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!printf command-token" } }),
  )
  const quickSince = Date.now()
  const quick = await runHook(env, sessionStart("startup", home))
  // The command runs in the refresher, a second bun process: the design carries its list when it
  // lands by the hook's deadline, and otherwise names the unfinished command, on the list the
  // refresher already left in answers.json or on the failure line.
  allowedOutcome(contextOf(quick, "SessionStart"), items, refresherMissed(quick))
  expectWithinDeadline(quick)
  await refresherFinished(state, quickSince)
  expect(dispatch.requests).toEqual([bearer])
  // Either way the next hook carries the list the refresher read with the command's token.
  const nextSince = Date.now()
  const next = await runHook(env, sessionStart("clear", home))
  expect(contextOf(next, "SessionStart")).toContain("Token from a command.")
  await refresherFinished(state, nextSince)
  expect(dispatch.requests).toEqual([bearer, bearer])

  // The slow command waits until the test opens the gate, so no refresher lands by a deadline.
  const gate = join(home, "gate")
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf late`,
      },
    }),
  )
  const slowSince = Date.now()
  // Before the scratch directory goes, the slow command's refresher ends.
  cleanup.push(async () => {
    await writeFile(gate, "")
    await refresherFinished(state, slowSince)
  })
  const hanging = await runHook(env, sessionStart("resume", home))
  const context = contextOf(hanging, "SessionStart")
  // The list read before stands in, naming why this read gave none.
  expect(context.split("\n")[1]).toStartWith("As read at ")
  expect(context.split("\n")[1]).toEndWith(`; this read failed: ${notFinishedReason(hanging)}.`)
  expect(context).toContain("Token from a command.")
  expectWithinDeadline(hanging)

  // With no list read before, the failure line says why none was read now.
  await rm(join(state, "sessions", SESSION, "answers.json"))
  const uncached = await runHook(env, sessionStart("compact", home))
  expect(contextOf(uncached, "SessionStart")).toBe(
    sessionAnswersFailureLine(notFinishedReason(uncached)),
  )
  expectWithinDeadline(uncached)
  expect(dispatch.requests).toHaveLength(2)

  // The slow command still ends, and its refresher reads with the token it printed.
  await writeFile(gate, "")
  await refresherFinished(state, slowSince)
  expect(dispatch.requests).toEqual([
    bearer,
    bearer,
    `Bearer late /api/v1/agents/${SESSION}/answers`,
  ])
  expect(await readdir(tmp)).toEqual([])
}, 20_000)

/** Processes whose command line holds `marker`, as `pgrep -f` finds them. */
function processesWith(marker: string): string {
  return Bun.spawnSync(["pgrep", "-f", marker]).stdout.toString().trim()
}

/** Whether no refresher of the session runs for the project directory `home` (its command line
 *  is `bun <bundle> answers-refresh <session> <directory>`) and none holds the lock. */
function noRefresher(home: string, state: string): boolean {
  return (
    processesWith(`answers-refresh ${SESSION} ${home}`) === "" &&
    !existsSync(join(state, "sessions", SESSION, "answers-refresh.lock"))
  )
}

test("a token command slower than the hook's deadline finishes and cleans up after the hook, and the next hook carries the list it read", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-5", "Read with a slow command's token.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  // The command waits until the test opens the gate, a path no other process names: `pgrep -f`
  // finds the command's shell by it.
  const gate = join(home, "gate")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf hook-token`,
      },
    }),
  )

  const firstSince = Date.now()
  cleanup.push(async () => {
    await writeFile(gate, "")
    await eventually(() => noRefresher(home, state), "the refreshers' end")
  })
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(sessionAnswersFailureLine(notFinishedReason(first)))
  expectWithinDeadline(first)
  // The command outlives the hook, in the refresher the hook started (whose own bun start-up may
  // end after the hook did).
  await eventually(() => processesWith(gate) !== "", "the command's start")

  await writeFile(gate, "")
  await refresherFinished(state, firstSince)
  expect(await readdir(tmp)).toEqual([])
  expect(processesWith(gate)).toBe("")
  expect(dispatch.requests).toEqual([`Bearer hook-token /api/v1/agents/${SESSION}/answers`])

  await rm(gate)
  const secondSince = Date.now()
  const second = await runHook(env, sessionStart("resume", home))
  const context = contextOf(second, "SessionStart")
  expect(context.split("\n")[1]).toStartWith("As read at ")
  expect(context.split("\n")[1]).toEndWith(`; this read failed: ${notFinishedReason(second)}.`)
  expect(context).toContain("Read with a slow command's token.")
  expectWithinDeadline(second)
  await writeFile(gate, "")
  await refresherFinished(state, secondSince)
  expect(processesWith(gate)).toBe("")
}, 20_000)

test("with a slow token command, a list read from another Dispatch than envoy.json names now never stands in", async () => {
  const before = standinDispatch()
  const beforeItems = [answer("ask-a", "Read from the Dispatch before.", 1)]
  before.route = { kind: "list", items: beforeItems }
  const now = standinDispatch()
  now.route = { kind: "list", items: [answer("ask-b", "Read from the Dispatch now.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: before.base, token: "!printf before-token" } }),
  )
  const firstSince = Date.now()
  const first = await runHook(env, sessionStart("startup", home))
  // The first hook carries `before`'s list when its refresher lands by the deadline, and otherwise
  // names the unfinished command, on the list the refresher already left or on the failure line;
  // either way the refresher leaves that list for the next hook.
  allowedOutcome(contextOf(first, "SessionStart"), beforeItems, refresherMissed(first))
  await refresherFinished(state, firstSince)
  expect(before.requests).toEqual([`Bearer before-token /api/v1/agents/${SESSION}/answers`])
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.dispatch_url).toBe(before.base)

  // The command sleeps past any deadline: the hook's is at most 800 ms after its process started
  // or 150 ms after its code began, and the refresher starts after that code.
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: now.base, token: "!sleep 3; printf now-token" } }),
  )
  const since = Date.now()
  cleanup.push(() => refresherFinished(state, since))
  const hanging = await runHook(env, sessionStart("resume", home))
  expect(contextOf(hanging, "SessionStart")).toBe(
    sessionAnswersFailureLine(notFinishedReason(hanging)),
  )
  expectWithinDeadline(hanging)

  // The refresher reads the Dispatch named now, with the token its command printed.
  await refresherFinished(state, since)
  expect(now.requests).toEqual([`Bearer now-token /api/v1/agents/${SESSION}/answers`])
  expect(before.requests).toHaveLength(1)
}, 20_000)

test("a refresher started for the Dispatch envoy.json named before never hands its list to a hook for the Dispatch it names now", async () => {
  // The refresher is shared by every hook of the session, whatever Dispatch each resolves. A
  // refresher started while envoy.json named `before` that finishes after a later hook, started
  // after envoy.json moved to `now`, began must not give that hook `before`'s list, the
  // invariant the review asked for ("no list from another Dispatch URL").
  const before = standinDispatch()
  before.route = { kind: "list", items: [answer("ask-x", "Read from the Dispatch before.", 1)] }
  const now = standinDispatch()
  now.route = { kind: "list", items: [] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const gate = join(home, "gate")

  // The first hook's refresher runs the command, which waits until the gate opens.
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: before.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf before-token`,
      },
    }),
  )
  cleanup.push(async () => {
    await writeFile(gate, "")
    await eventually(() => noRefresher(home, state), "the refreshers' end")
  })
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(sessionAnswersFailureLine(notFinishedReason(first)))

  // The refresher resolves envoy.json before its command runs: once the command runs, it reads
  // `before` whatever envoy.json names next.
  await eventually(() => processesWith(gate) !== "", "the command's start")

  // envoy.json now names another Dispatch. A hook starts while the refresher runs; it opens the
  // gate once its process has started and holds its code until the refresher has read `before`
  // and finished, so the hook finds that outcome, from after it started, at its first look.
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: now.base, token: "!printf now-token" } }),
  )
  const preload = await preloadAfterRefresher(home, state, gate)
  const context = contextOf(
    await runHook(env, sessionStart("resume", home), { preload }),
    "SessionStart",
  )

  // The refresher did read `before`, so the case is not vacuous.
  expect(before.requests).toEqual([`Bearer before-token /api/v1/agents/${SESSION}/answers`])
  expect(context).not.toContain("Read from the Dispatch before.")
  // The hook names why it carried nothing of that read; the list `before` answered is no list
  // read from `now`, so the failure line stands.
  expect(context).toBe(
    sessionAnswersFailureLine(
      `the refresher read ${before.base}, not the Dispatch envoy.json names now`,
    ),
  )
  // The refresher this hook started reads the Dispatch named now.
  await eventually(() => noRefresher(home, state), "the next refresher's end")
  expect(now.requests).toEqual([`Bearer now-token /api/v1/agents/${SESSION}/answers`])
}, 20_000)

test("a token command that fails gives the hook its error, not a Dispatch envoy.json no longer names", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [answer("ask-f", "Never read: the token failed.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!echo boom >&2; exit 3" } }),
  )
  const failed = sessionAnswersFailureLine(
    `${envoyJson}: dispatch.token's command exited with status 3`,
  )
  cleanup.push(() => eventually(() => noRefresher(home, state), "the refreshers' end"))

  // The waiting hook starts first and holds its code until a refresher started after its process
  // has finished, so it finds that refresher's outcome at its first look; the starting hook,
  // started once the waiting one runs, starts that refresher.
  const ready = join(home, "ready")
  const preload = await preloadAfterRefresher(home, state, ready)
  const waiting = runHook(env, sessionStart("resume", home), { preload })
  await eventually(() => existsSync(ready), "the waiting hook's start")
  const starting = await runHook(env, sessionStart("startup", home))
  const context = contextOf(await waiting, "SessionStart")

  // The refresher targeted the URL envoy.json names, so its failure line, naming the command's
  // exit, stands; never the reason for a refresher that read another Dispatch.
  expect(context).toBe(failed)
  expect(context).toContain("exited with status 3")
  expect(context).not.toContain("not the Dispatch envoy.json names now")
  // The starting hook carries the same line, or names the command as unfinished when its
  // refresher lands after its deadline.
  expect([failed, sessionAnswersFailureLine(notFinishedReason(starting))]).toContain(
    contextOf(starting, "SessionStart"),
  )
  await eventually(() => noRefresher(home, state), "the refreshers' end")
  expect(dispatch.requests).toEqual([])
  expect(await readdir(tmp)).toEqual([])
}, 20_000)

/** Writes, as a refresher would, what it read for `dispatchUrl` (null: no Dispatch), finished a
 *  minute from now so a hook started now takes it as its own refresher's. */
async function refresherOutcome(state: string, dispatchUrl: string | null, text: string) {
  const directory = join(state, "sessions", SESSION)
  await mkdir(directory, { recursive: true })
  await writeFile(
    join(directory, "answers-refresh.json"),
    JSON.stringify({ finished_at: Date.now() + 60_000, dispatch_url: dispatchUrl, text }),
  )
}

test("with envoy.json unreadable, a list read from another Dispatch never stands in: the failure line names why", async () => {
  const other = standinDispatch()
  const items = [answer("ask-o", "Read from a Dispatch envoy.json no longer names.", 1)]
  other.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  // A list read from `other`, in answers.json and as a refresher's fresh outcome.
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: other.base, token: "!printf other-token" } }),
  )
  const since = Date.now()
  await runHook(env, sessionStart("startup", home))
  await refresherFinished(state, since)
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.dispatch_url).toBe(other.base)
  await refresherOutcome(
    state,
    other.base,
    renderSessionAnswers(items, { now: Date.now(), total: 1 }) as string,
  )

  // envoy.json caught mid-edit: no URL can be read from it.
  await writeFile(envoyJson, '{ "dispatch": ')
  const run = await runHook(env, sessionStart("resume", home))
  const context = contextOf(run, "SessionStart")
  const [opening, closing] = sessionAnswersFailureLine("\u0000").split("\u0000") as [string, string]
  expect(context.startsWith(opening) && context.endsWith(closing)).toBe(true)
  expect(context).toContain(envoyJson)
  expect(context).not.toContain("Read from a Dispatch envoy.json no longer names.")
  expect(context).not.toContain("As read at")
  expectWithinDeadline(run)
  // No refresher started for a configuration that names no Dispatch.
  expect(other.requests).toHaveLength(1)
}, 20_000)

test("a list read for the Dispatch envoy.json names stands in even when a refresher read none", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-s", "Read for the Dispatch envoy.json names.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!printf same-token" } }),
  )
  const firstSince = Date.now()
  await runHook(env, sessionStart("startup", home))
  await refresherFinished(state, firstSince)
  expect(dispatch.requests).toEqual([`Bearer same-token /api/v1/agents/${SESSION}/answers`])

  // A refresher that ran while envoy.json was broken recorded no Dispatch and an error line.
  await refresherOutcome(state, null, sessionAnswersFailureLine("envoy.json: invalid JSON"))
  // envoy.json names the same Dispatch again, behind a command that waits on the test's gate.
  const gate = join(home, "gate")
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf same-token`,
      },
    }),
  )
  const since = Date.now()
  cleanup.push(async () => {
    await writeFile(gate, "")
    await refresherFinished(state, since)
  })
  const run = await runHook(env, sessionStart("resume", home))
  const context = contextOf(run, "SessionStart")

  // The list read for this Dispatch stands in, naming why this read gave none; never the stale
  // error line of a refresher that read no Dispatch.
  expect(allowedOutcome(context, items, /not the Dispatch envoy\.json names now$/)).toBe("cached")
  expect(context).toContain("the refresher read no Dispatch, not the Dispatch envoy.json names now")
  expect(context).not.toContain("invalid JSON")
  expectWithinDeadline(run)
}, 20_000)

test("hooks started together run a slow token command once", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [answer("ask-6", "One run.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const runs = join(home, "runs")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!printf 'run\\n' >> '${runs}'; sleep 1.5; printf hook-token`,
      },
    }),
  )

  const since = Date.now()
  const hooks = await Promise.all([
    runHook(env, sessionStart("startup", home)),
    runHook(env, {
      session_id: SESSION,
      cwd: home,
      hook_event_name: "SubagentStart",
      agent_id: "agent-1",
      agent_type: "general-purpose",
    }),
    runHook(env, {
      session_id: SESSION,
      cwd: home,
      hook_event_name: "SubagentStart",
      agent_id: "agent-2",
      agent_type: "general-purpose",
    }),
  ])
  for (const hook of hooks) expect(hook.exitCode).toBe(0)
  await refresherFinished(state, since)
  expect(await readFile(runs, "utf8")).toBe("run\n")
  expect(dispatch.requests).toHaveLength(1)
  expect(await readdir(tmp)).toEqual([])
}, 20_000)

test("a hook input it cannot read adds nothing and still exits 0", async () => {
  const { home } = await scratch()
  const run = await runHook(
    { HOME: home, DISPATCH_URL: "http://127.0.0.1:9", DISPATCH_TOKEN: "t" },
    {
      hook_event_name: "SessionStart",
    },
  )
  expect(run.exitCode).toBe(0)
  expect(run.stdout).toBe("")
  expect(run.stderr).toContain("envoy: people's answers were not added")
}, 20_000)

test("a hook whose start-up took past 800 ms still gives Dispatch its least wait, never none", async () => {
  // Claude Code starts the open-asks, dispatch-first and answers hooks at once beside itself, so on
  // a loaded host the answers hook's own bun start-up can pass 800 ms before its code runs. The
  // preload stands in for that start-up: it holds the process until 820 ms after it started.
  // Dispatch then answers at once, so the session gets the list Dispatch holds now, or, when even
  // the least wait is missed, the failure line naming that wait: never "did not answer within 0 ms".
  const dispatch = standinDispatch()
  const items = [answer("ask-slow", "SLOW-START-ANSWER: written before this resume.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const preload = join(home, "slow-start.ts")
  await writeFile(preload, "while (performance.now() < 820) {}\n")
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN: "hook-token",
  }
  const run = await runHook(env, sessionStart("resume", home), { preload })

  // On a quiet host the read lands and the list is carried. On a loaded one the stand-in can miss
  // even the 150 ms least wait, and the design then allows the failure line; what it never allows
  // is a read given no wait at all, which this case exists to catch ("within 0 ms").
  const outcome = allowedOutcome(
    contextOf(run, "SessionStart"),
    items,
    /^Dispatch did not answer within (1[5-9]\d|[2-9]\d\d|\d{4,}) ms$/,
  )
  expect(run.timing.began).toBeGreaterThanOrEqual(820)
  expectWithinDeadline(run)
  // A read that missed its wait may not have reached the stand-in before the hook exited.
  const read = `Bearer hook-token /api/v1/agents/${SESSION}/answers`
  if (outcome === "fresh") expect(dispatch.requests).toEqual([read])
  else expect(dispatch.requests.filter((request) => request !== read)).toEqual([])
}, 20_000)
