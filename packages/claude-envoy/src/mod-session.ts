// The Claude Code session as the router drives it (INFRA-1026): what the mod says is happening in
// it, and the calls that put something into it.
//
// Where a message goes — a row of the turn running, or the prompt of a new turn — and whether a
// turn that ended without reading a row is pointed at it are the mod's to decide, not this
// file's: the mod is the one process where the turn's events and this plugin's writes arrive in
// one order. Three review rounds found a race in deciding them here, each from the same cause —
// two posts whose order is not the order the session saw (INFRA-1026 review, rounds nine to
// eleven). So this file hands the mod one message and reads back the row it became
// (`session.message`, `../hooks/mod-protocol.ts`), and what is left here is what the row's own
// id answers:
//
//   - a `$.command.run` of this session's writes its rows after the call that asked for it has
//     answered (19 ms and 90 ms after it, measured on 2.1.310), and nothing in a row tells it
//     from a command a person typed at the session's own terminal: in a terminal session both
//     carry `origin.kind` `engine`, and the command a row names need not be the one asked for,
//     since Claude Code runs `/cost` as `/usage` and `/model anthropic/haiku` as
//     `/model haiku`. So a run claims the first echo row that arrives within `COMMAND_ECHO_MS`
//     of its own answer, and the output row directly behind that echo (`runsCommandRow`,
//     `runCommand`); every other command row is a person's.
//   - the live view draws a row by what it is, and asks here by the row's own id
//     (`appendedRow`, `runsUnreadPrompt`). Both are answered from the stamp the mod puts on the
//     row's own event (`ModEvent`'s `wrote`), which is in place before the view reads that
//     event, since the server hands every event here first (`./envoy-channel-server`).
//
// Nothing in this file opens a socket or reads the link's stream: the live view reads the mod's
// events and hands each one here (`observe`).
import { messageFor } from "@legion/envoy-client/errors"
import type {
  ModCallResults,
  ModConfigValue,
  ModContentBlock,
  ModEffort,
  ModEvent,
  ModUnreadMessage,
} from "../hooks/mod-protocol"
import { rememberBounded } from "./bounded-set"
import { claudeCommandOutput, claudeEchoedCommandLine } from "./claude-rows"
import type { ModLink } from "./mod-link"

/**
 * How long a card's write may take before its caller gives up on it (`show`).
 *
 * The mod answers as soon as the session has the message: a `$.session.append` into the turn
 * running resolved in 9 ms on 2.1.310, and an idle `$.prompt.submit` in 81 ms to 166 ms, each
 * with the row already written (`/legion/scratch/tmp/infra-1026-spike/logs`). What can take
 * longer is a person typing in the moment the mod found the session idle: the prompt then waits
 * for their turn to end — 8.5 s, 11.2 s and 12.4 s in the submits recorded — and the mod's own
 * store costs a little more under load (a `get` of 114 ms and a `set` of 175 ms were seen,
 * `/legion/scratch/tmp/infra-1026-store`). Thirty seconds covers all of that; past it the frame
 * is left unanswered, Envoy sends it again, and the mod's record of the event id is what keeps
 * the one message from being written twice (`../hooks/mod.ts`, `writeOnce`).
 */
const SHOW_TIMEOUT_MS = 30_000

/**
 * How long a `$.command.run` may take. A command waits for the turn running and for a dialog at
 * the terminal, neither of which has a bound of its own; a day is past any of that and inside
 * the 32-bit range a timer takes. What a person sees meanwhile is the router's waiting reply,
 * posted ten seconds in.
 */
const COMMAND_TIMEOUT_MS = 24 * 60 * 60 * 1_000

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

/** The most claimed rows of one kind this session remembers (`rememberBounded`). The live view
 *  asks about a row in the same tick this session claimed it (`runsCommandRow`,
 *  `runsUnreadPrompt`, `appendedRow`), so a handful would answer every ask; this is the bound
 *  that keeps a record from growing for the session's life. */
const CLAIMED_ROWS_MAX = 64

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
   * Whether the row `row` names is the row Claude Code wrote for the prompt this session
   * pointed the model at unread messages with (`unreadRowPrompt`), which the live view must not
   * draw: that prompt quotes rows the view has already drawn, so drawn it shows a viewer each
   * of those messages twice.
   *
   * Answered from the stamp the mod put on the row's own event (`ModEvent`'s `wrote`), as
   * `appendedRow` is. Read instead off the prompt's wording, a card carrying those words — a
   * review of this code, or a message quoting one — was a card the view never drew (INFRA-1026
   * review, round ten). Every other prompt row is a person's own prompt or a card an idle
   * session took, and the view draws it.
   */
  runsUnreadPrompt(row: string): boolean
  /**
   * Whether the row `row` names is the row one message this session was handed became: a card
   * or a person's message appended into the turn running. Claude Code marks such a row
   * `isMeta`, which the live view draws nothing of otherwise, so this is the one `isMeta` row
   * it must draw (`./agent-stream-claude`).
   *
   * Answered from the stamp the mod put on the row's own event (`ModEvent`'s `wrote`), which
   * the mod sends for the rows it appended itself: Claude Code does not re-enter a mod's own
   * `session.append` hook for them (`../hooks/mod.ts`). The stamp is what separates such a row
   * from another installed plugin's append, which reaches that hook under a `plugin` origin of
   * its own — `{"kind":"plugin","name":"<the other plugin>"}`, measured in the issue's spike
   * (`/legion/scratch/tmp/infra-1026-spike/logs`, where `spike-mod2` and `spike-mod3p` name
   * themselves) — so a rule keeping every `plugin` origin drew another mod's model-facing
   * context as the person's words (INFRA-1026 review, round ten).
   */
  appendedRow(row: string): boolean
  /**
   * Hand the session one card: an Envoy delivery that is not the person's own message — an ask
   * answer, a teammate's or an architect's message, an issue event, a notice of Envoy's own.
   *
   * The mod decides what the card becomes: the prompt of a new turn while nothing is running,
   * submitted without `asUser` as the card's own text and stored nowhere else — an append
   * starts nothing, so a card only appended would sit there until the person typed (INFRA-1026
   * review), and a card appended *and* quoted by a prompt reached the model twice (INFRA-1026,
   * handoff v6: both of the Agents page's rows 8 and 10 of session 225642d4 carried the one
   * card) — or a row of the turn running, which the model reads at that turn's next request and
   * which one prompt of the mod's quotes in full when the turn ends without a request having
   * read it (`unreadRowPrompt`).
   *
   * It resolves when the mod answers with the row the card became, and on nothing else: the
   * row is the proof the session has the card. A session that cannot take it — stopped, wedged,
   * gone — answers nothing, so this rejects when `showMs` is up, the frame goes unanswered, and
   * Envoy sends the card again (check k7's SIGSTOP, message, SIGKILL and `claude --resume <id>`
   * clause). That resend is written once: the mod records the row each `eventId` became and
   * answers a copy of that event from the record, writing nothing (`../hooks/mod.ts`,
   * `writeOnce`). A card with no `eventId` — a notice this server writes for itself — is
   * written as it comes.
   */
  show(card: {
    readonly text: string
    readonly content: readonly ModContentBlock[]
    /** The Envoy event this card is, which the mod's write is idempotent by. */
    readonly eventId?: string
  }): Promise<void>
  /**
   * Hand the session a person's own message, as their own words (`asUser`). The mod decides
   * what it becomes, as it does for a card: a row of the turn running, or the prompt of a new
   * turn.
   *
   * As a row it resolves with the row's id, which the caller records as that Dispatch message.
   * As a prompt it resolves with undefined: the row is the prompt's, and the caller recognises
   * it by its text. A prompt the session dropped reaches the caller through `failed`, since the
   * person is owed a reply on Dispatch for a message their session refused; anything else that
   * went wrong rejects, leaving the frame for Envoy to send again.
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
  /** How long a card's write waits for the mod's answer; `SHOW_TIMEOUT_MS` by default, named by
   *  the tests that drive it. */
  readonly showMs?: number
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
  /** The `$.command.run` calls whose rows are still to come, oldest first. A run is here from
   *  the call going out until the rows it claims have been read or the waits for them have
   *  passed, so this holds what a person has running and nothing else. */
  const commandRuns: CommandRun[] = []
  /** The echo rows this session's runs claimed, newest last and bounded by `CLAIMED_ROWS_MAX`:
   *  what `runsCommandRow` answers from. */
  const claimedEchoes = new Set<string>()
  /** The rows the mod stamped `pointer`, newest last and bounded by `CLAIMED_ROWS_MAX`: what
   *  `runsUnreadPrompt` answers from. */
  const claimedUnreadPrompts = new Set<string>()
  /** The rows the mod stamped `message`, newest last and bounded by `CLAIMED_ROWS_MAX`: what
   *  `appendedRow` answers from. */
  const claimedAppends = new Set<string>()
  /** The run whose claimed echo row was the last row of the `command` door to arrive, so the
   *  output row directly behind that echo is read as its output and no later row is. */
  let behindEcho: CommandRun | undefined
  const outputMs = options.commandOutputMs ?? COMMAND_OUTPUT_MS
  const echoMs = options.commandEchoMs ?? COMMAND_ECHO_MS
  const showMs = options.showMs ?? SHOW_TIMEOUT_MS

  const runsCommandRow = (row: string): boolean => claimedEchoes.has(row)
  const runsUnreadPrompt = (row: string): boolean => claimedUnreadPrompts.has(row)
  const appendedRow = (row: string): boolean => claimedAppends.has(row)

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
      rememberBounded(claimedEchoes, uuid, CLAIMED_ROWS_MAX)
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

  /**
   * Hand the mod one message and answer with the row it became.
   *
   * What the mod does with it — a row of the turn running, or the prompt of a new turn, and the
   * one prompt that points the model at a row no request read — is the mod's own
   * (`../hooks/mod.ts`, `writeMessage`). What this file needs back is the row, which is the
   * proof the session has the message, and `as`, which says which write wrote that row.
   */
  const written = async (
    message: ModUnreadMessage,
    content: readonly ModContentBlock[],
    eventId: string | undefined,
  ): Promise<ModCallResults["session.message"]> =>
    await link.request(
      "session.message",
      { content, message, ...(eventId === undefined ? {} : { eventId }) },
      { timeoutMs: showMs },
    )

  return {
    connected: () => link.connected(),
    observe(event) {
      if (event.kind !== "append") return
      // Every row of the `command` door is read, and `commandRow` says whose it is: a row a
      // run this session made claimed, or a row of a command a person typed at their own
      // terminal, which belongs to nobody here.
      if (event.door === "command") commandRow(event.uuid, event.message.content)
      // What the mod stamped the row it wrote: the row one message became, which is the one
      // `isMeta` row the live view draws, or the row of the prompt pointing the model at
      // messages a turn never read, which that view draws nothing of. Claimed here, where the
      // event arrives, and nowhere else: the server hands every event to this session before
      // the view records it (`./envoy-channel-server`), so the claim is in place for the row
      // being recorded, while a claim made from the answer of the call that wrote the row
      // lands after the view has already read it (INFRA-1026 k2, measured live on 2.1.310 — a
      // card delivered into a running turn reached the model and was drawn nowhere).
      if (event.wrote === "message") rememberBounded(claimedAppends, event.uuid, CLAIMED_ROWS_MAX)
      if (event.wrote === "pointer") {
        rememberBounded(claimedUnreadPrompts, event.uuid, CLAIMED_ROWS_MAX)
      }
    },
    async show({ text, content, eventId }) {
      const answer = await written({ kind: "card", text }, content, eventId)
      if (answer.deny !== undefined) throw new Error(`the session refused the card: ${answer.deny}`)
    },
    async deliver({ text, messageId, content, failed }) {
      let answer: ModCallResults["session.message"]
      try {
        answer = await written({ kind: "person", messageId, text }, content, undefined)
      } catch (error) {
        // The mod answered nothing: the frame stays unanswered and Envoy sends it again, which
        // the Dispatch attempt this message was accepted under turns into a card
        // (`./user-turn-router`, `acceptUserTurn`).
        warn(`a message from Dispatch did not enter this session — ${messageFor(error)}`)
        throw error
      }
      if (answer.deny !== undefined) {
        const refusal = new Error(`the session dropped the prompt: ${answer.deny}`)
        // A prompt the session dropped is the person's to be told about: their own words were
        // refused, and Dispatch is where they are waiting. A row it refused is this server's
        // failure to write, and the frame is left for Envoy to send again.
        if (answer.as !== "prompt" || failed === undefined) throw refusal
        warn(`a message from Dispatch did not enter this session — ${messageFor(refusal)}`)
        failed(refusal)
        return undefined
      }
      // The row of a prompt is the prompt's own, which the caller recognises by its text; the
      // row of an append is this message's, and the caller records it as that Dispatch message.
      return answer.as === "row" ? answer.uuid : undefined
    },
    appendedRow,
    runsCommandRow,
    runsUnreadPrompt,
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
