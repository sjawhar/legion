import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { createServer, request as httpRequest, type IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

import { devSignInPath, sessionCookieName } from "./api";
import {
  httpsTarget,
  plainHttpHost,
  plainHttpOrigin,
  plainHttpSkipReason,
} from "./plain-http-origin";
import { resetDatabase } from "./seed";

// Runs only in the `chromium-plain-http` project, whose host name Chromium maps to loopback: the
// proxy that stands in for a plain-HTTP origin (e2e/plain-http-proxy.ts) must not open the server
// under test to names its own fence refuses, nor hold a live socket open upstream after its page
// goes.
const proxyModule = fileURLToPath(new URL("./plain-http-proxy.ts", import.meta.url));

test.skip(httpsTarget, plainHttpSkipReason);

test.beforeEach(async () => {
  await resetDatabase();
});

interface PlainResponse {
  readonly body: string;
  readonly headers: IncomingHttpHeaders;
  readonly status: number;
}

/** One GET to the harness proxy's listener carrying `host`, the `Host` a browser sends for the
 *  name it reached the listener by; `fetch` decides that header itself. */
function getThroughProxy(path: string, host: string): Promise<PlainResponse> {
  const { promise, reject, resolve } = Promise.withResolvers<PlainResponse>();
  httpRequest(
    { headers: { host }, host: "127.0.0.1", path, port: new URL(plainHttpOrigin).port },
    (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk: string) => {
        body += chunk;
      });
      response.on("end", () =>
        resolve({ body, headers: response.headers, status: response.statusCode ?? 0 })
      );
    }
  )
    .on("error", reject)
    .end();
  return promise;
}

test("the proxy refuses a Host that is not the plain-HTTP origin and forwards its own", async () => {
  const { host, port } = new URL(plainHttpOrigin);
  // A page DNS-rebound to loopback sends its own name. The dev sign-in server answers that name
  // 421, and the proxy sends the server the server's own Host, so the proxy must refuse it first.
  const rebound = await getThroughProxy(devSignInPath("alice"), `rebind.example:${port}`);
  expect(rebound.status).toBe(421);
  expect(JSON.parse(rebound.body)).toMatchObject({ code: "HOST_MISMATCH" });
  expect(rebound.headers["set-cookie"]).toBeUndefined();

  const own = await getThroughProxy(devSignInPath("alice"), host);
  expect(own.status).toBe(302);
  expect(
    own.headers["set-cookie"]?.some((cookie) => cookie.startsWith(`${sessionCookieName}=`))
  ).toBe(true);
});

test("a page that closes leaves no live socket open upstream of the proxy", async ({ browser }) => {
  // A stand-in upstream counts what the server under test would see: the page's live socket
  // (`/ws/live`, the one connection a Dispatch page holds open) arriving and ending. A socket the
  // proxy keeps open upstream after the page has gone is one the server keeps feeding every
  // subscription's frames, for a page nobody has open. The stand-in answers the handshake by hand
  // and ends a socket at its peer's close frame, as a WebSocket server does.
  let socketsOpened = 0;
  let socketsOpen = 0;
  const upstream = createServer((_request, response) => {
    response.writeHead(200, { "Content-Type": "text/html" });
    response.end(
      '<!doctype html><script>new WebSocket(location.origin.replace("http", "ws") + "/ws/live");</script>'
    );
  });
  upstream.on("upgrade", (request, socket) => {
    if (request.url !== "/ws/live") {
      socket.destroy();
      return;
    }
    const accept = createHash("sha1")
      .update(`${request.headers["sec-websocket-key"]}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest("base64");
    socket.write(
      `HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`
    );
    socketsOpened += 1;
    socketsOpen += 1;
    let open = true;
    const ended = () => {
      if (!open) return;
      open = false;
      socketsOpen -= 1;
    };
    socket.on("data", (frame: Buffer) => {
      // Opcode 8 is the peer's close frame.
      if ((frame[0] ?? 0) % 16 === 8) {
        ended();
        socket.end();
      }
    });
    socket.on("close", ended);
    socket.on("error", ended);
  });
  upstream.listen(0, "127.0.0.1");
  await once(upstream, "listening");
  const upstreamPort = (upstream.address() as AddressInfo).port;

  // A second proxy, on a port of its own, in front of the stand-in: the harness's own proxy
  // forwards to the server under test, whose sockets this spec cannot count.
  const probe = createServer().listen(0, "127.0.0.1");
  await once(probe, "listening");
  const proxyPort = (probe.address() as AddressInfo).port;
  probe.close();
  await once(probe, "close");
  const proxy = spawn("bun", [proxyModule], {
    env: {
      ...process.env,
      PLAIN_HTTP_PORT: String(proxyPort),
      PLAYWRIGHT_BASE_URL: `http://127.0.0.1:${upstreamPort}`,
    },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = once(proxy, "exit");
  try {
    const ready = Promise.withResolvers<void>();
    let output = "";
    proxy.stdout.setEncoding("utf8");
    proxy.stdout.on("data", (chunk: string) => {
      output += chunk;
      if (output.includes("plain-HTTP proxy on")) ready.resolve();
    });
    proxy.once("exit", (code) => {
      ready.reject(new Error(`the proxy exited ${code} before it listened: ${output}`));
    });
    proxy.once("error", ready.reject);
    await ready.promise;

    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto(`http://${plainHttpHost}:${proxyPort}/`);
    await expect.poll(() => socketsOpen).toBe(1);
    await page.close();
    await expect.poll(() => socketsOpen, { timeout: 5_000 }).toBe(0);
    expect(socketsOpened).toBe(1);
    await context.close();
  } finally {
    proxy.kill();
    await exited;
    upstream.closeAllConnections();
    upstream.close();
    await once(upstream, "close");
  }
});
