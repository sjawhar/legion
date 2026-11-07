/**
 * The live views a person has open (INFRA-812, R2): one entry per session whose view has been
 * opened, holding what that view keeps while the person is elsewhere - the unsent draft, the
 * place in the conversation and the unread count the view opened with - and which views are
 * live.
 *
 * This store is the one owner of three things, and nothing else decides them:
 *
 *  - **The draft and the place.** They belonged to the view before: the draft to assistant-ui's
 *    composer runtime, parked in a ref that died with the view, and the place to
 *    `use-live-follow.ts`, which measured it afresh at every mount. A view that sleeps is
 *    unmounted (D6), so both now live here, written while the view is up and handed back when it
 *    wakes.
 *  - **Which views are live.** The view in front is live, the view left last lingers for
 *    `AGENT_STREAM_WATCH_TTL_MS` - the window a session keeps publishing for, so a view that
 *    comes back inside it finds the session still on the wire - and every other entry sleeps.
 *    Opening a third view puts the first to sleep at once. The rule is this store's: a view
 *    component decides nothing about it, and the shell mounts exactly what `mounted()` names.
 *  - **The state word.** `state()` answers `live`, `live 30s` or `asleep` for a session id, and
 *    the rail row renders what it answers and computes nothing.
 *
 * An entry that falls out of the front and lingering pair is discarded unless the person pinned
 * its session or it holds an unsent draft (D8): every link into a live view makes an entry,
 * pinned or not, and a discarded one costs nothing to make again, since opening the link
 * rebuilds the view from the session's replay. An unsent draft is never thrown away (R2).
 */
import { AGENT_STREAM_WATCH_TTL_MS } from "@legion/contracts";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";

/** Where a reader was in a view's transcript: the turn they were reading, by the key it carries
 *  in `data-message-id`, and where that turn sat on their screen. `use-live-follow.ts` measures
 *  it and hands it back here, and takes it from here when the view wakes. */
export interface LivePlace {
  readonly id: string;
  /** Its top edge in pixels below the viewport's own top edge; negative once scrolled past. */
  readonly top: number;
}

/** What a view is doing, in the words its rail row shows: in front and live, lingering for the
 *  watch window after being left, or asleep - holding no subscription, no drawn conversation and
 *  no publishing session, because nothing of it is mounted. */
export type LiveViewState = "live" | "live 30s" | "asleep";

/** One opened session's view, as this store holds it. */
interface Entry {
  /** Whether the view is mounted: the one in front and the one lingering are. */
  awake: boolean;
  draft: string;
  place: LivePlace | null;
  unreadAtOpen: number;
}

/** One view in the window snapshot below: a word and two numbers, nothing of what was written. */
interface LiveViewSnapshot {
  readonly draftChars: number;
  readonly state: LiveViewState;
  readonly unreadAtOpen: number;
}

/**
 * What the page reports about its live views on `window`, for a browser check to read (D15).
 * The registry writes `views` and leaves every other key alone, so another owner - the live
 * socket, reporting how many agent subscriptions it holds - reports on the same handle.
 */
export interface DispatchLiveViews {
  views?: Record<string, LiveViewSnapshot>;
  [key: string]: unknown;
}

declare global {
  interface Window {
    /** Each opened view's state, draft length and unread-at-open count (D15). */
    __dispatchLiveViews?: DispatchLiveViews;
    /** Set before the page loads, it turns the sleep rule off: every opened view stays mounted,
     *  which is the negative control of the check that counts what a sleeping view holds. */
    __dispatchNoViewSleep?: boolean;
  }
}

/** One array for every store with nothing mounted, so a host reading `mounted()` re-renders only
 *  when the set itself changes. */
const NOTHING_MOUNTED: readonly string[] = [];

/**
 * The live views of one app, as the shell mounts them and the rail reads them. One per
 * `QueryClient` (`liveViewsOf`), made above the router, so leaving a view - or the page that
 * showed it - drops nothing it holds.
 */
export class LiveViews {
  readonly #entries = new Map<string, Entry>();
  readonly #listeners = new Set<() => void>();
  /** The session whose view is in front, or `null` while the person is on another page. */
  #front: string | null = null;
  #linger: number | undefined;
  #mounted: readonly string[] = NOTHING_MOUNTED;
  /** The sessions the person has pinned, which outlive every entry. */
  #pinned: ReadonlySet<string> = new Set<string>();

  /**
   * Put `sessionId`'s view in front, making its entry if this is the first time it is opened, or
   * leave every view behind with `null`. The view that was in front lingers for the watch
   * window; every view that was already lingering sleeps at once.
   */
  show(sessionId: string | null): void {
    if (sessionId === this.#front) return;
    const left = this.#front;
    this.#front = sessionId;
    if (sessionId !== null) this.#entry(sessionId).awake = true;
    window.clearTimeout(this.#linger);
    this.#linger = undefined;
    // A view the person left is only lingering while its entry is still here: one discarded
    // while it was in front cannot be, and there is nothing to put to sleep later.
    const lingering = left !== null && this.#entries.has(left) ? left : null;
    // `__dispatchNoViewSleep` is the browser check's switch (D15): set before the page loaded,
    // it leaves every opened view mounted. Read at every move, since nothing else sets it.
    if (window.__dispatchNoViewSleep !== true) {
      for (const [id, entry] of [...this.#entries]) {
        if (entry.awake && id !== sessionId && id !== lingering) this.#sleep(id);
      }
      if (lingering !== null) {
        this.#linger = window.setTimeout(() => {
          this.#linger = undefined;
          this.#sleep(lingering);
          this.#changed();
        }, AGENT_STREAM_WATCH_TTL_MS);
      }
    }
    this.#changed();
  }

  /** What `sessionId`'s view is doing. A session with no entry - never opened, or discarded - is
   *  asleep: there is nothing of it on the page. */
  state(sessionId: string): LiveViewState {
    const entry = this.#entries.get(sessionId);
    if (entry === undefined || !entry.awake) return "asleep";
    return sessionId === this.#front ? "live" : "live 30s";
  }

  /** The sessions whose views are mounted now - at most the one in front and the one lingering
   *  (D6) - in the order they were first opened, so a view the person comes back to is not
   *  remounted by its place in the list changing. */
  mounted = (): readonly string[] => this.#mounted;

  /** What the person has pinned. A pinned entry is kept when it sleeps; an unpinned one is kept
   *  only by an unsent draft (D8). */
  setPinned(sessionIds: Iterable<string>): void {
    const pinned = new Set(sessionIds);
    const unpinned = [...this.#pinned].filter((sessionId) => !pinned.has(sessionId));
    this.#pinned = pinned;
    // A pin is known before its view is ever opened and outlives every entry, so it is the
    // store's own set rather than a field of an entry. Only losing a pin can discard anything.
    for (const sessionId of unpinned) this.#dropIfNothingKept(sessionId);
    this.#changed();
  }

  /** What the person has written to `sessionId` and not sent. */
  draft(sessionId: string): string {
    return this.#entries.get(sessionId)?.draft ?? "";
  }

  /** Writes the view's unsent text. Emptying the draft of a sleeping unpinned view discards it,
   *  as nothing is left to keep (D8); emptying one of a session with no entry does nothing. */
  setDraft(sessionId: string, text: string): void {
    const entry = text === "" ? this.#entries.get(sessionId) : this.#entry(sessionId);
    if (entry === undefined || entry.draft === text) return;
    entry.draft = text;
    this.#dropIfNothingKept(sessionId);
    this.#changed();
  }

  /** Where the reader was in `sessionId`'s transcript, or `null` when they were following its
   *  live end. */
  place(sessionId: string): LivePlace | null {
    return this.#entries.get(sessionId)?.place ?? null;
  }

  setPlace(sessionId: string, place: LivePlace | null): void {
    const entry = place === null ? this.#entries.get(sessionId) : this.#entry(sessionId);
    const held = entry?.place;
    // The same turn at the same offset is where the reader already was, which tells nobody
    // anything: the hook reports the place on every scroll event it answers.
    if (entry === undefined || (held?.id === place?.id && held?.top === place?.top)) return;
    entry.place = place;
    this.#changed();
  }

  /** How many replies were unread when the view opened, which a lingering or sleeping view
   *  leaves standing (R2). */
  unreadAtOpen(sessionId: string): number {
    return this.#entries.get(sessionId)?.unreadAtOpen ?? 0;
  }

  setUnreadAtOpen(sessionId: string, count: number): void {
    const entry = count === 0 ? this.#entries.get(sessionId) : this.#entry(sessionId);
    if (entry === undefined || entry.unreadAtOpen === count) return;
    entry.unreadAtOpen = count;
    this.#changed();
  }

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  };

  #entry(sessionId: string): Entry {
    let entry = this.#entries.get(sessionId);
    if (entry === undefined) {
      entry = { awake: false, draft: "", place: null, unreadAtOpen: 0 };
      this.#entries.set(sessionId, entry);
    }
    return entry;
  }

  /** `sessionId`'s view sleeps: it is mounted no longer, so it holds no subscription, draws no
   *  conversation and marks nothing read, and its entry goes unless something is kept in it. */
  #sleep(sessionId: string): void {
    const entry = this.#entries.get(sessionId);
    if (entry === undefined) return;
    entry.awake = false;
    this.#dropIfNothingKept(sessionId);
  }

  /** Drops `sessionId`'s entry when nothing is left to keep: it is asleep, the person has not
   *  pinned it, and it holds no unsent draft (D8). Opening its link makes it again. */
  #dropIfNothingKept(sessionId: string): void {
    const entry = this.#entries.get(sessionId);
    if (entry === undefined || entry.awake || entry.draft !== "") return;
    if (this.#pinned.has(sessionId)) return;
    this.#entries.delete(sessionId);
  }

  #changed(): void {
    const mounted: string[] = [];
    for (const [sessionId, entry] of this.#entries) {
      if (entry.awake) mounted.push(sessionId);
    }
    if (
      mounted.length !== this.#mounted.length ||
      mounted.some((sessionId, index) => sessionId !== this.#mounted[index])
    ) {
      this.#mounted = mounted.length === 0 ? NOTHING_MOUNTED : mounted;
    }
    this.#report();
    for (const listener of this.#listeners) listener();
  }

  /** The snapshot a browser check reads (D15): a word and two numbers per opened session. */
  #report(): void {
    const views: Record<string, LiveViewSnapshot> = {};
    for (const [sessionId, entry] of this.#entries) {
      views[sessionId] = {
        draftChars: entry.draft.length,
        state: this.state(sessionId),
        unreadAtOpen: entry.unreadAtOpen,
      };
    }
    window.__dispatchLiveViews ??= {};
    window.__dispatchLiveViews.views = views;
  }
}

const registries = new WeakMap<QueryClient, LiveViews>();

/** The live views that live with `client`: one store per app, as there is one client. */
export function liveViewsOf(client: QueryClient): LiveViews {
  let views = registries.get(client);
  if (views === undefined) {
    views = new LiveViews();
    registries.set(client, views);
  }
  return views;
}

export function useLiveViews(): LiveViews {
  return liveViewsOf(useQueryClient());
}

/** The word for `sessionId`'s view, re-rendering whenever it changes: what the rail row shows. */
export function useLiveViewState(sessionId: string): LiveViewState {
  const views = useLiveViews();
  return useSyncExternalStore(views.subscribe, () => views.state(sessionId));
}

/** The sessions whose views the shell mounts now, re-rendering only when that set changes. */
export function useMountedLiveViews(): readonly string[] {
  const views = useLiveViews();
  return useSyncExternalStore(views.subscribe, views.mounted);
}
