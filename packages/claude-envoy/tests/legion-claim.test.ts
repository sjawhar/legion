import { afterEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { SetRoleInput, SetRoleResult } from "@legion/envoy-client/transport"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { legion } from "../package.json" with { type: "json" }
import type { RoleRegainHook } from "../src/envoy-channel-server"
import { legionPane, startLegionClaim, waitForTranscript } from "../src/legion-claim"

const SESSION = "0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c"
const scratch: string[] = []
afterEach(async () => {
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

async function tempDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "claude-envoy-legion-claim-"))
  scratch.push(dir)
  return dir
}

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
async function daemonFixture(name: string): Promise<Record<string, unknown>> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

interface Posted {
  readonly path: string
  readonly body: Record<string, unknown>
}

function daemonAnswering(answer: (path: string) => Response): {
  readonly posted: Posted[]
  readonly fetch: typeof fetch
} {
  const posted: Posted[] = []
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    posted.push({ path, body: JSON.parse(String(init?.body)) })
    return answer(path)
  }) as typeof fetch
  return { posted, fetch: stub }
}

test("a session with no boot token is no Legion pane", () => {
  expect(legionPane({ LEGION_DAEMON_URL: "http://daemon.test" })).toBeUndefined()
  expect(legionPane({ LEGION_BOOT_TOKEN: " " })).toBeUndefined()
})

test("a Legion pane reads its boot token from the file the daemon named, and needs its address and directory", async () => {
  const dir = await tempDir()
  const tokenFile = join(dir, "boot")
  await writeFile(tokenFile, "boot-token-from-the-file\n")
  expect(
    legionPane({
      LEGION_BOOT_TOKEN_FILE: tokenFile,
      LEGION_BOOT_TOKEN: "ignored",
      LEGION_DAEMON_URL: "http://daemon.test",
      CLAUDE_CONFIG_DIR: dir,
    }),
  ).toEqual({
    daemonUrl: "http://daemon.test",
    bootToken: "boot-token-from-the-file",
    configDir: dir,
  })
  expect(() => legionPane({ LEGION_BOOT_TOKEN: "t", CLAUDE_CONFIG_DIR: dir })).toThrow(
    "LEGION_DAEMON_URL is required for Legion",
  )
  expect(() =>
    legionPane({ LEGION_BOOT_TOKEN: "t", LEGION_DAEMON_URL: "http://daemon.test" }),
  ).toThrow("CLAUDE_CONFIG_DIR is required for Legion")
  expect(() =>
    legionPane({
      LEGION_BOOT_TOKEN_FILE: join(dir, "missing"),
      LEGION_DAEMON_URL: "http://daemon.test",
      CLAUDE_CONFIG_DIR: dir,
    }),
  ).toThrow("LEGION_BOOT_TOKEN_FILE")
})

test("the transcript wait answers the file Claude Code wrote under any project, and stops when aborted", async () => {
  const configDir = await tempDir()
  const aborted = new AbortController()
  const waiting = waitForTranscript(configDir, SESSION, aborted.signal, 5)
  aborted.abort()
  await expect(waiting).rejects.toThrow()
  const file = join(configDir, "projects", "-work", `${SESSION}.jsonl`)
  await mkdir(join(configDir, "projects", "-work"), { recursive: true })
  await writeFile(file, "{}\n")
  await mkdir(join(configDir, "projects", "-other"), { recursive: true })
  await expect(waitForTranscript(configDir, SESSION, new AbortController().signal)).resolves.toBe(
    file,
  )
})

test("registers once the transcript exists, claims the claim token's role soft, says ready, and says it again on a regain", async () => {
  const configDir = await tempDir()
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const setRoles: SetRoleInput[] = []
  const remembered: string[] = []
  let regain: RoleRegainHook | undefined
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    client: {
      setRole: async (input): Promise<SetRoleResult> => {
        setRoles.push(input)
        return {
          claimed: true,
          interest: { session_id: SESSION, machine_id: "m", dir: "/w", topics: [] },
        }
      },
    },
    session: {
      rememberRole: async (role) => {
        remembered.push(role)
      },
      onRoleRegained: (hook) => {
        regain = hook
      },
    },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
    pollMs: 5,
  })

  // The claim is polling: the bridge's seed writes the transcript only after the server started.
  expect(claim.capability()).toBeUndefined()
  const transcript = join(configDir, "projects", "-work-legion-208", `${SESSION}.jsonl`)
  await mkdir(join(configDir, "projects", "-work-legion-208"), { recursive: true })
  await writeFile(transcript, "{}\n")
  await claim.booted

  const ready = {
    claimToken: registered["claimToken"],
    sessionId: SESSION,
    secret: registered["secret"],
    generation: registered["generation"],
  }
  expect(posted).toEqual([
    {
      path: "/legion/v1/claims/register",
      body: {
        bootToken: "boot-1",
        sessionId: SESSION,
        ompSessionFile: transcript,
        agentId: SESSION,
        pluginContract: legion.daemonApiVersion,
      },
    },
    { path: "/legion/v1/claims/ready", body: ready },
  ])
  expect(setRoles).toEqual([
    { sessionID: SESSION, role: String(registered["claimToken"]), soft: true },
  ])
  expect(remembered).toEqual([String(registered["claimToken"])])
  expect(claim.capability()).toEqual({
    sessionID: SESSION,
    tree: String(registered["tree"]),
    issue: String(registered["issue"]),
    role: registered["role"] as never,
    secret: String(registered["secret"]),
  })
  expect(stops).toBe(0)
  expect(lines).toEqual([])

  await regain?.("some-other-role", "reclaimed")
  expect(posted).toHaveLength(2)
  await regain?.(String(registered["claimToken"]), "reregistered")
  expect(posted.at(-1)).toEqual({ path: "/legion/v1/claims/ready", body: ready })
  expect(lines).toEqual([
    `re-ran claims/ready after role ${registered["claimToken"]} was reregistered`,
  ])
})

test("a registration the daemon refuses logs the refusal, stops the pane, and claims no role", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const refusal = await daemonFixture("error.json")
  const { fetch } = daemonAnswering(() => Response.json(refusal, { status: 409 }))
  const lines: string[] = []
  let stops = 0
  const setRoles: SetRoleInput[] = []
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "spent", configDir },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    client: {
      setRole: async (input) => {
        setRoles.push(input)
        return { claimed: false, holder: "x" }
      },
    },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  await claim.booted
  expect(lines).toEqual([`claims/register registration failed (409): ${refusal["error"]}`])
  expect(stops).toBe(1)
  expect(setRoles).toEqual([])
  expect(claim.capability()).toBeUndefined()
})

test("a role another live session holds stops the pane after the registration, logging why", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering(() => Response.json(registered))
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    client: { setRole: async () => ({ claimed: false, holder: "ses_other" }) },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  await claim.booted
  expect(posted.map(({ path }) => path)).toEqual(["/legion/v1/claims/register"])
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${registered["claimToken"]}; stopping the pane so the daemon launches it again: Envoy role ${registered["claimToken"]} is held by live session ses_other`,
  ])
  expect(stops).toBe(1)
})

test("a registration the daemon failed (5xx) is logged and leaves the pane to the registration deadline", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const { fetch } = daemonAnswering(() =>
    Response.json({ error: "register failed" }, { status: 503 }),
  )
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    client: { setRole: async () => ({ claimed: false, holder: "x" }) },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  await claim.booted
  expect(lines).toEqual([
    "claims/register registration failed (503): register failed",
    "claim boot failed before a registration; the daemon's registration deadline relaunches the pane: POST /legion/v1/claims/register failed with 503: register failed",
  ])
  expect(stops).toBe(0)
})
