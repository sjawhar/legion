import type { DedupeIdentity, DeliveryClaim } from "@legion/envoy-client/delivery";
import { replayLedgerIdentity } from "@legion/envoy-client/replay";
import type { ContentBlock } from "@legion/pi-shared/pi-types";
import { writtenCardIdentity } from "./missed-messages";

/**
 * How a card is handed to the host: as a steer or an aside, each starting a turn when the host is
 * idle, or `inert`, a steer that starts no turn (`triggerTurn: false`). An idle host writes an
 * inert card at once and runs nothing for it; one handed while a turn streams is steered into
 * that turn, and the agent loop runs another model turn for it (measured on omp 18.12.2). So an
 * inert card is handed only to a settled host (`UnwrittenFramesHost.settled`): one sent while the
 * host is busy is held until the first settled heartbeat (`handBackWithdrawn`), as is a replay
 * notice (`sendNotice`), which is handed the same way. A card handed again keeps its delivery.
 */
export type CardDelivery = "steer" | "aside" | "inert";

/**
 * A frame handed to the host that the host has not written to the session's transcript yet. Oh
 * My Pi holds a card or a user turn sent while a turn streams in memory until the turn takes it,
 * and writes it, emitting its `message_end`, only then (measured on omp 18.8.9, idle and mid-turn
 * alike); a process that ends first loses it. Until then the frame's claim stays pending, so a
 * replay sweep leaves the stream's copy and its cursor before it, and nothing records it shown.
 */
interface Unwritten {
  /** The session it was handed to: written once the host is on another, it records nothing. */
  readonly sessionID: string;
  readonly identity: DedupeIdentity;
  /**
   * The frame's claim, settled once the host writes it. None for a replayed card of a frame the
   * dedupe had already let go of (`showReplayCard` in extensions/envoy.ts): that card still waits
   * to be written.
   */
  readonly claim: DeliveryClaim | undefined;
}

/**
 * A card waiting to be written, under every key `unwrittenCardKeys` gives its frame: one handed to
 * the host, or an inert card held until the host is settled.
 */
interface UnwrittenCard extends Unwritten {
  readonly keys: readonly string[];
  /** What it is handed as: a card the host withdrew unwritten is handed again as it was. */
  readonly content: string | readonly ContentBlock[];
  readonly deliverAs: CardDelivery;
  /**
   * The host moments counted when it was handed (`handBackWithdrawn`); undefined for an inert
   * card held, never handed, which the next settled moment hands.
   */
  readonly handedAt: number | undefined;
}

/** A person's turn waiting to be written; a `pi.sendUserMessage` turn is found by its text. */
interface UnwrittenTurn extends Unwritten {
  readonly via: "text" | "typed";
  /**
   * The host moments counted when the host took it into its queue (`handBackWithdrawn`): a
   * `pi.sendUserMessage` turn's at the send, a typed input's once `pi.sendUserInput` answers that
   * it submitted a message; undefined until then.
   */
  readonly handedAt: number | undefined;
}

/** A person's turn as `UnwrittenFrames.awaitTurn` takes it. */
export interface HandedTurn {
  /** The session the turn was handed to. */
  readonly sessionID: string;
  /** The frame the turn carries; none for a frame with no identity, which nothing waits on. */
  readonly identity: DedupeIdentity | undefined;
  readonly claim: DeliveryClaim | undefined;
  /** `text`: sent with `pi.sendUserMessage`, in the host's queue from the send. `typed`: sent with
   *  `pi.sendUserInput`, in the queue only once the host answers that it submitted a message. */
  readonly via: "text" | "typed";
}

/** What `UnwrittenFrames` asks of the extension around it. */
export interface UnwrittenFramesHost {
  /** The session this instance serves now: a frame written once it is on another records nothing. */
  sessionID(): string;
  /**
   * Whether the host is settled: idle, holding no message, not shutting down, and on the session
   * this instance serves. Only a settled host is handed an inert card or a replay notice, or
   * handed back what an interrupt withdrew.
   */
  settled(): boolean;
  /** Records a frame the host wrote in the replay ledger (`ReplayEngine.recordDelivered`). */
  recordDelivered(identity: DedupeIdentity): Promise<void>;
  /** Hands the host a card (`pi.sendMessage`); a throw means it was not handed. */
  handCard(
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined
  ): void;
  /** Hands the host a replay notice (`pi.sendMessage` of a card naming `LOCAL_ENVOY_NOTICE`, a
   *  steer that starts no turn); a throw means it was not handed. */
  handNotice(content: string): void;
  /** The Dispatch message id of the person's turn `message` is, among the turns noted for this
   *  session (`matchInjectedUserTurn`), if any. */
  matchTurn(message: unknown): string | undefined;
  /** The interrupt withdrew the person's turn `messageID` handed to `sessionID`: no message will
   *  carry it, so whatever was noted to find it, or to go with it, goes. */
  turnWithdrawn(sessionID: string, messageID: string): void;
  /** A card the host withdrew unwritten, or an inert card held until the host settled, could not
   *  be handed; it keeps waiting, and the next settled moment tries again. */
  handBackFailed(identity: DedupeIdentity, error: unknown): void;
  /** A replay notice could not be handed; it stays held, and the next settled moment tries again. */
  handNoticeFailed(error: unknown): void;
}

/**
 * The keys a card waiting to be written is found under, from the frame it shows: its event id and
 * its dedupe key, as the ledger knows it (`replayLedgerIdentity`), so the written card names it
 * by either. None for a frame the ledger keeps nothing of.
 */
export function unwrittenCardKeys(identity: {
  readonly event_id?: string;
  readonly dedupe_key?: string;
}): string[] {
  return [
    ...(identity.event_id === undefined ? [] : [`event:${identity.event_id}`]),
    ...(identity.dedupe_key === undefined ? [] : [`key:${identity.dedupe_key}`]),
  ];
}

/**
 * The cards (`unwrittenCardKeys`) and the person's turns (by Dispatch message id) handed to the
 * host and not yet written (`Unwritten`), and the inert cards held until the host is settled,
 * each under the claim of its frame. This owns each claim from the hand-off on: the claim
 * settles, and the replay ledger records the frame, once the host writes it (`written`,
 * `turnHandled`, `textTurnsEnded`), or at once for a frame nothing waits on (`sendCard`,
 * `awaitTurn`); it is released, recording nothing, when the host refused the card, or an inert
 * card held had nothing to wait under (`sendCard`), or the host dropped the frame unwritten (a
 * session change, `releaseOtherSessions`; a person's turn an interrupt withdrew,
 * `handBackWithdrawn`), so a sweep shows the stream's copy.
 *
 * It also holds the replay notices (`sendNotice`) sent while the host is not settled, in order,
 * until the first settled moment hands them. A notice names no frame and has no claim: nothing
 * waits on its write, and nothing records it.
 */
export class UnwrittenFrames {
  readonly #host: UnwrittenFramesHost;
  readonly #cards = new Map<string, UnwrittenCard>();
  readonly #turns = new Map<string, UnwrittenTurn>();
  // The replay notices not handed yet, in the order they were sent, each with the session it was
  // raised for.
  #notices: { readonly sessionID: string; readonly content: string }[] = [];
  // The moments the host had to queue or write a card handed before it: each heartbeat, and each
  // run that ended aborted (`handBackWithdrawn`, `runAborted`).
  #moments = 0;

  constructor(host: UnwrittenFramesHost) {
    this.#host = host;
  }

  /**
   * Hands the host a card for the frame `identity` under `claim`: one that shows it, or a failure
   * notice naming it. `queued`: the card is shown only once the host writes it (`written`), and
   * the claim, when there is one, waits until then. `shown`: a card of a frame the ledger keeps
   * nothing of, which nothing waits on, so its claim settles at once. A send that throws leaves
   * nothing waiting and releases the claim, so a re-send or a sweep shows the frame again.
   *
   * An inert card sent while the host is not settled is not handed: it waits, `queued`, and the
   * first settled moment hands it (`handBackWithdrawn`). One whose frame the ledger keeps nothing
   * of has nothing to wait under, so its claim is released and a sweep shows the frame again.
   */
  sendCard(
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined,
    claim: DeliveryClaim | undefined
  ): "queued" | "shown" {
    const hand = deliverAs !== "inert" || this.#host.settled();
    const keys = identity === undefined ? [] : unwrittenCardKeys(replayLedgerIdentity(identity));
    const card: UnwrittenCard | undefined =
      identity === undefined || keys.length === 0
        ? undefined
        : {
            sessionID: this.#host.sessionID(),
            identity,
            claim,
            keys,
            content,
            deliverAs,
            handedAt: hand ? this.#moments : undefined,
          };
    if (card !== undefined) this.#awaitWrite(card);
    if (!hand) {
      if (card === undefined) claim?.release();
      return "queued";
    }
    try {
      this.#host.handCard(content, deliverAs, identity);
    } catch (error) {
      if (card !== undefined) this.#stopAwaitingWrite(card);
      claim?.release();
      throw error;
    }
    if (card !== undefined) return "queued";
    // The ledger keeps nothing of the frame, so there is nothing to record.
    claim?.settle();
    return "shown";
  }

  /** Whether a card of the frame `identity` waits to be written: that card shows it. */
  waitsOnCard(identity: DedupeIdentity): boolean {
    return unwrittenCardKeys(replayLedgerIdentity(identity)).some((key) => this.#cards.has(key));
  }

  /**
   * Hands the host a replay notice (`stream_recreated`, `retention_gap`) for the session this
   * instance serves now, at once when the host is settled; otherwise it is held, after any held
   * before it, until the first settled moment (`handBackWithdrawn`). A notice the host cannot be
   * handed is held too (`handNoticeFailed`). Nothing waits on a notice's write.
   */
  sendNotice(content: string): void {
    this.#notices.push({ sessionID: this.#host.sessionID(), content });
    if (this.#host.settled()) this.#handNotices();
  }

  /**
   * A person's turn `messageID` was handed to the host, which writes it as a user message. A turn
   * of a frame with no identity gives the ledger nothing to record, so nothing waits on its write:
   * its claim settles at once.
   */
  awaitTurn(messageID: string, turn: HandedTurn): void {
    const { identity } = turn;
    if (identity === undefined) {
      turn.claim?.settle();
      return;
    }
    this.#turns.set(messageID, {
      ...turn,
      identity,
      handedAt: turn.via === "text" ? this.#moments : undefined,
    });
  }

  /** The host answered that typed input `messageID` submitted a message: it is in the host's
   *  queue from now on, unless its message was written already. */
  turnSubmitted(messageID: string): void {
    const waiting = this.#turns.get(messageID);
    if (waiting !== undefined) this.#turns.set(messageID, { ...waiting, handedAt: this.#moments });
  }

  /** A person's turn the host wrote, or answered that it submitted no message for: done waiting. */
  async turnHandled(messageID: string): Promise<void> {
    const turn = this.#turns.get(messageID);
    if (turn === undefined) return;
    this.#turns.delete(messageID);
    await this.#recordWritten(turn);
  }

  /**
   * The host wrote `message` to the transcript as it ended it (`message_end`): a card or a
   * person's turn handed to it, idle or taken by a turn after it streamed, is shown from here on.
   * A card is found by the frame its `details` or text name (`writtenCardIdentity`), a turn by
   * the host's `matchTurn`.
   */
  async written(message: unknown): Promise<void> {
    const card = writtenCardIdentity(message);
    const waiting = unwrittenCardKeys(card ?? {})
      .map((key) => this.#cards.get(key))
      .find((found) => found !== undefined);
    if (waiting !== undefined) {
      this.#stopAwaitingWrite(waiting);
      await this.#recordWritten(waiting);
    }
    if (this.#turns.size === 0) return;
    const turn = this.#host.matchTurn(message);
    if (turn !== undefined) await this.turnHandled(turn);
  }

  /** A run ended aborted: it may have taken back the cards it held, and those handed before its
   *  end are looked at by the next `handBackWithdrawn`. */
  runAborted(): void {
    this.#moments += 1;
  }

  /**
   * A run's end stopped the search for this session's `pi.sendUserMessage` turns: each still
   * waiting is done with now, found or not. The host keeps no mark of such a turn, so its text was
   * the only way to tell it was written.
   */
  async textTurnsEnded(): Promise<void> {
    for (const [messageID, turn] of this.#turns) {
      if (turn.via === "text" && turn.sessionID === this.#host.sessionID()) {
        await this.turnHandled(messageID);
      }
    }
  }

  /**
   * Counts a host moment (each heartbeat tick) and, when the host is settled
   * (`UnwrittenFramesHost.settled`), hands the inert cards and the replay notices held while the
   * host was busy (`sendCard`, `sendNotice`) and hands back what an interrupt withdrew.
   *
   * Oh My Pi withdraws every card a streaming turn holds, writing none and restoring none, when
   * the person interrupts the turn (Escape; RPC `abort_and_restore_queue`), and is idle with an
   * empty queue from then on; after a plain abort it keeps them queued and writes them in the run
   * it starts at once (measured on omp 18.8.9: `hasPendingMessages()` false, then true, at the
   * aborted run's `agent_end`). So a card still unwritten while the host is idle and holds no
   * message was withdrawn: it is handed again, as it was, its claim kept. One handed since the
   * host's last moment is left alone, as it may still be on its way into the queue:
   * `pi.sendMessage` returns before the host queues the card or starts its run. A held inert card
   * was never handed, so any settled moment hands it, and so is each held notice, once. A card or
   * a notice that cannot be handed keeps waiting as it was (`handBackFailed`, `handNoticeFailed`),
   * and the next settled moment tries again.
   *
   * The interrupt withdraws a person's turn the same way: a typed input (`pi.sendUserInput`,
   * answered `handled: "prompt"`) or a `pi.sendUserMessage` turn the streaming turn held goes back
   * to whoever interrupted (measured on omp 18.8.9: RPC `abort_and_restore_queue` answers it in
   * its `steering` list, the terminal puts it in the editor) and is never written. Such a turn is
   * not handed again, since run late it could act on an instruction the person no longer means:
   * its claim is released and nothing records it, so the next sweep shows the stream's copy as a
   * card that runs nothing. That holds for a typed input only. A `pi.sendUserMessage` turn (sent
   * only on a host without `pi.sendUserInput`) never gets here: the aborted run's `agent_end` calls
   * `textTurnsEnded` right after `runAborted` (extensions/envoy.ts), which records the turn as
   * shown, since the host keeps no mark that would tell a withdrawn one from a written one: no
   * sweep shows the stream's copy of such a turn, which only the person who interrupted got back.
   */
  handBackWithdrawn(): void {
    const since = this.#moments;
    this.#moments += 1;
    if (!this.#host.settled()) return;
    this.#handNotices();
    const sessionID = this.#host.sessionID();
    for (const card of new Set(this.#cards.values())) {
      if (card.sessionID !== sessionID) continue;
      if (card.handedAt !== undefined && card.handedAt >= since) continue;
      const again: UnwrittenCard = { ...card, handedAt: this.#moments };
      this.#awaitWrite(again);
      try {
        this.#host.handCard(card.content, card.deliverAs, card.identity);
      } catch (error) {
        // Still waiting as it was: the next settled moment hands it.
        this.#stopAwaitingWrite(again);
        this.#awaitWrite(card);
        this.#host.handBackFailed(card.identity, error);
      }
    }
    for (const [messageID, turn] of this.#turns) {
      if (turn.sessionID !== sessionID || turn.handedAt === undefined || turn.handedAt >= since) {
        continue;
      }
      this.#turns.delete(messageID);
      this.#host.turnWithdrawn(turn.sessionID, messageID);
      turn.claim?.release();
    }
  }

  /**
   * The host moved to `sessionID`: what the session it left was handed and has not written is no
   * longer waited on, since the host dropped it with the session (measured on omp 18.8.9: a card a
   * turn held when RPC `new_session` ran got no `message_end`). Its claim is released and nothing
   * records it, so a sweep of that session, once the host is back on it or resumes it, shows the
   * stream's copy again and waits on it anew. A notice held for the session it left is dropped:
   * it concerns only the session it was raised for.
   */
  releaseOtherSessions(sessionID: string): void {
    for (const waiting of [this.#cards, this.#turns]) {
      for (const [key, frame] of waiting) {
        if (frame.sessionID === sessionID) continue;
        waiting.delete(key);
        frame.claim?.release();
      }
    }
    this.#notices = this.#notices.filter((notice) => notice.sessionID === sessionID);
  }

  // Hands the held notices of the session this instance serves, in order. One the host cannot be
  // handed stays held, with every notice after it, so the next settled moment keeps their order.
  #handNotices(): void {
    const sessionID = this.#host.sessionID();
    for (let index = 0; index < this.#notices.length; ) {
      const notice = this.#notices[index];
      if (notice === undefined || notice.sessionID !== sessionID) {
        index += 1;
        continue;
      }
      try {
        this.#host.handNotice(notice.content);
      } catch (error) {
        this.#host.handNoticeFailed(error);
        return;
      }
      this.#notices.splice(index, 1);
    }
  }

  // The host wrote what it was handed: the ledger records the frame, so no sweep shows the
  // stream's copy again, here or after a restart, and its claim settles. Written once the host is
  // on another session, it is recorded nowhere: that session's own sweep reads its transcript.
  async #recordWritten(frame: Unwritten): Promise<void> {
    if (frame.sessionID === this.#host.sessionID()) {
      await this.#host.recordDelivered(frame.identity);
    }
    frame.claim?.settle();
  }

  // A card waits under every key of its frame, and stops waiting only as the entry it is.
  #awaitWrite(card: UnwrittenCard): void {
    for (const key of card.keys) this.#cards.set(key, card);
  }

  #stopAwaitingWrite(card: UnwrittenCard): void {
    for (const key of card.keys) {
      if (this.#cards.get(key) === card) this.#cards.delete(key);
    }
  }
}
