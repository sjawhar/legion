import { afterEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
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

/** A scratch HOME, Dispatch state directory and Claude config directory for one case. */
async function scratch(): Promise<{
  readonly home: string
  readonly state: string
  readonly config: string
}> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-answers-"))
  cleanup.push(() => rm(root, { recursive: true, force: true }))
  const home = join(root, "home")
  const state = join(root, "state")
  const config = join(root, "claude")
  for (const directory of [home, state, config]) await mkdir(directory, { recursive: true })
  return { home, state, config }
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
  expect(context).toBe(
    renderSessionAnswers(items, { now: Date.now(), total: 1, cachedAt: cache.read_at }) as string,
  )
  expect(context).toContain("Read before Dispatch hung.")
  expect(run.wallMs).toBeLessThan(1_000)
  expect(dispatch.requests).toHaveLength(2)
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
  expect(context.length).toBeLessThanOrEqual(8_000)
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

test("a `!` token command runs off the hook's thread: a quick one gives the bearer, a hanging one cannot hold the hook", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-4", "Token from a command.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config }
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
  const hanging = await runHook(env, sessionStart("resume", home))
  const context = contextOf(hanging, "SessionStart")
  // The list the quick run read stands in.
  expect(context.startsWith(`${SESSION_ANSWERS_MARKER}\nAs read at `)).toBe(true)
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
})

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
