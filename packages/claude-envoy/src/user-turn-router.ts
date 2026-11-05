// Where a person's message from Dispatch's Agents page goes (INFRA-1026).
//
// Every inbound frame used to become one rendered card naming its sender. A frame that is a
// person's own direct message to this session becomes the session's own user turn instead, and a
// `/` message runs as the command it is, a pick sets what it names, and a BTW is answered off to
// the side. Everything else still arrives as the card, which is the delivery.
//
// The accept step is the security of all of it, and it follows `acceptedUserTurn` in
// `packages/pi-envoy/extensions/envoy.ts` step for step. The listener takes an envelope's source
// from whoever publishes it and every session on the bus holds that token, so a frame saying a
// person wrote it proves nothing. The session's own checks can only keep a card
// (`isUserTurnCandidate`, and an attempt it has already handled); what makes a turn is Dispatch
// recording that this session accepted that very attempt, and what it injects is the body
// Dispatch answers with, never the frame's own text.
//
// The attempt is recorded as handled before Dispatch is asked, so whatever the frame becomes - a
// turn, or the card every refusal, error and timeout keeps - no later frame naming that attempt
// is accepted: a replay, or one forged inside the accept's minute for a message that arrived as a
// card. The record is not skipped when the accept answers 404: that route answers 404 for a
// transient database error too, and forgetting the attempt would let a later forged frame spend
// it.
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery"
import type { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { pictureAddresses } from "@legion/envoy-client/dispatch-pictures"
import { messageFor } from "@legion/envoy-client/errors"
import { withDeliveredPictures } from "@legion/pi-shared/delivery-pictures"
import {
  type AcceptedUserTurn,
  handledAttemptKey,
  isUserTurnCandidate,
  turnFromAccept,
  typedInputReply,
} from "@legion/pi-shared/dispatch-user-turn"
import type { ContentBlock } from "@legion/pi-shared/pi-types"
import type { ModContentBlock, ModEffort, ModEvent } from "../hooks/mod-protocol"
import { CLAUDE_EFFORT_LEVELS, CLAUDE_PROVIDER } from "./agent-stream-claude"
import type { ModSession } from "./mod-session"
import type { UserTurnState } from "./user-turn-state"

/**
 * How long a person's direct message waits on Dispatch's acceptance before it arrives as a card
 * instead, as the Oh My Pi plugin bounds the same call: the frames reach this session one at a
 * time, so a hung Dispatch must not hold every message behind this one.
 */
const ACCEPT_TIMEOUT_MS = 10_000

/** How long the pictures a message embeds have to load before it goes in as text alone. */
const PICTURES_TIMEOUT_MS = 20_000

/**
 * How long a command runs before the person is told it is still running. Deliberately below the
 * 30 seconds the Agents page waits for a pick's answer (`SWITCH_ANSWER_DEADLINE_MS` in
 * `packages/dispatch/web/src/features/agent-view/use-session-switch.ts`), so a person learns why
 * the answer is late before the page stops waiting for it.
 */
const COMMAND_WAITING_MS = 10_000

/** The most messages whose row of the conversation has not arrived yet. A person's messages are
 *  delivered one at a time and the row follows within a moment; this is the bound that keeps a
 *  message whose row never came from being matched against a much later one. */
const AWAITED_ROWS_MAX = 16

/** What a person is told while a command they sent has not finished. */
export function commandStillRunning(command: string): string {
  return `${command} is still running: it waits for the turn this session is in, or for an answer at the session's own terminal. Its output follows in another reply.`
}

/** What a person is told once their model pick is in force. */
export function modelPicked(model: string): string {
  return `The session's model is now ${model}.`
}

/** What a person is told once their thinking pick is in force. */
export function thinkingPicked(effort: ModEffort): string {
  return `The session's thinking level is now ${effort}.`
}

/** A message whose row of the conversation has not arrived yet: the body the row will carry, and
 *  the Dispatch message that row is. */
interface AwaitedRow {
  readonly body: string
  readonly messageId: string
}

export interface UserTurnRouterOptions {
  /** This session as Dispatch knows it: the actor the accept names, and the author of its
   *  replies. Read at each use, since a `/clear` moves it. */
  readonly sessionId: () => string
  /** What this session has already done with the attempts Dispatch sent it. */
  readonly state: UserTurnState
  /** The Claude Code session itself (`./mod-session`). */
  readonly session: ModSession
  /** The session's Dispatch for one request, with `ms` to answer; undefined when Dispatch is not
   *  configured, and a throw when its configuration cannot be read. */
  readonly dispatch: (ms: number) => DispatchClient | undefined
  /** Answers a delivery on Dispatch: the reply the person reads, or the error that stopped it. */
  readonly reply: (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string },
  ) => Promise<void>
  /**
   * Where a person's own message goes in a Legion pane: the bridge's inbox, which makes it a user
   * turn (`./legion-inbox`). Undefined in a terminal session, where it goes to the mod.
   */
  readonly inbox?: (input: {
    readonly content: string
    readonly blocks?: readonly ModContentBlock[]
    readonly eventId?: string
  }) => Promise<void>
  /** Refreshes the live view's settings frame, so the header already shows a pick's new value
   *  when the reply naming it arrives. */
  readonly refreshSettings: () => void
  /** Where a failure that must not stop the session is written. */
  readonly warn: (text: string) => void
  /** How long a command runs before the person is told it is still running; `COMMAND_WAITING_MS`
   *  by default, named by the tests that drive it. */
  readonly commandWaitingMs?: number
}

export interface UserTurnRouter {
  /**
   * What one live frame becomes: `true` when the router took it - the person's own message, a
   * command, a pick or a BTW - and `false` when it keeps today's card. It throws when a message
   * Dispatch has accepted could not be put into the session, so the frame is not answered and the
   * listener sends it again.
   */
  route(rendered: RenderInboundResult): Promise<boolean>
  /** Takes one of the mod's events, to recognise the row each delivered message turned out to
   *  be, which tags that row on the live view's frames. */
  observe(event: ModEvent): void
}

/** The text a row's content holds, so a row can be recognised by the message it carries. */
function rowText(content: readonly ModContentBlock[]): string {
  const pieces: string[] = []
  for (const block of content) {
    const text = block["text"]
    if (block.type === "text" && typeof text === "string") pieces.push(text)
  }
  return pieces.join("\n")
}

/** A delivery's content as Claude Code's own blocks: the text, and one image block per picture,
 *  as the Messages API shapes them (`@legion/pi-shared/delivery-pictures` answers an Oh My Pi
 *  host's blocks, whose images carry their bytes and type under other names). */
export function claudeContentBlocks(content: string | readonly ContentBlock[]): ModContentBlock[] {
  if (typeof content === "string") return [{ type: "text", text: content }]
  return content.map((block) =>
    block.type === "text"
      ? { type: "text", text: block.text }
      : {
          type: "image",
          source: { type: "base64", media_type: block.mimeType, data: block.data },
        },
  )
}

/** The effort a `/effort` pick names: one of the levels the view offers, or a number for a model
 *  that takes one; undefined for anything else, which runs as the command it is. */
function pickedEffort(value: string): ModEffort | undefined {
  const named = CLAUDE_EFFORT_LEVELS.find((level) => level === value)
  if (named !== undefined) return named
  const number = Number(value)
  return value !== "" && Number.isFinite(number) ? number : undefined
}

export function createUserTurnRouter(options: UserTurnRouterOptions): UserTurnRouter {
  const { reply, session, state, warn } = options
  /** Whether this session is a Legion pane, which is exactly the session whose person messages
   *  go to the bridge's inbox rather than to the mod. */
  const inPane = options.inbox !== undefined
  const waitingMs = options.commandWaitingMs ?? COMMAND_WAITING_MS
  /** The pictures this session has been shown, so one is sent once (`withDeliveredPictures`). */
  const shownPictures = new Set<string>()
  /** The messages whose row of the conversation has not arrived yet, oldest first. */
  const awaited: AwaitedRow[] = []

  /** Expects the row a message is about to become, so the row is recorded as that message as soon
   *  as the session reports it - which it may do before the call that delivered it answers. */
  const expectRow = (body: string, messageId: string): void => {
    awaited.push({ body, messageId })
    while (awaited.length > AWAITED_ROWS_MAX) awaited.shift()
  }

  const record = (row: string, messageId: string): void => {
    state.delivered(row, messageId).catch((error: unknown) => {
      warn(
        `could not record which Dispatch message a row of this session is — ${messageFor(error)}`,
      )
    })
  }

  /**
   * The turn Dispatch says this frame is, or undefined for every frame that keeps its card. The
   * order of the steps is the rule itself; the comment at the top of this file says why.
   */
  const accepted = async (
    rendered: RenderInboundResult,
  ): Promise<
    { readonly delivery: DispatchDelivery; readonly turn: AcceptedUserTurn } | undefined
  > => {
    if (!isUserTurnCandidate(rendered)) return undefined
    const { delivery } = rendered
    const key = handledAttemptKey(delivery.id, delivery.attempt)
    if (state.handled(key)) {
      warn(
        `a frame names the Dispatch attempt ${delivery.attempt} of message ${delivery.id}, which this session has already delivered; it arrives as a card`,
      )
      return undefined
    }
    await state.handle(key)
    try {
      const dispatch = options.dispatch(ACCEPT_TIMEOUT_MS)
      if (dispatch === undefined) return undefined
      const answer = await dispatch.acceptMessageDelivery(delivery.id, delivery.attempt, {
        actor: { id: options.sessionId(), kind: "session" },
      })
      const turn = turnFromAccept(answer)
      if (turn === undefined) {
        warn(
          `Dispatch accepted message ${delivery.id} without the stored body to inject; it arrives as a card`,
        )
        return undefined
      }
      return { delivery, turn }
    } catch (error) {
      warn(
        `Dispatch did not accept message ${delivery.id} as this session's turn (${messageFor(error)}); it arrives as a card`,
      )
      return undefined
    }
  }

  /** The person's message, as the session's own: a row of the turn running, or the prompt of a
   *  new one. Throws when the session could not take it, so the frame is sent again. */
  const inject = async (delivery: DispatchDelivery, turn: AcceptedUserTurn): Promise<void> => {
    const pictures = await withDeliveredPictures(
      turn.body,
      pictureAddresses(turn.body),
      () => options.dispatch(PICTURES_TIMEOUT_MS),
      false,
      shownPictures,
    )
    const blocks = claudeContentBlocks(pictures.content)
    // Expected before the message is handed over: the session reports the row as an event of its
    // own, which can reach this process before the call that delivered it answers.
    expectRow(turn.body, turn.messageId)
    if (!inPane) {
      const row = await session.deliver({
        content: blocks,
        text: turn.body,
        // A prompt waits behind the turn before it and is never awaited here, so a prompt that
        // never entered is the one failure this path answers on Dispatch rather than throwing.
        failed: (error) => {
          void answer(delivery, typedInputReply(turn.body, { error: messageFor(error) }))
        },
      })
      if (row !== undefined) record(row, turn.messageId)
    } else {
      await options.inbox({
        content: turn.body,
        eventId: turn.messageId,
        ...(blocks.length > 1 ? { blocks } : {}),
      })
    }
    for (const address of pictures.shown) shownPictures.add(address)
  }

  /** Posts one reply to a delivery, naming on stderr a reply that did not reach Dispatch. */
  const answer = async (delivery: DispatchDelivery, body: string | undefined): Promise<void> => {
    if (body === undefined) return
    await reply(delivery, { body }).catch((error: unknown) => {
      warn(`a reply to message ${delivery.id} did not reach Dispatch — ${messageFor(error)}`)
    })
  }

  /** A person's pick of a model, which is their own `/model` message. */
  const pickModel = async (value: string): Promise<string> => {
    const model = value.startsWith(`${CLAUDE_PROVIDER}/`)
      ? value.slice(CLAUDE_PROVIDER.length + 1)
      : value
    // A pane's Claude Code runs in print mode, where the model is the command's to change; a
    // terminal session's is a `/config` row.
    if (inPane) {
      await session.runCommand("model", model)
      return modelPicked(`${CLAUDE_PROVIDER}/${model}`)
    }
    const stored = await session.setModel(model)
    return modelPicked(`${CLAUDE_PROVIDER}/${String(stored)}`)
  }

  /**
   * A `/` message: a pick of the model or the thinking level, or the command itself. Its output
   * is the reply; a command still running after `waitingMs` is answered twice, the waiting reply
   * first.
   */
  const runTyped = async (delivery: DispatchDelivery, body: string): Promise<void> => {
    const typed = /^\/(\S+)/.exec(body)
    const name = typed?.[1] ?? ""
    const value = body.slice(typed?.[0].length ?? 0).trim()
    const effort = name === "effort" ? pickedEffort(value) : undefined
    if (!session.connected()) {
      await answer(
        delivery,
        typedInputReply(body, { error: "this session has no link to Claude Code" }),
      )
      return
    }
    let running = true
    const waiting = setTimeout(() => {
      if (!running) return
      void answer(
        delivery,
        typedInputReply(body, {
          handled: "command",
          output: commandStillRunning(`/${name}`),
        }),
      )
    }, waitingMs)
    waiting.unref()
    let said: string | undefined
    try {
      if (name === "model" && value !== "") said = await pickModel(value)
      else if (effort !== undefined) {
        said = thinkingPicked((await session.setEffort(effort)) ?? effort)
      } else {
        said = typedInputReply(body, {
          handled: "command",
          output: await session.runCommand(name, value),
        })
      }
    } catch (error) {
      said = typedInputReply(body, { error: messageFor(error) })
    } finally {
      running = false
      clearTimeout(waiting)
    }
    // A pick has run by now: the header shows its new value before the reply that names it
    // reaches the page.
    options.refreshSettings()
    await answer(delivery, said)
  }

  /** A BTW: one question of the model with the conversation as its context, answered on Dispatch.
   *  It adds nothing to the conversation, and the frame is answered without waiting for it. */
  const runBtw = (delivery: DispatchDelivery): void => {
    void (async () => {
      if (!session.connected()) {
        await reply(delivery, { error: "this session has no link to Claude Code" })
        return
      }
      try {
        await reply(delivery, { body: await session.fork(delivery.body) })
      } catch (error) {
        await reply(delivery, { error: messageFor(error) })
      }
    })().catch((error: unknown) => {
      warn(`a BTW answer did not reach Dispatch — ${messageFor(error)}`)
    })
  }

  return {
    async route(rendered) {
      const btw = rendered.delivery
      if (btw?.mode === "btw") {
        runBtw(btw)
        return true
      }
      const accept = await accepted(rendered)
      if (accept === undefined) return false
      const { delivery, turn } = accept
      if (turn.body.startsWith("/")) {
        // The command runs for as long as it runs: the frame is answered now, and its output
        // reaches the person as a reply when it comes.
        void runTyped(delivery, turn.body).catch((error: unknown) => {
          warn(`a command from Dispatch failed — ${messageFor(error)}`)
        })
        return true
      }
      await inject(delivery, turn)
      return true
    },
    observe(event) {
      // The Claude Code session was handed this event before the router was (`startAgentStream`,
      // `./envoy-channel-server`): its turn tracking must not depend on a router existing.
      if (event.kind !== "append" || awaited.length === 0) return
      const { message } = event
      if (message.type !== "user" && message.role !== "user") return
      const text = rowText(message.content).trim()
      const index = awaited.findIndex((row) => row.body.trim() === text)
      const row = awaited[index]
      if (row === undefined) return
      awaited.splice(index, 1)
      record(event.uuid, row.messageId)
    },
  }
}
