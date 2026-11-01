import { messageTag } from "@legion/pi-shared/injected-user-turns";
import type { ImageContent } from "@legion/pi-shared/pi-types";

/** One typed turn's pictures, as `TypedPictures.admit` takes them. */
export interface TypedPictureTurn {
  /** The text the turn was sent with, which is all the host's `input` event names it by. */
  readonly body: string;
  /** The session the turn was sent into. */
  readonly sessionID: string;
  /** The `dispatch://` addresses of the pictures, which count as shown once its message starts. */
  readonly addresses: readonly string[];
  readonly images: readonly ImageContent[];
}

interface Admission {
  readonly body: string;
  readonly sessionID: string;
  readonly addresses: readonly string[];
  /** Until pi-envoy's `input` handler hands them over, then none: the host holds them. */
  images: readonly ImageContent[] | undefined;
}

/**
 * A person's typed turns' pictures, by Dispatch message id, from the send until the host's
 * message carrying that id as its `tag` starts (`pi.sendUserInput` takes text alone, and the host
 * asks the extension's `input` handler for that input's images). The event names its input by
 * text alone, so a turn is handed its pictures in send order among the turns sent with the same
 * text: the map's own order. Only a turn that was handed them counts them shown when its message
 * starts; one whose text an earlier extension's handler rewrote goes without its pictures, which
 * stay unshown so that a later delivery carries them. Dispatch accepts a message once, so a turn
 * is admitted at most once.
 */
export class TypedPictures {
  readonly #admissions = new Map<string, Admission>();

  /** Holds turn `messageId`'s pictures for the host's `input` handlers, behind every other turn. */
  admit(messageId: string, turn: TypedPictureTurn): void {
    this.#admissions.set(messageId, {
      addresses: turn.addresses,
      body: turn.body,
      images: turn.images,
      sessionID: turn.sessionID,
    });
  }

  /** The images of the first turn sent with `text` and not yet handed its pictures, which are
   *  handed over now; undefined when no such turn waits. */
  handOver(text: string): readonly ImageContent[] | undefined {
    for (const admission of this.#admissions.values()) {
      if (admission.images === undefined || admission.body !== text) continue;
      const { images } = admission;
      admission.images = undefined;
      return images;
    }
    return undefined;
  }

  /** Turn `messageId` submitted no message: no message will carry its pictures. */
  withdraw(messageId: string): void {
    this.#admissions.delete(messageId);
  }

  /** The host submitted turn `messageId` after running every `input` handler. A turn still
   *  holding its pictures then had its text rewritten ahead of pi-envoy's handler and will never
   *  be handed them, so it goes; a handed one waits for its message to start. */
  inputPassed(messageId: string): void {
    if (this.#admissions.get(messageId)?.images !== undefined) this.#admissions.delete(messageId);
  }

  /** The host's `message` started. When it carries a turn's tag, that turn is done; the addresses
   *  it showed are returned when it had been handed its pictures. The message starts in the
   *  session live now, whose set they join. */
  started(message: unknown): readonly string[] | undefined {
    const tag = messageTag(message);
    if (tag === undefined) return undefined;
    const admission = this.#admissions.get(tag);
    if (admission === undefined) return undefined;
    this.#admissions.delete(tag);
    return admission.images === undefined ? admission.addresses : undefined;
  }

  /**
   * The session is now `sessionID`: another session's turns already handed their pictures go. A
   * switch, `/new` or resume discards the queue of the session left, so such a turn's message never
   * starts; a branch or a session file's move-off keeps the queue, and a handed turn that starts
   * under the new id then counts nothing shown, so the next delivery naming its pictures sends
   * them once more. A turn still holding its pictures stays: its `sendUserInput` has not reached
   * pi-envoy's `input` handler yet, and will hand them over, withdraw the turn, or see it start.
   */
  dropOtherSessions(sessionID: string): void {
    for (const [messageId, admission] of this.#admissions) {
      if (admission.sessionID !== sessionID && admission.images === undefined) {
        this.#admissions.delete(messageId);
      }
    }
  }
}
