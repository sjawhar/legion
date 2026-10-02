// The `chromium-plain-http` project's origin (e2e/plain-http-origin.ts): a plain-HTTP name the
// browser maps to this listener, forwarded to the server under test at that server's own loopback
// origin, which its dev sign-in host fence requires.
import { baseUrl } from "./api";
import { plainHttpPort } from "./harness-ports";
import { plainHttpOrigin } from "./plain-http-origin";

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

const target = new URL(baseUrl);
const browserHost = new URL(plainHttpOrigin).host;
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
  if (headers.get("origin") === plainHttpOrigin) headers.set("origin", target.origin);
  return headers;
}

// The target is plain HTTP: the one spec this proxy serves skips an https target
// (e2e/plain-http-origin.e2e.ts), since it has no plain-HTTP origin to stand in for.
function upstreamUrl(request: Request, websocket: boolean): URL {
  const incoming = new URL(request.url);
  const url = new URL(`${incoming.pathname}${incoming.search}`, target);
  if (websocket) url.protocol = "ws:";
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
  // The workspace event stream is silent between the server's 15 s heartbeats, and Bun's default
  // 10 s idle timeout would cut it before the first one.
  idleTimeout: 0,
  port: plainHttpPort,
  async fetch(request, server) {
    // Under dev sign-in the server answers only its own dashboard Host, so a page that reached it
    // by another name (a DNS-rebinding page, a tunnel) is told so
    // (packages/envoy/internal/dispatch/routes/devsignin.go's requireHost). This proxy is such a
    // tunnel and sends the target's Host upstream, so it keeps the same fence for its own name.
    const host = request.headers.get("host") ?? "";
    if (host.toLowerCase() !== browserHost) {
      return Response.json(
        {
          code: "HOST_MISMATCH",
          error: `request Host ${JSON.stringify(host)} is not the plain-HTTP origin ${JSON.stringify(browserHost)}`,
        },
        { status: 421 }
      );
    }
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
      // A browser that leaves (a closed page, a reconnecting event stream) ends the upstream
      // request with it, so the server under test keeps no subscriber for a page that is gone.
      signal: request.signal,
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

console.log(`plain-HTTP proxy on ${plainHttpOrigin} for ${target.origin}`);
