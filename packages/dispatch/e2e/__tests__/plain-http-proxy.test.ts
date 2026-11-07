import { afterAll, beforeAll, expect, test } from "bun:test";
import { request as httpRequest, type IncomingHttpHeaders } from "node:http";
import { fileURLToPath } from "node:url";
import type { Server, Subprocess } from "bun";

import { devSignInPath, sessionCookieName } from "../api";
import { withoutHarnessPort } from "../harness-ports";
import { plainHttpHost } from "../plain-http-origin";

// The harness proxy that stands in for a plain-HTTP origin (e2e/plain-http-proxy.ts) is the
// browser harness's own fixture, and its host fence is its own: no browser varies it. The proxy
// sends the server under test that server's own Host, which the dev sign-in route requires, so a
// page that reached the proxy by another name (a DNS-rebinding page, a tunnel) must be refused
// here rather than tunnelled in. A stand-in upstream of this file's own records what the server
// under test would have seen, which the browser row could not: a refused request reaches no
// upstream at all. The upstream half - that the real dev sign-in route answers 421 for a foreign
// Host and 302 for its own - is Go's:
// packages/envoy/internal/dispatch/routes/devsignin_test.go:278-291.
//
// The proxy runs as the harness runs it, a `bun` process of its own, with the plain-HTTP port
// unset under every name this lane carries it under so it binds a port the OS chooses, and
// `PLAYWRIGHT_BASE_URL` naming the stand-in as the server under test. The stand-in answers with
// Bun's own Response, which the test preload (web/src/__tests__/setup.ts) does not replace, for
// the reason e2e/__tests__/round-robin-proxy.test.ts gives.
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;
const proxyModule = fileURLToPath(new URL("../plain-http-proxy.ts", import.meta.url));

/** One request the stand-in upstream saw. */
interface Seen {
  readonly host: string | null;
  readonly path: string;
}

const seen: Seen[] = [];
let upstream: Server<undefined>;
let proxy: Subprocess<"ignore", "pipe", "inherit">;
let proxyPort = 0;

beforeAll(async () => {
  upstream = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      seen.push({ host: request.headers.get("host"), path: `${url.pathname}${url.search}` });
      if (url.pathname !== "/auth/_dev/signin")
        return new NativeResponse("no route", { status: 404 });
      // The dev sign-in route's own answer: a 302 that sets the session cookie.
      return new NativeResponse(null, {
        headers: {
          Location: "/",
          "Set-Cookie": `${sessionCookieName}=stand-in; Path=/; HttpOnly; SameSite=Lax`,
        },
        status: 302,
      });
    },
  });
  proxy = Bun.spawn(["bun", proxyModule], {
    env: { ...withoutHarnessPort("plainHttp"), PLAYWRIGHT_BASE_URL: upstream.url.origin },
    stderr: "inherit",
    stdout: "pipe",
  });
  // The proxy logs one line once it listens, naming the origin it bound. The pattern opens with
  // literal text, so it matches nothing until the first chunk arrives; one decoder serves every
  // chunk, holding nothing between calls.
  const listens = /plain-HTTP proxy on http:\/\/[^/:]+:(\d+) /;
  const decoder = new TextDecoder();
  const reader = proxy.stdout.getReader();
  let logged = "";
  let listening: RegExpExecArray | null = null;
  while (listening === null) {
    const { done, value } = await reader.read();
    if (done) throw new Error(`the proxy exited before it listened: ${logged}`);
    logged += decoder.decode(value);
    listening = listens.exec(logged);
  }
  reader.releaseLock();
  proxyPort = Number(listening[1]);
});

afterAll(async () => {
  proxy.kill();
  await proxy.exited;
  upstream.stop(true);
});

interface PlainResponse {
  readonly body: string;
  readonly headers: IncomingHttpHeaders;
  readonly status: number;
}

/** One GET to the proxy's listener carrying `host`, the `Host` a browser sends for the name it
 *  reached the listener by; `fetch` decides that header itself. */
function getThroughProxy(path: string, host: string): Promise<PlainResponse> {
  const { promise, reject, resolve } = Promise.withResolvers<PlainResponse>();
  httpRequest({ headers: { host }, host: "127.0.0.1", path, port: proxyPort }, (response) => {
    let body = "";
    response.setEncoding("utf8");
    response.on("data", (chunk: string) => {
      body += chunk;
    });
    response.on("end", () =>
      resolve({ body, headers: response.headers, status: response.statusCode ?? 0 })
    );
  })
    .on("error", reject)
    .end();
  return promise;
}

test("the proxy refuses a Host that is not the plain-HTTP origin and sends nothing upstream", async () => {
  seen.length = 0;

  const rebound = await getThroughProxy(devSignInPath("alice"), `rebind.example:${proxyPort}`);

  expect(rebound.status).toBe(421);
  expect(JSON.parse(rebound.body)).toMatchObject({ code: "HOST_MISMATCH" });
  expect(rebound.headers["set-cookie"]).toBeUndefined();
  expect(seen).toEqual([]);
});

test("the proxy relays its own Host, with the upstream's status and cookie unchanged", async () => {
  seen.length = 0;

  const own = await getThroughProxy(devSignInPath("alice"), `${plainHttpHost}:${proxyPort}`);

  expect(own.status).toBe(302);
  expect(
    own.headers["set-cookie"]?.some((cookie) => cookie.startsWith(`${sessionCookieName}=`))
  ).toBe(true);
  // The upstream saw the request once, under its own Host: the server under test answers only
  // that name.
  expect(seen).toEqual([{ host: `127.0.0.1:${upstream.port}`, path: devSignInPath("alice") }]);
});
