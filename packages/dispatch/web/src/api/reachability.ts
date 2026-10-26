/**
 * The moments the page learns Dispatch can be reached again after it could not, which start a
 * failing write's backoff over and end its pause at once (`writeUnderKey`): the browser says it is
 * online again (`online`); the live socket opened again after it was lost (`socket`); Dispatch
 * answered a request on a route with something of its own (`answer`) after a request on that route
 * failed to reach it or went silent (`unreachable`). An answer is news of its own route only: a
 * write's own transport failure followed by another route's answer says nothing of the write's
 * route, so a route that keeps failing - a proxy dropping one request shape - backs off to its cap
 * while the page's other requests succeed, and so does a route answering its own 5xx.
 */
export type Reachable = "answer" | "online" | "socket";

const listeners = new Set<(reachable: Reachable, path: string | undefined) => void>();

/** The routes, by path, a request failed to reach Dispatch on or went silent on since Dispatch
 *  last answered one there. */
const lostRoutes = new Set<string>();

/** Whether the live socket was lost since it last opened. */
let socketLost = false;

/** Dispatch could not be reached: a request on `path` failed at the connection or went silent, or
 *  - with no path - the live socket was lost. The next answer on that route, or the next socket
 *  opening, says it can be again. */
export function unreachable(path?: string): void {
  if (path === undefined) socketLost = true;
  else lostRoutes.add(path);
}

/** Tells every listener that Dispatch can be reached again, and how the page learned it: an answer
 *  on `path` only after a request there could not reach it, a socket opening only after the socket
 *  was lost (`unreachable`), the browser's `online` whenever it comes. */
export function reachable(how: Reachable, path?: string): void {
  if (how === "answer" && (path === undefined || !lostRoutes.delete(path))) return;
  if (how === "socket") {
    if (!socketLost) return;
    socketLost = false;
  }
  for (const listener of [...listeners]) listener(how, how === "answer" ? path : undefined);
}

/** Forgets that Dispatch could not be reached: the unit-test preload, between tests. */
export function resetReachability(): void {
  lostRoutes.clear();
  socketLost = false;
}

const online = () => reachable("online");

/** Calls `listener` each time Dispatch can be reached, with the route an answer came on (none for
 *  `online` and `socket`), until the returned function is called. The browser's `online` is
 *  listened to only while someone is listening here. */
export function onReachable(
  listener: (how: Reachable, path: string | undefined) => void
): () => void {
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
