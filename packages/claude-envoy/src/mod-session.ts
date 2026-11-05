// The Claude Code session as the router drives it (INFRA-1026): what the mod says is happening in
// it, and the calls that put something into it.
//
// Two measured facts shape everything here, both on 2.1.310 in this pod:
//
//   - `$.prompt.submit` resolves only once the turn it starts has ended. So a submit is never
//     awaited by a caller that has a frame to answer, and at most one is ever in flight: a second
//     one would sit behind the first for a whole turn.
//   - a row `$.session.append` adds is read at the turn's next request, and not at all when no
//     request follows it. So a row added to a running turn is watched until that turn ends, and
//     one the turn never read is pointed at by a prompt of its own.
//
// The turn tracking is built from the mod's events alone (`../hooks/mod-protocol`), which have one
// consumer: the live view reads them and hands each one here (`observe`), so nothing in this file
// opens a socket or reads the link's stream.
import { messageFor } from "@legion/envoy-client/errors"
import type { ModConfigValue, ModContentBlock, ModEffort, ModEvent } from "../hooks/mod-protocol"
import type { ModLink } from "./mod-link"

/**
 * How long a `$.prompt.submit` may take: it resolves at the end of the turn it starts, and a turn
 * has no bound of its own. A day is past any turn a person waits through, and inside the 32-bit
 * range a timer takes.
 */
const SUBMIT_TIMEOUT_MS = 24 * 60 * 60 * 1_000

/**
 * How long a `$.command.run` may take. A command waits for the turn running and for a dialog at
 * the terminal, so it is bounded as a submit is; what a person sees meanwhile is the router's
 * waiting reply, posted ten seconds in.
 */
const COMMAND_TIMEOUT_MS = SUBMIT_TIMEOUT_MS

/** How long a `$.model.fork` may take: one model call with the conversation as its context, which
 *  the link's own 30 seconds would cut short. */
const FORK_TIMEOUT_MS = 10 * 60 * 1_000

/**
 * The prompt that points the model at a row the turn it was added to never read. It is submitted
 * without `asUser`, so the model reads it as the plugin speaking rather than the person, and the
 * row above it carries the person's own words.
 */
export const UNREAD_ROW_PROMPT =
  "A message from Dispatch reached this conversation after your last request, so you have not read it yet. Read the message above and answer it."

/** The session's model, as a `/config` row names it. */
const MODEL_CONFIG_KEY = "model"

/** What the router does to this session. */
export interface ModSession {
  /** Whether the mod is polling the link now; nothing below reaches the session without one. */
  connected(): boolean
  /** Take one of the mod's events. The live view hands over every event it reads, in order. */
  observe(event: ModEvent): void
  /** Whether a turn is running now. */
  inTurn(): boolean
  /** Add one row the model reads at its next request, and resolve with the row's id. Nothing is
   *  started by it: a card joins the conversation without a turn of its own. */
  show(content: readonly ModContentBlock[]): Promise<string>
  /**
   * Hand the session a person's own message. During a turn it is a row of the conversation, which
   * the model reads at the turn's next request and which is pointed at by a prompt of its own
   * when the turn ends without reading it; while the session is idle it is the prompt of a turn,
   * submitted as the person's own words.
   *
   * Resolves once the session has the message: with the row's id where a row carries it, and with
   * undefined where it entered as a prompt, whose row the caller recognises by its text. It never
   * waits for the turn a prompt starts, so a prompt that never entered - the session ended while
   * it waited behind another - reaches the caller as `failed` rather than as a rejection.
   */
  deliver(input: {
    readonly text: string
    readonly content: readonly ModContentBlock[]
    readonly failed?: (error: unknown) => void
  }): Promise<string | undefined>
  /** Run one slash command and answer what it printed. */
  runCommand(command: string, args: string): Promise<string>
  /** Ask the model one question off to the side, with the conversation as its context, and answer
   *  what it said. It adds nothing to the conversation. */
  fork(prompt: string): Promise<string>
  /** Set the session's model, and answer the value Claude Code stored. */
  setModel(model: string): Promise<ModConfigValue>
  /** Set the effort the mod puts on every later request, and answer what is now in force. */
  setEffort(effort: ModEffort | null): Promise<ModEffort | null>
}

export interface ModSessionOptions {
  readonly link: Pick<ModLink, "request" | "connected">
  /** Where a failure that must not stop the session is written. */
  readonly warn: (text: string) => void
}

/** One row handed to a running turn, watched until that turn has read it or ended. */
interface WatchedRow {
  readonly turnId: string
  /** The steps begun when the row was added: a step begun after it is the request that read it. */
  readonly steps: number
}

/**
 * The session behind `options.link`. Every call rejects when the mod does not answer; the caller
 * decides what a frame does then, and nothing here writes to the session on its own.
 */
export function modSession(options: ModSessionOptions): ModSession {
  const { link, warn } = options
  /** The turn running now, as `turn-start` and `turn-complete` report it. */
  let turn: string | undefined
  /** How many requests the session has begun, counted over its whole life: a step's snapshots all
   *  carry its own `(turnId, index)`, so the count rises once per request. */
  let steps = 0
  const stepsSeen = new Set<string>()
  /** The rows handed to a running turn that it has not read. */
  const watched = new Map<string, WatchedRow>()
  /** Whether a `$.prompt.submit` is in flight: it holds until its turn ends, so the next one
   *  waits behind it. */
  let submitting: Promise<void> = Promise.resolve()

  /** Hand the mod one prompt, behind every prompt handed before it. Never awaited by a caller:
   *  the call lasts the turn it starts. */
  const submit = (
    text: string,
    asUser: boolean,
    failed?: (error: unknown) => void,
  ): Promise<void> => {
    const sent = submitting.then(async () => {
      const result = await link.request(
        "prompt.submit",
        { text, ...(asUser ? { asUser: true as const } : {}) },
        { timeoutMs: SUBMIT_TIMEOUT_MS },
      )
      if (result.drop !== undefined) {
        throw new Error(`the session dropped the prompt: ${result.drop}`)
      }
    })
    submitting = sent.catch((error: unknown) => {
      warn(`a prompt from Dispatch did not enter this session — ${messageFor(error)}`)
      failed?.(error)
    })
    return sent
  }

  /** The turn `turnId` ended: every row it never read is pointed at by a prompt of its own. */
  const turnEnded = (turnId: string): void => {
    if (turn === turnId) turn = undefined
    let unread = false
    for (const [row, entry] of watched) {
      if (entry.turnId !== turnId) continue
      watched.delete(row)
      if (entry.steps === steps) unread = true
    }
    // One prompt however many rows went unread: they are all above it, and the model reads the
    // conversation, not a list.
    if (unread) void submit(UNREAD_ROW_PROMPT, false)
  }

  return {
    connected: () => link.connected(),
    observe(event) {
      if (event.kind === "turn-start") {
        turn = event.turnId
        return
      }
      if (event.kind === "step") {
        const key = `${event.turnId}\u0000${event.index}`
        if (stepsSeen.has(key)) return
        stepsSeen.add(key)
        steps++
        return
      }
      if (event.kind === "turn-complete") turnEnded(event.turnId)
    },
    inTurn: () => turn !== undefined,
    async show(content) {
      const result = await link.request("session.append", { message: { type: "user", content } })
      if (result.deny !== undefined) {
        throw new Error(`the session refused the message: ${result.deny}`)
      }
      return result.uuid
    },
    async deliver({ text, content, failed }) {
      const running = turn
      if (running === undefined) {
        // The turn this starts runs for as long as the model works; the frame is answered now.
        void submit(text, true, failed)
        return undefined
      }
      const result = await link.request("session.append", { message: { type: "user", content } })
      if (result.deny !== undefined) {
        throw new Error(`the session refused the message: ${result.deny}`)
      }
      // The turn may have ended while the row was being stored: then no request will read it, and
      // it is pointed at at once rather than waiting for a `turn-complete` that has been and gone.
      if (turn === running) watched.set(result.uuid, { turnId: running, steps })
      else void submit(UNREAD_ROW_PROMPT, false)
      return result.uuid
    },
    async runCommand(command, args) {
      const result = await link.request(
        "command.run",
        { command, ...(args === "" ? {} : { args }) },
        { timeoutMs: COMMAND_TIMEOUT_MS },
      )
      return result.text ?? ""
    },
    async fork(prompt) {
      const result = await link.request("model.fork", { prompt }, { timeoutMs: FORK_TIMEOUT_MS })
      if (!result.isAnswered) throw new Error(result.reason)
      return result.text
    },
    async setModel(model) {
      const result = await link.request("config.set", { key: MODEL_CONFIG_KEY, value: model })
      if (result.deny !== undefined) {
        throw new Error(`the session refused the model: ${result.deny}`)
      }
      return result.value
    },
    async setEffort(effort) {
      const result = await link.request("set-effort", { effort })
      return result.effort
    },
  }
}
