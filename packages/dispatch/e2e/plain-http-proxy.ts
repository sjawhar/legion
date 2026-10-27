// The `chromium-plain-http` project's origin (e2e/plain-http-origin.ts): a plain-HTTP name the
// browser maps to this listener, forwarded to the server under test at that server's own loopback
// origin, which its dev sign-in host fence requires. The relay itself is e2e/http-relay.ts.
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
import { plainHttpOrigin } from "./plain-http-origin";

// The target is plain HTTP: every spec this proxy serves skips an https target (`httpsTarget` in
// e2e/plain-http-origin.ts), since it has no plain-HTTP origin to stand in for.
const target = new URL(baseUrl);
const browserHost = new URL(plainHttpOrigin).host;

Bun.serve<RelaySocket>({
  hostname: "127.0.0.1",
  // The workspace event stream is silent between the server's 15 s heartbeats, and Bun's default
  // 10 s idle timeout would cut it before the first one.
  idleTimeout: 0,
  port: harnessPorts.plainHttp.port,
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
    if (headers.get("origin") === plainHttpOrigin) headers.set("origin", target.origin);
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

console.log(`plain-HTTP proxy on ${plainHttpOrigin} for ${target.origin}`);
