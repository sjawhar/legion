import { harnessPorts } from "./harness-ports";

/** The host name the `chromium-plain-http` project opens the harness by. Chromium resolves it to
 *  loopback through `--host-resolver-rules` (e2e/playwright.config.ts), where
 *  `e2e/plain-http-proxy.ts` listens on `PLAIN_HTTP_PORT` and forwards to the server under test.
 *  The page's origin is `http://<name>:<port>` — plain HTTP, not loopback, so not a secure
 *  context: `isSecureContext` is false and `crypto.randomUUID` is undefined, as on a LAN address,
 *  a tailnet name or the phone of the manual check. `.test` is reserved (RFC 6761) and resolves
 *  nowhere else; `.localhost` would not do, Chromium treats it as loopback. */
export const plainHttpHost = "dispatch-e2e.test";

/** The plain-HTTP origin of a proxy listening on `port`, and so the one `Host` that proxy answers.
 *  `e2e/plain-http-proxy.ts` passes the port it bound, which the OS chose when `PLAIN_HTTP_PORT`
 *  is unset. */
export function plainHttpOriginAt(port: number): string {
  return `http://${plainHttpHost}:${port}`;
}

/** The plain-HTTP project's browser origin: the harness proxy's, from `PLAIN_HTTP_PORT`, which
 *  `e2e/run.ts` exports from the proxy's report before Playwright starts. */
export const plainHttpOrigin = plainHttpOriginAt(harnessPorts().plainHttp.port);

/** This run targets a deployed https server, which the proxy cannot stand in front of: it forwards
 *  over plain HTTP. Every spec the `chromium-plain-http` project runs skips such a run with
 *  `plainHttpSkipReason`. */
export const httpsTarget = process.env.PLAYWRIGHT_BASE_URL?.startsWith("https:") === true;
export const plainHttpSkipReason = "a deployed https server has no plain-HTTP origin to map";
