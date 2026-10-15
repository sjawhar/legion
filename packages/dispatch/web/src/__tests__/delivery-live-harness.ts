import type { QueryClient } from "@tanstack/react-query";

import { deliveryRunQuery, deliveryTimelineQuery } from "../api/queries";
import type {
  DeliveryLiveBody,
  DeliveryLiveClientFrame,
  DeliveryLiveError,
  DeliveryLiveRevs,
  DeliveryLiveServerFrame,
  DeliveryLiveSnapshot,
  DeliveryLiveSubscribe,
  DeliveryLiveView,
  DeliveryRunDetail,
  DeliveryTimelineResponse,
} from "../api/types";

type Listener = (event: Event) => void;

/** A WebSocket the test drives: `open`, `receive` and `drop` act as the server; `sent` is every
 *  frame the page sent. `close`, called by the page, only records its code, as a browser fires its
 *  close event later and the page has already let the socket go. */
export class FakeDeliverySocket {
  readyState = 0;
  readonly sent: DeliveryLiveClientFrame[] = [];
  closedWith: number | undefined;
  private readonly listeners = new Map<string, Set<Listener>>();

  constructor(
    readonly url: string,
    registry: FakeDeliverySocket[]
  ) {
    registry.push(this);
  }

  addEventListener(type: string, listener: Listener): void {
    let set = this.listeners.get(type);
    if (set === undefined) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(listener);
  }

  removeEventListener(type: string, listener: Listener): void {
    this.listeners.get(type)?.delete(listener);
  }

  send(data: string): void {
    if (this.readyState !== 1) throw new Error(`sent on a socket in state ${this.readyState}`);
    this.sent.push(JSON.parse(data) as DeliveryLiveClientFrame);
  }

  close(code = 1000): void {
    this.readyState = 3;
    this.closedWith = code;
  }

  open(): void {
    this.readyState = 1;
    this.emit("open", new Event("open"));
  }

  receive(frame: DeliveryLiveServerFrame): void {
    this.emit("message", new MessageEvent("message", { data: JSON.stringify(frame) }));
  }

  /** The server or the network ends the connection. */
  drop(code = 1006): void {
    this.readyState = 3;
    this.emit("close", Object.assign(new Event("close"), { code }));
  }

  /** The browser reports a connection error before it has delivered a close event. */
  fail(): void {
    this.emit("error", new Event("error"));
  }

  /** The subscribe frames sent, in order. */
  subscribes(): DeliveryLiveSubscribe[] {
    return this.sent.filter((frame): frame is DeliveryLiveSubscribe => frame.type === "subscribe");
  }

  /** The subscribe frame sent last for a view of `kind`. */
  subscribed(kind: DeliveryLiveView["kind"]): DeliveryLiveSubscribe {
    const frame = this.subscribes()
      .filter((sent) => sent.view.kind === kind)
      .at(-1);
    if (frame === undefined) throw new Error(`no ${kind} subscription was sent`);
    return frame;
  }

  private emit(type: string, event: Event): void {
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

/** The fake sockets a test installed: every one the page opened, in order, the last of them, and
 *  the restore that puts the real WebSocket back. */
export interface FakeDeliverySockets {
  readonly sockets: FakeDeliverySocket[];
  latest(): FakeDeliverySocket;
  restore(): void;
}

/** Replaces `WebSocket` with FakeDeliverySocket until `restore`. `latest` is the socket the page
 *  opened last. */
export function installFakeDeliverySockets(): FakeDeliverySockets {
  const original = globalThis.WebSocket;
  const sockets: FakeDeliverySocket[] = [];
  globalThis.WebSocket = class extends FakeDeliverySocket {
    constructor(url: string | URL) {
      super(String(url), sockets);
    }
  } as unknown as typeof WebSocket;
  return {
    sockets,
    latest: () => {
      const socket = sockets.at(-1);
      if (socket === undefined) throw new Error("no delivery socket was opened");
      return socket;
    },
    restore: () => {
      globalThis.WebSocket = original;
    },
  };
}

/** What a page holds after a snapshot that names no revision; a test that sends no delta never
 *  reads them. */
const NO_REVS: DeliveryLiveRevs = { items: {}, maps: {}, counts: {}, fields: {} };

/** Dispatch's first answer to subscription `sub`: `body`, as of change `seq`. */
export function snapshotFrame(
  sub: string,
  body: DeliveryLiveBody,
  options: { seq?: string; changedAt?: string | null } = {}
): DeliveryLiveSnapshot {
  return {
    type: "snapshot",
    sub,
    seq: options.seq ?? "1",
    changed_at: options.changedAt ?? null,
    body,
    revs: NO_REVS,
  };
}

/** Dispatch refusing subscription `sub` with the code the matching GET route would answer. */
export function errorFrame(sub: string, code: string, message: string): DeliveryLiveError {
  return { type: "error", sub, code, message };
}

/** A live-owned timeline query and an ordinary run read, both held in `queryClient`'s cache with
 *  data and no observer, keyed and marked (`meta`) by the page's own query options, so a test reads
 *  which of them a write invalidates. The live one is kept past its `gcTime` of 0. */
export function seedDeliveryReads(queryClient: QueryClient): {
  live: readonly unknown[];
  run: readonly unknown[];
} {
  const timeline = deliveryTimelineQuery({ kind: "timeline", window: { preset: 28 }, query: "" });
  const live: readonly unknown[] = timeline.queryKey;
  const run: readonly unknown[] = deliveryRunQuery(42).queryKey;
  const cache = queryClient.getQueryCache();
  cache.build(queryClient, {
    queryKey: live,
    meta: timeline.meta,
    gcTime: Number.POSITIVE_INFINITY,
  });
  cache.build(queryClient, { queryKey: run });
  queryClient.setQueryData(live, {} as DeliveryTimelineResponse);
  queryClient.setQueryData(run, { id: 42, jobs: [] } as unknown as DeliveryRunDetail);
  return { live, run };
}
