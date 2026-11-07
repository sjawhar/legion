// The Claude Code session as the router drives it (INFRA-1026): what the mod says is happening in
// it, and the calls that put something into it.
//
// Five measured facts shape everything here, all on 2.1.310 in this pod:
//
//   - `$.prompt.submit` resolves only once the turn it starts has ended. So a submit is never
//     awaited by a caller that has a frame to answer, and no second one is ever started while
//     one is in flight: it would sit behind the first for a whole turn, and the card it carried
//     would be answered on a row that cannot arrive inside `PROMPT_ROW_MS`. A pending submit is
//     therefore a turn, as far as the next message is concerned (`inTurn`).
//   - Claude Code writes a row of the `prompt` door for every prompt it takes, before the turn
//     that reads it starts: 65 ms to 141 ms after the call went out in the five idle submits
//     recorded (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:52-53`,
//     `logs/mod3-q2-310.jsonl:49-51`, `logs/mod3-q1q3q5.jsonl:235-237,249-251,262-264`). That
//     row is the one proof a caller has that the session took a prompt, since the call itself
//     answers a turn later, so a card handed to an idle session waits for it (`promptRow`).
//   - a row `$.session.append` adds is read at the turn's next request, and not at all when no
//     request follows it, so an idle session reads nothing that is appended to it. So a row
//     added to a running turn is watched until that turn ends, one the turn never read is
//     quoted in full by a prompt of its own (`unreadRowPrompt`), and an idle session is handed
//     the message as the prompt itself and nothing else (`show`, `deliver`).
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
import {
  MOD_PLUGIN_NAME,
  type ModCallResults,
  type ModConfigValue,
  type ModContentBlock,
  type ModEffort,
  type ModEvent,
} from "../hooks/mod-protocol"
import { rememberBounded } from "./bounded-set"
import { claudeCommandOutput, claudeEchoedCommandLine, claudeRowText } from "./claude-rows"
import type { ModLink } from "./mod-link"

/**
 * How long a `$.prompt.submit` a mod took may take: it resolves at the end of the turn it
 * starts, and a turn has no bound of its own. A day is past any turn a person waits through, and
 * inside the 32-bit range a timer takes. A submit no mod took never waits this out: the caller
 * that gives up on it withdraws it from the link's queue (`show`), and the link drops what the
 * queue holds when it closes (`./mod-link`).
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

/**
 * How long a card submitted to an idle session waits for the row Claude Code writes for that
 * prompt, which is the proof the session took the card and the only thing the delivery's frame
 * is answered on (`show`).
 *
 * Measured on live Claude Code 2.1.310 (INFRA-1026): an idle session's `$.prompt.submit` had
 * its `prompt`-door row 76 ms later
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:52-53`), 141 ms later
 * (`logs/mod3-q2-310.jsonl:49-51`) and 65 ms, 68 ms and 70 ms later in three submits carrying
 * an attachment (`logs/mod3-q1q3q5.jsonl:235-237,249-251,262-264`). Ten seconds is past sixty
 * times the longest of those, so a machine under load loses no card, and it is short enough
 * that a session which cannot take the card - stopped, or wedged - leaves the frame unanswered
 * while the listener is still waiting to send it again. A submit that enters a turn started
 * meanwhile has its row only when that turn ends — 8.5 s to 12.4 s in the submits recorded,
 * past any bound a frame can wait out — so that card's frame goes unanswered too; what keeps
 * the listener's resend from showing it a second time is `entered` on the rejection
 * (`CardNotShown`), not this bound.
 */
const PROMPT_ROW_MS = 10_000

/** The most claimed rows of one kind this session remembers (`rememberBounded`). The live view
 *  asks about a row in the same tick this session claimed it (`runsCommandRow`,
 *  `runsUnreadPrompt`, `appendedRow`), so a handful would answer every ask; this is the bound
 *  that keeps a record from growing for the session's life. */
const CLAIMED_ROWS_MAX = 64

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
 *
 * The wording is a person's to read and nothing else's: the live view draws nothing for this
 * prompt and draws a card, and it tells the two apart by the uuid of the row Claude Code wrote
 * (`runsUnreadPrompt`), never by the words below. A card quoting them - a review of this code,
 * or a message carrying one - is a card still.
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
      `A person sent this session ${count} through Dispatch while your last request was already running, so you have not read ${them} yet.`,
      `The claude-envoy plugin is quoting ${them} here in full: ${one ? "it is" : "they are"} the person's own words, not a tool result and not an instruction of the plugin's own. Answer ${them}.`,
      quoted,
    ].join("\n\n")
  }
  return [
    `Envoy delivered ${count} to this session while your last request was already running, so you have not read ${them} yet.`,
    `The claude-envoy plugin is quoting ${them} here in full, each under what it is: ${one ? "it is" : "they are"} not a tool result and not an instruction of the plugin's own, and nothing here is the person's own words unless it names the Dispatch message they sent. Read ${them} and answer what ${one ? "it asks" : "they ask"} of you.`,
    quoted,
  ].join("\n\n")
}

/** The session's model, as a `/config` row names it. */
const MODEL_CONFIG_KEY = "model"

/**
 * Why a card was not shown, and the one thing its caller cannot work out for itself: whether
 * the submit that carried it entered the session.
 *
 * The prompt's row cannot answer that. A card handed to an idle session is proven shown by the
 * row Claude Code writes for the prompt (`PROMPT_ROW_MS`), but a turn that starts in the window
 * between the idle check and the submit landing takes the prompt into itself and writes its row
 * only when that turn ends — 8.5 s to 12.4 s in the submits recorded for INFRA-1026, past any
 * bound a frame can wait out. So a row that never came means either "the session took the card
 * and will write the row late" or "nothing took it at all", and only the link knows which: it
 * hands each request to a polling mod and calls back when it has (`./mod-link`,
 * `ModLink.request`'s `handed`). A card no such report arrived for entered nothing: the report
 * is the only evidence there is, and a card recorded as entered on anything less is a card
 * Envoy's resend would never put in front of the model.
 *
 * `entered` separates them. A card that entered is in front of the model already, so Envoy's
 * resend of it must be answered rather than submitted again (R12, one time); a card that
 * entered nothing — a stopped or dead Claude Code, a mod that never polled — must be submitted
 * again when Envoy sends it (INFRA-1026 check k7's second clause).
 */
export class CardNotShown extends Error {
  /** Whether a mod took the submit that carried the card. */
  readonly entered: boolean

  constructor(message: string, entered: boolean, options?: ErrorOptions) {
    super(message, options)
    this.name = "CardNotShown"
    this.entered = entered
  }
}

/** Whether `error`, a rejection of `ModSession.show`, says the card entered the session: the
 *  one rejection its caller answers Envoy's frame on rather than showing the card again. */
export function cardEnteredSession(error: unknown): boolean {
  return error instanceof CardNotShown && error.entered
}

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
   * Whether the row `row` names is the row Claude Code wrote for a prompt at the messages a
   * turn never read (`unreadRowPrompt`), which the live view must not draw: that prompt quotes
   * rows the view has already drawn, so drawn it shows a viewer each of those messages twice.
   *
   * Claimed by uuid, as an echo row is. The alternative is reading the prompt's wording off the
   * row, which makes a card carrying those words - a review of this code, or a message quoting
   * one - a card the view never draws (INFRA-1026 review, round ten). Every other prompt row is
   * a person's own prompt or a card submitted to an idle session, and the view draws it.
   */
  runsUnreadPrompt(row: string): boolean
  /**
   * Whether the row `row` names is one this session appended into the conversation
   * (`$.session.append`): a card handed to a running turn, or a person's own message handed to
   * one. Claude Code marks such a row `isMeta`, which the live view draws nothing of otherwise,
   * so this is the one `isMeta` row it must draw (`./agent-stream-claude`).
   *
   * Claimed by uuid, at the `note`-door event that reports the row under this plugin's own
   * name (`observe`, `MOD_PLUGIN_NAME`) and again at what the append answered. That name is
   * this mod reporting a row only it wrote (`../hooks/mod.ts`), and it is what separates the
   * row from another installed plugin's append, which reaches this mod's hook under the
   * engine's own origin - `{"kind":"plugin","name":"<the other plugin>"}`, measured in the
   * issue's spike (`/legion/scratch/tmp/infra-1026-spike/logs`, where `spike-mod2` and
   * `spike-mod3p` name themselves). So a rule keeping every `plugin` origin drew another
   * mod's model-facing context as the person's words (INFRA-1026 review, round ten), while
   * the claim from the call's answer came too late on its own: the mod posts the row's event
   * before the call's result, so the view had read the row and dropped it (INFRA-1026 k2).
   */
  appendedRow(row: string): boolean
  /**
   * Hand the session one card: an Envoy delivery that is not the person's own message — an ask
   * answer, a teammate's or an architect's message, an issue event, a notice of Envoy's own.
   *
   * A session with no turn running and no prompt of this plugin's still in flight takes the
   * card as the prompt of a new turn, submitted without `asUser` as the card's own text and
   * stored nowhere else: an append starts nothing — a row is read at the turn's next request,
   * and an idle session makes none — so a card only appended would sit there until the person
   * typed (INFRA-1026 review), and a card appended *and* quoted by a prompt reached the model
   * twice (INFRA-1026, handoff v6: both of the Agents page's rows 8 and 10 of session 225642d4
   * carried the one card). Otherwise it is a row of the conversation, which the model reads at
   * the turn's next request — the turn running, or the one a pending prompt is starting — and a
   * prompt of its own quotes it in full when that turn ends without a request having read it
   * (`unreadRowPrompt`).
   *
   * It resolves once the session has the card: the stored row during a turn, and while the
   * session is idle the row Claude Code writes for the prompt it took, waited for up to
   * `PROMPT_ROW_MS`. What the delivery's frame is answered on is that resolution, never the
   * submit going out - a submit resolves only at the end of the turn it starts, and a stopped
   * process writes no row at all, so a card a session cannot take rejects here, its frame goes
   * unanswered, and the listener sends it again (INFRA-1026, check k7's SIGSTOP, message,
   * SIGKILL and `claude --resume <id>` clause).
   *
   * A card submitted as a prompt rejects with a `CardNotShown` saying whether a mod took that
   * submit, which the row cannot say and the caller cannot work out: a card the session took
   * into a turn whose row comes late is in front of the model already, so its caller answers
   * Envoy's resend instead of submitting it a second time. A card no mod took is withdrawn
   * from the link's queue as it rejects, so the resend Envoy makes is the only copy that can
   * reach the model. A card stored as a row rejects with the append's own error (a refusal, or
   * the link giving up), which the caller reads as a card that did not enter.
   */
  show(card: { readonly text: string; readonly content: readonly ModContentBlock[] }): Promise<void>
  /**
   * Hand the session a person's own message. With a turn running, or one a pending prompt is
   * starting, it is a row of the conversation, which the model reads at the turn's next request
   * and which a prompt of its own quotes in full when the turn ends without reading it; with
   * neither it is the prompt of a turn, submitted as the person's own words.
   *
   * As a row it resolves once the session has stored it, with the row's id. As a prompt it
   * resolves at once with undefined, before the prompt has gone out (the caller recognises its
   * row by its text): it never waits for the turn a prompt starts, so a prompt that never
   * entered reaches the caller as `failed` rather than as a rejection.
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
  /** How long a card submitted to an idle session waits for the row Claude Code writes for that
   *  prompt; `PROMPT_ROW_MS` by default, named by the tests that drive it. */
  readonly promptRowMs?: number
}

/** One row handed to a turn, watched until that turn has read it or ended. */
interface WatchedRow {
  /** The turn the row was stored into, and undefined where the row was stored while the prompt
   *  that starts that turn was still in flight: the turn had not reported itself yet, so the
   *  next turn to end is the one that could have read the row (`turnEnded`). */
  readonly turnId?: string
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

/** One submitted prompt whose own row has not arrived: the text that went out, and what is
 *  woken by the row carrying it (`promptRow`). */
interface SubmittedPrompt {
  readonly text: string
  readonly seen: () => void
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
  /** The rows handed to a turn that it has not read, by the id Claude Code stored each under. */
  const watched = new Map<string, WatchedRow>()
  /** How many `$.prompt.submit` calls are in flight. One resolves only at the end of the turn
   *  it starts, so a session with one pending has a turn running or one about to start: a
   *  message handed over meanwhile is a row of that turn, never a second prompt behind the
   *  first (`inTurn`). Counted rather than flagged, since a turn that ends with rows unread
   *  submits the prompt at them (`turnEnded`) without waiting for the submit of the turn that
   *  just ended to answer. */
  let submits = 0
  /** The `$.command.run` calls whose rows are still to come, oldest first. A run is here from
   *  the call going out until the rows it claims have been read or the waits for them have
   *  passed, so this holds what a person has running and nothing else. */
  const commandRuns: CommandRun[] = []
  /** The echo rows this session's runs claimed, newest last and bounded by `CLAIMED_ROWS_MAX`:
   *  what `runsCommandRow` answers from. */
  const claimedEchoes = new Set<string>()
  /** The rows Claude Code wrote for this session's prompts at unread rows, newest last and
   *  bounded by `CLAIMED_ROWS_MAX`: what `runsUnreadPrompt` answers from. */
  const claimedUnreadPrompts = new Set<string>()
  /** The rows this session appended into the conversation, newest last and bounded by
   *  `CLAIMED_ROWS_MAX`: what `appendedRow` answers from. Claimed as each row's event arrives
   *  (`observe`) and at what the append answered (`append`). */
  const claimedAppends = new Set<string>()
  /** The run whose claimed echo row was the last row of the `command` door to arrive, so the
   *  output row directly behind that echo is read as its output and no later row is. */
  let behindEcho: CommandRun | undefined
  /** The prompts submitted whose own row this session is still waiting for, oldest first: a
   *  card to an idle session is answered on that row and on nothing else (`show`). */
  const submittedPrompts: SubmittedPrompt[] = []
  /** The prompts at unread rows this session submitted whose own row has not arrived, oldest
   *  first: the row carrying one is claimed as this session's (`promptRowArrived`). One is in
   *  flight per turn that ends with a row unread and its row follows the submit by 65 ms to
   *  141 ms (`PROMPT_ROW_MS`), so this holds one entry at a time in practice; it is bounded
   *  because a session whose rows stop coming - stopped, or gone - would otherwise keep every
   *  text it submitted. */
  const unreadPrompts: string[] = []
  const outputMs = options.commandOutputMs ?? COMMAND_OUTPUT_MS
  const echoMs = options.commandEchoMs ?? COMMAND_ECHO_MS
  const promptRowMs = options.promptRowMs ?? PROMPT_ROW_MS

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
   * Hand the mod one prompt. Never awaited by a caller: the call lasts the turn it starts.
   *
   * `handed` is called when a polling mod has the submit, which is the one proof a card's
   * caller gets that the prompt entered the session (`CardNotShown`); `signal` takes the submit
   * back out of the link's queue, where one no mod took waits (`./mod-link`, `ModLink.request`).
   * A withdrawn submit rejects, so it stops counting here too and the next message is a prompt
   * of its own again.
   */
  const submit = (
    text: string,
    asUser: boolean,
    options?: {
      readonly failed?: (error: unknown) => void
      readonly handed?: () => void
      readonly signal?: AbortSignal
    },
  ): Promise<void> => {
    submits += 1
    const sent = (async (): Promise<void> => {
      const result = await link.request(
        "prompt.submit",
        { text, ...(asUser ? { asUser: true as const } : {}) },
        {
          timeoutMs: SUBMIT_TIMEOUT_MS,
          ...(options?.handed === undefined ? {} : { handed: options.handed }),
          ...(options?.signal === undefined ? {} : { signal: options.signal }),
        },
      )
      if (result.drop !== undefined) {
        throw new Error(`the session dropped the prompt: ${result.drop}`)
      }
    })()
    // The first handler on `sent`, so the submit has stopped counting by the time the caller
    // that is waiting on its failure (`show`) hears of it.
    void sent.then(
      () => {
        submits -= 1
      },
      (error: unknown) => {
        submits -= 1
        warn(`a prompt from Dispatch did not enter this session — ${messageFor(error)}`)
        options?.failed?.(error)
      },
    )
    return sent
  }

  /** One row of the conversation, stored, and claimed as this session's by the uuid the append
   *  answers with: Claude Code marks an appended row `isMeta`, and the live view draws nothing
   *  of such a row unless this session appended it (`appendedRow`). The row itself is claimed
   *  where its event arrives, which is before this call answers (`observe`); this second claim
   *  is the first-hand one, and covers a copy of the row that reaches this session later under
   *  an origin that names no plugin. The model reads the row at the turn's next request, and
   *  not at all when no request follows it, which is what `handToTurn` watches it for. */
  const append = async (content: readonly ModContentBlock[]): Promise<string> => {
    const result = await link.request("session.append", { message: { type: "user", content } })
    if (result.deny !== undefined) {
      throw new Error(`the session refused the message: ${result.deny}`)
    }
    rememberBounded(claimedAppends, result.uuid, CLAIMED_ROWS_MAX)
    return result.uuid
  }

  /**
   * The row Claude Code writes for the prompt just submitted, waited for up to `PROMPT_ROW_MS`.
   *
   * It is the one proof this process has that the session wrote the card out: `sent` resolves
   * only at the end of the turn the prompt starts, and a Claude Code that is stopped, or gone,
   * takes the submit and writes nothing. The row reaches this session as an `append` event of
   * the `prompt` door (`observe`) carrying the submitted text, framed by Claude Code's own line
   * in front of it ("The claude-envoy plugin sent a message:", measured on 2.1.310). A submit
   * the session refuses outright rejects through `sent` rather than waiting the bound out.
   *
   * What the row cannot say is whether a prompt it never came for entered the session all the
   * same: a turn that starts in the window between the idle check and the submit landing takes
   * the prompt into itself and writes its row only at that turn's end, past the bound. So the
   * rejection carries `entered`, which only the link can report (`CardNotShown`).
   */
  const promptRow = async (
    text: string,
    sent: Promise<void>,
    entered: () => boolean,
  ): Promise<void> => {
    const settle = Promise.withResolvers<void>()
    const waiting: SubmittedPrompt = { text, seen: settle.resolve }
    submittedPrompts.push(waiting)
    // A submit that resolves says the turn it started has ended, which is no answer about the
    // row: only its failure ends this wait early. A failure carried the card nowhere — the mod
    // refused it, none ever took it, or its caller withdrew it — so nothing of it entered the
    // session.
    void sent.catch((error: unknown) =>
      settle.reject(new CardNotShown(messageFor(error), false, { cause: error })),
    )
    const timer = setTimeout(
      () =>
        settle.reject(
          new CardNotShown(
            `the session wrote no row for the prompt within ${promptRowMs} ms`,
            entered(),
          ),
        ),
      promptRowMs,
    )
    timer.unref()
    try {
      await settle.promise
    } finally {
      clearTimeout(timer)
      const at = submittedPrompts.indexOf(waiting)
      if (at !== -1) submittedPrompts.splice(at, 1)
    }
  }

  /**
   * One row of the `prompt` door, given to the prompt of this session's it is the row of, if
   * any: the oldest whose text this row carries, since Claude Code writes its own framing in
   * front of the submitted text ("The claude-envoy plugin sent a message:", measured on
   * 2.1.310). A card submitted to an idle session is what its caller is waiting on, and a
   * prompt at unread rows is claimed by this row's uuid, which is how the live view knows to
   * draw nothing of it (`runsUnreadPrompt`). Every other row of that door is a person's own
   * prompt, or a prompt of this plugin's that nothing is waiting on.
   */
  const promptRowArrived = (uuid: string, content: readonly ModContentBlock[]): void => {
    const text = claudeRowText(content)
    const waiting = submittedPrompts.find((prompt) => text.includes(prompt.text))
    waiting?.seen()
    const unreadAt = unreadPrompts.findIndex((prompt) => text.includes(prompt))
    if (unreadAt === -1) return
    unreadPrompts.splice(unreadAt, 1)
    rememberBounded(claimedUnreadPrompts, uuid, CLAIMED_ROWS_MAX)
  }

  /** The prompt at the messages `unread` holds, submitted and remembered until the row Claude
   *  Code writes for it arrives: that row is this session's rather than a person's message, and
   *  the live view draws nothing of it (`promptRowArrived`). */
  const submitUnreadPrompt = (unread: readonly UnreadMessage[]): void => {
    const text = unreadRowPrompt(unread)
    unreadPrompts.push(text)
    if (unreadPrompts.length > CLAIMED_ROWS_MAX) unreadPrompts.shift()
    void submit(text, false)
  }

  /**
   * One message into a session with a turn to read it: a row of the conversation, which the
   * model reads at the turn's next request. It resolves with the row's id once the session has
   * stored the row, and the row is watched until that turn has read it or ended — one no
   * request read is quoted in full by a prompt of its own when the turn ends (`turnEnded`).
   *
   * A card and a person's message take this one path alike; what separates them is `unread`,
   * the words that prompt carries, and what each caller does with a refusal (`show`,
   * `deliver`).
   */
  const handToTurn = async (
    content: readonly ModContentBlock[],
    unread: UnreadMessage,
  ): Promise<string> => {
    const started = turn
    const uuid = await append(content)
    // The turn may have ended while the row was being stored, with no other starting: then no
    // request will read it, and it is pointed at at once rather than waiting for a
    // `turn-complete` that has been and gone.
    if (started !== undefined && turn === undefined) {
      submitUnreadPrompt([unread])
      return uuid
    }
    watched.set(uuid, { requests, unread, ...(turn === undefined ? {} : { turnId: turn }) })
    return uuid
  }

  /**
   * Whether the session has a turn to hand a row to: one running, or one the submit in flight
   * is about to start.
   *
   * A `$.prompt.submit` resolves only at the end of the turn it starts, so a prompt submitted
   * behind one still in flight would leave this process a whole turn later — and a card
   * carrying it would be answered on a row that cannot come inside `PROMPT_ROW_MS`, which read
   * as a card the session had taken (INFRA-1026 review, round ten). A row is what a message
   * handed over meanwhile becomes instead: the turn the pending prompt starts reads it at its
   * next request, or its end quotes it.
   */
  const inTurn = (): boolean => turn !== undefined || submits > 0

  /** The turn `turnId` ended: every message it never read is carried by a prompt of its own.
   *  A row watched under no turn was stored while the prompt that started this one was still in
   *  flight, so this is the turn that could have read it (`WatchedRow`). */
  const turnEnded = (turnId: string): void => {
    if (turn === turnId) turn = undefined
    const unread: UnreadMessage[] = []
    for (const [row, entry] of watched) {
      if (entry.turnId !== undefined && entry.turnId !== turnId) continue
      watched.delete(row)
      if (entry.requests === requests) unread.push(entry.unread)
    }
    // One prompt however many messages went unread: a second submit would sit behind this one
    // for a whole turn, so the one prompt names each of them.
    if (unread.length > 0) submitUnreadPrompt(unread)
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
        // A row of the `prompt` door can be the row Claude Code wrote for a prompt this
        // session submitted: what a card handed to an idle session is answered on, or the row
        // of a prompt at unread rows, which the live view must draw nothing of.
        if (event.door === "prompt") promptRowArrived(event.uuid, event.message.content)
        // A row of the `note` door under this plugin's own name is a row this session
        // appended: the mod reports every row its `$.session.append` wrote, naming this plugin
        // as the engine names it (`../hooks/mod.ts`, `MOD_PLUGIN_NAME`), while another
        // installed mod's append reaches that hook under that mod's own name. Claimed here,
        // and not only where the call answers: the mod posts this event before it posts the
        // call's result, so the row reaches the live view first and a claim made from the
        // answer lands after the view has dropped it (INFRA-1026 k2, measured live on 2.1.310
        // — a card delivered into a running turn reached the model and was drawn nowhere).
        // The server hands every event here before the view records it
        // (`./envoy-channel-server`), so a claim made in this line is in place for it.
        if (event.door === "note" && event.origin.name === MOD_PLUGIN_NAME) {
          rememberBounded(claimedAppends, event.uuid, CLAIMED_ROWS_MAX)
        }
        return
      }
      if (event.kind === "turn-complete") turnEnded(event.turnId)
    },
    async show({ text, content }) {
      if (inTurn()) {
        await handToTurn(content, { kind: "card", text })
        return
      }
      // The card itself is the prompt, without `asUser` — the model reads it as the plugin
      // speaking, which is what a card is, where `asUser` would read as words the person never
      // typed — and nothing else is stored for it: a row appended beside the prompt that quotes
      // it put the one message in front of the model twice (INFRA-1026, handoff v6). The turn
      // this starts runs for as long as the model works, so the submit is never awaited; what
      // is awaited is the row Claude Code writes for the prompt, which is the proof the session
      // took it.
      //
      // Whether a mod was handed that submit is the one fact that separates a card the session
      // has from one no Claude Code ever saw: both leave the submit pending — it answers at the
      // turn's end — and both can leave the row unwritten inside the bound. Only the link
      // reports it, and a card no report arrived for entered nothing (`./mod-link`,
      // `ModLink.request`).
      let taken = false
      const withdrawal = new AbortController()
      const sent = submit(text, false, {
        handed: () => {
          taken = true
        },
        signal: withdrawal.signal,
      })
      try {
        await promptRow(text, sent, () => taken)
      } catch (error) {
        // A card no mod took is taken back out of the link's queue: left there, the mod that
        // polls next would submit a card the listener has long since sent again, so the one
        // card would reach the model twice (R12, one time). A submit a mod holds is left to
        // run — the card is in the session already.
        if (!taken) withdrawal.abort()
        throw error
      }
    },
    async deliver({ text, messageId, content, failed }) {
      if (inTurn()) return await handToTurn(content, { kind: "person", messageId, text })
      // The person's own words as the prompt of a new turn, with `asUser`. It answers before
      // the prompt has gone out: the router answers them on Dispatch and recognises the row by
      // its text, and a prompt that never entered reaches them through `failed`.
      void submit(text, true, failed === undefined ? {} : { failed })
      return undefined
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
