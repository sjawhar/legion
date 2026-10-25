import { afterEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { REMINDER } from "@legion/pi-shared/phase-stall"
import { phaseStallFile } from "../src/legion-phase-hook"
import { FakeNatsServer } from "./fake-nats-server"

/** One scripted reply's blocks: the shape `Block` has in `@legion/pi-shared/test/omp-harness`. */
type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> }

/** One request the stand-in served. */
interface Request {
  readonly path: string
  readonly body: Record<string, unknown>
}

/**
 * The Anthropic Messages stream for one scripted reply, event for event as the shared harness's
 * `messageStream` builds it (`@legion/pi-shared/test/omp-harness`).
 *
 * It is restated here rather than imported: that harness is written for pi-shared's tsconfig,
 * which leaves `noPropertyAccessFromIndexSignature` off, and importing it into this package —
 * whose tsconfig turns it on — puts its sources in this package's program and fails
 * `bunx tsc --noEmit` on eight accesses in `test/omp-harness.ts` and `test/omp-natives.ts` that
 * this file does not own. Make those eight accesses bracketed and this copy goes away.
 */
function messageStream(blocks: readonly Block[], id: string): string {
  const events: [string, unknown][] = [
    [
      "message_start",
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
    ],
  ]
  blocks.forEach((block, index) => {
    if (block.type === "text") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
      ])
      events.push([
        "content_block_delta",
        { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } },
      ])
    } else {
      events.push([
        "content_block_start",
        {
          type: "content_block_start",
          index,
          content_block: {
            type: "tool_use",
            id: `toolu_${id}_${index}`,
            name: block.name,
            input: {},
          },
        },
      ])
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        },
      ])
    }
    events.push(["content_block_stop", { type: "content_block_stop", index }])
  })
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: {
        stop_reason: blocks.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn",
        stop_sequence: null,
      },
      usage: { output_tokens: 1 },
    },
  ])
  events.push(["message_stop", { type: "message_stop" }])
  return events.map(([name, data]) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`).join("")
}

/**
 * Serves every request with `answer`, recording each first, as the shared harness's `serveStandin`
 * does and as this package's own stand-ins are stood up (`tests/legion-pane-mcp.test.ts`). The
 * same server is the Anthropic API, the Legion daemon and the Envoy listener: a worker's three
 * addresses, each a path of its own.
 */
function serveStandin(answer: (url: URL, body: Record<string, unknown>) => Response): {
  readonly requests: Request[]
  readonly base: string
} {
  const requests: Request[] = []
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url)
      const text = request.method === "POST" ? await request.text() : ""
      const body = text === "" ? {} : (JSON.parse(text) as Record<string, unknown>)
      requests.push({ path: url.pathname, body })
      return answer(url, body)
    },
  })
  cleanup.push(async () => {
    await server.stop(true)
  })
  return { requests, base: `http://127.0.0.1:${server.port}` }
}

// The phase reminder's Claude Code half on the real Claude Code binary (src/legion-phase-hook.ts,
// legion/hooks/hooks.json): only the real binary shows that Claude Code arms the generated plugin's
// `mcp_tool` hook entries at all under Legion's own invocation shape
// (`packages/daemon/internal/claudelaunch`), that such a hook reaches the pane's own MCP server
// under the bare name `--mcp-config` registers it as, that a `Stop` answered
// `{"decision":"block","reason":…}` turns into one more turn of the same session, and which of the
// model's calls `PostToolUse` fires for. Every other test of this path drives the endpoint
// directly and would pass just as well if Claude Code armed nothing.
//
// LEGION_TEST_CLAUDE names the binary: the one the worker image installs from this branch's
// `packages/daemon/docker/claude-code/package-lock.json`
// (`node_modules/@anthropic-ai/claude-code-linux-x64/claude`; the `node_modules/.bin/claude`
// symlink is a `.exe` shim that does not exec on Linux). A run without one skips the whole file.
// Unlike `packages/pi-legion/extensions/legion-phase-stall-omp.test.ts`, which also runs on
// GitHub Actions because CI's pi-envoy job installs the Oh My Pi pin, nothing in CI installs the
// Claude Code binary, so there is no host where a skip here would hide the only run of the check.
//
// What runs is real: the package's own generated `legion/` plugin tree (the artefact that ships),
// the package's built `dist/envoy-channel.js` as the pane's `legion` MCP server, and the real
// binary. The stand-ins are the ones a pod has no business reaching — the Anthropic API
// (ANTHROPIC_BASE_URL), the Legion daemon, the Envoy listener and NATS.
//
// Two of the four facts are read out of Claude Code's own `--debug` log, which is the one flag
// this file adds to Legion's invocation shape (it changes nothing but the logging): which hooks
// Claude Code ran, what each answered, and that each reached `legion/legion_phase_hook`. Its line
// shapes are Claude Code 2.1.310's. The third source is the pane's own log, which is the MCP
// server's stderr: Claude Code keeps only what a server writes around its startup, so the MCP
// document runs the real server under `sh -c exec` with its stderr redirected to a file — the
// server stays Claude Code's own child, and nothing but the redirection differs.
const claude = process.env["LEGION_TEST_CLAUDE"]

/** The pane's session, the form `--session-id` takes (`claudelaunch.NewSessionID`). */
const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

/** The claim the stand-in daemon registers: the implementer of the issue whose phase in the
 * daemon's state fixture is `implementing`, so the reminder's record check says this role still
 * works this phase and the `Stop` earns its reminder. */
const ISSUE = "LEGION-208"
const CLAIM_TOKEN = `legion-legion-${ISSUE.toLowerCase()}-implementer`

/** The package root, whose `legion/` is the generated plugin tree and whose `dist/` holds the
 * built MCP server: both are what ships, and a hand-written copy of either would prove nothing. */
const PACKAGE = resolve(import.meta.dir, "..")

/** The reminders stay far away: nothing in these cases waits for a timer, and the architect is
 * never told. The `Stop` continuation is immediate, so only the delays a case must not reach are
 * set here, each far longer than a case's run. */
const QUIET_LIMITS: Record<string, string> = {
  LEGION_STALL_REMINDER_MS: "600000",
  LEGION_STALL_WAITING_MS: "600000",
  LEGION_STALL_RESUME_MS: "600000",
  LEGION_STALL_ARCHITECT_AFTER: "9",
}

/** What each case undoes afterwards, run last first. */
const cleanup: (() => Promise<void>)[] = []
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step()
})

/** One hook Claude Code ran, as its `--debug` log records it: the matcher it logs the hook under
 * (`PostToolUse:Bash`), the event, and the answer it parsed out of the hook's output. */
interface HookRun {
  readonly matcher: string
  readonly event: string
  readonly answer: Record<string, unknown>
}

/** Claude Code logs a line of its own text as a JSON string; everything this file reads out of the
 * debug log is one of those, so each is parsed back rather than matched inside its escaping. */
function loggedText(log: string): string[] {
  const texts: string[] = []
  for (const line of log.split("\n")) {
    const quoted = line.indexOf('"')
    if (quoted < 0) continue
    let text: unknown
    try {
      text = JSON.parse(line.slice(quoted))
    } catch {
      continue
    }
    if (typeof text === "string") texts.push(text)
  }
  return texts
}

/** Every hook Claude Code ran and answered, in order (`Hook <matcher> (<event>) success:`). */
function hookRuns(log: string): HookRun[] {
  const runs: HookRun[] = []
  for (const text of loggedText(log)) {
    const match = /^Hook (\S+) \((\w+)\) success:\n([\s\S]*)$/.exec(text)
    if (match === null) continue
    const [, matcher = "", event = "", answer = ""] = match
    runs.push({ matcher, event, answer: JSON.parse(answer) as Record<string, unknown> })
  }
  return runs
}

/** `${stop_hook_active}` as the real server received it, one line per `Stop` it answered for a
 * pane whose claim has booted: the endpoint logs it at debug before it settles the machine, and
 * the pane's log is the server's stderr (`logLegion`). */
function stopHookActive(paneLog: readonly string[]): string[] {
  const seen: string[] = []
  for (const line of paneLog) {
    if (!line.includes("a Claude Code phase worker's turn is settling")) continue
    const parsed = JSON.parse(line.slice(line.indexOf("{"))) as { stopHookActive?: unknown }
    seen.push(String(parsed.stopHookActive))
  }
  return seen
}

/** What one worker run left behind. */
interface WorkerRun {
  /** Every stream-json line Claude Code wrote, in order. */
  readonly events: readonly Record<string, unknown>[]
  /** The Messages requests the stand-in gateway answered, in order. */
  readonly turns: readonly Request[]
  /** Every hook Claude Code ran, in order. */
  readonly hooks: readonly HookRun[]
  /** The `<server>/<tool>` each `mcp_tool` hook call named, in order. */
  readonly mcpHookCalls: readonly string[]
  /** `${stop_hook_active}` as the real server received it, in order. */
  readonly stopHookActive: readonly string[]
  /** The phase-stall record the real server left on disk (`phaseStallFile`). */
  readonly record: unknown
  /** Claude Code's whole `--debug` log, for a failure's sake. */
  readonly debug: string
}

/**
 * Runs one Legion phase worker on the real Claude Code binary until its last prompt's run ends:
 * Legion's own agent command (`claudelaunch.Argv`, less the `legion claude-bridge` wrapper, which
 * is transport and launches this same command), the package's built `dist/envoy-channel.js` as the
 * one `--mcp-config` server under the name every generated hook entry names, and both plugin
 * directories a worker loads.
 *
 * The prompts arrive on stdin as the bridge's do. The first is the seeding turn (the bridge's
 * `/rename`): Claude Code writes the session transcript only for a turn, and the transcript is
 * what the pane's claim boot waits for, so a worker's very first prompt always runs before any
 * claim exists. Every later prompt is sent once the daemon stand-in has been told the pane is
 * ready, as the daemon's own assignment is.
 */
async function runWorker(options: {
  readonly binary: string
  readonly prompts: readonly string[]
  readonly replies: readonly (readonly Block[])[]
}): Promise<WorkerRun> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-phase-hook-"))
  cleanup.push(() => rm(root, { recursive: true, force: true }))
  const configDir = join(root, "claude")
  const workspace = join(root, "work")
  await mkdir(configDir, { recursive: true })
  await mkdir(workspace, { recursive: true })
  const tokenFile = join(root, "boot-token")
  await writeFile(tokenFile, "boot-token-from-the-pane\n")

  const broker = new FakeNatsServer()
  cleanup.push(() => broker.stop())

  const fixture = async (name: string): Promise<Record<string, unknown>> =>
    JSON.parse(
      await readFile(resolve(PACKAGE, "../contracts/fixtures/daemon-api", name), "utf8"),
    ) as Record<string, unknown>
  // The daemon's own golden answers, so a field the daemon adds to either reaches this stand-in.
  const registered = await fixture("register.json")
  const state = await fixture("state.json")
  const ready = Promise.withResolvers<void>()
  let answered = 0
  const { requests, base } = serveStandin((url, body) => {
    // The stand-in Anthropic API: one scripted reply per model request, the last of them ending
    // the run, so every case ends on its own.
    if (url.pathname === "/v1/messages") {
      const blocks = options.replies[answered]
      answered += 1
      if (blocks === undefined) {
        return Response.json(
          { type: "error", error: { type: "invalid_request_error", message: "no reply scripted" } },
          { status: 400 },
        )
      }
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      })
    }
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: CLAIM_TOKEN,
        tree: ISSUE,
        issue: ISSUE,
        role: "implementer",
        generation: 1,
        secret: "Y2xhdWRlLXBhbmU=",
      })
    }
    if (url.pathname === "/legion/v1/claims/ready") {
      ready.resolve()
      return new Response(null, { status: 204 })
    }
    if (url.pathname === "/legion/v1/state") return Response.json(state)
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 })
    }
    // The Envoy listener: the registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body["session_id"] === "string" ? body["session_id"] : SESSION,
      machine_id: "claude-pane-machine",
      dir: workspace,
      topics: [],
    })
  })

  // The worker's MCP document, as `claudelaunch.MCPConfig` writes it, with the one difference
  // this file needs: the server's stderr, which is the pane's log, is kept. `sh -c exec` leaves
  // the real server as Claude Code's own child, so nothing but the redirection differs — Claude
  // Code's debug log keeps only what an MCP server writes around its startup, and the endpoint's
  // debug line for each `Stop` it answers comes later.
  const paneLogFile = join(root, "pane.log")
  const mcpConfig = join(root, "claude-mcp.json")
  await writeFile(
    mcpConfig,
    JSON.stringify({
      mcpServers: {
        legion: {
          command: "/bin/sh",
          args: [
            "-c",
            `exec '${process.execPath}' '${join(PACKAGE, "dist", "envoy-channel.js")}' 2>>'${paneLogFile}'`,
          ],
          env: { CLAUDE_PLUGIN_DATA: join(configDir, "envoy-plugin-data") },
        },
      },
    }),
  )

  const child = Bun.spawn(
    [
      options.binary,
      "-p",
      "--input-format",
      "stream-json",
      "--output-format",
      "stream-json",
      "--verbose",
      "--permission-mode",
      "bypassPermissions",
      "--strict-mcp-config",
      "--mcp-config",
      mcpConfig,
      "--plugin-dir",
      PACKAGE,
      "--plugin-dir",
      join(PACKAGE, "legion"),
      "--session-id",
      SESSION,
      // Not Legion's: the log this file reads its hook observations out of.
      "--debug",
    ],
    {
      cwd: workspace,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: root,
        // `claudelaunch.Environment`.
        CLAUDE_CONFIG_DIR: configDir,
        CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: "1",
        CLAUDE_CODE_DISABLE_CRON: "1",
        // The stand-in Anthropic API drives the model, and nothing else may reach a real one.
        ANTHROPIC_BASE_URL: base,
        ANTHROPIC_API_KEY: "standin-key",
        ANTHROPIC_MODEL: "standin-model",
        DISABLE_AUTOUPDATER: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_ERROR_REPORTING: "1",
        DISABLE_NON_ESSENTIAL_MODEL_CALLS: "1",
        // What the daemon puts on a Claude Code pane, which the MCP server inherits.
        LEGION_BOOT_TOKEN_FILE: tokenFile,
        LEGION_DAEMON_URL: base,
        LEGION_PROJECT: "legion",
        LEGION_WORKSPACE: workspace,
        ENVOY_URL: base,
        ENVOY_NATS_URL: broker.url,
        ...QUIET_LIMITS,
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  cleanup.push(async () => {
    child.kill()
    await child.exited
  })
  const prompt = (text: string): void => {
    child.stdin.write(
      `${JSON.stringify({
        type: "user",
        message: { role: "user", content: [{ type: "text", text }] },
      })}\n`,
    )
    void child.stdin.flush()
  }
  prompt(options.prompts[0] ?? "")

  const events: Record<string, unknown>[] = []
  const decoder = new TextDecoder()
  const reader = child.stdout.getReader()
  let buffered = ""
  let sent = 1
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffered += decoder.decode(value, { stream: true })
    for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
      const line = buffered.slice(0, end)
      buffered = buffered.slice(end + 1)
      if (line.trim() === "") continue
      const event = JSON.parse(line) as Record<string, unknown>
      events.push(event)
      // One `result` per run: a `Stop` that continued the turn carries on under the same run, so
      // the next prompt is sent, and the last one's end closes stdin and ends the session.
      if (event["type"] !== "result") continue
      const next = options.prompts[sent]
      sent += 1
      if (next === undefined) {
        child.stdin.end()
        continue
      }
      await ready.promise
      prompt(next)
    }
  }
  await child.exited

  const debug = await readFile(join(configDir, "debug", `${SESSION}.txt`), "utf8")
  const paneLog = (await readFile(paneLogFile, "utf8").catch(() => "")).split("\n").filter(Boolean)
  const record = await readFile(phaseStallFile(configDir, SESSION), "utf8")
    .then((text): unknown => JSON.parse(text))
    .catch(() => undefined)
  return {
    events,
    turns: requests.filter((request) => request.path === "/v1/messages"),
    hooks: hookRuns(debug),
    mcpHookCalls: debug.split("\n").flatMap((line) => {
      const match = /Hooks: mcp_tool calling (\S+) with \d+ arg\(s\)$/.exec(line)
      return match === null ? [] : [match[1] ?? ""]
    }),
    stopHookActive: stopHookActive(paneLog),
    record,
    debug,
  }
}

/** The text of every user message in a Messages request, joined. */
function userText(request: Request): string {
  const messages = Array.isArray(request.body["messages"]) ? request.body["messages"] : []
  return JSON.stringify(
    messages.filter(
      (message: unknown) =>
        typeof message === "object" &&
        message !== null &&
        "role" in message &&
        message.role === "user",
    ),
  )
}

/** The stream's synthetic user messages: what Claude Code wrote into the session itself. */
function syntheticMessages(run: WorkerRun): string[] {
  return run.events.flatMap((event) => {
    if (event["type"] !== "user" || event["isSynthetic"] !== true) return []
    const message = event["message"] as { content?: unknown }
    const content = Array.isArray(message.content) ? message.content : []
    return content.flatMap((block: unknown) =>
      typeof block === "object" && block !== null && "text" in block ? [String(block.text)] : [],
    )
  })
}

test.skipIf(claude === undefined)(
  "the generated hook entries arm on the real binary: a Stop answered with a block carries the reminder into one more turn of the same session",
  async () => {
    if (claude === undefined) throw new Error("LEGION_TEST_CLAUDE is unset")
    const run = await runWorker({
      binary: claude,
      prompts: ["Seed the session.", `Implement ${ISSUE}.`],
      replies: [
        // The seeding turn settles on a WAITING line, so it earns no reminder whether or not the
        // claim boot has finished by the time its own hooks run: a settle with no claim yet
        // answers `{}`, and one with a claim buys the quiet wait instead of a continuation. The
        // assignment that follows puts the phase back to open with no reminders, either way.
        [{ type: "text", text: "WAITING on the daemon's assignment." }],
        [{ type: "text", text: "I have started." }],
        [{ type: "text", text: "Still going." }],
      ],
    })

    // 1. Claude Code armed the generated plugin's hook entries, from the tree that ships.
    expect(run.debug).toContain(
      `Read hooks.json for plugin legion (enabled=true): ${join(PACKAGE, "legion", "hooks", "hooks.json")}`,
    )
    expect(run.hooks.map(({ event }) => event)).toEqual([
      "UserPromptSubmit",
      "Stop",
      "UserPromptSubmit",
      "Stop",
      "Stop",
    ])

    // 2. Every one of them reached the pane's own MCP server under the bare name `--mcp-config`
    // registered it as, and that server is the package's built one.
    expect([...new Set(run.mcpHookCalls)]).toEqual(["legion/legion_phase_hook"])
    expect(run.mcpHookCalls).toHaveLength(run.hooks.length)
    const init = run.events.find((event) => event["subtype"] === "init")
    expect(init).toBeDefined()
    // `dynamic` is Claude Code's own word for a server `--mcp-config` named.
    expect(init?.["mcp_servers"]).toEqual([
      { name: "legion", status: "connected", source: "dynamic" },
    ])
    expect(init?.["tools"]).toContain("mcp__legion__legion_phase_hook")

    // 3. The `Stop` of the assignment's turn was answered with the block, and that block became
    // one more turn of the same session: the reminder arrived as a synthetic user message, a
    // second model request followed carrying it, and the `Stop` that ended it said so. Claude
    // Code also writes a `stop-hook-error` system notification beside the continuation (measured;
    // a run whose hooks all answer `{}` writes none) — its word for a hook that decided to block,
    // and nothing the worker sees.
    expect(run.hooks.at(-2)?.answer).toEqual({ decision: "block", reason: REMINDER })
    expect(syntheticMessages(run)).toEqual([`Stop hook feedback:\n${REMINDER}`])
    expect(run.turns).toHaveLength(3)
    expect(userText(run.turns[2] as Request)).toContain(REMINDER)
    expect(run.stopHookActive.slice(-2)).toEqual(["false", "true"])
    // The second `Stop` was answered with nothing, so the run ended there rather than looping.
    expect(run.hooks.at(-1)?.answer).toEqual({})

    // 4. No `PostToolUse` ran at all: the model called no tool, and the four `mcp_tool` hook calls
    // of this run are Claude Code's own, not the model's. The count the real server recorded is
    // the same fact from its side — a `PostToolUse` for the hook's own call would have put it
    // back to 0.
    expect(run.hooks.filter(({ event }) => event === "PostToolUse")).toEqual([])
    expect(run.record).toEqual({ state: "open", count: 1, reported: false })
  },
  180_000,
)

test.skipIf(claude === undefined)(
  "PostToolUse fires for the model's own tool call and for no call the hook itself makes",
  async () => {
    if (claude === undefined) throw new Error("LEGION_TEST_CLAUDE is unset")
    const run = await runWorker({
      binary: claude,
      prompts: ["Seed the session.", `Implement ${ISSUE}.`],
      replies: [
        [{ type: "text", text: "WAITING on the daemon's assignment." }],
        [{ type: "text", text: "I have started." }],
        // The continuation the block bought: the worker works, which is what puts the reminder
        // count back to 0.
        [{ type: "tool_use", name: "Bash", input: { command: "echo working" } }],
        [{ type: "text", text: "Done for now." }],
      ],
    })

    expect(run.hooks.map(({ event }) => event)).toEqual([
      "UserPromptSubmit",
      "Stop",
      "UserPromptSubmit",
      "Stop",
      "PostToolUse",
      "Stop",
    ])
    // Claude Code ran it for the model's own `Bash` call, and for nothing else: the five
    // `mcp_tool` calls the hooks themselves made fired no `PostToolUse` of their own, which is
    // what would otherwise let the reminder clear its own count.
    expect(
      run.hooks.filter(({ event }) => event === "PostToolUse").map(({ matcher }) => matcher),
    ).toEqual(["PostToolUse:Bash"])
    expect([...new Set(run.mcpHookCalls)]).toEqual(["legion/legion_phase_hook"])
    expect(run.mcpHookCalls).toHaveLength(6)
    // The real server's own record says the same: the reminder the block carried took the count
    // to 1, and the model's tool call in the continuation put it back to 0.
    expect(run.record).toEqual({ state: "open", count: 0, reported: false })
    expect(run.stopHookActive.slice(-2)).toEqual(["false", "true"])
  },
  180_000,
)
