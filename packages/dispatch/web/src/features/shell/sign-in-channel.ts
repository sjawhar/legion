import { newSendKey } from "../../lib/send-key";

/** How long a sign-out waits for the other tabs to say what of the person's it would discard
 *  there before it asks the person (`SignInChannel.unsentElsewhere`). */
export const OTHER_TABS_ANSWER_MS = 300;

/** How often a tab whose sign-out is out says so again (`SignInChannel.signingOut`), so every
 *  other tab holding the person's writes for it knows it is still out. */
export const SIGN_OUT_RENEW_MS = 2_000;

/** How long a tab holds a person's writes for another tab's sign-out it has not heard say so
 *  again: past it, that tab is taken for gone without a word - crashed, killed, frozen or
 *  discarded - and the writes go on. Three renewals, so one late timer in a busy tab lifts
 *  nothing. */
export const SIGN_OUT_LEASE_MS = 6_000;

/** One write a sign-out would discard: its key, which a tab duplicated from another shares with
 *  it, and its name. */
export interface UnsentChange {
  readonly key: string;
  readonly label: string;
}

/** What a tab does with what the other tabs of its browser say (`SignInChannel.listen`). */
export interface SignInListener {
  /** Who is signed in may have changed in another tab: read it again. */
  readonly changed: () => void;
  /** `login` confirmed a sign-out in another tab, which is out: hold their writes until it ends. */
  readonly signingOut: (login: string) => void;
  /** Dispatch signed `login` out in another tab: their writes go. */
  readonly signedOut: (login: string) => void;
  /** No other tab's sign-out of `login` holds their writes any longer - it did not go through,
   *  its tab went, or its tab stopped saying it is out: their writes go on. */
  readonly stillSignedIn: (login: string) => void;
  /** What of `login`'s a sign-out would discard in this tab. */
  readonly unsent: (login: string) => readonly UnsentChange[];
}

/** What the other tabs said a sign-out would discard there, and whether a tab this one knows of
 *  did not answer in time. */
export interface UnsentElsewhere {
  readonly unsent: readonly UnsentChange[];
  readonly unanswered: boolean;
}

/** `data`'s `field`, when it is a string. */
function stringField(data: unknown, field: string): string | undefined {
  if (typeof data !== "object" || data === null) return undefined;
  const value: unknown = Reflect.get(data, field);
  return typeof value === "string" ? value : undefined;
}

/** The changes an answer names: each `{ key, label }` of its `unsent`, and nothing else. */
function unsentIn(data: object): UnsentChange[] {
  const unsent: unknown = Reflect.get(data, "unsent");
  if (!Array.isArray(unsent)) return [];
  return unsent.flatMap((change: unknown) => {
    const key = stringField(change, "key");
    const label = stringField(change, "label");
    return key === undefined || label === undefined ? [] : [{ key, label }];
  });
}

/** Another tab's sign-out that holds a person's writes here: whose, and when the hold lapses
 *  unless that tab says so again. */
interface Hold {
  readonly login: string;
  readonly lapse: ReturnType<typeof setTimeout>;
}

/**
 * Where the tabs of one browser tell each other who is signed in and what a sign-out does to the
 * person's writes (`BroadcastChannel` `dispatch-sign-in`): the session cookie is the browser's, so
 * a sign-in, a sign-out or a lapse in one tab is every tab's. Every message but the first names
 * the tab that says it (`tab`), and a tab that speaks is one the others know of. Its messages:
 *
 * - a login, or `null` for no one: who is signed in in the tab that says it; every other tab
 *   reads who is signed in again.
 * - `{ signingOut: login, tab }`: the person confirmed a sign-out there, which is out, said again
 *   every `SIGN_OUT_RENEW_MS` while it is. Every tab holds their writes for it, so nothing they
 *   chose to discard goes out meanwhile (`Writes.holdFor`): a hold is that tab's, a lease it lets
 *   lapse after `SIGN_OUT_LEASE_MS` unheard, so a tab that dies with its sign-out out holds
 *   nothing for good.
 * - `{ signedOut: login, tab }`: Dispatch signed them out; every tab drops their writes
 *   (`Writes.dropLogin`) and reads who is signed in again.
 * - `{ stillSignedIn: login, tab }`: the sign-out did not go through, or the tab that asked for it
 *   went before it was answered; that tab's hold lifts, and once none holds them every tab lets
 *   their writes go on (`Writes.release`).
 * - `{ ask, unsentOf: login, tab }`: what of theirs a sign-out would discard; every tab answers
 *   `{ answer: ask, tab, unsent }` with those writes, each by key and name.
 * - `{ tab, hello: true }` as a tab opens or comes back from the back/forward cache (`pageshow`),
 *   answered `{ tab, here: true }` by every other tab, and `{ tab, gone: true }` as it goes
 *   (`pagehide`), which lifts its hold too: so a tab that asks knows which tabs owe it an answer.
 *   One that owes an answer and gives none in time is taken for gone until it speaks again.
 */
export class SignInChannel {
  readonly #channel: BroadcastChannel | undefined;
  readonly #tab = newSendKey();
  /** The other tabs of this browser this one has heard from and not seen go, or miss an ask. */
  readonly #others = new Set<string>();
  #listener: SignInListener | undefined;
  /** Each sign-out's question still waiting for answers, by its id. */
  readonly #asks = new Map<string, (tab: string, unsent: readonly UnsentChange[]) => void>();
  /** The other tabs whose sign-out holds a person's writes here, by tab. */
  readonly #holds = new Map<string, Hold>();
  /** This tab's sign-out saying again that it is out, while it is. */
  #renewal: ReturnType<typeof setInterval> | undefined;

  /** `page` is where this tab goes and comes back: its window. */
  constructor(channel: BroadcastChannel | undefined, page: EventTarget | undefined) {
    this.#channel = channel;
    if (channel === undefined) return;
    channel.addEventListener("message", (event: MessageEvent) => this.#heard(event.data));
    // A tab that goes says so, which lifts its hold in every other tab, and renews nothing after:
    // one the back/forward cache brings back holds no other tab's writes again.
    page?.addEventListener("pagehide", () => {
      clearInterval(this.#renewal);
      this.#post({ gone: true, tab: this.#tab });
    });
    page?.addEventListener("pageshow", (event) => {
      if ((event as PageTransitionEvent).persisted) this.#post({ hello: true, tab: this.#tab });
    });
    this.#post({ hello: true, tab: this.#tab });
  }

  /** Says who is signed in here now: `login`, or no one. */
  signedIn(login: string | null): void {
    this.#post(login);
  }

  /** Says `login` confirmed a sign-out here, which is out, and says it again every
   *  `SIGN_OUT_RENEW_MS` until this tab says how it ended (`signedOut`, `stillSignedIn`). */
  signingOut(login: string): void {
    clearInterval(this.#renewal);
    const say = () => this.#post({ signingOut: login, tab: this.#tab });
    say();
    this.#renewal = setInterval(say, SIGN_OUT_RENEW_MS);
  }

  /** Says Dispatch signed `login` out here. */
  signedOut(login: string): void {
    clearInterval(this.#renewal);
    this.#post({ signedOut: login, tab: this.#tab });
  }

  /** Says `login`'s sign-out here did not go through, or went with the page. */
  stillSignedIn(login: string): void {
    clearInterval(this.#renewal);
    this.#post({ stillSignedIn: login, tab: this.#tab });
  }

  /** Asks every other tab what of `login`'s a sign-out would discard there, and answers once each
   *  tab this one knows of has answered, or `waitMs` has passed: every change they named, and
   *  whether one of them did not answer in time. A tab that did not is taken for gone - a tab
   *  closed without a word stays on no list - until it next speaks. */
  unsentElsewhere(login: string, waitMs = OTHER_TABS_ANSWER_MS): Promise<UnsentElsewhere> {
    const waiting = new Set(this.#others);
    if (waiting.size === 0) return Promise.resolve({ unanswered: false, unsent: [] });
    const ask = newSendKey();
    const unsent: UnsentChange[] = [];
    const { promise, resolve } = Promise.withResolvers<UnsentElsewhere>();
    const done = () => {
      clearTimeout(timer);
      this.#asks.delete(ask);
      for (const silent of waiting) this.#others.delete(silent);
      resolve({ unanswered: waiting.size > 0, unsent });
    };
    const timer = setTimeout(done, waitMs);
    this.#asks.set(ask, (tab, changes) => {
      waiting.delete(tab);
      unsent.push(...changes);
      if (waiting.size === 0) done();
    });
    this.#post({ ask, tab: this.#tab, unsentOf: login });
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

  /** Holds `login`'s writes for `tab`'s sign-out, or renews that hold, until it lapses. */
  #hold(tab: string, login: string): void {
    const before = this.#holds.get(tab);
    if (before !== undefined) clearTimeout(before.lapse);
    const held = this.#holding(login);
    this.#holds.set(tab, { lapse: setTimeout(() => this.#lift(tab), SIGN_OUT_LEASE_MS), login });
    if (before !== undefined && before.login !== login && !this.#holding(before.login)) {
      this.#listener?.stillSignedIn(before.login);
    }
    if (!held) this.#listener?.signingOut(login);
  }

  /** Lifts `tab`'s hold, if it has one: once no tab's holds the person's writes, they go on. */
  #lift(tab: string): void {
    const hold = this.#holds.get(tab);
    if (hold === undefined) return;
    clearTimeout(hold.lapse);
    this.#holds.delete(tab);
    if (!this.#holding(hold.login)) this.#listener?.stillSignedIn(hold.login);
  }

  /** Whether another tab's sign-out holds `login`'s writes here. */
  #holding(login: string): boolean {
    for (const hold of this.#holds.values()) if (hold.login === login) return true;
    return false;
  }

  #heard(data: unknown): void {
    const listener = this.#listener;
    if (typeof data === "string" || data === null) {
      listener?.changed();
      return;
    }
    if (typeof data !== "object") return;
    const tab = stringField(data, "tab");
    if (tab !== undefined) {
      if (Reflect.get(data, "gone") === true) {
        this.#others.delete(tab);
        this.#lift(tab);
        return;
      }
      // A tab that speaks is one this tab knows of, again if an ask had taken it for gone.
      this.#others.add(tab);
      // A tab opening learns of this one from its answer.
      if (Reflect.get(data, "hello") === true) this.#post({ here: true, tab: this.#tab });
    }
    const answer = stringField(data, "answer");
    if (answer !== undefined) {
      if (tab !== undefined) this.#asks.get(answer)?.(tab, unsentIn(data));
      return;
    }
    const ask = stringField(data, "ask");
    const unsentOf = stringField(data, "unsentOf");
    if (ask !== undefined && unsentOf !== undefined) {
      if (listener !== undefined) {
        this.#post({ answer: ask, tab: this.#tab, unsent: [...listener.unsent(unsentOf)] });
      }
      return;
    }
    // A tab of an older page names none, and holds under no name: its hold lapses unrenewed.
    const from = tab ?? "";
    const signingOut = stringField(data, "signingOut");
    if (signingOut !== undefined) this.#hold(from, signingOut);
    const signedOut = stringField(data, "signedOut");
    if (signedOut !== undefined) {
      for (const [holder, hold] of this.#holds) {
        if (hold.login !== signedOut) continue;
        clearTimeout(hold.lapse);
        this.#holds.delete(holder);
      }
      listener?.signedOut(signedOut);
      listener?.changed();
    }
    const stillSignedIn = stringField(data, "stillSignedIn");
    if (stillSignedIn !== undefined && this.#holds.get(from)?.login === stillSignedIn) {
      this.#lift(from);
    }
  }
}
