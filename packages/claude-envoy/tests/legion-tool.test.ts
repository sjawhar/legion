import { expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"
import type { LegionRole } from "@legion/contracts"
import type { LegionPhase } from "@legion/contracts/legion-api"
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { executeLegionTool, legionToolDefinition } from "../src/legion-tool"

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
async function daemonFixture(name: string): Promise<unknown> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

interface Request {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

async function daemon(): Promise<{
  readonly client: LegionDaemonClient
  readonly requests: Request[]
}> {
  const answers: Record<string, unknown> = {
    "/legion/v1/state": await daemonFixture("state.json"),
    "/legion/v1/grants": await daemonFixture("grant.json"),
    "/legion/v1/handoff/complete": await daemonFixture("handoff-complete.json"),
    "/legion/v1/phase/backward": {},
  }
  const requests: Request[] = []
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    requests.push({
      method: init?.method ?? "GET",
      path,
      body: init?.body == null ? undefined : JSON.parse(String(init.body)),
    })
    return Response.json(answers[path])
  }) as typeof fetch
  return { client: createLegionDaemonClient("http://daemon.test", stub), requests }
}

const implementer: ClaimCapability = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
}

const grantRequest = {
  method: "POST",
  path: "/legion/v1/grants",
  body: {
    sessionId: "ses_implementer",
    secret: "U2VjcmV0",
    tree: "LEGION-208",
    issue: "LEGION-208",
  },
}

test("declares the three phase-worker operations under one op, and names the Envoy publish tool as Claude Code shows it", () => {
  const definition = legionToolDefinition()
  expect(definition.name).toBe("legion")
  expect(definition.inputSchema).toMatchObject({
    properties: {
      op: { enum: ["read_record", "handoff_complete", "request_backward_move"] },
      issue: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "fail"] },
      ready: { type: "boolean" },
      reason: { type: "string" },
    },
    required: ["op"],
  })
  expect(definition.description).toContain(
    "this tool cannot spawn workers. read_record returns the record of `issue` from the daemon's state. handoff_complete",
  )
  expect(definition.description).toContain(
    "arm nothing. request_backward_move asks the daemon to move this issue back to an earlier phase, `to`, for `reason`. What a later phase needs",
  )
  expect(definition.description).toContain("with mcp__legion__envoy_publish.")
})

test("handoff_complete finds the commit for the issue's phase and the claim's role, then mints its grant and posts", async () => {
  const { client, requests } = await daemon()
  const lookups: string[] = []
  const answer = await executeLegionTool(
    {
      daemon: client,
      claim: () => implementer,
      handoffCommit: async (phase: LegionPhase, role: LegionRole) => {
        lookups.push(`${phase} ${role}`)
        return "a".repeat(40)
      },
    },
    { op: "handoff_complete", summary: "Implemented it.", verdict: "pass" },
  )
  expect(lookups).toEqual(["implementing implementer"])
  expect(answer).toEqual(await daemonFixture("handoff-complete.json"))
  expect(requests).toEqual([
    { method: "GET", path: "/legion/v1/state", body: undefined },
    grantRequest,
    {
      method: "POST",
      path: "/legion/v1/handoff/complete",
      body: {
        grantId: "grant-for-one-command",
        summary: "Implemented it.",
        verdict: "pass",
        ready: false,
        commit: "a".repeat(40),
      },
    },
  ])
})

test("a handoff the lookup refuses posts nothing and mints nothing", async () => {
  const { client, requests } = await daemon()
  await expect(
    executeLegionTool(
      {
        daemon: client,
        claim: () => implementer,
        handoffCommit: async () => {
          throw new Error(".legion/LEGION-208/implement.json is missing from the workspace")
        },
      },
      { op: "handoff_complete", summary: "Done." },
    ),
  ).rejects.toThrow("is missing from the workspace")
  expect(requests.map(({ path }) => path)).toEqual(["/legion/v1/state"])
})

test("read_record answers the issue's record, and request_backward_move posts with its grant", async () => {
  const { client, requests } = await daemon()
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffCommit: async (): Promise<never> => {
      throw new Error("no lookup expected")
    },
  }
  const state = (await daemonFixture("state.json")) as { issues: Record<string, unknown> }
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-208" }),
  ).resolves.toEqual({
    record: state.issues["LEGION-208"],
  })
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-999" }),
  ).rejects.toThrow("The daemon has no record for LEGION-999")
  await expect(
    executeLegionTool(runtime, {
      op: "request_backward_move",
      to: "planning",
      reason: "The plan misses X.",
    }),
  ).resolves.toEqual({})
  expect(requests.slice(2)).toEqual([
    grantRequest,
    {
      method: "POST",
      path: "/legion/v1/phase/backward",
      body: { grantId: "grant-for-one-command", to: "planning", reason: "The plan misses X." },
    },
  ])
})

test("refuses before the claim registered, an operation a phase worker has not, and a field its operation does not take", async () => {
  const { client, requests } = await daemon()
  const handoffCommit = async (): Promise<string> => "c"
  await expect(
    executeLegionTool(
      { daemon: client, claim: () => undefined, handoffCommit },
      { op: "read_record", issue: "LEGION-208" },
    ),
  ).rejects.toThrow("this session has no Legion claim yet")
  const runtime = { daemon: client, claim: () => implementer, handoffCommit }
  await expect(
    executeLegionTool(runtime, { op: "set_status", issue: "LEGION-208" }),
  ).rejects.toThrow("op is one of read_record, handoff_complete, request_backward_move")
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-208", summary: "x" }),
  ).rejects.toThrow('read_record does not accept field "summary"')
  await expect(executeLegionTool(runtime, { op: "handoff_complete" })).rejects.toThrow(
    "handoff_complete requires summary",
  )
  expect(requests).toEqual([])
})
