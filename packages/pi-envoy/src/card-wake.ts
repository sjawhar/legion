import { randomUUID } from "node:crypto";
import { type EnvoyPluginInterface, LOCAL_ENVOY_NOTICE } from "@legion/pi-shared/interface";
import type {
  AutoCompactionEndEvent,
  AutoRetryEndEvent,
  PiApi,
  SessionContext,
} from "@legion/pi-shared/pi-types";

/**
 * How an Envoy card is delivered, and the wake for a card the host folds into the context with no
 * turn.
 *
 * Every card goes as an aside with `triggerTurn`, busy host or idle, outside the startup phase
 * (below). The host takes every queued aside at its next step boundary (`IrcBridge.drainPending`),
 * and an idle host starts a turn for a `triggerTurn` aside, in plan mode and after an earlier
 * interrupt too, unless the person interrupts after the send (Oh My Pi v18.12.2-sami.20261027 and
 * later, `agent-session.ts` `#dispatchCustomMessage`'s `turnRequested`); one that turned busy in
 * the meantime queues it behind that run as an aside (v18.13.2-sami.20261027 and later). A steer
 * would join the host's one steering queue, the one a person's typed message waits in, which the
 * agent loop releases one message per step boundary: cards that arrive faster than the session
 * takes steps then wait without end, and every card ahead of a person's message holds it back a
 * step. A steer handed to a host seen idle is no exception: the host reads `isIdle` here, at the
 * hand-off, and checks `isStreaming` only after its own awaits, so of several cards handed
 * together to an idle host the first starts the run and the rest join the steering queue.
 *
 * The host folds an aside left over when a run settles (one taken after the run's last step,
 * during its post-run maintenance or auto-compaction) into the context with no turn when the
 * person interrupted since the aside was sent, emitting the record's `message_end` and persisting
 * it (`agent-session.ts` `#resumeStrandedIrcAsides`), and a card folded that way would sit unread.
 * So a card's `message_end` arms one check on the host's managed timer, and the check sends one
 * wake when the host is idle, no run started since the card's, no interrupt is in effect, and no
 * assistant message follows the card in the branch. The branch's order decides rather than the
 * events', because the host emits the extension `agent_end` unawaited after its post-run
 * maintenance and does not serialise separate emits: a fold's `message_end` can reach this module
 * before or after it. A card a run read has the model's reply after it, and one not yet in the
 * branch has nothing after it, since the host persists every `message_end` in one first-in,
 * first-out chain (`#queueMessageEndPersistence`).
 *
 * The wake goes as `nextTurn` with `triggerTurn`: on an idle host that starts a turn with no latch
 * or plan-mode check, and on a host that turned busy since the check it waits in the hidden
 * next-turn queue, never in the steering queue ahead of a person's Send. It carries
 * `LOCAL_ENVOY_NOTICE`, so it is no card itself and Legion's phase-stall check does not count it
 * as a second inbound event.
 *
 * An interrupt keeps the cards it finds folded until the person's next turn. Extensions see one
 * only as the interrupted run's last assistant message (aborted, `USER_INTERRUPT_LABEL`), an
 * aborted post-run auto-compaction, or a retry cancelled in its backoff; each holds the wake back
 * until the next run starts. Any other abort sets no latch, and the host wakes for the asides it
 * strands itself, so it holds nothing back here.
 *
 * An interrupt while the host sets up a prompt leaves no sign at all. From the prompt's start
 * (`agent-session.ts` `#promptWithMessage` marks the host busy, ~8255) to its run's `agent_start`
 * the host runs the usage preflight and the `before_agent_start` handlers, pi-envoy's own read of
 * the open asks (up to `OPEN_ASKS_TIMEOUT_MS`) among them. An abort there cancels the setup with
 * no loop running, so no aborted assistant message comes, and abort's `finally` folds the queued
 * card under the latch the abort set (~10250-10252). So a card sent while the host is busy and no
 * run is active, which is from a run's `agent_start` to its `agent_end`, is a setup card and never
 * a candidate for the wake: if the prompt does run, its loop reads the queued aside at the run's
 * first step boundary, after its first model response (`agent-loop.ts` ~1794, ~1822). So is a
 * card sent after a run's `agent_end` while the host still settles because a continuation follows
 * (`willContinue`): the continuation's loop reads it. The host can also drop a run with no
 * `agent_end` for extensions: a switch to a new or resumed session (`newSession`, ~10280-10282;
 * `switchSession`, ~11906-11907) and a manual or extension compaction (`session-maintenance.ts`
 * `compact`, ~1201-1202) disconnect from the agent before they abort it. So a run also ends once
 * the host is seen idle: by a card or a check, or at a `session_switch`, a `session_compact` or a
 * typed input (the `input` event an RPC prompt, a terminal Enter and `sendUserInput` emit before
 * they prompt: `rpc-mode.ts` ~1734-1735, `input-controller.ts` ~950, `send-user-input-handler.ts`
 * ~90) that finds `isIdle` true. The abort before a switch or a manual compaction waits for the
 * agent and resets the host's in-flight count (~10209-10227), so the host is idle at its event,
 * and a prompt sent from idle is typed while the host is idle. An automatic compaction runs while
 * the host is busy, between a run's steps (~2082-2093), in a prompt's setup (after ~8255) or in a
 * run's post-run maintenance, which the prompt awaits (~3662, ~5168-5193), so it leaves the run
 * going, whatever order its `session_compact` and `auto_compaction_end` reach this module in; an
 * idle compaction finds the host idle. The host emits the extension `agent_end` only at the end of
 * the run's maintenance pass (~4467-4828, then `#settleAgentEnd`, ~5336-5362), after its
 * auto-compaction, a retry's backoff (awaited inside it) and the `session_stop` pass, so a card
 * that lands during that maintenance is the run's, and a fold under an earlier interrupt's latch
 * still gets its wake. What stays unseen is an interrupt during that maintenance at an await that
 * is neither auto-compaction nor a retry's backoff (the `session_stop` hooks, ~4827, the todo
 * reminder and the goal and empty-stop passes before them): the card the run's last step missed
 * is folded under the latch and woken. A setup card the host folds with no turn and no interrupt,
 * in plan mode, gets no wake.
 *
 * The startup phase. Oh My Pi starts the sends an extension makes from `session_start` and
 * startup discovery only once that discovery is done (`runtime-init.ts` `initializeExtensions`
 * runs both inside `runner.sends.withHeld`, ~535-545), then awaits every send it tracks, those
 * started while it waits included (`runner.sends.drain()`, ~548; `send-queue.ts` `drain`), before
 * it returns, and RPC mode reads no input until it has returned. An idle card sent with
 * `triggerTurn` is a send that lasts as long as the run it starts, so a card that reached
 * pi-envoy in that window, as it opens its agent subject in `session_start`, ran a whole turn
 * inside the drain, and the host answered no RPC command (`get_state`, `prompt`) until it ended.
 * The host gives extensions no event once the drain is over. Without the start-turn mark (below)
 * the phase needs `ctx.mode` "rpc": RPC mode reads nothing until the drain returns. ACP
 * initialises extensions with "rpc" too (`acp-agent.ts` ~2732), so the phase holds there as well,
 * at no cost: ACP's bridge defers a turn the agent starts (`agent-session.ts` ~9301-9303), so the
 * wake becomes hidden next-turn context there, as an idle card's turn did. Print and JSON mode
 * prompt right after their own awaits that follow it (`print-mode.ts`, MCP readiness and a tool
 * refresh, ~250-254; `session.prompt`, ~286), and that prompt throws `AgentBusyError`
 * (`agent-session.ts` ~7710-7717) if a wake started a turn first. A terminal applies its
 * plan-mode default and subscribes to the agent only after the drain (`interactive-mode.ts`
 * ~2458-2498), so a wake's turn could start ahead of both. In those modes, and on a context with
 * no `mode`, an idle card is an aside with `triggerTurn` from the start unless the start-turn mark
 * holds.
 *
 * So with mode "rpc", or in any mode while the start-turn mark holds, a card that finds the host
 * idle while the phase holds goes as a `followUp` with no `triggerTurn`: on an idle host that
 * appends it to the context, persists it and emits its `message_end`, starting no turn and
 * awaiting nothing longer than its pictures (`agent-session.ts` `sendCustomMessage`'s idle
 * append, ~9360-9382). The card is its own witness that startup is over: the host starts a held
 * send only once the hold settles (`send-queue.ts` `dispatch`, ~52-55, on the hold `withHeld`
 * settles in its `finally`, ~38-49), so its `message_end` comes after the hold, and the check
 * that `message_end` arms runs on the host's timer, a macrotask, while everything from the drain
 * to `initializeExtensions` returning runs in microtasks and a send that starts a turn marks the
 * host busy before it awaits anything. A host idle at that check has no tracked send pending: the
 * drain has returned, and the wake the check sends starts its turn outside it. The one exception
 * is another extension's startup send still loading its pictures on an idle host. The first check
 * that finds the host idle, the start-turn mark not holding, ends the phase.
 *
 * A startup card is unseen from its send to its `message_end`. While one is younger than
 * `STARTUP_CARD_CAP_MS` the check waits for it, since it may still be inside the host's send and
 * so still hold the drain; one older than that is a send the host failed (a discarded hold rejects
 * it, and so does a send that throws) and waits the check no longer. A seen startup card is in the
 * context: a running turn reads it, so a busy check keeps it and the run's `agent_end` checks
 * again, and an idle check wakes for it whatever the run. It also wakes for one whatever the
 * interrupt latch, as an idle card's own turn starts after an interrupt too, unless the card
 * is owed (below): an interrupt then stopped the start turn that card waited for.
 *
 * The start turn. A session whose first turn is a start sent after Oh My Pi's startup, the Legion
 * controller's start message, is named in the plugin interface's start-turn mark
 * (`EnvoyPluginInterface.startupTurn`, which `@sjawhar/pi-legion` sets and clears): `session`,
 * and `sentAt` once the start was sent, undefined while it is due and unsent. The operator's
 * controller is marked unsent at its `session_start`, and `/legion-controller-start`, which Oh My
 * Pi runs after its startup, sends the start and marks it sent; the daemon's controller in RPC
 * mode is marked sent right before it reports ready, which is when the daemon writes the start
 * prompt, read only once the drain has returned. That start must be the session's first turn,
 * and every startup card still gets a turn after it. Without the mark, a terminal's idle card
 * starts a turn that runs ahead of the start command, and an RPC card's wake fires once the host is
 * idle after the drain, which can be before the daemon's prompt arrives. So while the mark holds —
 * it names this session (the instance's own and the host's live one), and its start is unsent
 * and this module first read that unsent mark less than `STARTUP_TURN_CAP_MS` ago, or was sent
 * less than `STARTUP_TURN_CAP_MS` ago — the phase holds whatever `ctx.mode` is: an idle card in
 * any mode goes as a startup card. An idle check while it holds keeps the phase and the seen
 * startup cards, sends no wake and checks again when the hold may end: at the sent mark's cap, or
 * every `STARTUP_TURN_POLL_MS` while the start is unsent, up to that mark's cap (a poll, since no
 * event tells this module of the send, nor of a start claim refused before the send, which clears
 * the mark). The unsent mark's cap counts from this module's first read of it, never again from a
 * later check, so a start that never comes (a claim that never answers, a session given no start
 * command) holds the cards back once, for that long. A busy check is as before. The start turn's
 * run clears the mark at its `agent_start`, and its `agent_end` checks again: that check ends the
 * phase and sends one wake. A startup card seen while the mark held, at its `message_end` or at a
 * check, is owed that wake unless a turn after the start turn read it, since the start turn's
 * replies after it ran the start procedure rather than reading it (`unreadCards`); every other
 * card is counted as before. An owed card gets no wake while an interrupt is in effect, which at
 * that check is one that stopped the start turn (its aborted reply, an aborted auto-compaction or
 * a cancelled retry): a wake would start the turn the person stopped, so the card stays folded
 * for the person's next turn, as a fold card does, and the check that ends the phase drops it. A
 * sent mark past its cap is a start the host failed and holds nothing back, and a mark naming
 * another session (a `task` subagent's, or a session switched away from) changes nothing.
 */

/** How long a card sent in the startup phase may go without its `message_end` before the wake's
 *  check stops waiting for it: one older than this is a send the host failed. */
export const STARTUP_CARD_CAP_MS = 30_000;

/** How long the start-turn mark holds the startup wake back: from its start's send
 *  (`startupTurn.sentAt`), or, while its start is unsent, from this module's first read of that
 *  unsent mark. A mark older than this is a start the host failed or one that is not coming. */
export const STARTUP_TURN_CAP_MS = 30_000;

/** How often a check reads a mark whose start is unsent again, until its cap: no event tells this
 *  module that the start was sent, or that a claim refused before the send cleared the mark. */
export const STARTUP_TURN_POLL_MS = 1_000;

/** The `errorMessage` of the assistant message an interrupt ends a run with: the host's
 *  `USER_INTERRUPT_LABEL` (`@oh-my-pi/pi-tui`), which RPC `abort`, Esc and `ctx.abort` all pass. */
const USER_INTERRUPT_LABEL = "Interrupted by user";

/** The `finalError` of a retry saga an abort cancelled in its backoff or credential wait. */
const RETRY_CANCELLED = "Retry cancelled";

/** What every card carries in its `details`, which the host keeps on the record and its persisted
 *  entry: an id no other card in the process shares, and what the card's `message_end` arms, decided
 *  when it is sent: `startup` for a card sent in the startup phase, `fold` for one the wake covers
 *  if the host folds it, and nothing for one it never covers. The session's own notices carry
 *  `LOCAL_ENVOY_NOTICE` instead. A type rather than an interface, so it is the host's
 *  `Record<string, unknown>` `details`. */
export type EnvoyCardMarker = {
  readonly envoyCard: string;
  readonly wake?: "startup" | "fold";
};

/** The card marker a host message's or a branch entry's `details` carries; undefined for none. */
function envoyCardMarker(details: unknown): EnvoyCardMarker | undefined {
  if (typeof details !== "object" || details === null || !("envoyCard" in details)) {
    return undefined;
  }
  const { envoyCard } = details;
  if (typeof envoyCard !== "string") return undefined;
  const wake = "wake" in details ? details.wake : undefined;
  return wake === "startup" || wake === "fold" ? { envoyCard, wake } : { envoyCard };
}

/** What of the host's context the wake reads: the session pi-envoy delivers into. */
export interface CardWakeContext {
  readonly isIdle?: SessionContext["isIdle"];
  readonly mode?: SessionContext["mode"];
  readonly setTimeout: SessionContext["setTimeout"];
  readonly sessionManager: {
    readonly getBranch?: SessionContext["sessionManager"]["getBranch"];
  };
}

/** How one card goes to the host: `pi.sendMessage`'s options, and the marker for its `details`. */
export interface CardDelivery {
  readonly deliverAs: "aside" | "followUp";
  readonly triggerTurn: boolean;
  readonly details: EnvoyCardMarker;
}

export interface CardWakeDeps {
  /** The host's `pi.sendMessage`, which the wake goes through. */
  readonly sendMessage: PiApi["sendMessage"];
  readonly now: () => number;
  /** The start-turn mark (`EnvoyPluginInterface.startupTurn`) as it stands now; undefined where
   *  there is none, or no interface at this build's version was published. */
  readonly startupTurn: () => EnvoyPluginInterface["startupTurn"];
  /** Whether `session` is the session the cards go into: this instance's own, and the host's
   *  live one. */
  readonly isOwnSession: (session: string) => boolean;
}

/** A card whose `message_end` was seen and that waits for the check: the run it came in, and
 *  whether it was sent in the startup phase. */
interface SeenCard {
  readonly id: string;
  readonly run: number;
  readonly startup: boolean;
}

/** Whether `message`, an assistant message, ends a model turn: the model stopped there rather than
 *  calling a tool, whose result the same turn goes on with. An errored one may be retried within
 *  the run (`auto_retry`), so it ends nothing. */
function endsModelTurn(message: object): boolean {
  if (!("stopReason" in message) || typeof message.stopReason !== "string") return false;
  return message.stopReason !== "toolUse" && message.stopReason !== "error";
}

const NO_CARDS: ReadonlySet<string> = new Set();

/**
 * How many of `cards` (card ids) are unread in `branch`, the session's active branch; one the
 * branch does not hold yet is unread. A card with the model's reply anywhere after it was read. A
 * card in `owed` (a startup card seen while the start-turn mark held) is ahead of the start turn,
 * whose replies ran the start procedure rather than reading it: it was read only by a reply after
 * the start turn ended, which is the first assistant message after the card that ends a model turn
 * (`endsModelTurn`). That later reply is a run of its own, or the start turn's run going on for an
 * aside, a steer or a follow-up the host took at the model's stop (`agent-loop.ts` ~1821-1827).
 * Only the branch's order decides, as for every other card, so the order the host emits its events
 * in changes nothing. Walked from the end, stopping once every card is placed.
 */
export function unreadCards(
  branch: readonly unknown[],
  cards: ReadonlySet<string>,
  owed: ReadonlySet<string> = NO_CARDS
): number {
  const unplaced = new Set(cards);
  let unread = 0;
  // Whether an assistant message is after the entry the walk is at.
  let replied = false;
  // Whether an assistant message is after an assistant message, after that entry, that ends a turn.
  let repliedAfterTurn = false;
  for (let index = branch.length - 1; index >= 0 && unplaced.size > 0; index -= 1) {
    const entry = branch[index];
    if (typeof entry !== "object" || entry === null || !("type" in entry)) continue;
    if (entry.type === "message" && "message" in entry) {
      const { message } = entry;
      if (typeof message === "object" && message !== null && "role" in message) {
        if (message.role !== "assistant") continue;
        repliedAfterTurn ||= replied && endsModelTurn(message);
        replied = true;
      }
    } else if (entry.type === "custom_message" && "details" in entry) {
      const marker = envoyCardMarker(entry.details);
      if (marker === undefined || !unplaced.delete(marker.envoyCard)) continue;
      if (!(owed.has(marker.envoyCard) ? repliedAfterTurn : replied)) unread += 1;
    }
  }
  return unread + unplaced.size;
}

export class CardWake {
  readonly #deps: CardWakeDeps;
  /** Bumped at every `agent_start`: a card seen in an earlier run than the current one was that
   *  later run's to read. */
  #run = 0;
  /** Whether an interrupt the extension saw holds the wake back until the next run starts. */
  #interrupted = false;
  /** Whether a run is active as this extension sees it: from its `agent_start` to its `agent_end`,
   *  or to the host being seen idle (a card, a check, `noteIfIdle`), which ends one the host
   *  dropped unseen too. A host busy with none is setting a prompt up, or settling after one. */
  #runActive = false;
  #startupPhase = true;
  /** Startup cards sent and not yet seen, with when each was sent. */
  readonly #unseenStartup = new Map<string, number>();
  #seen: SeenCard[] = [];
  /** Startup cards seen while the start-turn mark held, which only a reply after the start turn
   *  reads: the start turn's replies after them ran the start procedure (`unreadCards`). */
  readonly #owed = new Set<string>();
  /** The unsent start-turn mark this module last read, and when it first read it, which its cap
   *  counts from. Each marking writes a new object to the interface, so a mark set again is a new
   *  one whose cap counts from its own first read. */
  #unsentMark:
    | {
        readonly mark: NonNullable<EnvoyPluginInterface["startupTurn"]>;
        readonly firstRead: number;
      }
    | undefined;
  /** When the armed check runs, if one is armed: a sooner one replaces it, a later one waits. */
  #checkDue: number | undefined;
  // A check that a sooner one replaced, or a cancel dropped, is skipped by its generation when it
  // fires. The host's `setTimeout` returns a handle its `clearTimer` takes, but clearing it would
  // mean keeping the handle and the context that armed it, which `cancel` is not handed, for
  // nothing: a stale check that fires into a no-op costs nothing.
  #checkGeneration = 0;

  constructor(deps: CardWakeDeps) {
    this.#deps = deps;
  }

  /**
   * How to send a card that is a targeted Aside or not, given the context of the session it goes
   * into. A startup card is recorded unseen here, before the host has it: a send that throws
   * leaves an entry, which `STARTUP_CARD_CAP_MS` clears.
   */
  deliveryFor(targetedAside: boolean, context: CardWakeContext | undefined): CardDelivery {
    const idle = context?.isIdle?.();
    // An idle host has no run, whatever end of it no extension was told of.
    if (idle === true) this.#runActive = false;
    const startup =
      this.#startupPhase &&
      idle === true &&
      (context?.mode === "rpc" || this.#startTurnHold() !== undefined);
    // A card sent while the host sets a prompt up is never woken. Under main every targeted Aside
    // went as an aside, which the host folds with no turn under the latch or in plan mode, so a
    // wake for one would start a turn main never started. A startup targeted Aside does get one:
    // in the startup phase every idle card goes as a follow-up with no turn, where main's idle
    // aside started a turn outside the latch and plan mode, and a fresh process has no latch. Plan
    // mode at startup is the one case where that wake starts a turn main did not, which
    // `AGENTS.md` states.
    const setup = idle === false && !this.#runActive;
    const wake = startup ? "startup" : setup || targetedAside ? undefined : "fold";
    const details: EnvoyCardMarker = {
      envoyCard: randomUUID(),
      ...(wake !== undefined && { wake }),
    };
    if (startup) {
      this.#unseenStartup.set(details.envoyCard, this.#deps.now());
      return { deliverAs: "followUp", triggerTurn: false, details };
    }
    return { deliverAs: "aside", triggerTurn: true, details };
  }

  /** A host `message_end`: an interrupt holds the wake back, and a card the host appended arms
   *  the check that wakes for it. */
  messageEnd(message: unknown, context: CardWakeContext): void {
    if (typeof message !== "object" || message === null || !("role" in message)) return;
    if (message.role === "assistant") {
      if (
        "stopReason" in message &&
        message.stopReason === "aborted" &&
        "errorMessage" in message &&
        message.errorMessage === USER_INTERRUPT_LABEL
      ) {
        this.#interrupted = true;
      }
      return;
    }
    if (
      message.role !== "custom" ||
      !("customType" in message) ||
      message.customType !== "envoy-message" ||
      !("details" in message)
    ) {
      return;
    }
    const marker = envoyCardMarker(message.details);
    if (marker?.wake === undefined) return;
    if (marker.wake === "startup") {
      this.#unseenStartup.delete(marker.envoyCard);
      if (this.#startTurnHold() !== undefined) this.#owed.add(marker.envoyCard);
    }
    this.#seen.push({ id: marker.envoyCard, run: this.#run, startup: marker.wake === "startup" });
    this.#arm(context);
  }

  /** A run started: the cards seen from here on are its own, and an interrupt is over. */
  agentStart(): void {
    this.#run += 1;
    this.#interrupted = false;
    this.#runActive = true;
  }

  /** A run ended: a startup card a busy check kept, or one not yet seen, is checked again. */
  agentEnd(context: CardWakeContext): void {
    this.#runActive = false;
    if (this.#unseenStartup.size > 0 || this.#seen.some((card) => card.startup)) {
      this.#arm(context);
    }
  }

  /** The host may be idle: at a `session_switch` (a switch to a new or resumed session aborted any
   *  run unseen, `agent-session.ts` ~10280-10282, ~11906-11907), a `session_compact` (so did a
   *  manual or an extension's compaction, `session-maintenance.ts` ~1201-1202) and a typed input.
   *  If it is, no run is active. A busy host there runs a live run (a fork, an automatic
   *  compaction), sets a prompt up or settles, which changes nothing. */
  noteIfIdle(context: Pick<CardWakeContext, "isIdle">): void {
    if (context.isIdle?.() === true) this.#runActive = false;
  }

  autoCompactionEnd(event: AutoCompactionEndEvent): void {
    if (event.aborted) this.#interrupted = true;
  }

  autoRetryEnd(event: AutoRetryEndEvent): void {
    if (!event.success && event.finalError === RETRY_CANCELLED) this.#interrupted = true;
  }

  /** The session the cards went into is gone (a switch, a branch or tree move, shutdown): drop the
   *  armed check and every card it was armed for. */
  cancel(): void {
    this.#checkGeneration += 1;
    this.#checkDue = undefined;
    this.#seen = [];
    this.#unseenStartup.clear();
    this.#owed.clear();
  }

  /**
   * How long from now the start-turn mark holds the startup wake back, which is when the check
   * reads it again: undefined when it does not hold (no mark, a mark naming another session, or one
   * past its cap), the time to its cap once its start was sent, and while it is unsent the time to
   * its cap from this module's first read of it, but no more than `STARTUP_TURN_POLL_MS`, so a
   * mark a refused claim cleared, or one the send replaced, is read again soon.
   */
  #startTurnHold(): number | undefined {
    const mark = this.#deps.startupTurn();
    if (mark === undefined || !this.#deps.isOwnSession(mark.session)) return undefined;
    const now = this.#deps.now();
    if (mark.sentAt !== undefined) {
      const left = mark.sentAt + STARTUP_TURN_CAP_MS - now;
      return left > 0 ? left : undefined;
    }
    if (this.#unsentMark?.mark !== mark) this.#unsentMark = { mark, firstRead: now };
    const left = this.#unsentMark.firstRead + STARTUP_TURN_CAP_MS - now;
    return left > 0 ? Math.min(left, STARTUP_TURN_POLL_MS) : undefined;
  }

  #arm(context: CardWakeContext, delayMs = 0): void {
    const due = this.#deps.now() + delayMs;
    if (this.#checkDue !== undefined && this.#checkDue <= due) return;
    this.#checkGeneration += 1;
    this.#checkDue = due;
    const generation = this.#checkGeneration;
    context.setTimeout(() => {
      if (generation !== this.#checkGeneration) return;
      this.#checkDue = undefined;
      this.#check(context);
    }, delayMs);
  }

  #check(context: CardWakeContext): void {
    // The soonest an unseen startup card stops holding the check back, if one still does.
    let waitMs: number | undefined;
    const now = this.#deps.now();
    for (const [id, sentAt] of this.#unseenStartup) {
      const left = sentAt + STARTUP_CARD_CAP_MS - now;
      if (left <= 0) this.#unseenStartup.delete(id);
      else waitMs = Math.min(waitMs ?? left, left);
    }
    if (waitMs !== undefined) {
      this.#arm(context, waitMs);
      return;
    }
    if (context.isIdle?.() !== true) {
      // A host busy at the check runs a turn, which reads the cards, or settles an interrupt no
      // extension saw (an Esc while it set a prompt up), whose folded cards wait for the next
      // turn. A startup card stays for the run's `agent_end` to check again.
      this.#seen = this.#seen.filter((card) => card.startup);
      return;
    }
    // An idle host has no run, whatever end of it no extension was told of.
    this.#runActive = false;
    // The start turn is still to come: the seen startup cards wait for it, owed their wake after
    // it, and the phase goes on.
    const holdMs = this.#startTurnHold();
    if (holdMs !== undefined) {
      for (const card of this.#seen) {
        if (card.startup) this.#owed.add(card.id);
      }
      if (this.#seen.length > 0) this.#arm(context, holdMs);
      return;
    }
    // A card fed to a run that has since started is that run's to read, and one an interrupt
    // found stays folded; a startup card is neither's, except that an owed one stays folded under
    // an interrupt too, which stopped the start turn it waited for. The branch decides which are
    // unread, an owed card's start-turn replies not counting as its reading.
    const pending = new Set(
      this.#seen
        .filter((card) =>
          card.startup
            ? !(this.#interrupted && this.#owed.has(card.id))
            : !this.#interrupted && card.run === this.#run
        )
        .map((card) => card.id)
    );
    const unread =
      pending.size === 0
        ? 0
        : unreadCards(context.sessionManager.getBranch?.() ?? [], pending, this.#owed);
    this.#seen = [];
    this.#owed.clear();
    this.#startupPhase = false;
    if (unread === 0) return;
    // Worded for a session that has had no turn yet too: a startup card's wake may be its first.
    this.#deps.sendMessage(
      {
        customType: "envoy-message",
        content:
          unread === 1
            ? "1 Envoy message above arrived with no turn to read it: read it and act on it."
            : `${unread} Envoy messages above arrived with no turn to read them: read them and act on them.`,
        display: false,
        details: LOCAL_ENVOY_NOTICE,
      },
      { deliverAs: "nextTurn", triggerTurn: true }
    );
  }
}
