import { newSendKey } from "../../lib/send-key";

/** How long a sign-out waits for the other tabs to say what of the person's it would discard
 *  there before it asks the person (`SignInChannel.unsentElsewhere`). */
export const OTHER_TABS_ANSWER_MS = 300;

/** What a tab does with what the other tabs of its browser say (`SignInChannel.listen`). */
export interface SignInListener {
  /** Who is signed in may have changed in another tab: read it again. */
  readonly changed: () => void;
  /** `login` confirmed a sign-out in another tab, which is out: hold their writes until it ends. */
  readonly signingOut: (login: string) => void;
  /** Dispatch signed `login` out in another tab: their writes go. */
  readonly signedOut: (login: string) => void;
  /** `login`'s sign-out in another tab did not go through: their writes go on. */
  readonly stillSignedIn: (login: string) => void;
  /** What of `login`'s a sign-out would discard in this tab, by name. */
  readonly unsent: (login: string) => readonly string[];
}

/** What the other tabs said a sign-out would discard there, and whether a tab this one knows of
 *  did not answer in time. */
export interface UnsentElsewhere {
  readonly labels: readonly string[];
  readonly unanswered: boolean;
}

/** `data`'s `field`, when it is a string. */
function stringField(data: unknown, field: string): string | undefined {
  if (typeof data !== "object" || data === null) return undefined;
  const value: unknown = Reflect.get(data, field);
  return typeof value === "string" ? value : undefined;
}

/**
 * Where the tabs of one browser tell each other who is signed in and what a sign-out does to the
 * person's writes (`BroadcastChannel` `dispatch-sign-in`): the session cookie is the browser's, so
 * a sign-in, a sign-out or a lapse in one tab is every tab's. Its messages:
 *
 * - a login, or `null` for no one: who is signed in in the tab that says it; every other tab
 *   reads who is signed in again.
 * - `{ signingOut: login }`: the person confirmed a sign-out there, which is out; every tab holds
 *   their writes, so nothing they chose to discard goes out meanwhile (`Writes.holdFor`).
 * - `{ signedOut: login }`: Dispatch signed them out; every tab drops their writes
 *   (`Writes.dropLogin`) and reads who is signed in again.
 * - `{ stillSignedIn: login }`: the sign-out did not go through, or the tab that asked for it went
 *   before it was answered; every tab lets their writes go on (`Writes.release`).
 * - `{ ask, unsentOf: login }`: what of theirs a sign-out would discard; every tab answers
 *   `{ answer: ask, from: tab, unsent }` with the names of those writes.
 * - `{ tab, hello: true }` as a tab opens or comes back from the back/forward cache (`pageshow`),
 *   answered `{ tab, here: true }` by every other tab, and `{ tab, gone: true }` as it goes
 *   (`pagehide`): so a tab that asks knows which tabs owe it an answer.
 */
export class SignInChannel {
  readonly #channel: BroadcastChannel | undefined;
  readonly #tab = newSendKey();
  /** The other tabs of this browser this one has heard from and not seen go. */
  readonly #others = new Set<string>();
  #listener: SignInListener | undefined;
  /** Each sign-out's question still waiting for answers, by its id. */
  readonly #asks = new Map<string, (tab: string, unsent: readonly string[]) => void>();

  /** `page` is where this tab goes and comes back: its window. */
  constructor(channel: BroadcastChannel | undefined, page: EventTarget | undefined) {
    this.#channel = channel;
    if (channel === undefined) return;
    channel.addEventListener("message", (event: MessageEvent) => this.#heard(event.data));
    page?.addEventListener("pagehide", () => this.#post({ gone: true, tab: this.#tab }));
    page?.addEventListener("pageshow", (event) => {
      if ((event as PageTransitionEvent).persisted) this.#post({ hello: true, tab: this.#tab });
    });
    this.#post({ hello: true, tab: this.#tab });
  }

  /** Says who is signed in here now: `login`, or no one. */
  signedIn(login: string | null): void {
    this.#post(login);
  }

  /** Says `login` confirmed a sign-out here, which is out. */
  signingOut(login: string): void {
    this.#post({ signingOut: login });
  }

  /** Says Dispatch signed `login` out here. */
  signedOut(login: string): void {
    this.#post({ signedOut: login });
  }

  /** Says `login`'s sign-out here did not go through, or went with the page. */
  stillSignedIn(login: string): void {
    this.#post({ stillSignedIn: login });
  }

  /** Asks every other tab what of `login`'s a sign-out would discard there, and answers once each
   *  tab this one knows of has answered, or `waitMs` has passed: every name they gave, and
   *  whether one of them did not answer in time. */
  unsentElsewhere(login: string, waitMs = OTHER_TABS_ANSWER_MS): Promise<UnsentElsewhere> {
    const waiting = new Set(this.#others);
    if (waiting.size === 0) return Promise.resolve({ labels: [], unanswered: false });
    const ask = newSendKey();
    const labels: string[] = [];
    const { promise, resolve } = Promise.withResolvers<UnsentElsewhere>();
    const done = () => {
      clearTimeout(timer);
      this.#asks.delete(ask);
      resolve({ labels, unanswered: waiting.size > 0 });
    };
    const timer = setTimeout(done, waitMs);
    this.#asks.set(ask, (tab, unsent) => {
      waiting.delete(tab);
      labels.push(...unsent);
      if (waiting.size === 0) done();
    });
    this.#post({ ask, unsentOf: login });
    return promise;
  }

  /** Has `listener` act on what the other tabs say, until the returned function is called. */
  listen(listener: SignInListener): () => void {
    this.#listener = listener;
    return () => {
      if (this.#listener === listener) this.#listener = undefined;
    };
  }

  #post(message: unknown): void {
    this.#channel?.postMessage(message);
  }

  #heard(data: unknown): void {
    const listener = this.#listener;
    if (typeof data === "string" || data === null) {
      listener?.changed();
      return;
    }
    const tab = stringField(data, "tab");
    if (tab !== undefined) {
      if (Reflect.get(data as object, "gone") === true) {
        this.#others.delete(tab);
        return;
      }
      this.#others.add(tab);
      // A tab opening learns of this one from its answer.
      if (Reflect.get(data as object, "hello") === true) this.#post({ here: true, tab: this.#tab });
      return;
    }
    const answer = stringField(data, "answer");
    const from = stringField(data, "from");
    if (answer !== undefined && from !== undefined) {
      this.#others.add(from);
      const unsent: unknown = Reflect.get(data as object, "unsent");
      const names = Array.isArray(unsent)
        ? unsent.filter((name): name is string => typeof name === "string")
        : [];
      this.#asks.get(answer)?.(from, names);
      return;
    }
    const ask = stringField(data, "ask");
    const unsentOf = stringField(data, "unsentOf");
    if (ask !== undefined && unsentOf !== undefined) {
      if (listener !== undefined) {
        this.#post({ answer: ask, from: this.#tab, unsent: [...listener.unsent(unsentOf)] });
      }
      return;
    }
    const signingOut = stringField(data, "signingOut");
    if (signingOut !== undefined) listener?.signingOut(signingOut);
    const signedOut = stringField(data, "signedOut");
    if (signedOut !== undefined) {
      listener?.signedOut(signedOut);
      listener?.changed();
    }
    const stillSignedIn = stringField(data, "stillSignedIn");
    if (stillSignedIn !== undefined) listener?.stillSignedIn(stillSignedIn);
  }
}
