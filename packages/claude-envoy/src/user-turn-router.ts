// Where a person's message from Dispatch's Agents page goes (INFRA-1026).
//
// Every inbound frame used to become one rendered card naming its sender. A frame that is a
// person's own direct message to this session becomes the session's own user turn instead, and a
// `/` message runs as the command it is, a pick sets what it names, and a BTW is answered off to
// the side. Everything else still arrives as the card, which is the delivery.
//
// The accept step is the security of all of it, and it is one step both Envoy plugins take:
// `acceptUserTurn` in `@legion/pi-shared/accept-user-turn`, which the Oh My Pi plugin
// (`acceptedUserTurn`, `packages/pi-envoy/extensions/envoy.ts`) calls too, so a fix to the rule
// lands in both. The order of its steps - and why the attempt is recorded before Dispatch is
// asked, a 404 included - is stated there. What this host puts into it: its own record of
// handled attempts (`./user-turn-state`, a file that outlives the process), the session's
// Dispatch bounded by `ACCEPT_TIMEOUT_MS`, the session id a `/clear` moves, and these warnings.
// The listener takes an envelope's source from whoever publishes it and every session on the
// bus holds that token, so a frame saying a person wrote it proves nothing: what makes a turn
// is Dispatch recording that this session accepted that very attempt, and what it injects is
// the body Dispatch answers with, never the frame's own text.
import { comparableLines } from "@legion/contracts"
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery"
import type { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { pictureAddresses } from "@legion/envoy-client/dispatch-pictures"
import { messageFor } from "@legion/envoy-client/errors"
import { type AcceptedUserTurnDelivery, acceptUserTurn } from "@legion/pi-shared/accept-user-turn"
import { withDeliveredPictures } from "@legion/pi-shared/delivery-pictures"
import {
  type AcceptedUserTurn,
  handledAttemptKey,
  typedInputReply,
} from "@legion/pi-shared/dispatch-user-turn"
import type { ContentBlock } from "@legion/pi-shared/pi-types"
import type { ModContentBlock, ModEffort, ModEvent } from "../hooks/mod-protocol"
import { CLAUDE_EFFORT_LEVELS, CLAUDE_PROVIDER } from "./agent-stream-claude"
import { rememberBounded } from "./bounded-set"
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

/** The most answered deliveries this session remembers, so a second answer to one asks Dispatch
 *  to post it. A delivery is answered twice at most, within one command's run, and messages are
 *  delivered one at a time; this is the bound that keeps the record from growing for the
 *  session's life. */
const ANSWERED_DELIVERIES_MAX = 64

/** What a person is told while a command they sent has not finished. */
export function commandStillRunning(command: string): string {
  return `${command} is still running: it waits for the turn this session is in, or for an answer at the session's own terminal. Its output follows in another reply.`
}

/**
 * What a person is told when the command they sent printed nothing at all, so a Send is never
 * met with silence.
 *
 * `typedInputReply` answers undefined for an empty output, and that helper is the Oh My Pi
 * plugin's too (`packages/pi-shared/src/dispatch-user-turn.ts`), so the line is written here
 * rather than there.
 */
export function commandPrintedNothing(command: string): string {
  return `${command} ran in this session and printed nothing.`
}

/** What a person is told once their model pick is in force. */
export function modelPicked(model: string): string {
  return `The session's model is now ${model}.`
}

/** What a person is told once their thinking pick is in force. */
export function thinkingPicked(effort: ModEffort): string {
  return `The session's thinking level is now ${effort}.`
}

/** A message whose row of the conversation has not arrived yet: the person's own words, which
 *  is what the row will say, and the Dispatch message that row is. A `/` message has no such
 *  row - the only row it writes is the one Claude Code echoes the command it ran in, which the
 *  live view draws nothing of (`./agent-stream-claude`, `runsCommandRow`). */
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
  /** Answers a delivery on Dispatch: the reply the person reads, or the error that stopped it.
   *  `followUp` is the ask that Dispatch post this answer although the attempt is already
   *  answered, which every answer to a delivery after the first carries (`post`). */
  readonly reply: (
    delivery: DispatchDelivery,
    result: {
      readonly body?: string
      readonly error?: string
      readonly followUp?: boolean
    },
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
  /** Tells the live view the effort a thinking pick put in force. Claude Code reports an effort
   *  only on a request's own `turn.step`, so a refresh alone leaves the header on the level the
   *  session ran at before the pick until its next turn. */
  readonly effortPicked: (effort: ModEffort) => void
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

/** The text a row's content holds, so a row can be recognised by the message it carries: its text
 *  blocks alone, since a picture crosses as an image block that carries no text of its own. */
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
  /** The deliveries this session has already answered, oldest first: every answer after the
   *  first has to ask Dispatch to post it (`post`). */
  const answered = new Set<string>()

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
   * The turn Dispatch says this frame is, or undefined for every frame that keeps its card: the
   * shared accept step (`@legion/pi-shared/accept-user-turn`), which both Envoy plugins take,
   * with this session's record of handled attempts, its Dispatch and its stderr. The order of
   * the steps is the rule itself, and it is stated where the step is written; the comment at
   * the top of this file says what this host puts into it.
   */
  const accepted = (rendered: RenderInboundResult): Promise<AcceptedUserTurnDelivery | undefined> =>
    acceptUserTurn({
      actorId: options.sessionId(),
      dispatch: () => options.dispatch(ACCEPT_TIMEOUT_MS),
      handle: state.handle,
      handled: state.handled,
      rendered,
      warn: (refusal) => {
        const { delivery } = refusal
        if (refusal.reason === "handled") {
          warn(
            `a frame names the Dispatch attempt ${delivery.attempt} of message ${delivery.id}, which this session has already delivered; it arrives as a card`,
          )
          return
        }
        if (refusal.reason === "no-body") {
          warn(
            `Dispatch accepted message ${delivery.id} without the stored body to inject; it arrives as a card`,
          )
          return
        }
        warn(
          `Dispatch did not accept message ${delivery.id} as this session's turn (${messageFor(refusal.error)}); it arrives as a card`,
        )
      },
    })

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
        // The prompt at a row the turn never read carries both: the person's own words, and
        // which Dispatch message they are (`unreadRowPrompt`, `./mod-session`).
        messageId: turn.messageId,
        text: turn.body,
        // A prompt's submit resolves only at the end of the turn it starts, so it is never
        // awaited here, and a prompt that never entered is the one failure this path answers on
        // Dispatch rather than throwing.
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

  /**
   * Posts one answer to a delivery, and asks Dispatch to post it when this session has answered
   * that delivery before. Dispatch settles a delivery attempt with the first reply it takes and
   * answers a later POST to the same attempt by handing the stored reply back, posting nothing
   * unless the request asks for a follow-up (`replyAfterAnswer`,
   * `packages/envoy/internal/dispatch/api/message_delivery.go`) - so without the ask the output
   * a waiting reply promised never reaches the person. The ask is decided before the post is
   * awaited, so a waiting reply and the output racing it cannot both be the first.
   */
  const post = async (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string },
  ): Promise<void> => {
    const key = handledAttemptKey(delivery.id, delivery.attempt)
    const followUp = answered.has(key)
    if (!followUp) {
      // A reply Dispatch refused leaves the attempt unsettled, and a follow-up on an unsettled
      // attempt is inserted as the first reply anyway, so the record is kept either way.
      rememberBounded(answered, key, ANSWERED_DELIVERIES_MAX)
    }
    await reply(delivery, { ...result, followUp })
  }

  /** Posts one reply to a delivery, naming on stderr a reply that did not reach Dispatch. */
  const answer = async (delivery: DispatchDelivery, body: string | undefined): Promise<void> => {
    if (body === undefined) return
    await post(delivery, { body }).catch((error: unknown) => {
      warn(`a reply to message ${delivery.id} did not reach Dispatch — ${messageFor(error)}`)
    })
  }

  /**
   * A person's pick of a model, which is their own `/model` message. Nothing of the run is
   * expected back as a row: Claude Code echoes the command it ran - `/model haiku` for the
   * `/model anthropic/haiku` the person sent, measured live on 2.1.310 (INFRA-1026) - and the
   * live view draws no row of a run of this session's at all, so their own line reaches the
   * page as the message Dispatch stored and nothing beside it.
   */
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
        const inForce = (await session.setEffort(effort)) ?? effort
        // The mod puts this effort on every later request from here on, so the header can say
        // so now rather than at the session's next turn.
        options.effortPicked(inForce)
        said = thinkingPicked(inForce)
      } else {
        // The person sees this command once, as the message Dispatch stored: the row Claude
        // Code echoes the run in is drawn nowhere (`pickModel`), so no row is expected for it.
        const output = await session.runCommand(name, value)
        // A command that printed nothing is answered all the same: `typedInputReply` has
        // nothing to say for an empty output, and silence is what the person was left with.
        const printed = typedInputReply(body, { handled: "command", output })
        said = printed ?? commandPrintedNothing(`/${name}`)
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
   *  It adds nothing to the conversation, and the frame is answered without waiting for it. Its
   *  answer never asks for a follow-up: a BTW is answered one time, and the rule that a frame
   *  this session is handed twice posts no second answer is what Dispatch leans on for it
   *  (`replyAfterAnswer`, `packages/envoy/internal/dispatch/api/message_delivery.go`), since
   *  nothing here guards a redelivered BTW and the fork would answer it afresh. */
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
      // Every row is read as the text it carries. A command a person sent writes no row that
      // is theirs - Claude Code echoes the command it ran, in a row the live view draws
      // nothing of (`./agent-stream-claude`, `runsCommandRow`) - so nothing here waits for one
      // and nothing tags one.
      //
      // A row and a body are compared by the shared line rule (`comparableLines`,
      // `@legion/contracts`), each side joined back into one string to match on. A message
      // carrying a picture reaches the session as its text and one image block per picture
      // (`claudeContentBlocks`), and the row it becomes holds the text alone - either as the
      // person wrote it, markdown and all (measured on 2.1.310:
      // `/legion/scratch/tmp/i3-k5-pane/config/projects/-legion-scratch-tmp-i3-k5-pane-cwd/f376a3a9-6e75-461b-806b-22dea4e83ddf.jsonl:121`),
      // or with the markdown gone, as the live view measured it, since the picture is a block
      // of its own and no longer a word of the text. Taking the markdown off both sides
      // recognises the row either way, so the message is tagged with its Dispatch id and the
      // page shows it once. Both sides here are this session's own, which is why both are read
      // by the rule alone: the page reads a stored body by this same rule and so must not
      // drift from it, but it reads the streamed row - a frame any client of the bus can
      // publish - by a check of its own first (`turnSaysBody` and `rowLines`,
      // `packages/dispatch/web/src/features/agent-view/conversation.ts`).
      const said = comparableLines(rowText(message.content)).join("\n")
      const index = awaited.findIndex((row) => comparableLines(row.body).join("\n") === said)
      const row = awaited[index]
      if (row === undefined) return
      awaited.splice(index, 1)
      record(event.uuid, row.messageId)
    },
  }
}
