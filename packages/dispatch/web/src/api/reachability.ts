/**
 * The moments the page learns Dispatch can be reached again, which end a waiting write's pause at
 * once (`writeUnderKey`): Dispatch answered a request - any request, a read included - with
 * something of its own (`answer`); the browser says it is online again (`online`); the live socket
 * opened (`socket`). A write waiting out a long backoff after an outage would otherwise keep the
 * reader looking at "not confirmed" for up to its whole pause while the network is fine.
 */
export type Reachable = "answer" | "online" | "socket";

const listeners = new Set<(reachable: Reachable) => void>();

/** Tells every listener that Dispatch can be reached, and how the page learned it. */
export function reachable(how: Reachable): void {
  for (const listener of [...listeners]) listener(how);
}

const online = () => reachable("online");

/** Calls `listener` each time Dispatch can be reached, until the returned function is called. The
 *  browser's `online` is listened to only while someone is listening here. */
export function onReachable(listener: (how: Reachable) => void): () => void {
  if (listeners.size === 0 && typeof window !== "undefined") {
    window.addEventListener("online", online);
  }
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && typeof window !== "undefined") {
      window.removeEventListener("online", online);
    }
  };
}
