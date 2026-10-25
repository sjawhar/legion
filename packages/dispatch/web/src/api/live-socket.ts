// The page's one live socket: one websocket to `GET /ws/live` per query client, over which every
// live subscription of the page runs. The Delivery and Pipeline pages' live-owned queries
// (`meta.live`, `api/queries.ts`'s three delivery reads) subscribe to their views (LEGION-690):
// Dispatch answers each with a snapshot, the matching GET route's body, and from then on sends only
// what changed, and this client writes both into the query cache with `setQueryData`, so the page
// renders them as any read. The event feed (`subscribeEvents`) and each agent live view
// (`subscribeAgent`) ride the same socket with handlers of their own. Nothing here reads on a
// timer: a lost socket reconnects and resumes every subscription, a delivery view from the last
// change it applied with the revisions it holds and the event feed from the last event it applied,
// and Dispatch answers what changed since. Dispatch rotates every socket once it reaches its age
// (close 4001); the page reopens at once and shows nothing of it.
import {
  type AgentStreamFrame,
  type AgentStreamReplay,
  type AgentStreamResponder,
  LIVE_SOCKET_MAX_AGE_CLOSE,
} from "@legion/contracts";
import { hashKey, type QueryClient, type QueryKey, useQueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";

import { applyDelta, applyRevs } from "../features/delivery/lib/applyDelta";
import type { DeliveryFreshness } from "../features/delivery/lib/freshness";
import { ApiError } from "./client";
import type {
  DeliveryLiveBody,
  DeliveryLiveDelta,
  DeliveryLiveRevs,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  LiveAgentFrame,
  LiveClientFrame,
  LiveEnded,
  LiveError,
  LiveServerFrame,
  LiveSubscribe,
} from "./types";

/** A socket that has carried no frame for this long, three of the server's 15 s heartbeats
 *  (`StreamHeartbeat`), is replaced. Any frame counts, `heartbeat` included: a browser never shows
 *  the page the socket's own pings. */
export const LIVE_SOCKET_SILENT_MS = 45_000;

/** Reconnecting this long reads as stuck: the page says live updates are not reaching it. */
export const LIVE_SOCKET_STUCK_MS = 120_000;

const BASE_RECONNECT_DELAY_MS = 1_000;
const MAX_RECONNECT_DELAY_MS = 30_000;

/** Exponential backoff for stream reconnect attempts, capped at 30s. */
export function reconnectDelayMs(attempt: number): number {
  return Math.min(BASE_RECONNECT_DELAY_MS * 2 ** attempt, MAX_RECONNECT_DELAY_MS);
}

/** How often a socket is checked for silence while the tab stays visible. */
const SILENCE_CHECK_MS = 15_000;
/** A lost sign-in is checked at most this often, however often the socket fails. */
const SIGN_IN_CHECK_MS = 30_000;
/** Dispatch closes every socket with this code when it shuts down: the page reconnects at once. */
const SERVICE_RESTART = 1012;
/** Dispatch closes a socket with this code once its caller no longer resolves (signed out). */
const CALLER_GONE = 1008;
/** The code the page closes a silent socket with before it opens another. */
const REPLACED = 4000;

/** The status the matching GET route answers each error frame's code with, so the page's error
 *  handling (`isDeliveryNotConfigured`, the shared retry policy) reads a live refusal as it read
 *  the route's. Any other code is the server's own failure. */
const ERROR_STATUS: Record<string, number> = {
  DELIVERY_NOT_CONFIGURED: 404,
  INVALID_FRAME: 400,
  INVALID_QUERY: 400,
};

export type LiveSocketConnection = "connecting" | "open" | "reconnecting" | "closed";

/** The freshness row's verdict as Dispatch last sent it in a `freshness` frame, which it sends only
 *  when the verdict changes: the freshness object and the sources past their threshold. */
export interface DeliveryLiveVerdict {
  readonly freshness: DeliveryFreshness;
  readonly stale: readonly string[];
}

export interface LiveSocketState {
  /** `connecting` while the first socket since nothing was subscribed opens, `open` while a socket
   *  is connected and while it is replaced after Dispatch rotated it (4001), `reconnecting` from
   *  any other loss, a failed first handshake included, until the next open, `closed` while
   *  nothing is subscribed. */
  readonly state: LiveSocketConnection;
  /** While reconnecting, when the socket was lost (epoch milliseconds); otherwise null. */
  readonly since: number | null;
  /** True once the state has been `reconnecting` for `LIVE_SOCKET_STUCK_MS`; false again at the
   *  next open. */
  readonly stuck: boolean;
  /** When the newest change the page applied was recorded (a snapshot's or delta's `changed_at`);
   *  null before any, or while Dispatch's change log is empty. */
  readonly changedAt: string | null;
  readonly verdict: DeliveryLiveVerdict | null;
}

/** The event feed's side of its subscription. */
export interface EventsHandlers {
  /** The decimal id of the last event the page applied, or null: the subscription resumes after
   *  it. Read each time the subscription is sent. */
  since(): string | null;
  /** The subscription's `events_start`: the id it starts after, which the feed adopts as its
   *  cursor when it holds none. */
  onStart(since: string): void;
  onEvent(id: string, event: unknown): void;
  /** Called each time the subscription is sent on an open socket, from within `subscribeEvents`
   *  when one is already open. `afterGap` is false on its first send and after a 4001; true after
   *  any other loss of the socket, a failed attempt before the first open included, and after an
   *  `ended`, when events may have been missed. */
  onSubscribed(afterGap: boolean): void;
}

/** An agent live view's side of its subscription. */
export interface AgentHandlers {
  onResponder(data: AgentStreamResponder): void;
  onReplay(data: AgentStreamReplay): void;
  onFrame(data: AgentStreamFrame): void;
  /** An error frame for this subscription. Terminal: the page drops the subscription and never
   *  sends it again. */
  onUnavailable(code: string, message: string): void;
}

export interface LiveSocket {
  /** Subscribes `queryKey`'s query to the delivery `view`, replacing any subscription it held.
   *  Resolves with the snapshot's body, which is also written to the cache; every later change is
   *  written there. */
  subscribe(queryKey: QueryKey, view: DeliveryLiveView): Promise<DeliveryLiveBody>;
  /** Subscribes the event feed; returns the unsubscribe. */
  subscribeEvents(handlers: EventsHandlers): () => void;
  /** Subscribes session `sessionId`'s agent stream; returns the unsubscribe. */
  subscribeAgent(sessionId: string, handlers: AgentHandlers): () => void;
  state(): LiveSocketState;
  /** Calls `listener` with `state()` whenever it changes; returns the unsubscribe. */
  onState(listener: (state: LiveSocketState) => void): () => void;
  /** Ends every subscription and the socket. */
  close(): void;
}

/** A first load's promise and the two functions that settle it. */
interface PendingLoad {
  readonly promise: Promise<DeliveryLiveBody>;
  readonly resolve: (body: DeliveryLiveBody) => void;
  readonly reject: (reason: unknown) => void;
}

interface DeliverySubscription {
  readonly kind: "delivery";
  readonly id: string;
  readonly hash: string;
  readonly queryKey: QueryKey;
  readonly view: DeliveryLiveView;
  /** The last change applied, and what the page holds, once a snapshot arrived. */
  seq: string | null;
  revs: DeliveryLiveRevs | null;
  /** The first load's promise, until the snapshot or an error frame settles it. */
  pending: PendingLoad | null;
}

interface EventsSubscription {
  readonly kind: "events";
  readonly id: string;
  readonly handlers: EventsHandlers;
  /** Set when events may have been missed since the subscription was last sent: its socket was
   *  lost, or Dispatch ended it. Cleared by the next send, which reports it (`onSubscribed`). */
  gap: boolean;
}

interface AgentSubscription {
  readonly kind: "agent";
  readonly id: string;
  readonly sessionId: string;
  readonly handlers: AgentHandlers;
}

type Subscription = DeliverySubscription | EventsSubscription | AgentSubscription;

/** The live route on this page's own origin, as the document socket builds its own. */
function liveSocketUrl(): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/live`;
}

/** `fn` after `ms`, and the function that cancels it. */
function after(ms: number, fn: () => void): () => void {
  const handle = setTimeout(fn, ms);
  return () => clearTimeout(handle);
}

export function createLiveSocket(queryClient: QueryClient): LiveSocket {
  const cache = queryClient.getQueryCache();
  const subs = new Map<string, Subscription>();
  const byHash = new Map<string, DeliverySubscription>();
  const listeners = new Set<(state: LiveSocketState) => void>();
  let current: LiveSocketState = {
    state: "closed",
    since: null,
    stuck: false,
    changedAt: null,
    verdict: null,
  };
  let changedSeq: bigint | null = null;
  let socket: WebSocket | null = null;
  let isOpen = false;
  let cancelReconnect: (() => void) | undefined;
  let stopSilenceCheck: (() => void) | undefined;
  let cancelStuck: (() => void) | undefined;
  let idleClose = false;
  let attempt = 0;
  let lastFrameAt = 0;
  let signInCheckedAt = Number.NEGATIVE_INFINITY;
  let nextId = 0;

  const update = (next: Partial<LiveSocketState>) => {
    current = { ...current, ...next };
    for (const listener of listeners) listener(current);
  };

  const send = (frame: LiveClientFrame) => {
    if (socket !== null && isOpen) socket.send(JSON.stringify(frame));
  };

  const subscribeFrame = (sub: Subscription): LiveSubscribe => {
    switch (sub.kind) {
      case "delivery":
        return sub.seq !== null && sub.revs !== null
          ? {
              type: "subscribe",
              sub: sub.id,
              view: sub.view,
              resume: { seq: sub.seq, revs: sub.revs },
            }
          : { type: "subscribe", sub: sub.id, view: sub.view };
      case "events": {
        const since = sub.handlers.since();
        return since === null
          ? { type: "subscribe", sub: sub.id, view: { kind: "events" } }
          : { type: "subscribe", sub: sub.id, view: { kind: "events" }, resume: { since } };
      }
      case "agent":
        return {
          type: "subscribe",
          sub: sub.id,
          view: { kind: "agent", session_id: sub.sessionId },
        };
    }
  };

  /** Sends `sub` when a socket is open; otherwise the next open sends it. */
  const sendSubscribe = (sub: Subscription) => {
    if (socket === null || !isOpen) return;
    socket.send(JSON.stringify(subscribeFrame(sub)));
    if (sub.kind !== "events") return;
    const afterGap = sub.gap;
    sub.gap = false;
    sub.handlers.onSubscribed(afterGap);
  };

  /** The subscription `id` names when it is one of `kind`. A frame for one the page already
   *  dropped was in flight when it did. */
  const held = <K extends Subscription["kind"]>(
    id: string,
    kind: K
  ): Extract<Subscription, { kind: K }> | undefined => {
    const sub = subs.get(id);
    if (sub === undefined) return undefined;
    if (sub.kind !== kind) {
      console.error(`live socket: a ${kind} frame for a ${sub.kind} subscription`, id);
      return undefined;
    }
    return sub as Extract<Subscription, { kind: K }>;
  };

  const noteChange = (seq: string, changedAt: string | null) => {
    const position = BigInt(seq);
    if (changedSeq !== null && position < changedSeq) return;
    changedSeq = position;
    if (changedAt !== current.changedAt) update({ changedAt });
  };

  const onSnapshot = (frame: DeliveryLiveSnapshot) => {
    const sub = held(frame.sub, "delivery");
    if (sub === undefined) return;
    sub.seq = frame.seq;
    sub.revs = frame.revs;
    queryClient.setQueryData(sub.queryKey, frame.body);
    sub.pending?.resolve(frame.body);
    sub.pending = null;
    noteChange(frame.seq, frame.changed_at);
  };

  const onDelta = (frame: DeliveryLiveDelta) => {
    const sub = held(frame.sub, "delivery");
    if (sub === undefined) return;
    if (sub.revs === null) {
      console.error("live socket: a delta arrived before its subscription's snapshot", frame);
      return;
    }
    queryClient.setQueryData<DeliveryLiveBody>(
      sub.queryKey,
      (data: DeliveryLiveBody | undefined) => (data === undefined ? data : applyDelta(data, frame))
    );
    sub.seq = frame.seq;
    sub.revs = applyRevs(sub.revs, frame.revs);
    noteChange(frame.seq, frame.changed_at);
    // A run that changed may be one whose drill-down is open; only an open one reads again.
    for (const run of frame.items.runs?.upsert ?? []) {
      void queryClient.invalidateQueries({ exact: true, queryKey: ["delivery", "run", run.id] });
    }
  };

  const onAgent = (frame: LiveAgentFrame) => {
    const sub = held(frame.sub, "agent");
    if (sub === undefined) return;
    switch (frame.event) {
      case "responder":
        return sub.handlers.onResponder(frame.data);
      case "replay":
        return sub.handlers.onReplay(frame.data);
      case "frame":
        return sub.handlers.onFrame(frame.data);
      default:
        console.error("live socket: an agent frame of an unknown event", frame as unknown);
    }
  };

  const onEnded = (frame: LiveEnded) => {
    const sub = subs.get(frame.sub);
    if (sub === undefined) return;
    // Dispatch dropped this one subscription; the socket and every other subscription stay.
    if (sub.kind === "events") sub.gap = true;
    sendSubscribe(sub);
  };

  const failDelivery = (sub: DeliverySubscription, frame: LiveError) => {
    const failure = new ApiError(ERROR_STATUS[frame.code] ?? 500, {
      code: frame.code,
      error: frame.message,
    });
    if (sub.pending !== null) {
      sub.pending.reject(failure);
      sub.pending = null;
      return;
    }
    // A subscription that already answered fails as a failed refetch does, keeping its data. It
    // stays subscribed: Dispatch answers it again once it can (a configuration saved, say).
    const query = cache.find({ exact: true, queryKey: sub.queryKey });
    query?.setState({
      ...query.state,
      error: failure,
      errorUpdateCount: query.state.errorUpdateCount + 1,
      errorUpdatedAt: Date.now(),
      fetchFailureReason: failure,
      status: "error",
    });
  };

  const onError = (frame: LiveError) => {
    const sub = frame.sub === undefined ? undefined : subs.get(frame.sub);
    if (sub === undefined) {
      console.error("live socket: Dispatch refused a frame this page sent", frame);
      return;
    }
    switch (sub.kind) {
      case "delivery":
        return failDelivery(sub, frame);
      case "agent":
        // Dispatch holds no subscription it refused, so the page lets it go without telling it.
        remove(sub, false);
        return sub.handlers.onUnavailable(frame.code, frame.message);
      case "events":
        // It stays subscribed, and the next socket sends it again.
        console.error("live socket: Dispatch refused the event feed", frame);
    }
  };

  const receive = (data: unknown) => {
    lastFrameAt = Date.now();
    attempt = 0;
    const frame = JSON.parse(String(data)) as LiveServerFrame;
    switch (frame.type) {
      case "heartbeat":
        return;
      case "snapshot":
        return onSnapshot(frame);
      case "delta":
        return onDelta(frame);
      case "freshness":
        return update({ verdict: { freshness: frame.freshness, stale: frame.stale } });
      case "events_start":
        return held(frame.sub, "events")?.handlers.onStart(frame.since);
      case "event":
        return held(frame.sub, "events")?.handlers.onEvent(frame.id, frame.event);
      case "agent":
        return onAgent(frame);
      case "ended":
        return onEnded(frame);
      case "error":
        return onError(frame);
      default:
        console.error("live socket: a frame of an unknown type", frame as unknown);
    }
  };

  /** Every event feed subscription may have missed events by its next send. */
  const markGap = () => {
    for (const sub of subs.values()) if (sub.kind === "events") sub.gap = true;
  };

  /** A socket refused before it opened, or closed because its caller no longer resolves, can mean
   *  the person was signed out: reading `whoami` again puts a signed-out person on the sign-in
   *  page (`AuthGate`), at any point of an outage. */
  const checkSignIn = () => {
    const now = Date.now();
    if (now - signInCheckedAt < SIGN_IN_CHECK_MS) return;
    signInCheckedAt = now;
    void queryClient.invalidateQueries({ queryKey: ["whoami"] });
  };

  const reconnecting = () => {
    if (current.state === "reconnecting") return;
    update({ since: Date.now(), state: "reconnecting" });
    cancelStuck = after(LIVE_SOCKET_STUCK_MS, () => update({ stuck: true }));
  };

  const replaceIfSilent = () => {
    if (socket === null || Date.now() - lastFrameAt <= LIVE_SOCKET_SILENT_MS) return;
    const silent = socket;
    socket = null;
    isOpen = false;
    silent.close(REPLACED);
    markGap();
    reconnecting();
    open();
  };

  const onVisibility = () => {
    if (document.visibilityState === "visible") replaceIfSilent();
  };

  const open = () => {
    cancelReconnect = undefined;
    const next = new WebSocket(liveSocketUrl());
    socket = next;
    isOpen = false;
    // A handshake that never ends is as silent as an open socket that carries nothing.
    lastFrameAt = Date.now();
    stopSilenceCheck?.();
    const check = setInterval(replaceIfSilent, SILENCE_CHECK_MS);
    stopSilenceCheck = () => clearInterval(check);
    // Each listener ignores a socket the page has already let go, whose events still arrive.
    next.addEventListener("open", () => {
      if (socket !== next) return;
      isOpen = true;
      lastFrameAt = Date.now();
      cancelStuck?.();
      cancelStuck = undefined;
      update({ since: null, state: "open", stuck: false });
      for (const sub of subs.values()) sendSubscribe(sub);
    });
    next.addEventListener("message", (event) => {
      if (socket === next) receive(event.data);
    });
    // WebKit can report a failed opening handshake only as an error, with no close event. Treat it
    // as the same dropped connection the close path handles, so the first page load retries rather
    // than remaining pending forever.
    next.addEventListener("error", () => {
      if (socket === next) lost(0);
    });
    next.addEventListener("close", (event) => {
      if (socket === next) lost(event.code);
    });
  };

  const lost = (code: number) => {
    const wasOpen = isOpen;
    socket = null;
    isOpen = false;
    stopSilenceCheck?.();
    stopSilenceCheck = undefined;
    if (wasOpen && code === LIVE_SOCKET_MAX_AGE_CLOSE) {
      // Dispatch rotated a socket that reached its age: every subscription resumes on the next,
      // nothing was missed, and nothing shows.
      open();
      return;
    }
    markGap();
    if (!wasOpen || code === CALLER_GONE) checkSignIn();
    reconnecting();
    if (wasOpen && code === SERVICE_RESTART) {
      open();
      return;
    }
    const delay = reconnectDelayMs(attempt) * (0.75 + Math.random() * 0.5);
    attempt += 1;
    cancelReconnect = after(delay, open);
  };

  const shutDown = () => {
    cancelReconnect?.();
    cancelReconnect = undefined;
    stopSilenceCheck?.();
    stopSilenceCheck = undefined;
    cancelStuck?.();
    cancelStuck = undefined;
    document.removeEventListener("visibilitychange", onVisibility);
    const closing = socket;
    socket = null;
    isOpen = false;
    closing?.close(1000);
    update({ since: null, state: "closed", stuck: false });
  };

  const add = (sub: Subscription) => {
    subs.set(sub.id, sub);
    if (socket !== null) {
      sendSubscribe(sub);
    } else if (cancelReconnect === undefined) {
      document.addEventListener("visibilitychange", onVisibility);
      update({ since: null, state: "connecting", stuck: false });
      open();
    }
  };

  /** Drops `sub`, telling Dispatch when `unsubscribe`; a second call does nothing. */
  const remove = (sub: Subscription, unsubscribe: boolean) => {
    if (subs.get(sub.id) !== sub) return;
    subs.delete(sub.id);
    if (sub.kind === "delivery" && byHash.get(sub.hash) === sub) byHash.delete(sub.hash);
    if (unsubscribe) send({ type: "unsubscribe", sub: sub.id });
    if (subs.size > 0 || idleClose) return;
    // A query switching keys drops its old subscription just before it adds its new one, so the
    // socket closes only if nothing subscribes again within this task.
    idleClose = true;
    queueMicrotask(() => {
      idleClose = false;
      if (subs.size === 0) shutDown();
    });
  };

  const newId = () => {
    nextId += 1;
    return `s${nextId}`;
  };

  const stopCache = cache.subscribe((event) => {
    if (event.type !== "observerRemoved" && event.type !== "removed") return;
    // gcTime 0 can collect the query a render built before its observer subscribes; subscribing
    // then builds a second query under the same hash, and the first still reports its own removal
    // afterwards (QueryCache.remove notifies for a query it no longer holds). The subscription
    // belongs to the query the cache holds now. That relies on every query under a live key being
    // built from queries.ts's live options, so the query the cache holds subscribes through
    // live.subscribe itself, which replaces the hash's subscription. A query built under one of
    // these keys any other way (a prefetch with its own queryFn, say) would break it: the live
    // query's final removal would be skipped and its subscription never dropped.
    const cached = cache.get(event.query.queryHash);
    if (cached !== undefined && cached !== event.query) return;
    if (event.type === "observerRemoved" && event.query.getObserversCount() > 0) return;
    const sub = byHash.get(event.query.queryHash);
    if (sub === undefined) return;
    remove(sub, true);
    // A first load nothing observes any more can never settle now: revert it, so the query goes
    // idle and is collected rather than held fetching.
    if (event.type === "observerRemoved" && event.query.state.fetchStatus !== "idle") {
      void event.query.cancel({ revert: true });
    }
  });

  return {
    subscribe(queryKey, view) {
      const hash = hashKey(queryKey);
      const previous = byHash.get(hash);
      if (previous !== undefined) remove(previous, true);
      const pending = Promise.withResolvers<DeliveryLiveBody>();
      const sub: DeliverySubscription = {
        kind: "delivery",
        hash,
        id: newId(),
        pending,
        queryKey,
        revs: null,
        seq: null,
        view,
      };
      byHash.set(hash, sub);
      add(sub);
      return pending.promise;
    },
    subscribeEvents(handlers) {
      const sub: EventsSubscription = { kind: "events", id: newId(), handlers, gap: false };
      add(sub);
      return () => remove(sub, true);
    },
    subscribeAgent(sessionId, handlers) {
      const sub: AgentSubscription = { kind: "agent", id: newId(), sessionId, handlers };
      add(sub);
      return () => remove(sub, true);
    },
    state: () => current,
    onState(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    close() {
      stopCache();
      subs.clear();
      byHash.clear();
      shutDown();
    },
  };
}

const sockets = new WeakMap<QueryClient, LiveSocket>();

/** The one live socket of `queryClient`, created on first use. */
export function liveSocketFor(queryClient: QueryClient): LiveSocket {
  let live = sockets.get(queryClient);
  if (live === undefined) {
    live = createLiveSocket(queryClient);
    sockets.set(queryClient, live);
  }
  return live;
}

/** The live socket's state, for the connection pill and the freshness row. */
export function useLiveSocketState(): LiveSocketState {
  const live = liveSocketFor(useQueryClient());
  return useSyncExternalStore(live.onState, live.state);
}
