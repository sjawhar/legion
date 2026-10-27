import { newSendKey } from "../../lib/send-key";

/** How long a sign-out waits for the other tabs to say what of the person's it would discard
 *  there before it asks the person (`SignInChannel.unsentElsewhere`). */
export const OTHER_TABS_ANSWER_MS = 300;

/** How often a tab whose sign-out is out says so again (`SignInChannel.signingOut`), so every
 *  other tab holding the person's writes for it knows it is still out. */
export const SIGN_OUT_RENEW_MS = 2_000;

/** How long a tab holds a person's writes for another tab's sign-out it has not heard say so
 *  again, where the browser has no `navigator.locks` (the lock below is how the hold ends
 *  wherever it does): past it, that tab is taken for gone without a word - crashed, killed,
 *  frozen or discarded - and the writes go on. Three renewals, so one late timer in a busy tab
 *  lifts nothing. */
export const SIGN_OUT_LEASE_MS = 6_000;

/** The lock a tab takes for as long as its sign-out is out, named for the person it signs out
 *  and for the tab signing them out - the `tab` its `{signingOut}` carries - so each sign-out
 *  has a lock of its own and no tab ever waits on, or holds, another's. The browser releases it
 *  when that tab goes - closed, crashed or discarded - and holds it while the tab is frozen or
 *  its timers are throttled, which no lease of renewals can tell apart from a tab that died. A
 *  tab holding the person's writes waits on the lock of every signing-out tab it has heard of,
 *  and lets the writes go only once every one of those holds has ended (`holderGone`). */
export function signOutLockName(login: string, tab: string): string {
  return `dispatch-sign-out:${login}:${tab}`;
}

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
  /** No other tab's sign-out of `login` holds their writes any longer, because the tab that
   *  asked for it said how it ended: it did not go through, so their writes go on. */
  readonly stillSignedIn: (login: string) => void;
  /** The last tab whose sign-out held `login`'s writes went without saying how it ended - it
   *  closed, crashed or was discarded (`gone`, its lock released, or its lease unrenewed) - so
   *  whether the sign-out landed is unknown here: the page reads who is signed in before it
   *  either discards those writes or lets them go on. */
  readonly holderGone: (login: string) => void;
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

/** Another tab's sign-out that holds a person's writes here: whose, how this tab learns the
 *  signing-out tab is gone (its lock, where the browser has `navigator.locks`; the renewal lease
 *  otherwise), and nothing else. */
interface Hold {
  readonly login: string;
  /** The lease, where there is no lock manager: when it lapses the hold ends. */
  readonly lapse: ReturnType<typeof setTimeout> | undefined;
  /** Calls off this hold's wait for the signing-out tab's lock, when the hold ends some other
   *  way (that tab said how its sign-out ended, or it said it is gone). */
  readonly waiting: AbortController | undefined;
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
 *   chose to discard goes out meanwhile (`Writes.holdFor`): a hold is that tab's, and it ends
 *   when the browser releases that tab's own lock (`signOutLockName`, named for the person and
 *   the tab), or - where the browser has no `navigator.locks` - when its lease lapses
 *   `SIGN_OUT_LEASE_MS` after the last renewal heard. Either way a tab that dies with its
 *   sign-out out holds nothing for good, a tab the browser froze holds for as long as it lives,
 *   and two tabs signing the same person out hold the writes until both are done
 *   (`holderGone`).
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
  /** The browser's locks, where it has them (`navigator.locks`; a page outside a secure context
   *  has none, and neither does a test that passes none). */
  readonly #locks: LockManager | undefined;
  /** Lets go of the lock this tab holds while its sign-out is out (`signOutLockName`). */
  #releaseLock: (() => void) | undefined;

  /** `page` is where this tab goes and comes back: its window. `locks` is the browser's lock
   *  manager, `navigator.locks`. */
  constructor(
    channel: BroadcastChannel | undefined,
    page: EventTarget | undefined,
    locks?: LockManager
  ) {
    this.#channel = channel;
    this.#locks = locks;
    if (channel === undefined) return;
    channel.addEventListener("message", (event: MessageEvent) => this.#heard(event.data));
    // A tab that goes says so, which lifts its hold in every other tab, and renews nothing after:
    // one the back/forward cache brings back holds no other tab's writes again. It lets go of its
    // own sign-out's lock after saying it, since a page the cache keeps is not destroyed and
    // would hold the lock while it sits there.
    page?.addEventListener("pagehide", () => {
      clearInterval(this.#renewal);
      this.#post({ gone: true, tab: this.#tab });
      this.#letGoOfLock();
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
   *  `SIGN_OUT_RENEW_MS` until this tab says how it ended (`signedOut`, `stillSignedIn`). It
   *  takes the person's sign-out lock first, so a tab holding their writes waits on this tab's
   *  life rather than on its timers; the lock is taken before the first word goes out, so a
   *  holder's own request for it is always behind this one. */
  signingOut(login: string): void {
    clearInterval(this.#renewal);
    this.#takeLock(login);
    const say = () => this.#post({ signingOut: login, tab: this.#tab });
    say();
    this.#renewal = setInterval(say, SIGN_OUT_RENEW_MS);
  }

  /** Says Dispatch signed `login` out here. The word goes out before the lock, so a tab holding
   *  their writes hears how the sign-out ended rather than finding the lock free and asking
   *  Dispatch itself; either way it ends with the same writes dropped. */
  signedOut(login: string): void {
    clearInterval(this.#renewal);
    this.#post({ signedOut: login, tab: this.#tab });
    this.#letGoOfLock();
  }

  /** Says `login`'s sign-out here did not go through, or went with the page. */
  stillSignedIn(login: string): void {
    clearInterval(this.#renewal);
    this.#post({ stillSignedIn: login, tab: this.#tab });
    this.#letGoOfLock();
  }

  /** Takes this tab's lock for `login`'s sign-out, held for as long as it is out. The name
   *  carries this tab (`signOutLockName`), so it is this tab's alone: another tab signing the
   *  same person out holds a lock of its own, and neither waits on the other. */
  #takeLock(login: string): void {
    this.#letGoOfLock();
    const locks = this.#locks;
    if (locks === undefined) return;
    const { promise, resolve } = Promise.withResolvers<void>();
    this.#releaseLock = resolve;
    void locks.request(signOutLockName(login, this.#tab), () => promise).catch(() => {});
  }

  /** Lets go of this tab's sign-out lock, if it has one: its sign-out is over, or its page is. */
  #letGoOfLock(): void {
    this.#releaseLock?.();
    this.#releaseLock = undefined;
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

  /** Has `listener` act on what the other tabs say, until the returned function is called. Every
   *  hold this tab already knows of is applied to `listener` at once: a page that loads, or
   *  reloads, while another tab's sign-out is out hears of it before its app mounts, and the
   *  writes that sign-out would discard must be held here as they are in every other tab rather
   *  than resuming under a listener that never heard it. */
  listen(listener: SignInListener): () => void {
    this.#listener = listener;
    for (const login of new Set([...this.#holds.values()].map((hold) => hold.login))) {
      listener.signingOut(login);
    }
    return () => {
      if (this.#listener === listener) this.#listener = undefined;
    };
  }

  #post(message: unknown): void {
    this.#channel?.postMessage(message);
  }

  /** Holds `login`'s writes for `tab`'s sign-out, or renews that hold. The hold ends when the
   *  browser releases that tab's lock, or - with no lock manager - when its lease lapses
   *  unrenewed. */
  #hold(tab: string, login: string): void {
    const before = this.#holds.get(tab);
    // A tab of an older page names none (`tab` is ""), and takes no lock either: its hold lives
    // on the lease, wherever the browser's locks are.
    const locks = tab === "" ? undefined : this.#locks;
    // Where the browser has locks the hold ends with the signing-out tab's, so a renewal says
    // nothing this tab does not know; where it has none, each renewal is another lease.
    const lease =
      locks !== undefined
        ? undefined
        : setTimeout(() => this.#lift(tab, "vanished"), SIGN_OUT_LEASE_MS);
    if (before?.login === login) {
      if (before.lapse === undefined) return;
      clearTimeout(before.lapse);
      this.#holds.set(tab, { ...before, lapse: lease });
      return;
    }
    if (before !== undefined) this.#endWait(before);
    const held = this.#holding(login);
    const waiting = locks === undefined ? undefined : new AbortController();
    this.#holds.set(tab, { lapse: lease, login, waiting });
    if (locks !== undefined && waiting !== undefined) {
      // That tab holds this lock, its own, while its sign-out is out, so this request is granted
      // only once the browser takes it back: that tab said how the sign-out ended (and lifted the
      // hold before this), or it closed, crashed or was discarded. A tab frozen or throttled with
      // its sign-out out keeps the lock, and keeps the hold with it. Another tab signing the same
      // person out holds a lock of its own, waited on under its own hold, so the writes go on
      // only once every one of those tabs has let go.
      void locks
        .request(signOutLockName(login, tab), { signal: waiting.signal }, () => {
          this.#lift(tab, "vanished");
        })
        .catch(() => {
          // The wait was called off: the hold had already ended another way.
        });
    }
    if (before !== undefined && before.login !== login && !this.#holding(before.login)) {
      this.#listener?.holderGone(before.login);
    }
    if (!held) this.#listener?.signingOut(login);
  }

  /** Stops watching for the end of `hold`: its lease, and its wait for the signing-out tab's
   *  lock. */
  #endWait(hold: Hold): void {
    clearTimeout(hold.lapse);
    hold.waiting?.abort();
  }

  /** Lifts `tab`'s hold, if it has one: once no tab's holds the person's writes, they go on - as
   *  writes still the person's (`stillSignedIn`, the tab said its sign-out did not go through),
   *  or as writes whose sign-out may have landed unheard (`holderGone`, the tab went). */
  #lift(tab: string, ended: "said" | "vanished"): void {
    const hold = this.#holds.get(tab);
    if (hold === undefined) return;
    this.#endWait(hold);
    this.#holds.delete(tab);
    if (this.#holding(hold.login)) return;
    const listener = this.#listener;
    if (ended === "said") listener?.stillSignedIn(hold.login);
    else listener?.holderGone(hold.login);
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
        // The tab went with its page: whether its sign-out landed, nothing here heard.
        this.#lift(tab, "vanished");
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
        this.#endWait(hold);
        this.#holds.delete(holder);
      }
      listener?.signedOut(signedOut);
      listener?.changed();
    }
    const stillSignedIn = stringField(data, "stillSignedIn");
    if (stillSignedIn !== undefined && this.#holds.get(from)?.login === stillSignedIn) {
      this.#lift(from, "said");
    }
  }
}
