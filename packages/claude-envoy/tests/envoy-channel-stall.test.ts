import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
} from "@legion/envoy-client/testing/envoy-stack"
import { startStallProxy } from "@legion/envoy-client/testing/stall-proxy"
import { z } from "zod"
import { readReplayState, replayStateFile } from "../src/replay-state"

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

/** What a replayed channel message ends with (`replayedText`); a live one has no such line. */
const REPLAYED = "while this session's connection was down."

const ChannelNotificationSchema = z.object({
  method: z.literal("notifications/claude/channel"),
  params: z.object({ content: z.string() }),
})

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
      const proxy = await startStallProxy(stack.natsHostPort)
      const home = await mkdtemp(join(tmpdir(), "claude-envoy-stall-"))
      const configDirectory = join(home, "claude-config")
      await mkdir(configDirectory)
      await writeFile(
        join(configDirectory, "envoy.json"),
        JSON.stringify({ envoy: { enabled: true, url: envoyUrl, natsUrls: [proxy.url] } }),
      )
      const sessionID = `ses_stall_${crypto.randomUUID().replaceAll("-", "")}`
      const directSubject = `notifications.agent.${sessionID}`
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
      // The channel messages Claude Code would be shown, in the order the server sent them.
      const channel: string[] = []
      void (async () => {
        let buffered = ""
        for await (const chunk of server.stdout.pipeThrough(new TextDecoderStream())) {
          buffered += chunk
          for (
            let newline = buffered.indexOf("\n");
            newline !== -1;
            newline = buffered.indexOf("\n")
          ) {
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
        for await (const chunk of server.stderr.pipeThrough(new TextDecoderStream()))
          stderr += chunk
      })()
      // The server runs on nats.js's real ping and dial timers and a real listener, so each wait
      // polls the real clock until its condition holds or its deadline passes.
      const waitFor = async (
        condition: () => boolean | Promise<boolean>,
        deadline: number,
        what: string,
      ): Promise<void> => {
        while (!(await condition())) {
          if (server.exitCode !== null) {
            throw new Error(
              `the channel server exited ${server.exitCode} waiting for ${what}: ${stderr}`,
            )
          }
          if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}: ${stderr}`)
          await Bun.sleep(100)
        }
      }
      const shown = (marker: string) => channel.filter((content) => content.includes(marker))
      const send = async (marker: string) => {
        const answer = await fetch(`${envoyUrl}/v1/messages/send`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ target_session: sessionID, source: "agent", message: marker }),
        })
        expect(answer.status).toBe(200)
      }
      const run = crypto.randomUUID()
      try {
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
        server.stdin.write(
          `${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`,
        )
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

        const live = `stall-live-before-${run}`
        await send(live)
        await waitFor(() => shown(live).length > 0, Date.now() + 10_000, "the live message")

        const frozenAt = proxy.freeze()
        const missed = `stall-missed-${run}`
        await send(missed)
        await waitFor(
          () => shown(missed).length > 0,
          frozenAt + 60_000,
          "the message sent while the path was frozen",
        )
        const shownAt = Date.now()
        const resubscribed = proxy.connections.find(
          (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined,
        )
        console.log(
          `claude-envoy stall: resubscribed ${(((resubscribed?.subscribedAt ?? Number.NaN) - frozenAt) / 1000).toFixed(1)} s, missed message shown ${((shownAt - frozenAt) / 1000).toFixed(1)} s after the freeze`,
        )
        expect(resubscribed?.subscribedAt).toBeDefined()
        expect((resubscribed?.subscribedAt ?? Number.POSITIVE_INFINITY) - frozenAt).toBeLessThan(
          60_000,
        )
        expect(shownAt - frozenAt).toBeLessThan(60_000)

        const after = `stall-live-after-${run}`
        await send(after)
        await waitFor(
          () => shown(after).length > 0,
          Date.now() + 10_000,
          "the message after the reconnect",
        )

        // Each shown once: the two live without the replay line, the frozen window's replayed.
        for (const [marker, replayed] of [
          [live, false],
          [missed, true],
          [after, false],
        ] as const) {
          const messages = shown(marker)
          expect(messages).toHaveLength(1)
          expect(messages[0]?.includes(REPLAYED)).toBe(replayed)
        }
      } finally {
        server.kill("SIGTERM")
        const killer = setTimeout(() => server.kill("SIGKILL"), 10_000)
        await server.exited
        clearTimeout(killer)
        await proxy.close()
        await rm(home, { recursive: true, force: true })
      }
    }, 120_000)

    test("a session whose redial once hung still exits on SIGTERM", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      const proxy = await startStallProxy(stack.natsHostPort)
      const home = await mkdtemp(join(tmpdir(), "claude-envoy-stall-exit-"))
      const configDirectory = join(home, "claude-config")
      await mkdir(configDirectory)
      await writeFile(
        join(configDirectory, "envoy.json"),
        JSON.stringify({ envoy: { enabled: true, url: envoyUrl, natsUrls: [proxy.url] } }),
      )
      const sessionID = `ses_stall_exit_${crypto.randomUUID().replaceAll("-", "")}`
      const server = Bun.spawn(["bun", channelEntry], {
        cwd: home,
        env: {
          PATH: process.env["PATH"] ?? "",
          HOME: home,
          CLAUDE_CONFIG_DIR: configDirectory,
          CLAUDE_CODE_SESSION_ID: sessionID,
          CLAUDE_PLUGIN_DATA: home,
          ENVOY_HEARTBEAT_MS: "240000",
        },
        stdin: "pipe",
        stdout: "ignore",
        stderr: "pipe",
      })
      let stderr = ""
      void (async () => {
        for await (const chunk of server.stderr.pipeThrough(new TextDecoderStream()))
          stderr += chunk
      })()
      // Real nats.js ping and dial timers against a real listener: each wait polls the real clock.
      const waitFor = async (
        condition: () => boolean | Promise<boolean>,
        deadline: number,
        what: string,
      ) => {
        while (!(await condition())) {
          if (server.exitCode !== null)
            throw new Error(`the channel server exited waiting for ${what}: ${stderr}`)
          if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}: ${stderr}`)
          await Bun.sleep(100)
        }
      }
      try {
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
        server.stdin.write(
          `${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`,
        )
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
        server.kill("SIGKILL")
        await server.exited
        await proxy.close()
        await rm(home, { recursive: true, force: true })
      }
    }, 120_000)
  },
)
