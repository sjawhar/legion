import { createConnection } from "node:net"
import { agentSubject } from "@legion/contracts"
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { renderInbound } from "@legion/envoy-client/delivery"
import { messageFor } from "@legion/envoy-client/errors"
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport"
import { connect, StringCodec } from "nats"

import { monitorSessionId } from "./monitor-identity"
import { subscriptionTopics } from "./subscription-topics"

const SEEN_KEYS_LIMIT = 1_000

type NativeMessageInput = {
  readonly token: string
  readonly message: string
}

type NativeMessagingEnvironment = {
  readonly CLAUDE_CODE_MESSAGING_SOCKET?: string | undefined
  readonly CLAUDE_CODE_MESSAGING_TOKEN?: string | undefined
}

export type NativeMessagingCredentials = {
  readonly socketPath: string
  readonly token: string
}

class NativeMessagingUnavailableError extends Error {
  constructor() {
    super(
      "Claude Code native messaging is unavailable: CLAUDE_CODE_MESSAGING_SOCKET and CLAUDE_CODE_MESSAGING_TOKEN are required",
    )
    this.name = "NativeMessagingUnavailableError"
  }
}

export function nativeMessageFrames(input: NativeMessageInput): readonly [string, string] {
  return [
    JSON.stringify({ type: "auth", token: input.token }),
    JSON.stringify({ type: "user", message: { role: "user", content: input.message } }),
  ]
}

export function envoyInboundMessage(
  input: string,
  sessionId?: string,
  subject?: string,
): string | undefined {
  const rendered = renderInbound(input, sessionId ?? "", subject)
  return rendered.skip ? undefined : rendered.content
}

export function nativeMessagingCredentials(
  environment: NativeMessagingEnvironment,
): NativeMessagingCredentials {
  const socketPath = environment.CLAUDE_CODE_MESSAGING_SOCKET
  const token = environment.CLAUDE_CODE_MESSAGING_TOKEN
  if (!socketPath || !token) throw new NativeMessagingUnavailableError()
  return { socketPath, token }
}

export async function sendNativeMessage(
  credentials: NativeMessagingCredentials,
  message: string,
): Promise<void> {
  const frames = nativeMessageFrames({ token: credentials.token, message })
  await new Promise<void>((resolve, reject) => {
    const socket = createConnection({ path: credentials.socketPath })
    socket.once("connect", () => socket.end(`${frames.join("\n")}\n`))
    socket.once("error", reject)
    socket.once("close", () => resolve())
  })
}

function terminationSignal(): Promise<void> {
  return new Promise((resolve) => {
    const stop = (): void => {
      process.off("SIGINT", stop)
      process.off("SIGTERM", stop)
      resolve()
    }
    process.once("SIGINT", stop)
    process.once("SIGTERM", stop)
  })
}

function registerMonitorSession(client: EnvoyClient, sessionId: string): Promise<void> {
  return client
    .subscribe({
      sessionID: sessionId,
      directory: process.cwd(),
      topics: [agentSubject(sessionId)],
      port: 0,
      title: "",
      driving: false,
      selfSubscribed: true,
    })
    .then(() => undefined)
}

export async function runEnvoyMonitor(): Promise<void> {
  const sessionId = monitorSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const credentials = nativeMessagingCredentials({
    CLAUDE_CODE_MESSAGING_SOCKET: process.env["CLAUDE_CODE_MESSAGING_SOCKET"],
    CLAUDE_CODE_MESSAGING_TOKEN: process.env["CLAUDE_CODE_MESSAGING_TOKEN"],
  })
  const defaults = envoyDefaultsFromEnvironment(process.env)
  if (defaults.natsUrls.length === 0) {
    process.stderr.write(
      "envoy monitor: ENVOY_NATS_URL is not set; inbound envoy messages are disabled\n",
    )
    return
  }
  const connection = await connect({
    servers: [...defaults.natsUrls],
    name: `claude-envoy-${sessionId}`,
  })
  const client = createEnvoyClient({ baseUrl: defaults.envoyUrl, fetch: globalThis.fetch })
  const codec = StringCodec()
  const seen = new Set<string>()
  const topics = subscriptionTopics({
    sessionId,
    additionalTopics: process.env["ENVOY_TOPICS"],
  })
  // Subscribe before registering: once the session is advertised, a sender
  // passes the listener's liveness check and its direct message is delivered
  // to this connection, so the direct subject must already be listening.
  const subscriptions = topics.map((topic) => connection.subscribe(topic))
  await connection.flush()
  // One line per registry outage, not one per failed heartbeat; a successful
  // registration clears it.
  let outageReported = false
  const register = (): Promise<void> =>
    registerMonitorSession(client, sessionId)
      .then(() => {
        outageReported = false
      })
      .catch((error) => {
        if (outageReported) return
        outageReported = true
        process.stderr.write(
          `envoy monitor: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat\n`,
        )
      })
  await register()
  const heartbeat = setInterval(() => {
    void register()
  }, defaults.heartbeatMs)
  const forwarding = subscriptions.map(async (subscription) => {
    for await (const message of subscription) {
      const rendered = renderInbound(codec.decode(message.data), sessionId, message.subject)
      const key = rendered.envelope?.dedupe_key ?? rendered.envelope?.event_id
      if (key !== undefined) {
        if (seen.has(key)) continue
        seen.add(key)
        if (seen.size > SEEN_KEYS_LIMIT) {
          const oldest = seen.values().next()
          if (!oldest.done) seen.delete(oldest.value)
        }
      }
      if (!rendered.skip) await sendNativeMessage(credentials, rendered.content)
    }
  })
  try {
    await Promise.race([terminationSignal(), Promise.all(forwarding)])
  } finally {
    clearInterval(heartbeat)
    for (const subscription of subscriptions) subscription.unsubscribe()
    const deadline = Promise.withResolvers<void>()
    const timer = setTimeout(deadline.resolve, 1_000)
    try {
      const deregistration = client.unregisterSession(sessionId).catch(() => undefined)
      const draining = connection.drain().catch(() => undefined)
      await Promise.race([
        Promise.allSettled([...forwarding, deregistration, draining]).then(() => undefined),
        deadline.promise,
      ])
    } finally {
      clearTimeout(timer)
    }
  }
}
