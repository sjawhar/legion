// The Delivery and Pipeline pages' live client (LEGION-690): one websocket to `GET /ws/delivery`
// per query client, over which every live-owned query (`meta.live`, `api/queries.ts`'s three
// delivery reads) subscribes to its view. Dispatch answers each subscription with a snapshot, the
// matching GET route's body, and from then on sends only what changed; this client writes both into
// the query cache with `setQueryData`, so the page renders them as any read. Nothing here reads on a
// timer: a dropped socket reconnects with backoff and resumes every subscription from the last
// change it applied, with the revisions it holds, and Dispatch answers what changed since.
import { hashKey, type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";

import { applyDelta, applyRevs } from "../features/delivery/lib/applyDelta";
import type { DeliveryFreshness } from "../features/delivery/lib/freshness";
import { ApiError } from "./client";
import { reconnectDelayMs } from "./live";
import type {
  DeliveryLiveBody,
  DeliveryLiveClientFrame,
  DeliveryLiveDelta,
  DeliveryLiveError,
  DeliveryLiveRevs,
  DeliveryLiveServerFrame,
  DeliveryLiveSnapshot,
  DeliveryLiveSubscribe,
  DeliveryLiveView,
} from "./types";

/** A socket that has carried no frame for this long, two of the server's 15 s heartbeats
 *  (`StreamHeartbeat`), is replaced when the tab becomes visible again. Any frame counts,
 *  `heartbeat` included: a browser never shows the page the socket's own pings. */
export const DELIVERY_LIVE_SILENT_MS = 30_000;

/** Dispatch closes every socket with this code when it shuts down: the page reconnects at once. */
const SERVICE_RESTART = 1012;
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

export type DeliveryLiveConnection = "open" | "reconnecting" | "closed";

/** The freshness row's verdict as Dispatch last sent it in a `freshness` frame, which it sends only
 *  when the verdict changes: the freshness object and the sources past their threshold. */
export interface DeliveryLiveVerdict {
  readonly freshness: DeliveryFreshness;
  readonly stale: readonly string[];
}

export interface DeliveryLiveState {
  /** `open` while the socket is connected, `reconnecting` while a page still subscribed waits for
   *  one, `closed` while nothing is subscribed. */
  readonly state: DeliveryLiveConnection;
  /** While reconnecting, when the socket was lost (epoch milliseconds); otherwise null. */
  readonly since: number | null;
  /** When the newest change the page applied was recorded (a snapshot's or delta's `changed_at`);
   *  null before any, or while Dispatch's change log is empty. */
  readonly changedAt: string | null;
  readonly verdict: DeliveryLiveVerdict | null;
}

export interface DeliveryLive {
  /** Subscribes `queryKey`'s query to `view`, replacing any subscription it held. Resolves with the
   *  snapshot's body, which is also written to the cache; every later change is written there. */
  subscribe(queryKey: readonly unknown[], view: DeliveryLiveView): Promise<DeliveryLiveBody>;
  state(): DeliveryLiveState;
  /** Calls `listener` whenever `state()` changes; returns the unsubscribe. */
  onState(listener: () => void): () => void;
  /** Ends every subscription and the socket. */
  close(): void;
}

/** A first load's promise and the two functions that settle it. */
interface PendingLoad {
  readonly promise: Promise<DeliveryLiveBody>;
  readonly resolve: (body: DeliveryLiveBody) => void;
  readonly reject: (reason: unknown) => void;
}

interface Subscription {
  readonly id: string;
  readonly hash: string;
  readonly queryKey: readonly unknown[];
  readonly view: DeliveryLiveView;
  /** The last change applied, and what the page holds, once a snapshot arrived. */
  seq: string | null;
  revs: DeliveryLiveRevs | null;
  /** The first load's promise, until the snapshot or an error frame settles it. */
  pending: PendingLoad | null;
}

/** The live route on this page's own origin, as the document socket builds its own. */
function deliveryLiveUrl(): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/delivery`;
}

/** `fn` after `ms`, and the function that cancels it. */
function after(ms: number, fn: () => void): () => void {
  const handle = setTimeout(fn, ms);
  return () => clearTimeout(handle);
}

export function createDeliveryLive(queryClient: QueryClient): DeliveryLive {
  const cache = queryClient.getQueryCache();
  const subs = new Map<string, Subscription>();
  const byHash = new Map<string, Subscription>();
  const listeners = new Set<() => void>();
  let current: DeliveryLiveState = { state: "closed", since: null, changedAt: null, verdict: null };
  let changedSeq: bigint | null = null;
  let socket: WebSocket | null = null;
  let isOpen = false;
  let cancelReconnect: (() => void) | undefined;
  let idleClose = false;
  let attempt = 0;
  let lastFrameAt = 0;
  let nextId = 0;

  const update = (next: Partial<DeliveryLiveState>) => {
    current = { ...current, ...next };
    for (const listener of listeners) listener();
  };

  const send = (frame: DeliveryLiveClientFrame) => {
    if (socket !== null && isOpen) socket.send(JSON.stringify(frame));
  };

  const subscribeFrame = (sub: Subscription): DeliveryLiveSubscribe =>
    sub.seq !== null && sub.revs !== null
      ? { type: "subscribe", sub: sub.id, view: sub.view, resume: { seq: sub.seq, revs: sub.revs } }
      : { type: "subscribe", sub: sub.id, view: sub.view };

  const noteChange = (seq: string, changedAt: string | null) => {
    const position = BigInt(seq);
    if (changedSeq !== null && position < changedSeq) return;
    changedSeq = position;
    if (changedAt !== current.changedAt) update({ changedAt });
  };

  const onSnapshot = (frame: DeliveryLiveSnapshot) => {
    const sub = subs.get(frame.sub);
    // A frame for a subscription the page already dropped was in flight when it did.
    if (sub === undefined) return;
    sub.seq = frame.seq;
    sub.revs = frame.revs;
    queryClient.setQueryData(sub.queryKey, frame.body);
    sub.pending?.resolve(frame.body);
    sub.pending = null;
    noteChange(frame.seq, frame.changed_at);
  };

  const onDelta = (frame: DeliveryLiveDelta) => {
    const sub = subs.get(frame.sub);
    if (sub === undefined) return;
    if (sub.revs === null) {
      console.error("delivery live: a delta arrived before its subscription's snapshot", frame);
      return;
    }
    queryClient.setQueryData<DeliveryLiveBody>(
      sub.queryKey,
      (held: DeliveryLiveBody | undefined) => (held === undefined ? held : applyDelta(held, frame))
    );
    sub.seq = frame.seq;
    sub.revs = applyRevs(sub.revs, frame.revs);
    noteChange(frame.seq, frame.changed_at);
    // A run that changed may be one whose drill-down is open; only an open one reads again.
    for (const run of frame.items.runs?.upsert ?? []) {
      void queryClient.invalidateQueries({ exact: true, queryKey: ["delivery", "run", run.id] });
    }
  };

  const onError = (frame: DeliveryLiveError) => {
    const sub = frame.sub === undefined ? undefined : subs.get(frame.sub);
    if (sub === undefined) {
      console.error("delivery live: Dispatch refused a frame this page sent", frame);
      return;
    }
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

  const receive = (data: unknown) => {
    lastFrameAt = Date.now();
    attempt = 0;
    const frame = JSON.parse(String(data)) as DeliveryLiveServerFrame;
    switch (frame.type) {
      case "heartbeat":
        return;
      case "snapshot":
        return onSnapshot(frame);
      case "delta":
        return onDelta(frame);
      case "freshness":
        return update({ verdict: { freshness: frame.freshness, stale: frame.stale } });
      case "error":
        return onError(frame);
      default:
        console.error("delivery live: a frame of an unknown type", frame as unknown);
    }
  };

  const open = () => {
    cancelReconnect = undefined;
    const next = new WebSocket(deliveryLiveUrl());
    socket = next;
    isOpen = false;
    // Each listener ignores a socket the page has already let go, whose events still arrive.
    next.addEventListener("open", () => {
      if (socket !== next) return;
      isOpen = true;
      lastFrameAt = Date.now();
      update({ since: null, state: "open" });
      for (const sub of subs.values()) send(subscribeFrame(sub));
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
    socket = null;
    isOpen = false;
    update({ since: current.since ?? Date.now(), state: "reconnecting" });
    if (code === SERVICE_RESTART) {
      open();
      return;
    }
    const delay = reconnectDelayMs(attempt) * (0.75 + Math.random() * 0.5);
    attempt += 1;
    cancelReconnect = after(delay, open);
  };

  const onVisibility = () => {
    if (document.visibilityState !== "visible" || socket === null) return;
    if (Date.now() - lastFrameAt <= DELIVERY_LIVE_SILENT_MS) return;
    const silent = socket;
    socket = null;
    isOpen = false;
    silent.close(REPLACED);
    update({ since: current.since ?? Date.now(), state: "reconnecting" });
    open();
  };

  const shutDown = () => {
    cancelReconnect?.();
    cancelReconnect = undefined;
    document.removeEventListener("visibilitychange", onVisibility);
    const closing = socket;
    socket = null;
    isOpen = false;
    closing?.close(1000);
    update({ since: null, state: "closed" });
  };

  const drop = (sub: Subscription) => {
    subs.delete(sub.id);
    if (byHash.get(sub.hash) === sub) byHash.delete(sub.hash);
    send({ type: "unsubscribe", sub: sub.id });
    if (subs.size > 0 || idleClose) return;
    // A query switching keys drops its old subscription just before it adds its new one, so the
    // socket closes only if nothing subscribes again within this task.
    idleClose = true;
    queueMicrotask(() => {
      idleClose = false;
      if (subs.size === 0) shutDown();
    });
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
    const held = cache.get(event.query.queryHash);
    if (held !== undefined && held !== event.query) return;
    if (event.type === "observerRemoved" && event.query.getObserversCount() > 0) return;
    const sub = byHash.get(event.query.queryHash);
    if (sub === undefined) return;
    drop(sub);
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
      if (previous !== undefined) drop(previous);
      nextId += 1;
      const pending = Promise.withResolvers<DeliveryLiveBody>();
      const sub: Subscription = {
        hash,
        id: `s${nextId}`,
        pending,
        queryKey,
        revs: null,
        seq: null,
        view,
      };
      subs.set(sub.id, sub);
      byHash.set(hash, sub);
      if (socket !== null) {
        send(subscribeFrame(sub));
      } else if (cancelReconnect === undefined) {
        document.addEventListener("visibilitychange", onVisibility);
        update({ since: Date.now(), state: "reconnecting" });
        open();
      }
      return pending.promise;
    },
    state: () => current,
    onState(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    close() {
      stopCache();
      subs.clear();
      byHash.clear();
      shutDown();
    },
  };
}

const clients = new WeakMap<QueryClient, DeliveryLive>();

/** The one live client of `queryClient`, created on first use. */
export function deliveryLiveFor(queryClient: QueryClient): DeliveryLive {
  let live = clients.get(queryClient);
  if (live === undefined) {
    live = createDeliveryLive(queryClient);
    clients.set(queryClient, live);
  }
  return live;
}

/** The live connection's state, for the freshness row. */
export function useDeliveryLiveState(): DeliveryLiveState {
  const live = deliveryLiveFor(useQueryClient());
  return useSyncExternalStore(live.onState, live.state);
}
