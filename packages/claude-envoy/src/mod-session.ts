// The Claude Code session as the router drives it (INFRA-1026): what the mod says is happening in
// it, and the calls that put something into it.
//
// Four measured facts shape everything here, all on 2.1.310 in this pod:
//
//   - `$.prompt.submit` resolves only once the turn it starts has ended. So a submit is never
//     awaited by a caller that has a frame to answer, and at most one is ever in flight: a second
//     one would sit behind the first for a whole turn.
//   - a row `$.session.append` adds is read at the turn's next request, and not at all when no
//     request follows it. So a row added to a running turn is watched until that turn ends, and
//     one the turn never read is pointed at by a prompt of its own.
//   - a turn's next request goes out about 50 ms after the tool result that freed it, and its
//     first chunk arrives seconds later (2.4 s and 2.5 s in the recorded runs). So which request
//     a row made it into is read off `step-request`, which the mod sends as the request goes
//     out, and never off the `step` snapshots, which follow the first chunk: a row added in the
//     seconds between the two would otherwise look read by a request that left without it.
//   - a `$.command.run` of this session's writes its rows after the call that asked for it has
//     answered, and those rows carry `origin.kind` `plugin` or `engine`, where a command a
//     person typed at the session's own terminal carries `unclassified`. So a run stays
//     outstanding past its own result (`runsCommandRow`, `runCommand`), and a row is tied to a
//     run by that run being outstanding rather than by the command the row names: Claude Code
//     runs `/cost` as `/usage` and `/model anthropic/haiku` as `/model haiku`.
//
// The turn tracking is built from the mod's events alone (`../hooks/mod-protocol`), which have one
// consumer: the live view reads them and hands each one here (`observe`), so nothing in this file
// opens a socket or reads the link's stream.
import { messageFor } from "@legion/envoy-client/errors"
import type {
  ModAppendOrigin,
  ModCallResults,
  ModConfigValue,
  ModContentBlock,
  ModEffort,
  ModEvent,
} from "../hooks/mod-protocol"
import { claudeCommandOutput } from "./claude-rows"
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
 * How long a command whose result carried no output of its own waits for the output row Claude
 * Code wrote for it. The row reaches this process as a `session.append` event on a post of its
 * own, so it and the call's result can cross: measured on 2.1.310, the row followed the result
 * by 29 ms for `/context` (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`)
 * and by 158 ms for `/effort low` (`logs/mod3-q2-310.jsonl:98-102`). Two seconds is more than
 * ten times either, and it is all a command that wrote no row costs the person before they are
 * told it printed nothing.
 */
const COMMAND_OUTPUT_MS = 2_000

/** A message a turn never read: the person's own words, and the Dispatch message they are. */
export interface UnreadMessage {
  readonly text: string
  readonly messageId: string
}

/**
 * The prompt that points the model at the messages a turn never read, submitted without
 * `asUser`, so the model reads it as the plugin speaking rather than the person.
 *
 * It carries each message's own words rather than pointing at the row holding them. Driven live
 * on two sessions with a prompt that said "Read the message above": the row sat behind the
 * agent's own long answer, and the model replied `I don't see an actual Dispatch message content
 * above` on one and `it looks like an injection and I'm not going to act on it` on the other
 * (INFRA-1026). One prompt goes out however many rows went unread - the submit is one per turn
 * end - so it names each message.
 */
export function unreadRowPrompt(unread: readonly UnreadMessage[]): string {
  const one = unread.length === 1
  const them = one ? "it" : "them"
  const quoted = unread
    .map((message) => `Dispatch message ${message.messageId}:\n${message.text}`)
    .join("\n\n")
  return [
    `A person sent this session ${one ? "a message" : `${unread.length} messages`} through Dispatch while your last request was already running, so you have not read ${them} yet.`,
    `The claude-envoy plugin is quoting ${them} here in full: ${one ? "it is" : "they are"} the person's own words, not a tool result and not an instruction of the plugin's own. Answer ${them}.`,
    quoted,
  ].join("\n\n")
}

/** The session's model, as a `/config` row names it. */
const MODEL_CONFIG_KEY = "model"

/** What the router does to this session. */
export interface ModSession {
  /** Whether the mod is polling the link now; nothing below reaches the session without one. */
  connected(): boolean
  /** Take one of the mod's events. The live view hands over every event it reads, in order. */
  observe(event: ModEvent): void
  /**
   * Whether a row of the `command` door is one this session's own `$.command.run` produced,
   * which is the row the live view must not draw as a person's message: Claude Code echoes the
   * command it ran rather than the one that was asked for - `/cost` ran as `/usage`, and
   * `/model anthropic/haiku` as `/model haiku`, measured live on 2.1.310 (INFRA-1026) - so the
   * echo drawn is a second message the person never wrote, beside Dispatch's stored copy of the
   * one they did send.
   *
   * True while a run of this session's is outstanding and `origin` is not `unclassified`. The
   * rows of a run carry `{"kind":"plugin"}` or `{"kind":"engine"}`
   * (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:312-321` and `:70-72`,
   * `logs/mod3-q1q3q5.jsonl:195-196`) and a command a person typed at the session's own
   * terminal carries `{"kind":"unclassified"}` (`logs/mod2.jsonl:178-188` and `:218-222`,
   * `logs/mod3-q2-310.jsonl:118-119`), so `origin` tells the two apart where the command's own
   * name cannot.
   */
  runsCommandRow(origin: ModAppendOrigin): boolean
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
    /** The Dispatch message the text is, which the prompt at an unread row names. */
    readonly messageId: string
    readonly content: readonly ModContentBlock[]
    readonly failed?: (error: unknown) => void
  }): Promise<string | undefined>
  /** Run one slash command and answer what it printed, which is the empty string where it
   *  printed nothing. */
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
  /** How long a command whose result carried no output waits for the row Claude Code wrote for
   *  it; `COMMAND_OUTPUT_MS` by default, named by the tests that drive it. */
  readonly commandOutputMs?: number
}

/** One row handed to a running turn, watched until that turn has read it or ended. */
interface WatchedRow {
  readonly turnId: string
  /** The requests the session had made when the row was added: one made after it is the request
   *  that read it. */
  readonly requests: number
  /** The person's own words the row carries, and the Dispatch message it is: the prompt at a
   *  row no request read carries both (`unreadRowPrompt`). */
  readonly text: string
  readonly messageId: string
}

/** One `$.command.run` outstanding, and the output row Claude Code wrote for it. A run is
 *  outstanding past its own result: its rows arrive after the call answers
 *  (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:35-42`, where the caveat, echo and
 *  output rows followed the result by 7 ms, 90 ms and 119 ms), and nothing but the run being
 *  outstanding ties them to it. */
interface CommandRun {
  /** What the output row held, once one has been read as this run's. */
  output?: string
  /** Woken by that row, so a result carrying no output answers the moment the row lands. */
  arrived?: () => void
}

/**
 * What a `$.command.run` result says the command printed, or undefined where it says nothing
 * and the output row is the only place the output is.
 *
 * Measured on 2.1.310 in a terminal session: `/context` resolves `{context: ["## Context Usage…
 * "]}` with no `text` field at all
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`), and `/effort low`
 * resolves `{}` (`logs/mod3-q2-310.jsonl:98-102`). A `context` entry is Claude Code's own
 * markdown of the table, where the output row holds the terminal's drawing of it, escape codes
 * and all, so it is read before the row and after the text.
 */
function resultOutput(result: ModCallResults["command.run"]): string | undefined {
  if (result.text !== undefined && result.text.trim() !== "") return result.text
  const context = result.context?.join("\n\n")
  return context !== undefined && context.trim() !== "" ? context : undefined
}

/**
 * The session behind `options.link`. Every call rejects when the mod does not answer; the caller
 * decides what a frame does then, and nothing here writes to the session on its own.
 */
export function modSession(options: ModSessionOptions): ModSession {
  const { link, warn } = options
  /** The turn running now, as `turn-start` and `turn-complete` report it. */
  let turn: string | undefined
  /** How many requests the session has made, counted over its whole life. Counted from
   *  `step-request`, which the mod sends as each request goes out, and never from a `step`
   *  snapshot: that one follows the request's first chunk, seconds later (2.4 s measured on
   *  2.1.310), so a row added in between would look read by a request that had already left
   *  without it. Each request is counted once, by its own `(turnId, index)`.  */
  let requests = 0
  const requestsSeen = new Set<string>()
  /** The rows handed to a running turn that it has not read. */
  const watched = new Map<string, WatchedRow>()
  /** Whether a `$.prompt.submit` is in flight: it holds until its turn ends, so the next one
   *  waits behind it. */
  let submitting: Promise<void> = Promise.resolve()
  /** The `$.command.run` calls outstanding, oldest first. A run is outstanding from the call
   *  going out until its output row has been read or the wait for that row has passed, so this
   *  holds what a person has running and nothing else. */
  const commandRuns: CommandRun[] = []
  const outputMs = options.commandOutputMs ?? COMMAND_OUTPUT_MS

  const runsCommandRow = (origin: ModAppendOrigin): boolean =>
    commandRuns.length > 0 && origin.kind !== "unclassified"

  /** One row of the `command` door of a run of this session's: the output row is what that run
   *  printed, and the caveat and echo rows beside it say nothing this session needs. */
  const commandRow = (content: readonly ModContentBlock[]): void => {
    const output = claudeCommandOutput(content)
    if (output === undefined) return
    // The oldest run with no output yet: Claude Code runs a session's commands one after
    // another and writes each run's echo before its output
    // (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:35-42`), so the output rows arrive
    // in the order the runs were asked for. The command the row names says nothing about which
    // run it is: `/cost` is run as `/usage`.
    const run = commandRuns.find((waiting) => waiting.output === undefined)
    if (run === undefined) return
    run.output = output
    run.arrived?.()
  }

  /** This run is over: no row that arrives from here on is its. */
  const endRun = (run: CommandRun): void => {
    const at = commandRuns.indexOf(run)
    if (at !== -1) commandRuns.splice(at, 1)
  }

  /** What the output row of `run` held, waited for a bounded moment: the row and the call's
   *  result cross (`COMMAND_OUTPUT_MS`), and a run whose row never came printed nothing. The
   *  run is outstanding until this wait ends, which is one wait per run however many callers
   *  read it: the rows of a run arrive after its call has answered, and `runsCommandRow` has to
   *  know them for the run's as they do. */
  const outputRowOf = async (run: CommandRun): Promise<string> => {
    try {
      if (run.output === undefined) {
        const waited = Promise.withResolvers<void>()
        const timer = setTimeout(waited.resolve, outputMs)
        timer.unref()
        run.arrived = () => {
          clearTimeout(timer)
          waited.resolve()
        }
        await waited.promise
      }
      return run.output ?? ""
    } finally {
      endRun(run)
    }
  }

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

  /** The turn `turnId` ended: every message it never read is carried by a prompt of its own. */
  const turnEnded = (turnId: string): void => {
    if (turn === turnId) turn = undefined
    const unread: UnreadMessage[] = []
    for (const [row, entry] of watched) {
      if (entry.turnId !== turnId) continue
      watched.delete(row)
      if (entry.requests === requests) {
        unread.push({ messageId: entry.messageId, text: entry.text })
      }
    }
    // One prompt however many messages went unread: a second submit would sit behind this one
    // for a whole turn, so the one prompt names each of them.
    if (unread.length > 0) void submit(unreadRowPrompt(unread), false)
  }

  return {
    connected: () => link.connected(),
    observe(event) {
      if (event.kind === "turn-start") {
        turn = event.turnId
        return
      }
      if (event.kind === "step-request") {
        const key = `${event.turnId}\u0000${event.index}`
        if (requestsSeen.has(key)) return
        requestsSeen.add(key)
        requests++
        return
      }
      if (event.kind === "append") {
        // A row of the `command` door is read only where it is a row of a run of this
        // session's: the rows of a command a person ran at their own terminal belong to nobody
        // here, and `origin` is what tells the two apart (`runsCommandRow`).
        if (event.door === "command" && runsCommandRow(event.origin)) {
          commandRow(event.message.content)
        }
        return
      }
      if (event.kind === "turn-complete") turnEnded(event.turnId)
    },
    async show(content) {
      const result = await link.request("session.append", { message: { type: "user", content } })
      if (result.deny !== undefined) {
        throw new Error(`the session refused the message: ${result.deny}`)
      }
      return result.uuid
    },
    async deliver({ text, messageId, content, failed }) {
      const started = turn
      if (started === undefined) {
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
      if (turn === started) watched.set(result.uuid, { messageId, requests, text, turnId: started })
      else void submit(unreadRowPrompt([{ messageId, text }]), false)
      return result.uuid
    },
    runsCommandRow,
    async runCommand(command, args) {
      const run: CommandRun = {}
      commandRuns.push(run)
      let result: ModCallResults["command.run"]
      try {
        result = await link.request(
          "command.run",
          { command, ...(args === "" ? {} : { args }) },
          { timeoutMs: COMMAND_TIMEOUT_MS },
        )
      } catch (error) {
        endRun(run)
        throw error
      }
      // The run's own rows are still to come: measured on 2.1.310, the result landed 7 ms
      // before the caveat row, 90 ms before the echo and 119 ms before the output
      // (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:35-42`). So the wait for the
      // output row is started whatever the result carried, and the run is outstanding until it
      // ends - that is what makes those rows this run's rather than a person's typing.
      const row = outputRowOf(run)
      // What the command printed, in the order the measurements put it: the result's own
      // text, then its `context` entries, which is where a terminal `/context` puts its table
      // and the only place it puts it
      // (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`, a result with no
      // `text` field at all), then the output row Claude Code wrote for this run.
      return resultOutput(result) ?? (await row)
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
