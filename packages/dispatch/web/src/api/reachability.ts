/**
 * The moments the page learns Dispatch can be reached again after it could not, which start a
 * failing write's backoff over and end its pause at once (`writeUnderKey`): the browser says it is
 * online again (`online`); the live socket opened again after it was lost (`socket`); Dispatch
 * answered a request on a route with something of its own (`answer`) after a request on that route
 * failed to reach it or went silent (`unreachable`). A route is a method and a path (`routeOf`),
 * and an answer is news of its own route only: a write's own transport failure followed by another
 * route's answer - a read of the same path among them, which the page makes on every live event -
 * says nothing of the write's route, so a route that keeps failing - a proxy dropping one request
 * shape - backs off to its cap while the page's other requests succeed, and so does a route
 * answering its own 5xx.
 */
export type Reachable = "answer" | "online" | "socket";

const listeners = new Set<(reachable: Reachable, route: string | undefined) => void>();

/** The routes (`routeOf`) a write failed to reach Dispatch on or went silent on since Dispatch
 *  last answered one there. Only a write's route: a write is the only thing that waits to learn
 *  its route can be reached again (`onReachable`), and a read's route - one per query string -
 *  would grow this set for a page's whole life without anything reading it. */
const lostRoutes = new Set<string>();

/** Whether the live socket was lost since it last opened. */
let socketLost = false;

/** The route of a request: its method and its path, `PATCH /api/v1/issues/CORE-1`. A `GET` of a
 *  path is not a `PATCH` of it: a proxy can drop one and pass the other. */
export function routeOf(method: string | undefined, path: string): string {
  return `${(method ?? "GET").toUpperCase()} ${path}`;
}

/** Dispatch could not be reached: a request on `route` (`routeOf`) failed at the connection or
 *  went silent, or - with no route - the live socket was lost. The next answer on that route, or
 *  the next socket opening, says it can be again. A read's route is not kept: nothing waits on
 *  it. */
export function unreachable(route?: string): void {
  if (route === undefined) {
    socketLost = true;
    return;
  }
  if (!route.startsWith("GET ") && !route.startsWith("HEAD ")) lostRoutes.add(route);
}

/** Tells every listener that Dispatch can be reached again, and how the page learned it: an answer
 *  on `route` only after a request there could not reach it, a socket opening only after the
 *  socket was lost (`unreachable`), the browser's `online` whenever it comes. The route is
 *  forgotten as it answers, so the set holds only the routes still unreachable. */
export function reachable(how: Reachable, route?: string): void {
  if (how === "answer" && (route === undefined || !lostRoutes.delete(route))) return;
  if (how === "socket") {
    if (!socketLost) return;
    socketLost = false;
  }
  for (const listener of [...listeners]) listener(how, how === "answer" ? route : undefined);
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
  listener: (how: Reachable, route: string | undefined) => void
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
