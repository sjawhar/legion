// The Claude Code session as the router drives it (INFRA-1026): what the mod says is happening in
// it, and the calls that put something into it.
//
// Four measured facts shape everything here, all on 2.1.310 in this pod:
//
//   - `$.prompt.submit` resolves only once the turn it starts has ended. So a submit is never
//     awaited by a caller that has a frame to answer, and at most one is ever in flight: a second
//     one would sit behind the first for a whole turn.
//   - a row `$.session.append` adds is read at the turn's next request, and not at all when no
//     request follows it, so an idle session reads nothing that is appended to it. So a row
//     added to a running turn is watched until that turn ends, one the turn never read is
//     quoted in full by a prompt of its own (`unreadRowPrompt`), and an idle session is handed
//     a prompt: a person's message as the prompt alone, a card as the stored row first and the
//     prompt that starts the turn reading it second (`hand`).
//   - a turn's next request goes out about 50 ms after the tool result that freed it, and its
//     first chunk arrives seconds later (2.4 s and 2.5 s in the recorded runs). So which request
//     a row made it into is read off `step-request`, which the mod sends as the request goes
//     out, and never off the `step` snapshots, which follow the first chunk: a row added in the
//     seconds between the two would otherwise look read by a request that left without it.
//   - a `$.command.run` of this session's writes its rows after the call that asked for it has
//     answered (19 ms and 90 ms after it, measured), and nothing in a row tells it from a
//     command a person typed at the session's own terminal: in a terminal session both carry
//     `origin.kind` `engine`, and the command a row names need not be the one asked for, since
//     Claude Code runs `/cost` as `/usage` and `/model anthropic/haiku` as `/model haiku`. So a
//     run claims the first echo row that arrives within `COMMAND_ECHO_MS` of its own answer,
//     and the output row directly behind that echo (`runsCommandRow`, `runCommand`); every
//     other command row is a person's.
//
// The turn tracking is built from the mod's events alone (`../hooks/mod-protocol`), which have one
// consumer: the live view reads them and hands each one here (`observe`), so nothing in this file
// opens a socket or reads the link's stream.
import { messageFor } from "@legion/envoy-client/errors"
import type {
  ModCallResults,
  ModConfigValue,
  ModContentBlock,
  ModEffort,
  ModEvent,
} from "../hooks/mod-protocol"
import { claudeCommandOutput, claudeEchoedCommandLine } from "./claude-rows"
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
 * How long a run whose echo row has been claimed waits for the output row Claude Code wrote
 * behind it. The row reaches this process as a `session.append` event on a post of its own, so
 * it and the call's result can cross: measured on 2.1.310, the row followed the result by 29 ms
 * for `/context` (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`) and by
 * 158 ms for `/effort low` (`logs/mod3-q2-310.jsonl:98-102`). Two seconds is more than ten times
 * either, and it is all a command that wrote no row costs the person before they are told it
 * printed nothing.
 */
const COMMAND_OUTPUT_MS = 2_000

/**
 * How long after its own answer a `$.command.run` may still claim the echo row Claude Code
 * wrote for it.
 *
 * Measured on live Claude Code 2.1.310 (INFRA-1026): a `/model haiku` this session ran answered
 * at 08:14:06.839 and its echo row landed at 08:14:06.858, 19 ms later; an earlier recording of
 * the same shape put that echo 90 ms after the answer. One second is more than ten times the
 * longest seen.
 *
 * It is bounded at all because a run that claims every later row takes rows that are not its
 * own: a Dispatch `/cost` whose dialog waited for the person was asked at 08:09:56.390 and had
 * written no row of its own when the person typed `/config` at their own terminal 36 seconds
 * later, and the rule before this one gave that run the person's echo and output rows - the
 * person's command vanished from the live view, and their `/cost` reply read
 * `Config dialog dismissed`.
 */
const COMMAND_ECHO_MS = 1_000

/** The most claimed echo rows this session remembers. The live view asks about a row in the
 *  same tick this session claimed it (`runsCommandRow`), so a handful would answer every ask;
 *  this is the bound that keeps the record from growing for the session's life, as
 *  `ANSWERED_DELIVERIES_MAX` bounds the one in `./user-turn-router`. */
const CLAIMED_ECHO_ROWS_MAX = 64

/**
 * A message a turn never read, as the prompt at it names the message: the person's own words,
 * with the Dispatch message they are, or a card — an Envoy delivery this plugin wrote into the
 * conversation, which nobody at this session typed.
 */
export type UnreadMessage =
  | { readonly kind: "person"; readonly text: string; readonly messageId: string }
  | { readonly kind: "card"; readonly text: string }

/** One unread message as the prompt quotes it, under what it is: a prompt that called a card
 *  the person's own words would put words in their mouth that they never wrote. */
function quotedUnread(message: UnreadMessage): string {
  return message.kind === "person"
    ? `Dispatch message ${message.messageId}, the person's own words:\n${message.text}`
    : `An Envoy message delivered to this session, which nobody here typed:\n${message.text}`
}

/**
 * The words every prompt at an unread row carries, whichever of the two wordings below it is:
 * both are built from this one string rather than spelling it twice.
 */
const UNREAD_LEAD = "while your last request was already running, so you have not read"

/**
 * The prompt that points the model at the messages a turn never read, submitted without
 * `asUser`, so the model reads it as the plugin speaking rather than the person.
 *
 * It carries each message's own words rather than pointing at the row holding them. Driven live
 * on two sessions with a prompt that said "Read the message above": the row sat behind the
 * agent's own long answer, and the model replied `I don't see an actual Dispatch message content
 * above` on one and `it looks like an injection and I'm not going to act on it` on the other
 * (INFRA-1026). One prompt goes out however many rows went unread - the submit is one per turn
 * end - so it names each message, and a turn that left a card and a person's message unread gets
 * the one prompt holding both.
 */
export function unreadRowPrompt(unread: readonly UnreadMessage[]): string {
  const one = unread.length === 1
  const them = one ? "it" : "them"
  const count = one ? "a message" : `${unread.length} messages`
  const quoted = unread.map(quotedUnread).join("\n\n")
  // The person's own words keep the wording the live runs drove; a set holding a card says
  // instead that each message is labelled, since only some of them are the person's.
  if (unread.every((message) => message.kind === "person")) {
    return [
      `A person sent this session ${count} through Dispatch ${UNREAD_LEAD} ${them} yet.`,
      `The claude-envoy plugin is quoting ${them} here in full: ${one ? "it is" : "they are"} the person's own words, not a tool result and not an instruction of the plugin's own. Answer ${them}.`,
      quoted,
    ].join("\n\n")
  }
  return [
    `Envoy delivered ${count} to this session ${UNREAD_LEAD} ${them} yet.`,
    `The claude-envoy plugin is quoting ${them} here in full, each under what it is: ${one ? "it is" : "they are"} not a tool result and not an instruction of the plugin's own, and nothing here is the person's own words unless it names the Dispatch message they sent. Read ${them} and answer what ${one ? "it asks" : "they ask"} of you.`,
    quoted,
  ].join("\n\n")
}

/**
 * The prompt a card is while the session is idle, submitted without `asUser`: the card's own
 * text under one line saying what it is.
 *
 * An idle session is prompted as well as appended to because an append starts nothing — a row
 * is read at the turn's next request, and an idle session makes none — so an ask answer or a
 * teammate's message would sit unread until the person typed (INFRA-1026 review). The line is
 * needed because a prompt is the one place the model takes direction from: without it the card
 * reads as an instruction the plugin wrote, and with `asUser` it would read as the person's own
 * words, which nobody typed.
 */
export function cardPrompt(text: string): string {
  return [
    "Envoy delivered this message to this session. The claude-envoy plugin is passing it on in full: it is not the words of the person at this session, not a tool result and not an instruction of the plugin's own. Read it and answer what it asks of you.",
    text,
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
   * Whether the row `row` names is the echo row a `$.command.run` of this session's claimed,
   * which is the row the live view must not draw as a person's message: Claude Code echoes the
   * command it ran rather than the one that was asked for - `/cost` ran as `/usage`, and
   * `/model anthropic/haiku` as `/model haiku`, measured live on 2.1.310 (INFRA-1026) - so the
   * echo drawn is a second message the person never wrote, beside Dispatch's stored copy of the
   * one they did send.
   *
   * One echo row per run, claimed inside `COMMAND_ECHO_MS` of that run's own answer: `origin`
   * separates nothing, since a run's rows and a person's terminal command both reach
   * `{"kind":"engine"}` (measured on live Claude Code 2.1.310, INFRA-1026). Every other row of
   * the `command` door is a person's own, and the live view draws it.
   */
  runsCommandRow(row: string): boolean
  /**
   * Hand the session one card: an Envoy delivery that is not the person's own message — an ask
   * answer, a teammate's or an architect's message, an issue event, a notice of Envoy's own.
   *
   * It takes the three states a person's message takes (`deliver`), all but `asUser`: a row of
   * the turn running, a prompt of its own when that turn ends without reading the row, and,
   * while the session is idle, the row stored first and the prompt of a new turn behind it
   * (`cardPrompt`). An append alone starts nothing, so an idle session would sit on the card
   * until the person typed, and a card appended after a turn's last request would never be
   * read at all (INFRA-1026 review).
   *
   * It resolves once the session has stored the row, in every one of those states, and never
   * waits for the prompt, which lasts the turn it starts. What the delivery's frame is
   * answered on is that resolution: a session that cannot take the card rejects it here, the
   * frame goes unanswered, and the listener sends the card again.
   */
  show(card: { readonly text: string; readonly content: readonly ModContentBlock[] }): Promise<void>
  /**
   * Hand the session a person's own message. During a turn it is a row of the conversation, which
   * the model reads at the turn's next request and which a prompt of its own quotes in full
   * when the turn ends without reading it; while the session is idle it is the prompt of a turn,
   * submitted as the person's own words.
   *
   * During a turn it resolves once the session has stored the row, with the row's id. While the
   * session is idle it resolves at once with undefined, before the prompt has gone out (the caller
   * recognises its row by its text): it never waits for the turn a prompt starts, so a prompt that
   * never entered - the session ended while it waited behind another - reaches the caller as
   * `failed` rather than as a rejection.
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
  /** How long a run whose echo row has been claimed waits for the output row behind it;
   *  `COMMAND_OUTPUT_MS` by default, named by the tests that drive it. */
  readonly commandOutputMs?: number
  /** How long after its own answer a run may still claim an echo row; `COMMAND_ECHO_MS` by
   *  default, named by the tests that drive it. */
  readonly commandEchoMs?: number
}

/** One row handed to a running turn, watched until that turn has read it or ended. */
interface WatchedRow {
  readonly turnId: string
  /** The requests the session had made when the row was added: one made after it is the request
   *  that read it. */
  readonly requests: number
  /** What the prompt at a row no request read carries: the message's own words, and what the
   *  message is (`unreadRowPrompt`). */
  readonly unread: UnreadMessage
}

/** One `$.command.run` whose rows are still to come, and the rows it claimed. A run's rows
 *  arrive after the call answers - 5 ms, 19 ms and 49 ms behind it for a `/model haiku`
 *  measured on live Claude Code 2.1.310 (INFRA-1026) - so a run claims the first echo row
 *  inside `COMMAND_ECHO_MS` of its own answer, and the output row directly behind that echo. */
interface CommandRun {
  /** Whether this run may claim an echo row now: true from the moment its own call answers
   *  until it has claimed one or `COMMAND_ECHO_MS` has passed. */
  claiming: boolean
  /** The echo row this run claimed, which is the row the live view draws nothing of. */
  echo?: string
  /** Woken by that row, so a run claims and moves on the moment its echo lands. */
  claimed?: () => void
  /** What the output row held, once the row behind this run's echo has been read. */
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
  /** The `$.command.run` calls whose rows are still to come, oldest first. A run is here from
   *  the call going out until the rows it claims have been read or the waits for them have
   *  passed, so this holds what a person has running and nothing else. */
  const commandRuns: CommandRun[] = []
  /** The echo rows this session's runs claimed, newest last and bounded by
   *  `CLAIMED_ECHO_ROWS_MAX`: what `runsCommandRow` answers from. */
  const claimedEchoes = new Set<string>()
  /** The run whose claimed echo row was the last row of the `command` door to arrive, so the
   *  output row directly behind that echo is read as its output and no later row is. */
  let behindEcho: CommandRun | undefined
  const outputMs = options.commandOutputMs ?? COMMAND_OUTPUT_MS
  const echoMs = options.commandEchoMs ?? COMMAND_ECHO_MS

  const runsCommandRow = (row: string): boolean => claimedEchoes.has(row)

  /** One row of the `command` door, given to the run it belongs to or to no run at all.
   *
   * A run claims one echo row: the first to arrive after its own call answered and inside
   * `COMMAND_ECHO_MS` of that answer, oldest unclaimed run first. The output row directly
   * behind that echo is what the run printed. An echo no run claimed is a command a person
   * typed at their own terminal, and the output row behind it is theirs too: measured on live
   * Claude Code 2.1.310 (INFRA-1026), the two are told apart by that timing and by nothing
   * else - the person's `/config` echo and the `/usage` echo of a run this session made both
   * carried `origin {"kind":"engine"}`, and the command a row names says nothing either, since
   * `/cost` runs as `/usage`.
   */
  const commandRow = (uuid: string, content: readonly ModContentBlock[]): void => {
    if (claudeEchoedCommandLine(content) !== undefined) {
      const claiming = commandRuns.find((waiting) => waiting.claiming)
      behindEcho = claiming
      if (claiming === undefined) return
      claiming.claiming = false
      claiming.echo = uuid
      claimedEchoes.add(uuid)
      if (claimedEchoes.size > CLAIMED_ECHO_ROWS_MAX) {
        const oldest = claimedEchoes.values().next().value
        if (oldest !== undefined) claimedEchoes.delete(oldest)
      }
      claiming.claimed?.()
      return
    }
    const run = behindEcho
    behindEcho = undefined
    const output = claudeCommandOutput(content)
    if (output === undefined || run === undefined) return
    run.output = output
    run.arrived?.()
  }

  /** This run is over: no row that arrives from here on is its. */
  const endRun = (run: CommandRun): void => {
    const at = commandRuns.indexOf(run)
    if (at !== -1) commandRuns.splice(at, 1)
  }

  /**
   * What the rows of `run` held, waited for in the order they arrive: the echo row inside
   * `COMMAND_ECHO_MS` of the call's answer, then the output row directly behind it inside
   * `COMMAND_OUTPUT_MS`. The waits are started the moment the call answers, which is when the
   * run may first claim a row, and the run is one wait however many callers read it.
   *
   * A run that claimed no echo claimed nothing: it printed nothing here, and whatever the call
   * itself answered is the only reply there is (`runCommand`). A run whose echo was claimed and
   * whose output row never came printed nothing too.
   */
  const commandRowsOf = async (run: CommandRun): Promise<string> => {
    try {
      const echoed = Promise.withResolvers<void>()
      const echoTimer = setTimeout(echoed.resolve, echoMs)
      echoTimer.unref()
      run.claimed = () => {
        clearTimeout(echoTimer)
        echoed.resolve()
      }
      run.claiming = true
      await echoed.promise
      run.claiming = false
      if (run.echo === undefined) return ""
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

  /** One row of the conversation, stored. The model reads it at the turn's next request, and
   *  not at all when no request follows it, which is what `hand` watches it for. */
  const append = async (content: readonly ModContentBlock[]): Promise<string> => {
    const result = await link.request("session.append", { message: { type: "user", content } })
    if (result.deny !== undefined) {
      throw new Error(`the session refused the message: ${result.deny}`)
    }
    return result.uuid
  }

  /**
   * One message into the session, in whichever of the three states the session is in: the prompt
   * of a new turn while it is idle, a row of the turn running, and a prompt of its own when that
   * turn ends without a request having read the row. A person's message and a card take the same
   * three (`deliver`, `show`); what separates them is `asUser`, the words of the prompts, and
   * what an idle session does with the message before the prompt goes out.
   */
  const hand = async (message: {
    readonly content: readonly ModContentBlock[]
    /** What goes out while the session is idle, and whether it is the person's own words. */
    readonly prompt: { readonly text: string; readonly asUser: boolean }
    /** What the message is - a person's own words or a card - and what the prompt at an unread
     *  row carries, where the turn never reads it. */
    readonly unread: UnreadMessage
    readonly failed?: (error: unknown) => void
  }): Promise<string | undefined> => {
    const started = turn
    if (started === undefined) {
      // An idle session stores a card before anything is answered for it, and takes a person's
      // message as the prompt alone: what a card's frame is answered on is `show` resolving,
      // where the router answers the person on Dispatch itself. A `$.prompt.submit` is never
      // awaited - it lasts the turn it starts - so an idle branch that only submitted answered
      // the frame although the prompt had not entered the session. Driven live (INFRA-1026,
      // k7's second clause: Claude Code SIGSTOPped, an Envoy message sent to it from another
      // session, then SIGKILL and `claude --resume <id>`), the resumed conversation held that
      // message once while this branch awaited the append and zero times once it did not: the
      // channel server had answered the frame `done`, so the listener never sent the message
      // again, while the prompt itself never entered the stopped process. The append rejects
      // where the session cannot take the message, which is what leaves the frame unanswered
      // and brings the message back - one time, through a restart.
      const uuid = message.unread.kind === "card" ? await append(message.content) : undefined
      // The turn this starts runs for as long as the model works, so the submit is not awaited
      // and the frame is answered now; a submit that fails is a line through `warn`, with a
      // card's row already in the conversation for the next turn to read.
      void submit(message.prompt.text, message.prompt.asUser, message.failed)
      return uuid
    }
    const uuid = await append(message.content)
    // The turn may have ended while the row was being stored: then no request will read it, and
    // it is pointed at at once rather than waiting for a `turn-complete` that has been and gone.
    if (turn === started) watched.set(uuid, { requests, turnId: started, unread: message.unread })
    else void submit(unreadRowPrompt([message.unread]), false)
    return uuid
  }

  /** The turn `turnId` ended: every message it never read is carried by a prompt of its own. */
  const turnEnded = (turnId: string): void => {
    if (turn === turnId) turn = undefined
    const unread: UnreadMessage[] = []
    for (const [row, entry] of watched) {
      if (entry.turnId !== turnId) continue
      watched.delete(row)
      if (entry.requests === requests) unread.push(entry.unread)
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
        // Every row of the `command` door is read, and `commandRow` says whose it is: a row a
        // run this session made claimed, or a row of a command a person typed at their own
        // terminal, which belongs to nobody here.
        if (event.door === "command") commandRow(event.uuid, event.message.content)
        return
      }
      if (event.kind === "turn-complete") turnEnded(event.turnId)
    },
    async show({ text, content }) {
      await hand({
        content,
        // Without `asUser` the model reads the prompt as the plugin speaking, which is what a
        // card is: nobody at this session typed it.
        prompt: { asUser: false, text: cardPrompt(text) },
        unread: { kind: "card", text },
      })
    },
    deliver: ({ text, messageId, content, failed }) =>
      hand({
        content,
        prompt: { asUser: true, text },
        unread: { kind: "person", messageId, text },
        ...(failed === undefined ? {} : { failed }),
      }),
    runsCommandRow,
    async runCommand(command, args) {
      const run: CommandRun = { claiming: false }
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
      // The run's own rows are still to come: measured on live Claude Code 2.1.310
      // (INFRA-1026), a `/model haiku` answered at 08:14:06.839 and its caveat, echo and
      // output rows followed at .844, .858 and .888. So the wait for them is started whatever
      // the result carried - the echo this run claims is the row the live view must not draw,
      // and the claim is what makes the output row behind it this run's rather than a
      // person's typing.
      const rows = commandRowsOf(run)
      // What the command printed, in the order the measurements put it: the result's own
      // text, then its `context` entries, which is where a terminal `/context` puts its table
      // and the only place it puts it
      // (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`, a result with no
      // `text` field at all), then the output row this run claimed.
      return resultOutput(result) ?? (await rows)
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
