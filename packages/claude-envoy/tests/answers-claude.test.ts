import { afterEach, expect, test } from "bun:test"
import { randomUUID } from "node:crypto"
import { mkdir, mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { SessionAnswer } from "@legion/contracts"
import { renderSessionAnswers } from "@legion/envoy-client/session-answers"

// What people wrote to a session, on the real Claude Code (hooks/session-hook.ts `answers`,
// dispatch://INFRA-148): Claude Code keeps a hook's additionalContext where only the requests it
// sends to the model show it, so each moment is read off those. Claude Code runs with this package
// as `--plugin-dir`, an isolated HOME and CLAUDE_CONFIG_DIR, and one stand-in as both its Messages
// endpoint (`ANTHROPIC_BASE_URL`, streaming as the API does) and Dispatch (`DISPATCH_URL`), whose
// answers route serves a list each case changes between moments. LEGION_TEST_CLAUDE names the
// binary: the version the repository's .claude-code-pin pins, which CI installs
// (.github/actions/install-claude-code). A run without one skips, except on GitHub Actions, where a
// skip would hide the only run of the check (GITHUB_ACTIONS, not CI: agent harnesses on the devbox
// export CI=true).
const claude = process.env["LEGION_TEST_CLAUDE"]
const onActions = process.env["GITHUB_ACTIONS"] === "true"
const skipped = claude === undefined && !onActions
const binary = (): string => {
  if (claude === undefined) throw new Error("LEGION_TEST_CLAUDE is unset on GitHub Actions")
  return claude
}

const packageRoot = resolve(import.meta.dir, "..")
/** What a case waits for at most: a process's exit, or a turn's result line. */
const WAIT_MS = 60_000
/** The assignment the stand-in's Agent call hands the subagent, which tells its requests apart. */
const ASSIGNMENT = "SUBAGENT-ASSIGNMENT: report that you are done."
const SUBAGENT_PROMPT = "Run a subagent."

const cleanup: (() => Promise<void>)[] = []
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step()
})

/** One content block of a stand-in reply. */
type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> }

/** The Messages API's server-sent events for one reply, as Claude Code reads a streamed one. */
function messageStream(blocks: readonly Block[], id: string): string {
  const events: Record<string, unknown>[] = [
    {
      type: "message_start",
      message: {
        id,
        type: "message",
        role: "assistant",
        model: "standin-model",
        content: [],
        stop_reason: null,
        stop_sequence: null,
        usage: { input_tokens: 5, output_tokens: 1 },
      },
    },
  ]
  blocks.forEach((block, index) => {
    if (block.type === "text") {
      events.push(
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
        { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } },
      )
    } else {
      const toolUse = { type: "tool_use", id: `toolu_${id}_${index}`, name: block.name, input: {} }
      events.push(
        { type: "content_block_start", index, content_block: toolUse },
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        },
      )
    }
    events.push({ type: "content_block_stop", index })
  })
  const stopReason = blocks.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn"
  events.push(
    {
      type: "message_delta",
      delta: { stop_reason: stopReason, stop_sequence: null },
      usage: { output_tokens: 1 },
    },
    { type: "message_stop" },
  )
  return events
    .map((event) => `event: ${String(event["type"])}\ndata: ${JSON.stringify(event)}\n\n`)
    .join("")
}

/** One Messages request the stand-in served, with its place among every request it served. */
interface Seen {
  readonly index: number
  readonly path: string
  readonly body: Record<string, unknown>
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

/** The list as the hook renders it, which the request must carry word for word. */
function rendered(items: readonly SessionAnswer[]): string {
  const text = renderSessionAnswers(items, { now: Date.now(), total: items.length })
  if (text === undefined) throw new Error("an empty list renders nothing")
  return text
}

/** Every text a request's messages hold: string contents and text blocks, in order. */
function textsOf(request: Seen): string[] {
  const messages = Array.isArray(request.body["messages"]) ? request.body["messages"] : []
  return messages.flatMap((message: unknown): string[] => {
    if (typeof message !== "object" || message === null || !("content" in message)) return []
    const { content } = message
    if (typeof content === "string") return [content]
    return (Array.isArray(content) ? content : []).flatMap((block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "text" in block &&
      typeof block.text === "string"
        ? [block.text]
        : [],
    )
  })
}

function carries(request: Seen, text: string): boolean {
  return textsOf(request).some((each) => each.includes(text))
}

/** A compaction's summary call, which Claude Code sends as a turn with its instructions last. */
function isCompaction(request: Seen): boolean {
  return textsOf(request).some((text) => text.includes("Respond with TEXT ONLY"))
}

/** A subagent's turn: it carries the assignment and none of the parent's conversation. */
function isSubagent(request: Seen): boolean {
  return carries(request, ASSIGNMENT) && !carries(request, SUBAGENT_PROMPT)
}

/** The first turn whose texts hold `prompt`, the user message that started it. */
function turnOf(seen: readonly Seen[], prompt: string): Seen {
  const turn = seen.find(
    (request) => !isCompaction(request) && textsOf(request).some((text) => text === prompt),
  )
  if (turn === undefined) {
    throw new Error(`no Messages request carries ${JSON.stringify(prompt)}: ${seen.length} seen`)
  }
  return turn
}

interface Standin {
  base: string
  readonly messages: Seen[]
  /** Every Dispatch answers request, by path, in order, with its place among every request. */
  readonly answers: { readonly index: number; readonly path: string }[]
  /** What the answers route lists from now on. */
  list: readonly SessionAnswer[]
  /** Set as the stand-in answers the parent's turn with its Agent call. */
  onAgentCall: (() => void) | undefined
}

/** The stand-in: Claude Code's Messages endpoint and Dispatch's answers and open-asks routes. */
function serveStandin(session: string): Standin {
  let served = 0
  const standin: Standin = { base: "", messages: [], answers: [], list: [], onAgentCall: undefined }
  const reply = (request: Seen): readonly Block[] => {
    if (isCompaction(request)) {
      return [
        {
          type: "text",
          text: "<analysis>Short.</analysis>\n<summary>SUMMARY-OF-THE-TURNS</summary>",
        },
      ]
    }
    if (isSubagent(request)) return [{ type: "text", text: "Done." }]
    const agentCalled = JSON.stringify(request.body["messages"]).includes('"tool_use"')
    if (carries(request, SUBAGENT_PROMPT) && !agentCalled) {
      standin.onAgentCall?.()
      return [
        {
          type: "tool_use",
          name: "Agent",
          input: {
            description: "Report done",
            prompt: ASSIGNMENT,
            subagent_type: "general-purpose",
          },
        },
      ]
    }
    return [{ type: "text", text: "Noted." }]
  }
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const index = served++
      const url = new URL(request.url)
      if (url.pathname === "/v1/messages") {
        const seen: Seen = {
          index,
          path: url.pathname,
          body: (await request.json()) as Record<string, unknown>,
        }
        standin.messages.push(seen)
        const blocks = reply(seen)
        if (seen.body["stream"] !== true) {
          return Response.json({
            id: `msg_${index}`,
            type: "message",
            role: "assistant",
            model: seen.body["model"],
            content: blocks,
            stop_reason: "end_turn",
            stop_sequence: null,
            usage: { input_tokens: 5, output_tokens: 1 },
          })
        }
        return new Response(messageStream(blocks, `msg_${index}`), {
          headers: { "content-type": "text/event-stream" },
        })
      }
      if (url.pathname.endsWith("/answers")) {
        standin.answers.push({ index, path: url.pathname })
        const items = url.pathname === `/api/v1/agents/${session}/answers` ? standin.list : []
        return Response.json({
          session_id: session,
          since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
          total: items.length,
          items,
        })
      }
      if (url.pathname === "/api/v1/asks/open") {
        return Response.json({
          session_id: session,
          as_of: new Date().toISOString(),
          opened_since: false,
          count: 0,
          waiting_on_human: 0,
          waiting_on_agent: 0,
          asks: [],
        })
      }
      return new Response("not found", { status: 404 })
    },
  })
  cleanup.push(async () => {
    await server.stop(true)
  })
  standin.base = `http://127.0.0.1:${server.port}`
  return standin
}

/** A session's scratch directories, the stand-in, and Claude Code's whole environment. */
async function setUp(): Promise<{
  readonly session: string
  readonly standin: Standin
  readonly work: string
  readonly env: Record<string, string>
}> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-answers-claude-"))
  cleanup.push(() => rm(root, { recursive: true, force: true }))
  const [home, config, work, state] = ["home", "claude", "work", "state"].map((name) =>
    join(root, name),
  )
  for (const directory of [home, config, work, state])
    await mkdir(directory as string, { recursive: true })
  const session = randomUUID()
  const standin = serveStandin(session)
  return {
    session,
    standin,
    work: work as string,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: home as string,
      CLAUDE_CONFIG_DIR: config as string,
      ANTHROPIC_BASE_URL: standin.base,
      ANTHROPIC_API_KEY: "sk-ant-placeholder",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
      DISPATCH_URL: standin.base,
      DISPATCH_TOKEN: "dispatch-test-token",
      DISPATCH_STATE_DIR: state as string,
    },
  }
}

const CLAUDE_FLAGS = ["--plugin-dir", packageRoot, "--permission-mode", "bypassPermissions"]

/** One `claude -p` run to its exit; a failed run throws with what Claude Code printed. */
async function print(
  setup: { readonly work: string; readonly env: Record<string, string> },
  args: readonly string[],
): Promise<void> {
  const run = Bun.spawn([binary(), ...CLAUDE_FLAGS, "-p", "--output-format", "json", ...args], {
    cwd: setup.work,
    env: setup.env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: WAIT_MS,
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    run.exited,
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
  ])
  if (exitCode !== 0) {
    throw new Error(
      `claude ${args.join(" ")} exited ${exitCode}\nstdout: ${stdout}\nstderr: ${stderr}`,
    )
  }
}

/** A `claude -p` session in stream-json, the way `legion claude-bridge` drives one: each user
 *  message one line on stdin, each turn's end a `result` line on stdout. */
function streamSession(
  setup: { readonly work: string; readonly env: Record<string, string> },
  args: readonly string[],
): { readonly turn: (text: string) => Promise<void>; readonly end: () => Promise<void> } {
  const run = Bun.spawn(
    [
      binary(),
      ...CLAUDE_FLAGS,
      "-p",
      "--input-format",
      "stream-json",
      "--output-format",
      "stream-json",
      "--verbose",
      ...args,
    ],
    {
      cwd: setup.work,
      env: setup.env,
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
      timeout: WAIT_MS,
    },
  )
  cleanup.push(async () => {
    run.kill()
    await run.exited
  })
  const lines: string[] = []
  /** Each turn's `result` line, parsed: Claude Code orders its keys as it likes. */
  const results: Record<string, unknown>[] = []
  let waiting: (() => void) | undefined
  const reading = (async () => {
    const decoder = new TextDecoder()
    let buffered = ""
    for await (const chunk of run.stdout) {
      buffered += decoder.decode(chunk, { stream: true })
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = buffered.slice(0, end)
        buffered = buffered.slice(end + 1)
        lines.push(line)
        let parsed: unknown
        try {
          parsed = JSON.parse(line)
        } catch {
          continue
        }
        if (typeof parsed === "object" && parsed !== null && "type" in parsed) {
          if (parsed.type === "result") {
            results.push(parsed as Record<string, unknown>)
            waiting?.()
          }
        }
      }
    }
  })()
  const stderr = new Response(run.stderr).text()
  const output = async () => `stdout:\n${lines.join("\n")}\nstderr: ${await stderr}`
  return {
    async turn(text) {
      const before = results.length
      run.stdin.write(
        `${JSON.stringify({ type: "user", message: { role: "user", content: [{ type: "text", text }] } })}\n`,
      )
      run.stdin.flush()
      const done = new Promise<void>((settle) => {
        waiting = () => {
          if (results.length > before) settle()
        }
      })
      // The process's own `timeout` bounds the wait: Bun kills it, and `exited` settles.
      const outcome = await Promise.race([
        done.then(() => "done" as const),
        run.exited.then(() => "exited" as const),
      ])
      if (outcome !== "done" && results.length <= before) {
        throw new Error(
          `claude ended no turn for ${JSON.stringify(text)} (${outcome})\n${await output()}`,
        )
      }
      if (results.at(-1)?.["is_error"] !== false) {
        throw new Error(`the turn for ${JSON.stringify(text)} failed\n${await output()}`)
      }
    },
    async end() {
      run.stdin.end()
      const exitCode = await run.exited
      await reading
      if (exitCode !== 0) throw new Error(`claude exited ${exitCode}\n${await output()}`)
    },
  }
}

test.skipIf(skipped)(
  "startup: the first Messages request of `claude -p` carries the list",
  async () => {
    const setup = await setUp()
    const items = [answer("ask-1", "STARTUP-ANSWER: take the second way.", 5)]
    setup.standin.list = items

    await print(setup, ["--session-id", setup.session, "Turn one."])

    const turn = turnOf(setup.standin.messages, "Turn one.")
    expect(carries(turn, rendered(items))).toBe(true)
    expect(setup.standin.answers.map((read) => read.path)).toEqual([
      `/api/v1/agents/${setup.session}/answers`,
    ])
  },
  WAIT_MS * 2,
)

test.skipIf(skipped)(
  "resume: `claude -p --resume <id>` carries the list as it reads at the resume",
  async () => {
    const setup = await setUp()
    const first = answer("ask-1", "FIRST-ANSWER: before the resume.", 30)
    setup.standin.list = [first]
    await print(setup, ["--session-id", setup.session, "Turn one."])

    const items = [answer("ask-2", "RESUME-ANSWER: written after turn one.", 1), first]
    setup.standin.list = items
    await print(setup, ["--resume", setup.session, "Turn two."])

    expect(carries(turnOf(setup.standin.messages, "Turn one."), "RESUME-ANSWER")).toBe(false)
    expect(carries(turnOf(setup.standin.messages, "Turn two."), rendered(items))).toBe(true)
  },
  WAIT_MS * 3,
)

test.skipIf(skipped)(
  "compaction: after `/compact` in stream-json, the next Messages request carries the list",
  async () => {
    const setup = await setUp()
    const first = answer("ask-1", "FIRST-ANSWER: before the compaction.", 30)
    setup.standin.list = [first]
    const session = streamSession(setup, ["--session-id", setup.session])
    await session.turn("Turn one.")

    const items = [answer("ask-3", "COMPACT-ANSWER: written before the compaction.", 1), first]
    setup.standin.list = items
    await session.turn("/compact")
    await session.turn("Turn two.")
    await session.end()

    expect(setup.standin.messages.some(isCompaction)).toBe(true)
    expect(carries(turnOf(setup.standin.messages, "Turn one."), "COMPACT-ANSWER")).toBe(false)
    const after = turnOf(setup.standin.messages, "Turn two.")
    expect(carries(after, "SUMMARY-OF-THE-TURNS")).toBe(true)
    expect(carries(after, rendered(items))).toBe(true)
  },
  WAIT_MS * 4,
)

test.skipIf(skipped)(
  "subagent: the subagent's first Messages request carries the list, read for the parent's session",
  async () => {
    const setup = await setUp()
    const first = answer("ask-1", "FIRST-ANSWER: before the subagent.", 30)
    setup.standin.list = [first]
    const items = [answer("ask-4", "SUBAGENT-ANSWER: written as the subagent starts.", 1), first]
    let switchedAt = -1
    setup.standin.onAgentCall = () => {
      // Only SubagentStart reads the list from here on, so only its context can carry this answer.
      switchedAt = setup.standin.answers.length
      setup.standin.list = items
    }

    await print(setup, ["--session-id", setup.session, SUBAGENT_PROMPT])

    const subagent = setup.standin.messages.find(isSubagent)
    if (subagent === undefined) throw new Error("the subagent sent no Messages request")
    expect(carries(subagent, rendered(items))).toBe(true)
    expect(
      setup.standin.messages
        .filter((request) => !isSubagent(request))
        .some((request) => carries(request, "SUBAGENT-ANSWER")),
    ).toBe(false)
    // D1: SubagentStart's session_id is the parent's: its one read named the parent's session.
    expect(switchedAt).toBeGreaterThan(0)
    const subagentReads = setup.standin.answers.slice(switchedAt)
    expect(subagentReads.map((read) => read.path)).toEqual([
      `/api/v1/agents/${setup.session}/answers`,
    ])
    expect(subagentReads[0]?.index ?? Number.POSITIVE_INFINITY).toBeLessThan(subagent.index)
  },
  WAIT_MS * 2,
)
