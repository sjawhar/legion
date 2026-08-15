import { createConnection } from "node:net"
import { connect, StringCodec } from "nats"
import { z } from "zod"

import { monitorSessionId } from "./monitor-identity"
import { subscriptionTopics } from "./subscription-topics"

const NATS_URL = "nats://envoy-nats:4222"
const EnvoyEnvelopeSchema = z.object({ payload_summary: z.string().min(1) })

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

export function envoyInboundMessage(input: string): string {
  return EnvoyEnvelopeSchema.parse(JSON.parse(input)).payload_summary
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
  const connection = await connect({
    servers: process.env["ENVOY_NATS_URL"] ?? NATS_URL,
    name: `claude-envoy-${sessionId}`,
  })
  const codec = StringCodec()
  const subscriptions = topics.map((topic) => connection.subscribe(topic))
  const forwarding = subscriptions.map(async (subscription) => {
    for await (const message of subscription) {
      await sendNativeMessage(credentials, envoyInboundMessage(codec.decode(message.data)))
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
