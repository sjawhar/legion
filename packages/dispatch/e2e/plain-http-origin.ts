/** The host name the `chromium-plain-http` project opens the harness by. Chromium resolves it to
 *  the harness host through `--host-resolver-rules` (e2e/playwright.config.ts), so the page's origin
 *  is `http://<name>:<port>` — plain HTTP, not loopback, so not a secure context: `isSecureContext`
 *  is false and `crypto.randomUUID` is undefined, as on a LAN address, a tailnet name or the phone of
 *  the manual check — while every request still reaches the listener. `.test` is reserved (RFC 6761)
 *  and resolves nowhere else; `.localhost` would not do, Chromium treats it as loopback. */
export const plainHttpHost = "dispatch-e2e.test";
