import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { REMINDER } from "@legion/pi-shared/phase-stall"
import { DEFAULT_STALL_LIMITS } from "@legion/pi-shared/stall-limits"
import {
  createPhaseHookEndpoint,
  PHASE_HOOK_REFUSAL,
  PHASE_HOOK_TOOL_NAME,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "../src/legion-phase-hook"

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

interface Harness {
  readonly endpoint: PhaseHookEndpoint
  readonly file: string
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon answered the record check. */
  readonly recordAtDaemonRead: () => unknown
  readonly lines: readonly string[]
  readonly close: () => Promise<void>
}

async function harness(
  options: { readonly record?: string; readonly claim?: ClaimCapability } = {},
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
  const endpoint = createPhaseHookEndpoint({
    stateFile: file,
    claim: () => options.claim,
    readState: () => daemon.state(),
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: paneReminderLog((line) => lines.push(line)),
  })
  return {
    endpoint,
    file,
    record,
    recordAtDaemonRead: () => atRead,
    lines,
    close: () => rm(directory, { recursive: true, force: true }),
  }
}

test("the tool names itself Claude Code's hook endpoint and tells the model not to call it", () => {
  const definition = phaseHookToolDefinition()
  expect(definition.name).toBe(PHASE_HOOK_TOOL_NAME)
  expect(definition.description).toContain("hook endpoint")
  expect(definition.description).toContain("Do not call it")
})

test("a turn settling on an open phase is blocked once, with the reminder, and the count is recorded before the answer", async () => {
  const { endpoint, record, recordAtDaemonRead, close } = await harness({
    record: JSON.stringify({ state: "open", count: 0, reported: false }),
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
    // by the time the hook answers: a reminder the worker has been given is never uncounted.
    expect(recordAtDaemonRead()).toEqual({ state: "open", count: 0, reported: false })
    expect(await record()).toEqual({ state: "open", count: 1, reported: false })

    // The second settle of the run is the timer's, which this build does not arm yet.
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Still here.",
        stopHookActive: "true",
      }),
    ).toEqual({})
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
  const { endpoint, record, close } = await harness({
    record: JSON.stringify({ state: "open", count: 0, reported: false }),
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Renamed." })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("an architect's session runs no phase reminder", async () => {
  const { endpoint, record, close } = await harness({
    record: JSON.stringify({ state: "open", count: 0, reported: false }),
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
    record: JSON.stringify({ state: "open", count: 2, reported: false }),
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

test("the four events this build does not yet handle answer {} and record nothing", async () => {
  const { endpoint, record, close } = await harness({
    record: JSON.stringify({ state: "open", count: 2, reported: false }),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "StopFailure", lastAssistantMessage: "" })).toEqual({})
    expect(await endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })).toEqual(
      {},
    )
    expect(
      await endpoint.handle({ event: "PostToolUseFailure", toolName: "Bash", agentId: "" }),
    ).toEqual({})
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: "go on" })).toEqual({})
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
