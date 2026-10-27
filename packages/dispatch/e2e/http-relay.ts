/** The relay both harness proxies forward through: e2e/plain-http-proxy.ts (the plain-HTTP
 *  project's origin, one upstream) and e2e/round-robin-proxy.ts (two-process mode, two). It
 *  forwards an HTTP request with its response streamed back as it arrives, so an event stream
 *  flows through it, and a websocket both ways. Which upstream a request goes to, and the `Host`
 *  and `Origin` it carries there, are each proxy's own. Nothing here runs on import. */
import type { ServerWebSocket, WebSocketHandler } from "bun";

type Message = ArrayBuffer | string | Uint8Array;

/** One relayed websocket: the upstream URL and headers it opens with, what the browser sent
 *  before the upstream opened, and the upstream once it exists. */
export interface RelaySocket {
  readonly headers: Record<string, string>;
  readonly pending: Message[];
  upstream?: WebSocket;
  readonly url: string;
}

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

const hopByHopHeaders: Record<string, true> = {
  connection: true,
  host: true,
  "keep-alive": true,
  "proxy-authenticate": true,
  "proxy-authorization": true,
  te: true,
  trailer: true,
  "transfer-encoding": true,
  upgrade: true,
};

/** `source` without its hop-by-hop headers, nor a websocket handshake's own, which the upstream
 *  client makes anew. `Host` is among those dropped: each proxy sets the one its upstream sees. */
export function forwardedHeaders(source: Headers, websocket: boolean): Headers {
  const headers = new Headers();
  for (const [name, value] of source) {
    const key = name.toLowerCase();
    if (Object.hasOwn(hopByHopHeaders, key) || (websocket && key.startsWith("sec-websocket-"))) {
      continue;
    }
    headers.set(name, value);
  }
  return headers;
}

export function isWebSocketUpgrade(request: Request): boolean {
  return request.headers.get("upgrade")?.toLowerCase() === "websocket";
}

/** `request`'s path and query on `origin`, as `ws:` for a websocket. The upstreams are plain
 *  HTTP: the harness serves no https target. */
export function upstreamUrl(request: Request, origin: string | URL, websocket: boolean): URL {
  const incoming = new URL(request.url);
  const url = new URL(`${incoming.pathname}${incoming.search}`, origin);
  if (websocket) url.protocol = "ws:";
  return url;
}

/** `request`'s body, read once, so a request sent to two upstreams carries it to both. */
export async function requestBody(request: Request): Promise<ArrayBuffer | undefined> {
  return request.method === "GET" || request.method === "HEAD"
    ? undefined
    : await request.arrayBuffer();
}

/** Forwards one HTTP request to `url`. The response is the upstream's own, redirects included,
 *  and its body streams back as the upstream writes it. */
export function relayHttp(
  request: Request,
  url: URL,
  headers: Headers,
  body: ArrayBuffer | undefined
): Promise<Response> {
  return fetch(url, {
    body,
    headers,
    method: request.method,
    redirect: "manual",
    // A browser that leaves (a closed page, a reconnecting event stream) ends the upstream
    // request with it, so the server under test keeps no subscriber for a page that is gone.
    signal: request.signal,
  });
}

// A close code one side sent, if the other side may send it too: 1005 and 1006 only ever
// describe a close and are never sent, and a client may send only 1000 and 3000-4999.
function sendableCloseCode(code: number, client: boolean): number | undefined {
  if (code === 1000 || (code >= 3000 && code <= 4999)) return code;
  if (client) return undefined;
  return (code >= 1001 && code <= 1003) || (code >= 1007 && code <= 1014) ? code : undefined;
}

function sendUpstream(socket: RelaySocket, message: Message): void {
  if (socket.upstream?.readyState === WebSocket.OPEN) {
    socket.upstream.send(message);
  } else {
    socket.pending.push(message);
  }
}

/** What a proxy learns of each relayed websocket's life. */
export interface RelaySocketHooks<T extends RelaySocket> {
  /** The browser's side opened; its upstream is being opened. */
  readonly opened?: (ws: ServerWebSocket<T>) => void;
  /** The browser's side closed, whichever side closed first. */
  readonly closed?: (ws: ServerWebSocket<T>) => void;
}

/** `Bun.serve`'s websocket handlers for the sockets a proxy's `fetch` upgraded with `RelaySocket`
 *  data: each opens its upstream, relays every message both ways, holding what the browser sends
 *  until the upstream opens, and closes each side with the other, passing on the close code
 *  where the other side may send it (a server's 1012 restart, say). */
export function relayWebSocket<T extends RelaySocket>(
  hooks: RelaySocketHooks<T> = {}
): WebSocketHandler<T> {
  return {
    close(ws, code, reason) {
      const forwarded = sendableCloseCode(code, true);
      if (forwarded === undefined) ws.data.upstream?.close();
      else ws.data.upstream?.close(forwarded, reason);
      hooks.closed?.(ws);
    },
    message(ws, message) {
      sendUpstream(ws.data, message);
    },
    open(ws) {
      hooks.opened?.(ws);
      const upstream = new BunWebSocket(ws.data.url, { headers: ws.data.headers });
      upstream.binaryType = "arraybuffer";
      upstream.addEventListener("open", () => {
        for (const message of ws.data.pending.splice(0)) upstream.send(message);
      });
      upstream.addEventListener("message", (event) => {
        ws.send(event.data as ArrayBuffer | string);
      });
      upstream.addEventListener("close", (event) => {
        const forwarded = sendableCloseCode(event.code, false);
        if (forwarded === undefined) ws.close();
        else ws.close(forwarded, event.reason);
      });
      upstream.addEventListener("error", () => {
        ws.close(1011, "upstream WebSocket failed");
      });
      ws.data.upstream = upstream;
    },
  };
}
