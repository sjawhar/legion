import { dispatchPort, plainHttpPort } from "./harness-ports";
import { plainHttpHost } from "./plain-http-origin";

interface ProxySocket {
  readonly headers: Record<string, string>;
  readonly pending: (ArrayBuffer | string | Uint8Array)[];
  upstream?: WebSocket;
  readonly url: string;
}

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

const target = new URL(process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${dispatchPort}`);
const browserOrigin = `http://${plainHttpHost}:${plainHttpPort}`;
const hopByHopHeaders = new Set([
  "connection",
  "host",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

function forwardedHeaders(source: Headers, websocket: boolean): Headers {
  const headers = new Headers();
  for (const [name, value] of source) {
    const key = name.toLowerCase();
    if (hopByHopHeaders.has(key) || (websocket && key.startsWith("sec-websocket-"))) continue;
    headers.set(name, value);
  }
  // The page's origin is the plain-HTTP name; the dev sign-in server accepts its own loopback
  // origin, so writes are forwarded as same-origin requests to the server under test.
  if (headers.get("origin") === browserOrigin) headers.set("origin", target.origin);
  const referer = headers.get("referer");
  if (referer?.startsWith(`${browserOrigin}/`)) {
    headers.set("referer", `${target.origin}${referer.slice(browserOrigin.length)}`);
  }
  return headers;
}

function upstreamUrl(request: Request, websocket: boolean): URL {
  const incoming = new URL(request.url);
  const url = new URL(`${incoming.pathname}${incoming.search}`, target);
  if (websocket) url.protocol = target.protocol === "https:" ? "wss:" : "ws:";
  return url;
}

function sendUpstream(socket: ProxySocket, message: ArrayBuffer | string | Uint8Array): void {
  if (socket.upstream?.readyState === WebSocket.OPEN) {
    socket.upstream.send(message);
  } else {
    socket.pending.push(message);
  }
}

Bun.serve<ProxySocket>({
  hostname: "127.0.0.1",
  port: plainHttpPort,
  async fetch(request, server) {
    const websocket = request.headers.get("upgrade")?.toLowerCase() === "websocket";
    const url = upstreamUrl(request, websocket);
    const headers = forwardedHeaders(request.headers, websocket);
    if (websocket) {
      const upgraded = server.upgrade(request, {
        data: { headers: Object.fromEntries(headers), pending: [], url: url.href },
      });
      return upgraded ? undefined : new Response("WebSocket upgrade failed", { status: 400 });
    }
    return fetch(url, {
      body:
        request.method === "GET" || request.method === "HEAD"
          ? undefined
          : await request.arrayBuffer(),
      headers,
      method: request.method,
      redirect: "manual",
    });
  },
  websocket: {
    close(ws) {
      ws.data.upstream?.close();
    },
    message(ws, message) {
      sendUpstream(ws.data, message);
    },
    open(ws) {
      const upstream = new BunWebSocket(ws.data.url, { headers: ws.data.headers });
      upstream.binaryType = "arraybuffer";
      upstream.addEventListener("open", () => {
        for (const message of ws.data.pending.splice(0)) upstream.send(message);
      });
      upstream.addEventListener("message", (event) => {
        ws.send(event.data as ArrayBuffer | string);
      });
      upstream.addEventListener("close", () => {
        ws.close();
      });
      upstream.addEventListener("error", () => {
        ws.close(1011, "upstream WebSocket failed");
      });
      ws.data.upstream = upstream;
    },
  },
});

console.log(`plain-HTTP proxy on ${browserOrigin} for ${target.origin}`);
