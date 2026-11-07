/** Two-process mode's origin (e2e/harness-processes.ts): the one listener on `DISPATCH_E2E_PORT`,
 *  in front of Dispatch A on `DISPATCH_E2E_PORT_A` and B on `DISPATCH_E2E_PORT_B`, as a load
 *  balancer stands in front of a deployment's two pods.
 *
 *  - Each HTTP request and each websocket upgrade goes to the other process from the last one of
 *    its kind, starting with A, so a page's document, its assets, its API calls and its sockets
 *    (its live socket among them) land on both. Its response streams back as it arrives, and its
 *    `Host` and `Origin` reach the process unchanged: both processes serve the proxy's origin as
 *    their dashboard origin (`DISPATCH_SERVER_URL`), which the dev sign-in host fence and the CSRF
 *    guard compare against.
 *  - A request carrying `X-Dispatch-E2E-Process: a|b` goes to that process; failing that, one
 *    carrying the cookie of its kind goes to the process it names: `dispatch-e2e-page=a|b` for a
 *    page request (a navigation outside `/api/`, `/auth/` and `/ws/`, `/index.html`, `/assets/*`)
 *    and `dispatch-e2e-api=a|b` for every other request (the API, its live socket and websockets,
 *    `/auth`), so a browser context runs one process's page against the other's server; failing
 *    that, one carrying the cookie `dispatch-e2e-process=a|b` does, which pins every request of a
 *    browser context, websocket upgrades included. The header wins. Any other value of any of
 *    them is refused (400), never read as no pin. A pinned request takes no turn from the
 *    alternation.
 *  - An HTTP request whose path contains `/_test/` (a `DISPATCH_TEST_HOOKS` route: close every
 *    live socket, quiesce every document) goes to both processes whatever its pin, since each
 *    holds its own connections and rooms. The answer is A's, or the first non-2xx of A's and B's.
 *  - A path containing `/_proxy/` is the proxy's own and never forwarded: `GET /_proxy/requests`
 *    lists every HTTP request forwarded (`[{path, method, upstream}]`, path with its query), `GET
 *    /_proxy/sockets` every websocket relayed (`[{id, path, upstream, open, openedAt}]` in the
 *    order they opened, `openedAt` in epoch milliseconds, `open` false once either side closed),
 *    and `POST /_proxy/reset` empties both lists and starts both alternations over at A.
 *
 *  The relay is e2e/http-relay.ts, which e2e/plain-http-proxy.ts shares. A spec reads the two
 *  lists' rows as `ProxiedRequest` and `ProxiedSocket`, imported as types only: importing this
 *  file's values starts the listener. */
import { harnessPorts } from "./harness-ports";
import {
  apiPinCookie,
  type DispatchProcess,
  dispatchProcesses,
  pagePinCookie,
  processPinCookie,
  processPinHeader,
} from "./harness-processes";
import {
  forwardedHeaders,
  isWebSocketUpgrade,
  type RelaySocket,
  relayHttp,
  relayWebSocket,
  requestBody,
  upstreamUrl,
} from "./http-relay";

/** One HTTP request the proxy forwarded, as `GET /_proxy/requests` lists it. */
export interface ProxiedRequest {
  readonly path: string;
  readonly method: string;
  readonly upstream: DispatchProcess;
}

/** One websocket the proxy relayed, as `GET /_proxy/sockets` lists it. */
export interface ProxiedSocket {
  readonly id: number;
  readonly path: string;
  readonly upstream: DispatchProcess;
  open: boolean;
  readonly openedAt: number;
}

interface RoundRobinSocket extends RelaySocket {
  readonly path: string;
  /** The process it was sent to (`upstream` is the relay's socket to it). */
  readonly target: DispatchProcess;
  /** Its row in the socket log, from the moment the browser's side opened. */
  record?: ProxiedSocket;
}

// DISPATCH_E2E_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads. A's and B's ports are set by then: the harness
// holds them before it starts this proxy (`heldAhead` in e2e/harness-listeners.ts).
const ports = harnessPorts();
for (const upstream of [ports.dispatchA, ports.dispatchB]) {
  if (upstream.port === 0) {
    throw new Error(
      `The round-robin proxy cannot start: ${upstream.variable} is unset, so it has no process ` +
        "to forward to. e2e/harness-start.ts sets it before it starts this proxy."
    );
  }
}
const upstreams: Record<DispatchProcess, { readonly name: string; readonly origin: string }> = {
  a: { name: "Dispatch A", origin: `http://127.0.0.1:${ports.dispatchA.port}` },
  b: { name: "Dispatch B", origin: `http://127.0.0.1:${ports.dispatchB.port}` },
};

let requests: ProxiedRequest[] = [];
let sockets: ProxiedSocket[] = [];
let nextSocketId = 1;
const turns = { http: 0, websocket: 0 };

function takeTurn(kind: keyof typeof turns): DispatchProcess {
  return dispatchProcesses[turns[kind]++ % dispatchProcesses.length] as DispatchProcess;
}

function isProcess(value: string): value is DispatchProcess {
  return (dispatchProcesses as readonly string[]).includes(value);
}

/** Whether `request` loads the page rather than talking to the server behind it: a navigation to
 *  an app route, `index.html`, or one of the build's `/assets/`. */
function isPageRequest(request: Request, pathname: string): boolean {
  if (/^\/(api|auth|ws)\//.test(pathname)) return false;
  return (
    pathname === "/index.html" ||
    pathname.startsWith("/assets/") ||
    request.headers.get("sec-fetch-mode") === "navigate"
  );
}

/** The process `request` is pinned to, undefined when it names none, or the refusal of a pin
 *  that names no process: the header, else the cookie of the request's kind, else the context's
 *  cookie. Every pin cookie is read, so one of the other kind that names no process is refused
 *  too. */
function pinOf(request: Request, pathname: string): DispatchProcess | undefined | Response {
  const refuse = (source: string, value: string) =>
    Response.json(
      {
        code: "E2E_PROCESS_UNKNOWN",
        error: `${source} names process ${JSON.stringify(value)}; the processes are "a" and "b"`,
      },
      { status: 400 }
    );
  const header = request.headers.get(processPinHeader);
  if (header !== null) return isProcess(header) ? header : refuse(processPinHeader, header);
  const cookies = new Map<string, DispatchProcess>();
  for (const pair of (request.headers.get("cookie") ?? "").split(";")) {
    const separator = pair.indexOf("=");
    const name = separator === -1 ? "" : pair.slice(0, separator).trim();
    if (![processPinCookie, pagePinCookie, apiPinCookie].includes(name) || cookies.has(name)) {
      continue;
    }
    const value = pair.slice(separator + 1).trim();
    if (!isProcess(value)) return refuse(`cookie ${name}`, value);
    cookies.set(name, value);
  }
  return (
    cookies.get(isPageRequest(request, pathname) ? pagePinCookie : apiPinCookie) ??
    cookies.get(processPinCookie)
  );
}

async function forward(
  request: Request,
  upstream: DispatchProcess,
  headers: Headers,
  body: ArrayBuffer | undefined
): Promise<Response> {
  const { name, origin } = upstreams[upstream];
  const url = upstreamUrl(request, origin, false);
  requests.push({ method: request.method, path: `${url.pathname}${url.search}`, upstream });
  try {
    return await relayHttp(request, url, headers, body);
  } catch (error) {
    return Response.json(
      {
        code: "E2E_PROCESS_UNREACHABLE",
        error: `${name} (${origin}) did not answer: ${error instanceof Error ? error.message : String(error)}`,
      },
      { status: 502 }
    );
  }
}

const ownRoutes: Record<string, { readonly method: string; readonly answer: () => Response }> = {
  "/_proxy/requests": { answer: () => Response.json(requests), method: "GET" },
  "/_proxy/reset": {
    answer: () => {
      requests = [];
      sockets = [];
      turns.http = 0;
      turns.websocket = 0;
      return new Response(null, { status: 204 });
    },
    method: "POST",
  },
  "/_proxy/sockets": { answer: () => Response.json(sockets), method: "GET" },
};

function answerOwn(request: Request, path: string): Response {
  const route = Object.hasOwn(ownRoutes, path) ? ownRoutes[path] : undefined;
  if (route === undefined) {
    return Response.json(
      { code: "NOT_FOUND", error: `the round-robin proxy has no route ${path}` },
      { status: 404 }
    );
  }
  if (request.method !== route.method) {
    return Response.json(
      { code: "METHOD_NOT_ALLOWED", error: `${path} answers ${route.method} only` },
      { headers: { Allow: route.method }, status: 405 }
    );
  }
  return route.answer();
}

const listener = Bun.serve<RoundRobinSocket>({
  hostname: "127.0.0.1",
  // A request the server under test is slow to answer sends nothing while it waits, and Bun's
  // default 10 s idle timeout would cut it; the browser and the server keep their own deadlines.
  idleTimeout: 0,
  port: ports.dispatch.port,
  async fetch(request, server) {
    const { pathname } = new URL(request.url);
    if (pathname.includes("/_proxy/")) return answerOwn(request, pathname);
    const pin = pinOf(request, pathname);
    if (pin instanceof Response) return pin;
    const websocket = isWebSocketUpgrade(request);
    const headers = forwardedHeaders(request.headers, websocket);
    headers.delete(processPinHeader);
    const host = request.headers.get("host");
    if (host !== null) headers.set("host", host);
    if (websocket) {
      const target = pin ?? takeTurn("websocket");
      const url = upstreamUrl(request, upstreams[target].origin, true);
      const data: RoundRobinSocket = {
        headers: Object.fromEntries(headers),
        path: `${url.pathname}${url.search}`,
        pending: [],
        target,
        url: url.href,
      };
      return server.upgrade(request, { data })
        ? undefined
        : new Response("WebSocket upgrade failed", { status: 400 });
    }
    const body = await requestBody(request);
    if (!pathname.includes("/_test/")) {
      return forward(request, pin ?? takeTurn("http"), headers, body);
    }
    const [a, b] = await Promise.all([
      forward(request, "a", headers, body),
      forward(request, "b", headers, body),
    ]);
    if (a.ok && !b.ok) {
      await a.body?.cancel();
      return b;
    }
    await b.body?.cancel();
    return a;
  },
  websocket: relayWebSocket<RoundRobinSocket>({
    closed(ws) {
      if (ws.data.record) ws.data.record.open = false;
    },
    opened(ws) {
      const { path, target } = ws.data;
      ws.data.record = {
        id: nextSocketId++,
        open: true,
        openedAt: Date.now(),
        path,
        upstream: target,
      };
      sockets.push(ws.data.record);
    },
  }),
});

console.log(
  `round-robin proxy on ${listener.url.origin} for ${upstreams.a.name} at ${upstreams.a.origin} ` +
    `and ${upstreams.b.name} at ${upstreams.b.origin}`
);
