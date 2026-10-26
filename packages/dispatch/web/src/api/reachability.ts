/**
 * The moments the page learns Dispatch can be reached again after it could not, which start a
 * failing write's backoff over and end its pause at once (`writeUnderKey`): Dispatch answered a
 * request with something of its own (`answer`) after a request failed to reach it or went silent
 * (`unreachable`); the browser says it is online again (`online`); the live socket opened again
 * after it was lost (`socket`). An answer while Dispatch was reachable all along - another route's
 * 2xx, a route's own 5xx - is no such moment, so a route that keeps failing backs off to its cap
 * while the page's other requests succeed.
 */
export type Reachable = "answer" | "online" | "socket";

const listeners = new Set<(reachable: Reachable) => void>();

/** Whether the page last learned Dispatch cannot be reached: a request failed to reach it or went
 *  silent, or the live socket was lost. */
let lost = false;

/** Dispatch could not be reached: a request failed at the connection or went silent, or the live
 *  socket was lost. The next answer, or socket opening, says it can be again. */
export function unreachable(): void {
  lost = true;
}

/** Tells every listener that Dispatch can be reached again, and how the page learned it: an
 *  answer or a socket opening only after it could not be (`unreachable`), the browser's `online`
 *  whenever it comes. */
export function reachable(how: Reachable): void {
  if (how !== "online" && !lost) return;
  lost = false;
  for (const listener of [...listeners]) listener(how);
}

/** Forgets that Dispatch could not be reached: the unit-test preload, between tests. */
export function resetReachability(): void {
  lost = false;
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
