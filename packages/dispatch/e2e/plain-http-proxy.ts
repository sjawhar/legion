// The `chromium-plain-http` project's origin (e2e/plain-http-origin.ts): a plain-HTTP name the
// browser maps to this listener, forwarded to the server under test at that server's own loopback
// origin, which its dev sign-in host fence requires. The relay itself is e2e/http-relay.ts. It
// listens on PLAIN_HTTP_PORT, or with that unset on a port the OS chooses, and derives its own
// origin from the port it bound: the `plainHttpOrigin` other modules read comes from the report it
// prints below, which e2e/harness-start.ts reads. It starts after the server, so the server's
// origin is known here.
import { baseUrl } from "./api";
import { harnessPorts } from "./harness-ports";
import {
  forwardedHeaders,
  isWebSocketUpgrade,
  type RelaySocket,
  relayHttp,
  relayWebSocket,
  requestBody,
  upstreamUrl,
} from "./http-relay";
import { plainHttpOriginAt } from "./plain-http-origin";

// The target is plain HTTP: every spec this proxy serves skips an https target (`httpsTarget` in
// e2e/plain-http-origin.ts), since it has no plain-HTTP origin to stand in for.
const target = new URL(baseUrl);

const listener = Bun.serve<RelaySocket>({
  hostname: "127.0.0.1",
  // A request the server under test is slow to answer sends nothing while it waits, and Bun's
  // default 10 s idle timeout would cut it; the browser and the server keep their own deadlines.
  idleTimeout: 0,
  port: harnessPorts().plainHttp.port,
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
    const websocket = isWebSocketUpgrade(request);
    const url = upstreamUrl(request, target, websocket);
    // The upstream sees the target's own Host (the fetch sets it from the URL). The page's origin
    // is the plain-HTTP name; the dev sign-in server accepts its own loopback origin, so writes
    // are forwarded as same-origin requests to the server under test.
    const headers = forwardedHeaders(request.headers, websocket);
    if (headers.get("origin") === browserOrigin) headers.set("origin", target.origin);
    if (websocket) {
      const upgraded = server.upgrade(request, {
        data: { headers: Object.fromEntries(headers), pending: [], url: url.href },
      });
      return upgraded ? undefined : new Response("WebSocket upgrade failed", { status: 400 });
    }
    return relayHttp(request, url, headers, await requestBody(request));
  },
  websocket: relayWebSocket(),
});

// Set before any request can arrive: a handler runs on a later turn of the event loop.
const browserOrigin = plainHttpOriginAt(Number(listener.url.port));
const browserHost = new URL(browserOrigin).host;
console.log(`plain-HTTP proxy on ${browserOrigin} for ${target.origin}`);
