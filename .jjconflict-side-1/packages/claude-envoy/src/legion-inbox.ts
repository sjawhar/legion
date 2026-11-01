import { randomUUID } from "node:crypto"
import { createConnection, type Socket } from "node:net"
import type { DeliveryCapability } from "@legion/contracts"
import type { ChannelNotification, ChannelNotifier } from "./envoy-channel-server"

/**
 * The variable `legion claude-bridge` exports to Claude Code, and so to this server, naming its
 * inbox socket (`packages/daemon/internal/claudebridge/inbox.go`): Claude Code's print mode never
 * delivers a `notifications/claude/channel`, so in a Legion pane each Envoy delivery goes there and
 * the bridge sends it to Claude Code as a user turn.
 */
export const INBOX_VARIABLE = "LEGION_CLAUDE_INBOX"

/**
 * The delivery modes a session whose deliveries go to the bridge's inbox advertises. The bridge
 * (`deliver` in `packages/daemon/internal/claudebridge/bridge.go`) starts a turn with a delivery
 * when Claude Code is idle and otherwise sends it as the next user message of the turn running,
 * once Claude Code ends its current reply: a Send. An Aside gets the same handling, which keeps
 * its promise: the message joins the main conversation without interrupting the work in hand.
 * The inbox line carries no mode, since the bridge treats both alike. `btw` stays off: the bridge
 * runs no side turn.
 */
export const INBOX_CAPABILITIES = [
  "aside",
  "steer",
] as const satisfies readonly DeliveryCapability[]

/** `InboxAnswer`: the bridge's line back for one delivery, once it has queued it. */
interface InboxAnswer {
  readonly id: string
  readonly ok?: boolean
  readonly error?: string
}

/**
 * A notifier that writes each delivery to the bridge's inbox at `socketPath`, one JSON line
 * `{id, content}` (the envelope's `event_id`, else a fresh id; the rendered content), and resolves
 * once the bridge answers `{id, ok: true}`: the delivery is queued as a user turn, which is what a
 * forwarded lane's receipt then says. An answer with an error, a closed socket or an answer for
 * another id rejects. The bridge answers one connection's lines in order, so one connection, opened
 * on first use and again after it closes, carries every delivery.
 */
export function createInboxNotifier(socketPath: string): ChannelNotifier {
  let socket: Socket | undefined
  let buffered = ""
  const waiting: {
    readonly id: string
    readonly settle: PromiseWithResolvers<void>
  }[] = []

  const fail = (error: Error): void => {
    for (const pending of waiting.splice(0)) pending.settle.reject(error)
  }

  const answer = (line: string): void => {
    const pending = waiting.shift()
    if (pending === undefined) return
    let parsed: InboxAnswer
    try {
      parsed = JSON.parse(line) as InboxAnswer
    } catch {
      pending.settle.reject(new Error(`the Legion inbox answered ${JSON.stringify(line)}`))
      return
    }
    if (parsed.ok === true && parsed.id === pending.id) pending.settle.resolve()
    else {
      pending.settle.reject(
        new Error(
          `the Legion inbox refused delivery ${pending.id}: ${parsed.error ?? `answered ${line}`}`,
        ),
      )
    }
  }

  const connected = (): Socket => {
    if (socket !== undefined && !socket.destroyed) return socket
    buffered = ""
    const opened = createConnection(socketPath)
    opened.setEncoding("utf8")
    opened.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = buffered.slice(0, end)
        buffered = buffered.slice(end + 1)
        answer(line)
      }
    })
    opened.on("error", (error) =>
      fail(new Error(`the Legion inbox ${socketPath}: ${error.message}`)),
    )
    opened.on("close", () => {
      fail(new Error(`the Legion inbox ${socketPath} closed before it answered`))
      if (socket === opened) socket = undefined
    })
    socket = opened
    return opened
  }

  return {
    notification(notification: ChannelNotification): Promise<void> {
      const id = notification.params.meta["event_id"] ?? randomUUID()
      const settle = Promise.withResolvers<void>()
      waiting.push({ id, settle })
      connected().write(`${JSON.stringify({ id, content: notification.params.content })}\n`)
      return settle.promise
    },
  }
}
