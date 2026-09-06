import { createConnection } from "node:net"
import { connect, StringCodec } from "nats"
import { z } from "zod"

import { monitorSessionId } from "./monitor-identity"
import { subscriptionTopics } from "./subscription-topics"

const EnvoyEnvelopeSchema = z.object({
  dedupe_key: z.string().min(1).optional(),
  event_id: z.string().min(1).optional(),
  payload_summary: z.string().min(1),
  payload: z.string().optional(),
})

type EnvoyEnvelope = z.infer<typeof EnvoyEnvelopeSchema>

const SEEN_KEYS_LIMIT = 1_000

function parseEnvoyEnvelope(input: string): EnvoyEnvelope {
  return EnvoyEnvelopeSchema.parse(JSON.parse(input))
}

function dispatchSession(payload: string | undefined): string | undefined {
  if (payload === undefined) return undefined
  try {
    const message: unknown = JSON.parse(payload)
    if (
      typeof message === "object" &&
      message !== null &&
      "dispatch_session" in message &&
      typeof message.dispatch_session === "string"
    ) {
      return message.dispatch_session
    }
  } catch {
    // Non-JSON payloads cannot carry a dispatch echo marker.
  }
  return undefined
}

function inboundMessage(
  envelope: EnvoyEnvelope,
  sessionId: string | undefined,
): string | undefined {
  if (sessionId !== undefined && dispatchSession(envelope.payload) === sessionId) return undefined
  return envelope.payload_summary
}

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

export function envoyInboundMessage(input: string, sessionId?: string): string | undefined {
  return inboundMessage(parseEnvoyEnvelope(input), sessionId)
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

export async function runEnvoyMonitor(): Promise<void> {
  const sessionId = monitorSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const credentials = nativeMessagingCredentials({
    CLAUDE_CODE_MESSAGING_SOCKET: process.env["CLAUDE_CODE_MESSAGING_SOCKET"],
    CLAUDE_CODE_MESSAGING_TOKEN: process.env["CLAUDE_CODE_MESSAGING_TOKEN"],
  })
  const topics = subscriptionTopics({
    sessionId,
    additionalTopics: process.env["ENVOY_TOPICS"],
  })
  const natsUrl = process.env["ENVOY_NATS_URL"]
  if (natsUrl === undefined || natsUrl.trim().length === 0) {
    // The broker's location is deployment configuration; there is no default.
    process.stderr.write(
      "envoy monitor: ENVOY_NATS_URL is not set; inbound envoy messages are disabled\n",
    )
    return
  }
  const connection = await connect({
    servers: natsUrl,
    name: `claude-envoy-${sessionId}`,
  })
  const codec = StringCodec()
  const seen = new Set<string>()
  const subscriptions = topics.map((topic) => connection.subscribe(topic))
  const forwarding = subscriptions.map(async (subscription) => {
    for await (const message of subscription) {
      const envelope = parseEnvoyEnvelope(codec.decode(message.data))
      const key = envelope.dedupe_key ?? envelope.event_id
      if (key !== undefined) {
        if (seen.has(key)) continue
        seen.add(key)
        if (seen.size > SEEN_KEYS_LIMIT) {
          const oldest = seen.values().next()
          if (!oldest.done) seen.delete(oldest.value)
        }
      }
      const inbound = inboundMessage(envelope, sessionId)
      if (inbound !== undefined) await sendNativeMessage(credentials, inbound)
    }
  })
  try {
    await Promise.race([terminationSignal(), Promise.all(forwarding)])
  } finally {
    for (const subscription of subscriptions) subscription.unsubscribe()
    await Promise.all(forwarding)
    await connection.drain()
  }
}
