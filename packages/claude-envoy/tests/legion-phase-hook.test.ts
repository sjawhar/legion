import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { EnvoyApiError, type PublishInput } from "@legion/envoy-client/transport"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { REMINDER } from "@legion/pi-shared/phase-stall"
import {
  createPhaseHookEndpoint,
  PHASE_HOOK_REFUSAL,
  PHASE_HOOK_TOOL_NAME,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "../src/legion-phase-hook"
import { waitFor } from "./channel-session-harness"

const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

/** The pane's claim: the implementer of LEGION-208, whose phase in the daemon's state fixture is
 * `implementing`, so the reminder's record check says this role still works this phase. */
const implementer: ClaimCapability = {
  sessionID: SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
}

/**
 * The four numbers every case runs on, through the environment the endpoint is given: no test
 * waits a production interval. The architect is held off at nine reminders so only the case that
 * drives the notice reaches it.
 *
 * The clock is the real one, as `waitFor`'s is: what the cases drive is the endpoint's own
 * `setTimeout`, whose callback then awaits a daemon read, so advancing a fake clock would settle
 * nothing on its own. Every case waits for the condition it is about; the few fixed waits are the
 * ones asserting that nothing happens, which have no condition to wait for, and each is a small
 * multiple of the interval above.
 */
const QUICK_LIMITS: Record<string, string> = {
  LEGION_STALL_REMINDER_MS: "20",
  LEGION_STALL_WAITING_MS: "80",
  LEGION_STALL_RESUME_MS: "20",
  LEGION_STALL_ARCHITECT_AFTER: "9",
}

/** One record on disk, as the Go bridge or an earlier run of this server left it. */
function recorded(state: string, count: number, extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ state, count, reported: false, ...extra })
}

interface Harness {
  readonly endpoint: PhaseHookEndpoint
  readonly file: string
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon answered the record check. */
  readonly recordAtDaemonRead: () => unknown
  /** The lines this endpoint wrote into the bridge's inbox, in order. */
  readonly inbox: readonly string[]
  /** Every message published to the Envoy listener, in order. */
  readonly published: readonly PublishInput[]
  /** Settles the claim's boot, as `startLegionClaim`'s `booted` does. */
  readonly boot: () => void
  readonly lines: readonly string[]
  readonly close: () => Promise<void>
}

async function harness(
  options: {
    readonly record?: string
    readonly claim?: ClaimCapability
    /** Added to (and overriding) `QUICK_LIMITS` and `LEGION_PROJECT`. */
    readonly env?: Record<string, string | undefined>
    /** False for a session outside a Legion pane, which has no inbox at all. */
    readonly inbox?: boolean
    /** What the listener answers the `attempt`th publish; accepted by default. */
    readonly publish?: (input: PublishInput, attempt: number) => Promise<unknown>
  } = {},
): Promise<Harness> {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-phase-hook-"))
  const file = phaseStallFile(directory, SESSION)
  if (options.record !== undefined) await writeFile(file, options.record)
  const state = JSON.parse(
    await readFile(
      resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
      "utf8",
    ),
  )
  const record = async (): Promise<unknown> => {
    const raw = await readFile(file, "utf8").catch(() => undefined)
    return raw === undefined ? undefined : JSON.parse(raw)
  }
  let atRead: unknown
  const fetchStub = (async (_input, _init) => {
    atRead = await record()
    return Response.json(state)
  }) as typeof fetch
  const daemon = createLegionDaemonClient("http://daemon.test", fetchStub)
  const lines: string[] = []
  const inbox: string[] = []
  const published: PublishInput[] = []
  const booted = Promise.withResolvers<void>()
  const endpoint = createPhaseHookEndpoint({
    stateFile: file,
    claim: () => options.claim,
    booted: booted.promise,
    readState: () => daemon.state(),
    inbox:
      options.inbox === false
        ? undefined
        : async (text) => {
            inbox.push(text)
          },
    publish: async (input) => {
      published.push(input)
      return options.publish === undefined ? {} : options.publish(input, published.length)
    },
    env: { LEGION_PROJECT: "legion", ...QUICK_LIMITS, ...options.env },
    log: paneReminderLog((line) => lines.push(line)),
  })
  return {
    endpoint,
    file,
    record,
    recordAtDaemonRead: () => atRead,
    inbox,
    published,
    boot: booted.resolve,
    lines,
    close: async () => {
      // As the server's own `stop` does: no armed timer outlives the case that armed it.
      endpoint.stop()
      await rm(directory, { recursive: true, force: true })
    },
  }
}

test("the tool names itself Claude Code's hook endpoint and tells the model not to call it", () => {
  const definition = phaseHookToolDefinition()
  expect(definition.name).toBe(PHASE_HOOK_TOOL_NAME)
  expect(definition.description).toContain("hook endpoint")
  expect(definition.description).toContain("Do not call it")
})

test("a turn settling on an open phase is blocked once, with the reminder, and the count is recorded before the answer", async () => {
  const { endpoint, record, recordAtDaemonRead, inbox, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "I pushed the branch.",
        stopHookActive: "false",
      }),
    ).toEqual({ decision: "block", reason: REMINDER })
    // The record check reads the daemon before the reminder is counted, and the count is on disk
    // by the time the hook answers: a reminder the worker has been given is never uncounted. What
    // the worker said rides the record with it, for a relaunch's architect notice to quote.
    const said = { reported: false, lastMessage: "I pushed the branch." }
    expect(recordAtDaemonRead()).toEqual({ state: "open", count: 0, ...said })
    expect(await record()).toEqual({ state: "open", count: 1, ...said })

    // The second settle of the run lets the turn end and carries its reminder from the timer, so
    // the daemon sees the turn end and can deliver a queued task.
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Still here.",
        stopHookActive: "true",
      }),
    ).toEqual({})
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the timer's reminder to reach the inbox")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({
      state: "open",
      count: 2,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

test("a settle that says what it waits on buys the quiet wait, and is reminded once after it", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "WAITING on CI to go green on the branch.",
      }),
    ).toEqual({})
    const waiting = { reported: false, lastMessage: "WAITING on CI to go green on the branch." }
    expect(await record()).toEqual({ state: "waiting", count: 0, ...waiting })
    // Nothing is sent into the wait itself.
    await Bun.sleep(30)
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the reminder the wait earned")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "open", count: 1, ...waiting })
    // One reminder, not a second from the same wait.
    await Bun.sleep(60)
    expect(inbox).toEqual([REMINDER])
  } finally {
    await close()
  }
})

test("a turn that ended on an API error is settled, answered {} and reminded from the timer", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    // Claude Code ignores what a StopFailure hook returns, so the machine is told this settle can
    // carry no continuation: it arms the reminder itself, under the guards every armed reminder
    // has, and the count moves when that reminder is written into the session.
    expect(
      await endpoint.handle({ event: "StopFailure", lastAssistantMessage: "I pushed the branch." }),
    ).toEqual({})
    const said = { reported: false, lastMessage: "I pushed the branch." }
    expect(await record()).toEqual({ state: "open", count: 0, ...said })
    await waitFor(async () => inbox.length > 0, "the reminder the failed turn earned")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "open", count: 1, ...said })
  } finally {
    await close()
  }
})

test("a successful handoff_complete closes the phase, and the next settle blocks nothing", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
  })
  try {
    // What the server's CallTool handler does once the `legion` tool returned without throwing
    // (`./src/envoy-channel-server`): the worker completed its phase, so nothing is open to
    // remind it about any more.
    endpoint.phaseClosed()
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })

    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Completed the phase." }),
    ).toEqual({})
    // Neither a continuation nor a timer: the record check is not what stands between a finished
    // worker and a reminder telling it to complete again. A case asserting that nothing happens
    // has no condition to wait for, so this is one of the file's fixed waits, three times the
    // reminder interval above.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("the prompt that carries this endpoint's own reminder does not reset its count", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length > 0, "the timer's reminder to reach the inbox")
    const said = { reported: false, lastMessage: "Still here." }
    expect(await record()).toEqual({ state: "open", count: 2, ...said })
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 2, ...said })
  } finally {
    await close()
  }
})

test("a prompt carrying a line this server wrote for a delivery is that delivery's turn", async () => {
  const delivery = "<channel>the architect asks for a status</channel>"
  const open = await harness({ record: recorded("open", 2), claim: implementer })
  try {
    open.endpoint.deliveryWritten(delivery)
    expect(await open.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })).toEqual({})
    expect(await open.record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await open.close()
  }

  // A delivery is something happening, but it is not an assignment: it reopens no closed phase.
  // The line is dropped as its prompt arrives, so the same text next time is an assignment.
  const closed = await harness({ record: recorded("closed", 0), claim: implementer })
  try {
    closed.endpoint.deliveryWritten(delivery)
    await closed.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })
    expect(await closed.record()).toEqual({ state: "closed", count: 0, reported: false })
    await closed.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })
    expect(await closed.record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await closed.close()
  }
})

test("a prompt carrying anything else is the daemon's assignment and opens the phase", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("closed", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement INFRA-220." }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a helper agent's tool call is not the worker working, and does not reset the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    for (const event of ["PostToolUse", "PostToolUseFailure"]) {
      expect(await endpoint.handle({ event, toolName: "Bash", agentId: "agent-7" })).toEqual({})
      expect(await record()).toEqual({ state: "open", count: 2, reported: false })
    }
    expect(await endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })).toEqual(
      {},
    )
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a tool call that failed is work too, and resets the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({ event: "PostToolUseFailure", toolName: "Bash", agentId: "" }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("the architect is told at the threshold, asked again when no session holds the role, and never twice", async () => {
  const { endpoint, record, inbox, published, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "2" },
    publish: async (_input, attempt) => {
      if (attempt > 1) return {}
      // The listener's "no live session holds that role".
      throw new EnvoyApiError({
        method: "POST",
        url: "http://listener.test/v1/messages/publish",
        status: 404,
        responseBody: JSON.stringify({ error: "no holder", reason: "unclaimed" }),
      })
    },
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => published.length === 1, "the architect to be told")
    const refused = (await record()) as { reported: boolean; reportKey: string }
    // The refusal leaves the notice to be sent again, under the key it was minted with.
    expect(refused.reported).toBe(false)
    expect(refused.reportKey).toMatch(/^[0-9a-f-]{36}$/)
    expect(published[0]?.topic).toContain("architect")
    expect(published[0]?.message).toContain("2 phase reminders")

    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => published.length === 2, "the architect to be told again")
    expect(published[1]?.idempotencyKey).toBe(refused.reportKey)
    expect(await record()).toEqual({
      state: "open",
      count: 3,
      reported: true,
      reportKey: refused.reportKey,
      lastMessage: "Still here.",
    })

    // Accepted once is told once: the run's later reminders add no second notice.
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length === 3, "the third reminder of the run")
    expect(published.length).toBe(2)
  } finally {
    await close()
  }
})

test("a timer whose turn has started again writes nothing", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with INFRA-220." }),
    ).toEqual({})
    // Three times the 20 ms the armed reminder was waiting for.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "open",
      count: 0,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

test("a session resumed on an open phase is reminded once its claim has booted", async () => {
  const { record, inbox, boot, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    // Nothing is armed before the claim has booted: the reminder needs the claim it gates on.
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    boot()
    await waitFor(async () => inbox.length > 0, "the resume reminder")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "open", count: 1, reported: false })
  } finally {
    await close()
  }
})

test("a resumed session whose task arrives first is not reminded on top of it", async () => {
  const { endpoint, inbox, boot, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    // The daemon re-sending an interrupted task arrives as a prompt, and drops the resume
    // reminder through the same generation guard.
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with INFRA-220." }),
    ).toEqual({})
    boot()
    await Bun.sleep(60)
    expect(inbox).toEqual([])
  } finally {
    await close()
  }
})

test("a worker whose workspace was recreated is told once, as a turn of its own", async () => {
  const { endpoint, record, inbox, boot, close } = await harness({
    record: recorded("closed", 0),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
  })
  try {
    boot()
    await waitFor(async () => inbox.length > 0, "the recreated-workspace notice")
    expect(inbox[0]).toContain("it holds what was pushed to legion/LEGION-208")
    // Recorded in the record the phase and count live in, so a server Claude Code restarts inside
    // the same process does not repeat it.
    expect(await record()).toEqual({
      state: "closed",
      count: 0,
      reported: false,
      recreatedNoticeSent: true,
    })
    // The turn it starts is this server's own line, not the daemon's assignment: a phase that was
    // over stays over.
    await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })
    expect(await record()).toEqual({
      state: "closed",
      count: 0,
      reported: false,
      recreatedNoticeSent: true,
    })
    await Bun.sleep(40)
    expect(inbox.length).toBe(1)
  } finally {
    await close()
  }
})

test("a record that already says the recreated-workspace notice was sent earns no second one", async () => {
  const { inbox, boot, close } = await harness({
    record: recorded("closed", 0, { recreatedNoticeSent: true }),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
  })
  try {
    boot()
    await Bun.sleep(40)
    expect(inbox).toEqual([])
  } finally {
    await close()
  }
})

test("a workspace recreated again, after a launch that recreated nothing, is told again", async () => {
  // Three launches of one session, each a server reading the record the one before it left: the
  // first recreated the workspace, the second did not, and the third recreated it again. The
  // launcher decides `LEGION_WORKSPACE_RECREATED` afresh for every launch, so the third is a new
  // recreation the worker has not been told of.
  const launch = async (record: string, recreated: "true" | "false") => {
    const {
      inbox,
      boot,
      record: written,
      close,
    } = await harness({
      record,
      claim: implementer,
      env: { LEGION_WORKSPACE_RECREATED: recreated, LEGION_STALL_RESUME_MS: "600000" },
    })
    try {
      boot()
      if (recreated === "true") {
        await waitFor(async () => inbox.length > 0, "the recreated-workspace notice")
      } else {
        // Nothing is to happen, so there is no condition to wait for: the file's own small
        // multiple of the 20 ms interval, as its other nothing-happens cases use.
        await Bun.sleep(40)
      }
      return { told: inbox.length, record: JSON.stringify(await written()) }
    } finally {
      await close()
    }
  }
  const first = await launch(recorded("closed", 0), "true")
  expect(first.told).toBe(1)
  const second = await launch(first.record, "false")
  expect(second.told).toBe(0)
  const third = await launch(second.record, "true")
  expect(third.told).toBe(1)
  // Three launches, each of which may wait out `waitFor`'s own 5 s deadline.
}, 20_000)

test("the architect notice a resumed worker's reminder earns names what the worker last said before its relaunch", async () => {
  const before = await harness({
    record: recorded("open", 0),
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "3" },
  })
  let carried: string
  try {
    expect(
      await before.endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed the change." }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(
      await before.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Still pushing the change.",
      }),
    ).toEqual({})
    await waitFor(async () => before.inbox.length === 1, "the timer's reminder")
    carried = JSON.stringify(await before.record())
  } finally {
    await before.close()
  }

  // The relaunch: the same record, no task re-sent, so the resume reminder is the third
  // unanswered one and earns the architect notice.
  const after = await harness({
    record: carried,
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "3" },
  })
  try {
    after.boot()
    await waitFor(async () => after.published.length === 1, "the architect notice")
    const notice = after.published[0]
    expect(notice?.message).toContain("Still pushing the change.")
    expect(JSON.parse(String(notice?.payload)).lastMessage).toBe("Still pushing the change.")
  } finally {
    await after.close()
  }
})

test("a launcher that recreated nothing sends no notice and records no key", async () => {
  const { record, inbox, boot, close } = await harness({
    record: recorded("closed", 0),
    claim: implementer,
  })
  try {
    boot()
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a record the deployed build wrote reads as an open phase with no reminders yet", async () => {
  const { endpoint, record, close } = await harness({
    record: JSON.stringify({ state: "quiet" }),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "" })).toEqual({
      decision: "block",
      reason: REMINDER,
    })
    expect(await record()).toEqual({ state: "open", count: 1, reported: false })
  } finally {
    await close()
  }
})

test("a session holding no claim, as the bridge's seeding /rename turn does, is never reminded", async () => {
  const { endpoint, record, close } = await harness({ record: recorded("open", 0) })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Renamed." })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("an architect's session runs no phase reminder", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 0),
    claim: { ...implementer, role: "architect" },
  })
  try {
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Started the tester." }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a call naming no event, or one this endpoint does not know, is refused and moves nothing", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    for (const call of [{}, { event: "SessionEnd" }, "Stop", undefined]) {
      expect(await endpoint.handle(call)).toEqual({ error: PHASE_HOOK_REFUSAL })
    }
    expect(await record()).toEqual({ state: "open", count: 2, reported: false })
  } finally {
    await close()
  }
})

test("a session with no record at all is no record, not a fresh open phase", async () => {
  const { endpoint, record, close } = await harness({ claim: implementer })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Done." })).toEqual({})
    expect(await record()).toBeUndefined()
  } finally {
    await close()
  }
})

test("a session outside a Legion pane has no inbox, and says so rather than losing the reminder", async () => {
  const { endpoint, lines, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
    inbox: false,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(
      async () => lines.some((line) => line.includes("has no way to reach this worker")),
      "the log line naming the missing inbox",
    )
  } finally {
    await close()
  }
})
