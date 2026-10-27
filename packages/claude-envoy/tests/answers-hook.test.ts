import { afterEach, expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { SessionAnswer } from "@legion/contracts"
import {
  renderSessionAnswers,
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

interface HookRun {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
  /** From the spawn to the process's exit. */
  readonly wallMs: number
}

async function runHook(
  env: Record<string, string>,
  input: Record<string, unknown>,
): Promise<HookRun> {
  const started = performance.now()
  const run = Bun.spawn(["bun", bundle, "answers"], {
    env: { PATH: process.env["PATH"] ?? "", CLAUDE_PLUGIN_ROOT: packageRoot, ...env },
    stdin: new TextEncoder().encode(JSON.stringify(input)),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    run.exited,
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
  ])
  return { exitCode, stdout, stderr, wallMs: performance.now() - started }
}

function sessionStart(source: string, cwd: string): Record<string, unknown> {
  return { session_id: SESSION, source, cwd, hook_event_name: "SessionStart" }
}

/** The hook's additionalContext, checked to be under the event that ran it. */
function contextOf(run: HookRun, event: string): string {
  expect(run.exitCode).toBe(0)
  const output = JSON.parse(run.stdout)
  expect(output.hookSpecificOutput.hookEventName).toBe(event)
  const context: string = output.hookSpecificOutput.additionalContext
  // Claude Code keeps an additionalContext string whole only up to 10,000 characters.
  expect(context.length).toBeLessThan(10_000)
  return context
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

  for (const source of ["startup", "resume", "clear", "compact", "fork"]) {
    const run = await runHook(env, sessionStart(source, home))
    expect({ source, context: contextOf(run, "SessionStart") }).toEqual({
      source,
      context: expected as string,
    })
  }
  const subagent = await runHook(env, {
    session_id: SESSION,
    cwd: home,
    hook_event_name: "SubagentStart",
    agent_id: "agent-1",
    agent_type: "general-purpose",
  })
  expect(contextOf(subagent, "SubagentStart")).toBe(expected as string)
  expect(dispatch.requests).toEqual(
    Array(6).fill(`Bearer hook-token /api/v1/agents/${SESSION}/answers`),
  )
  // Each read left the list for the next hook process.
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.items).toEqual(items)
})

test("with Dispatch hanging, answers within 1 s with the list a hook read before", async () => {
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
  expect(run.wallMs).toBeLessThan(1_000)
  expect(dispatch.requests).toHaveLength(2)
})

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
  expect(await runHook(env, sessionStart("startup", home))).toMatchObject({
    exitCode: 0,
    stdout: "",
  })
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.items).toEqual([])

  dispatch.route = { kind: "hang" }
  const context = contextOf(await runHook(env, sessionStart("resume", home)), "SessionStart")
  const waited =
    /^\[Dispatch: people's answers from the last 72 hours could not be read \(Dispatch did not answer within (\d+) ms\); run `dispatch answers`\.\]$/.exec(
      context,
    )
  expect(waited).not.toBeNull()
})

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
  expect(run.wallMs).toBeLessThan(1_000)
})

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

  const context = contextOf(await runHook(env, sessionStart("startup", home)), "SessionStart")
  expect(context.length).toBeLessThan(8_000)
  expect(context).toBe(renderSessionAnswers(items, { now: Date.now(), total: 12 }) as string)
  expect(context).toMatch(
    /\n\n\d+ older items left out to stay under 8,000 characters: run `dispatch answers` to read every one in full\.$/,
  )
})

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
  expect(empty).toMatchObject({ exitCode: 0, stdout: "" })
  const off = await runHook(
    { HOME: home, DISPATCH_STATE_DIR: state },
    sessionStart("startup", home),
  )
  expect(off).toMatchObject({ exitCode: 0, stdout: "", stderr: "" })
  expect(dispatch.requests).toHaveLength(1)
})

test("a `!` token command runs outside the hook: a quick one gives the bearer at once, a slow one cannot hold the hook", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-4", "Token from a command.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!printf command-token" } }),
  )
  const quick = await runHook(env, sessionStart("startup", home))
  expect(contextOf(quick, "SessionStart")).toContain("Token from a command.")
  expect(dispatch.requests).toEqual([`Bearer command-token /api/v1/agents/${SESSION}/answers`])

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!sleep 3; printf late" } }),
  )
  const slowSince = Date.now()
  // Before the scratch directory goes, the slow command's refresher ends on its own.
  cleanup.push(() => refresherFinished(state, slowSince))
  const hanging = await runHook(env, sessionStart("resume", home))
  const context = contextOf(hanging, "SessionStart")
  // The list the quick run read stands in, naming why this read gave none.
  expect(context.split("\n")[1]).toMatch(
    /^As read at .*; this read failed: a "!" command in envoy\.json had not finished 800 ms after the hook started\.$/,
  )
  expect(context).toContain("Token from a command.")
  expect(hanging.wallMs).toBeLessThan(1_000)

  // With no list read before, the failure line says why none was read now.
  await rm(join(state, "sessions", SESSION, "answers.json"))
  const uncached = await runHook(env, sessionStart("compact", home))
  expect(contextOf(uncached, "SessionStart")).toBe(
    sessionAnswersFailureLine(
      `a "!" command in envoy.json had not finished 800 ms after the hook started`,
    ),
  )
  expect(uncached.wallMs).toBeLessThan(1_000)
  expect(dispatch.requests).toHaveLength(1)

  // The slow command still ends, and its refresher reads with the token it printed.
  await refresherFinished(state, slowSince)
  expect(dispatch.requests).toEqual([
    `Bearer command-token /api/v1/agents/${SESSION}/answers`,
    `Bearer late /api/v1/agents/${SESSION}/answers`,
  ])
  expect(await readdir(tmp)).toEqual([])
}, 20_000)

/** Processes whose command line holds `marker`, as `pgrep -f` finds them. */
function processesWith(marker: string): string {
  return Bun.spawnSync(["pgrep", "-f", marker]).stdout.toString().trim()
}

test("a token command slower than the hook's deadline finishes and cleans up after the hook, and the next hook carries the list it read", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-5", "Read with a slow command's token.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  // A sleep of a length no other process uses: `pgrep -f` finds the command's shell and its sleep.
  const pause = `sleep 1.5${String(Math.floor(Math.random() * 1e6)).padStart(6, "0")}`
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: { serverUrl: dispatch.base, token: `!${pause}; printf hook-token` },
    }),
  )

  const firstSince = Date.now()
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(
    sessionAnswersFailureLine(
      `a "!" command in envoy.json had not finished 800 ms after the hook started`,
    ),
  )
  expect(first.wallMs).toBeLessThan(1_000)
  // The command outlives the hook, in the refresher the hook started.
  expect(processesWith(pause)).not.toBe("")

  await refresherFinished(state, firstSince)
  expect(await readdir(tmp)).toEqual([])
  expect(processesWith(pause)).toBe("")
  expect(dispatch.requests).toEqual([`Bearer hook-token /api/v1/agents/${SESSION}/answers`])

  const secondSince = Date.now()
  cleanup.push(() => refresherFinished(state, secondSince))
  const second = await runHook(env, sessionStart("resume", home))
  const context = contextOf(second, "SessionStart")
  expect(context.split("\n")[1]).toMatch(
    /^As read at .*; this read failed: a "!" command in envoy\.json had not finished 800 ms after the hook started\.$/,
  )
  expect(context).toContain("Read with a slow command's token.")
  expect(second.wallMs).toBeLessThan(1_000)
  await refresherFinished(state, secondSince)
  expect(processesWith(pause)).toBe("")
}, 20_000)

test("with a slow token command, a list read from another Dispatch than envoy.json names now never stands in", async () => {
  const before = standinDispatch()
  before.route = { kind: "list", items: [answer("ask-a", "Read from the Dispatch before.", 1)] }
  const now = standinDispatch()
  now.route = { kind: "list", items: [answer("ask-b", "Read from the Dispatch now.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: before.base, token: "!printf before-token" } }),
  )
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toContain("Read from the Dispatch before.")
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.dispatch_url).toBe(before.base)

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: now.base, token: "!sleep 3; printf now-token" } }),
  )
  const since = Date.now()
  cleanup.push(() => refresherFinished(state, since))
  const hanging = await runHook(env, sessionStart("resume", home))
  expect(contextOf(hanging, "SessionStart")).toBe(
    sessionAnswersFailureLine(
      `a "!" command in envoy.json had not finished 800 ms after the hook started`,
    ),
  )
  expect(hanging.wallMs).toBeLessThan(1_000)

  // The refresher reads the Dispatch named now, with the token its command printed.
  await refresherFinished(state, since)
  expect(now.requests).toEqual([`Bearer now-token /api/v1/agents/${SESSION}/answers`])
  expect(before.requests).toHaveLength(1)
}, 20_000)

test("a refresher started for the Dispatch envoy.json named before never hands its list to a hook for the Dispatch it names now", async () => {
  // The refresher is shared by every hook of the session, whatever Dispatch each resolves. A
  // refresher started while envoy.json named `before` that finishes while a later hook, started
  // after envoy.json moved to `now`, is still waiting must not give that hook `before`'s list, the
  // invariant the review asked for ("no list from another Dispatch URL").
  const before = standinDispatch()
  before.route = { kind: "list", items: [answer("ask-x", "Read from the Dispatch before.", 1)] }
  const now = standinDispatch()
  now.route = { kind: "list", items: [] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const gate = join(home, "gate")

  // The first hook's refresher runs the command, which waits until the test opens the gate.
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: before.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf before-token`,
      },
    }),
  )
  const since = Date.now()
  cleanup.push(async () => {
    await writeFile(gate, "")
    await refresherFinished(state, since)
  })
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(
    sessionAnswersFailureLine(
      `a "!" command in envoy.json had not finished 800 ms after the hook started`,
    ),
  )

  // envoy.json now names another Dispatch. A hook starts and finds the refresher running; the gate
  // opens as it starts, so the refresher reads `before` and finishes after this hook started and
  // before its print deadline, an outcome the hook waits for.
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: now.base, token: "!printf now-token" } }),
  )
  const second = runHook(env, sessionStart("resume", home))
  await writeFile(gate, "")
  const context = contextOf(await second, "SessionStart")
  await refresherFinished(state, since)

  // The refresher did read `before`, so the case is not vacuous.
  expect(before.requests).toEqual([`Bearer before-token /api/v1/agents/${SESSION}/answers`])
  expect(context).not.toContain("Read from the Dispatch before.")
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
})

test("a hook whose start-up took past 800 ms still carries the list Dispatch answers at once", async () => {
  // Claude Code starts the open-asks, dispatch-first and answers hooks at once beside itself, so on
  // a loaded host the answers hook's own bun start-up can pass 800 ms before its code runs. The
  // preload stands in for that start-up: it holds the process until 820 ms after it started.
  // Dispatch then answers at once, so the session must get the list Dispatch holds now, not the
  // failure line ("did not answer within 0 ms") or a stale list.
  const dispatch = standinDispatch()
  const items = [answer("ask-slow", "SLOW-START-ANSWER: written before this resume.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const preload = join(home, "slow-start.ts")
  await writeFile(preload, "while (performance.now() < 820) {}\n")
  const started = performance.now()
  const hook = Bun.spawn(["bun", "--preload", preload, bundle, "answers"], {
    env: {
      PATH: process.env["PATH"] ?? "",
      CLAUDE_PLUGIN_ROOT: packageRoot,
      HOME: home,
      DISPATCH_STATE_DIR: state,
      DISPATCH_URL: dispatch.base,
      DISPATCH_TOKEN: "hook-token",
    },
    stdin: new TextEncoder().encode(JSON.stringify(sessionStart("resume", home))),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    hook.exited,
    new Response(hook.stdout).text(),
    new Response(hook.stderr).text(),
  ])
  const run: HookRun = { exitCode, stdout, stderr, wallMs: performance.now() - started }

  expect(contextOf(run, "SessionStart")).toBe(
    renderSessionAnswers(items, { now: Date.now(), total: items.length }) as string,
  )
  expect(dispatch.requests).toEqual([`Bearer hook-token /api/v1/agents/${SESSION}/answers`])
})
