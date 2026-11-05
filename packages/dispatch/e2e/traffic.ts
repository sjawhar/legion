import type { Page, Request } from "@playwright/test";

/** What one page sends: its GETs under `/api/` since the last `reset`, every read of one issue's
 *  detail since the page opened, and whether any request is still in flight. The event feed rides
 *  the page's live socket (`/ws/live`), which Playwright reports as a websocket and never as a
 *  request, so none of these counts it. */
export interface Traffic {
  /** Path and query of each GET under `/api/` since the last `reset`. */
  readonly gets: string[];
  /** How many times `GET /api/v1/issues/<key>` (the full issue, not its summary) was sent. */
  issueReads(key: string): number;
  /** Whether no request has been in flight for `ms`. */
  quietFor(ms: number): boolean;
  reset(): void;
}

export function watchTraffic(page: Page): Traffic {
  const gets: string[] = [];
  const issueReads = new Map<string, number>();
  const inFlight = new Set<Request>();
  let lastSettledAt = Date.now();
  page.on("request", (request) => {
    inFlight.add(request);
    const url = new URL(request.url());
    if (request.method() !== "GET" || !url.pathname.startsWith("/api/")) return;
    gets.push(`${url.pathname}${url.search}`);
    const issue = /^\/api\/v1\/issues\/([^/]+)$/.exec(url.pathname)?.[1];
    if (issue !== undefined) {
      const key = decodeURIComponent(issue);
      issueReads.set(key, (issueReads.get(key) ?? 0) + 1);
    }
  });
  const settle = (request: Request) => {
    if (inFlight.delete(request)) lastSettledAt = Date.now();
  };
  page.on("requestfinished", settle);
  page.on("requestfailed", settle);
  return {
    gets,
    issueReads: (key) => issueReads.get(key) ?? 0,
    quietFor: (ms) => inFlight.size === 0 && Date.now() - lastSettledAt >= ms,
    reset: () => {
      gets.length = 0;
    },
  };
}

/** One request under `/api/`, as `watchApiRequests` counts it. */
export interface ApiRequest {
  readonly method: string;
  /** Path and query, as sent. */
  readonly path: string;
  /** The path with its ids replaced, which is what `byRoute` groups by. */
  readonly route: string;
}

/** How many requests one route took, for the per-route breakdown. */
export interface RouteCount {
  readonly count: number;
  readonly route: string;
}

/** Every request the page sends under `/api/`, whatever its method: the total and the count per
 *  route, which is what a request budget is read against. A caller measures one load by calling
 *  `reset()` at the navigation and reading the counts at network idle. */
export interface ApiRequests {
  /** The count per route, most sent first and then by route, so two runs print one order. */
  byRoute(): readonly RouteCount[];
  /** Every request since the last `reset`, in the order the page sent them. */
  readonly requests: readonly ApiRequest[];
  reset(): void;
  /** How many requests under `/api/` were sent since the last `reset`. */
  total(): number;
}

/**
 * The route one `/api/` path belongs to: the path with every id replaced by `:id`, and the query
 * dropped, so `GET /api/v1/issues/LAY-3/references?to=…` is the one route
 * `/api/v1/issues/:id/references` whatever its query. A segment is read as an id by its shape,
 * because the path alone says nothing about which collection addresses by id: a route's own words
 * are lowercase letters and hyphens (`references`, `architecture-sources`), the API version is
 * `v<digits>`, and anything else - an issue key (`LAY-3`), a numeric id, a UUID, a session id -
 * is an id, as is a hex token of seven characters or more, which is a commit (no route word is
 * hex throughout). A name made of lowercase words - a document slug, a repository owner - is
 * therefore its own route rather than an id, which is why this is read as a grouping and never as
 * a route table.
 */
export function apiRoute(pathname: string): string {
  return pathname
    .split("/")
    .map((segment) =>
      segment === "" ||
      /^v\d+$/.test(segment) ||
      (/^[a-z][a-z-]*$/.test(segment) && !/^[0-9a-f]{7,}$/.test(segment))
        ? segment
        : ":id"
    )
    .join("/");
}

export function watchApiRequests(page: Page): ApiRequests {
  const requests: ApiRequest[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (!url.pathname.startsWith("/api/")) return;
    requests.push({
      method: request.method(),
      path: `${url.pathname}${url.search}`,
      route: apiRoute(url.pathname),
    });
  });
  return {
    byRoute: () => {
      const counts = new Map<string, number>();
      for (const request of requests) {
        counts.set(request.route, (counts.get(request.route) ?? 0) + 1);
      }
      return [...counts]
        .map(([route, count]) => ({ count, route }))
        .sort((a, b) => b.count - a.count || a.route.localeCompare(b.route));
    },
    requests,
    reset: () => {
      requests.length = 0;
    },
    total: () => requests.length,
  };
}
