import { plainHttpPort } from "./harness-ports";

/** The host name the `chromium-plain-http` project opens the harness by. Chromium resolves it to
 *  loopback through `--host-resolver-rules` (e2e/playwright.config.ts), where
 *  `e2e/plain-http-proxy.ts` listens on `PLAIN_HTTP_PORT` and forwards to the server under test.
 *  The page's origin is `http://<name>:<port>` — plain HTTP, not loopback, so not a secure
 *  context: `isSecureContext` is false and `crypto.randomUUID` is undefined, as on a LAN address,
 *  a tailnet name or the phone of the manual check. `.test` is reserved (RFC 6761) and resolves
 *  nowhere else; `.localhost` would not do, Chromium treats it as loopback. */
export const plainHttpHost = "dispatch-e2e.test";

/** The plain-HTTP project's browser origin, and so the one `Host` the proxy answers. */
export const plainHttpOrigin = `http://${plainHttpHost}:${plainHttpPort}`;
