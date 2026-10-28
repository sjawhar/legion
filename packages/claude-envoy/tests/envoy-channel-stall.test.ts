import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
} from "@legion/envoy-client/test/envoy-stack"
import { type StallProxy, startStallProxy } from "@legion/envoy-client/test/stall-proxy"
import { runFreezeScenario } from "@legion/envoy-client/test/stall-scenario"
import type { Subprocess } from "bun"
import { z } from "zod"
import { readReplayState, replayStateFile } from "../src/replay-state"

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

const ChannelNotificationSchema = z.object({
  method: z.literal("notifications/claude/channel"),
  params: z.object({ content: z.string() }),
})

/** A channel server process, initialized, whose only NATS path goes through `proxy`. */
interface ChannelServer {
  readonly server: Subprocess<"pipe", "pipe", "pipe">
  readonly proxy: StallProxy
  readonly sessionID: string
  readonly home: string
  /** The channel messages Claude Code would be shown, in the order the server sent them. */
  readonly channel: readonly string[]
  /** Resolves once `condition` holds; throws when the server exits or `deadline` passes first. */
  waitFor(
    condition: () => boolean | Promise<boolean>,
    deadline: number,
    what: string,
  ): Promise<void>
  /** Stops the server (SIGTERM, then SIGKILL after 10 s), closes the proxy, removes its home. */
  stop(): Promise<void>
}

/**
 * Starts the channel server from this checkout as Claude Code would, for a new session whose
 * Envoy is `stack`'s listener and whose NATS goes through a stall proxy to `stack`'s natsd, and
 * sends it the MCP initialize handshake.
 */
async function startChannelServer(
  stack: EnvoyStack,
  sessionPrefix: string,
): Promise<ChannelServer> {
  const proxy = await startStallProxy(stack.natsHostPort)
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-stall-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({ envoy: { enabled: true, url: stack.envoyUrl, natsUrls: [proxy.url] } }),
  )
  const sessionID = `${sessionPrefix}_${crypto.randomUUID().replaceAll("-", "")}`
  const server = Bun.spawn(["bun", channelEntry], {
    cwd: home,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: sessionID,
      CLAUDE_PLUGIN_DATA: home,
      // No heartbeat sweep runs in the test's window: the reconnect is the only catch-up trigger.
      ENVOY_HEARTBEAT_MS: "240000",
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  const channel: string[] = []
  void (async () => {
    let buffered = ""
    for await (const chunk of server.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk
      for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
        const notification = ChannelNotificationSchema.safeParse(
          JSON.parse(buffered.slice(0, newline)),
        )
        buffered = buffered.slice(newline + 1)
        if (notification.success) channel.push(notification.data.params.content)
      }
    }
  })()
  let stderr = ""
  void (async () => {
    for await (const chunk of server.stderr.pipeThrough(new TextDecoderStream())) stderr += chunk
  })()
  server.stdin.write(
    `${JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "test", version: "0" },
      },
    })}\n`,
  )
  server.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`)
  return {
    server,
    proxy,
    sessionID,
    home,
    channel,
    // The server runs on nats.js's real ping and dial timers and a real listener, so each wait
    // polls the real clock until its condition holds or its deadline passes.
    async waitFor(condition, deadline, what) {
      while (!(await condition())) {
        if (server.exitCode !== null) {
          throw new Error(
            `the channel server exited ${server.exitCode} waiting for ${what}: ${stderr}`,
          )
        }
        if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}: ${stderr}`)
        await Bun.sleep(100)
      }
    },
    async stop() {
      server.kill("SIGTERM")
      const killer = setTimeout(() => server.kill("SIGKILL"), 10_000)
      await server.exited
      clearTimeout(killer)
      await proxy.close()
      await rm(home, { recursive: true, force: true })
    },
  }
}

describe.skipIf(!envoyStackEnabled)(
  "the channel server against a real natsd and Envoy listener",
  () => {
    let stack: EnvoyStack | undefined
    beforeAll(async () => {
      stack = await startEnvoyStack()
    }, 120_000)
    afterAll(async () => {
      await stack?.stop()
    }, 60_000)

    test("a session whose NATS path freezes reconnects within 60 s and shows the message sent meanwhile once, replayed", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      const { proxy, sessionID, home, channel, waitFor, stop } = await startChannelServer(
        stack,
        "ses_stall",
      )
      const directSubject = `notifications.agent.${sessionID}`
      try {
        const started = Date.now()
        await waitFor(
          async () => (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok,
          started + 30_000,
          "the session's registration",
        )
        // The bundle runs from this checkout, outside Claude Code's plugin cache.
        const registration = {
          session_id: sessionID,
          plugin: "@sjawhar/claude-envoy",
          plugin_version: "source",
        }
        expect(await (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).json()).toMatchObject(
          registration,
        )
        expect(await (await fetch(`${envoyUrl}/v1/sessions`)).json()).toContainEqual(
          expect.objectContaining(registration),
        )
        // The first sweep started the direct subject's cursor, and the subscription went out first.
        const stateFile = replayStateFile(home, sessionID)
        await waitFor(
          async () =>
            (await readReplayState(stateFile)).state?.subjects[directSubject]?.after !==
              undefined && proxy.connections[0]?.subscribedAt !== undefined,
          started + 30_000,
          "the session's first sweep",
        )

        await runFreezeScenario({
          label: "claude-envoy stall",
          envoyUrl,
          sessionID,
          proxy,
          shown: (marker) => channel.filter((content) => content.includes(marker)),
          waitFor,
        })
      } finally {
        await stop()
      }
    }, 120_000)

    test("a session whose redial once hung still exits on SIGTERM", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      const { server, proxy, sessionID, waitFor, stop } = await startChannelServer(
        stack,
        "ses_stall_exit",
      )
      try {
        const started = Date.now()
        await waitFor(
          async () =>
            (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok &&
            proxy.connections[0]?.subscribedAt !== undefined,
          started + 30_000,
          "the session's registration and first subscription",
        )
        // The first redial after the freeze hangs (accepted, never answered); the next one forwards.
        proxy.stallNext(1)
        const frozenAt = proxy.freeze()
        await waitFor(
          () =>
            proxy.connections.some(
              (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined,
            ),
          frozenAt + 60_000,
          "the session to subscribe again after the hung redial",
        )
        const signalledAt = Date.now()
        server.kill("SIGTERM")
        const outcome = await Promise.race([
          server.exited.then(() => "exited"),
          Bun.sleep(15_000).then(() => "running"),
        ])
        console.log(
          `claude-envoy stall exit: SIGTERM -> ${outcome} after ${((Date.now() - signalledAt) / 1000).toFixed(1)} s`,
        )
        expect(outcome).toBe("exited")
      } finally {
        await stop()
      }
    }, 120_000)
  },
)
