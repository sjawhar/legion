import { expect, test } from "@playwright/test";

import { baseUrl } from "./api";
import { type DispatchProcess, pinnedTo, processPinCookie } from "./harness-processes";
import { oidcSignIn } from "./harness-signin";
import type { ProxiedRequest } from "./round-robin-proxy";
import { resetDatabase } from "./seed";

/**
 * A deployment runs Dispatch as several processes behind one load balancer, so the browser's
 * return from the issuer at `/auth/callback` may reach another process than the one that sent it
 * there from `/auth/start`. The sign-in in flight must therefore travel with the browser rather
 * than live in the memory of the process that started it, or the other process answers 400
 * "invalid or expired state". Here, under `DISPATCH_E2E_SIGNIN=oidc` (e2e/harness-signin.ts),
 * both processes sign people in through the dev OIDC issuer the harness runs, and the browser is
 * sent to Dispatch A for `/auth/start` and to Dispatch B for `/auth/callback`.
 *
 * The pin is the round-robin proxy's cookie, set twice with a path apiece: the browser sends the
 * cookie whose path matches each request, so `/auth/start` goes to A and `/auth/callback` to B
 * however the issuer's redirect chain runs, and the page it lands on goes wherever the proxy's
 * turn sends it. A route's header could pin only the navigation's first request, never a request
 * a redirect makes.
 */

/** The page the sign-in returns to, and what shows it rendered signed in: the shell renders a page
 *  only for a person whose `/auth/whoami` answered (AuthGate). */
const next = "/settings";

test.beforeEach(async () => {
  await resetDatabase();
  const reset = await fetch(new URL("/_proxy/reset", baseUrl), { method: "POST" });
  expect(reset.status, "the round-robin proxy's log reset").toBe(204);
});

test("a sign-in started on Dispatch A completes on Dispatch B", async ({ browser }) => {
  const { hostname } = new URL(baseUrl);
  const context = await browser.newContext();
  await context.addCookies([
    { domain: hostname, name: processPinCookie, path: "/auth/start", value: "a" },
    { domain: hostname, name: processPinCookie, path: "/auth/callback", value: "b" },
  ]);
  const page = await context.newPage();
  const callback = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/auth/callback"
  );

  await page.goto(`/auth/start?next=${encodeURIComponent(next)}`);

  const answered = await callback;
  // A redirect's body cannot be read; any other answer's body says why the sign-in stopped.
  const why = answered.status() === 302 ? "" : ` ${await answered.text()}`;
  expect(answered.status(), `/auth/callback answered ${answered.status()}${why}`).toBe(302);
  await expect(page).toHaveURL(new URL(next, baseUrl).href);
  await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();

  const served = (await (
    await fetch(new URL("/_proxy/requests", baseUrl))
  ).json()) as ProxiedRequest[];
  const [start, callbackServed] = ["/auth/start", "/auth/callback"].map((path) =>
    served
      .filter((request) => new URL(request.path, baseUrl).pathname === path)
      .map((request) => request.upstream)
  );
  expect(start, "the processes that answered /auth/start").toEqual(["a"]);
  expect(callbackServed, "the processes that answered /auth/callback").toEqual(["b"]);

  // The session cookie the callback set names the person on whichever process a request reaches.
  const whoami = async (target?: DispatchProcess) => {
    const response = await context.request.get("/auth/whoami", {
      headers: target === undefined ? {} : pinnedTo(target),
    });
    return { body: await response.json(), status: response.status() };
  };
  // Every answer carries the event log's head as a decimal id, the cursor a page's event feed
  // starts from (`AuthenticatedUser.event_head`).
  const signedIn = {
    body: { event_head: expect.stringMatching(/^\d+$/), kind: "user", login: oidcSignIn.person },
    status: 200,
  };
  expect(await whoami(), "GET /auth/whoami").toEqual(signedIn);
  expect(await whoami("a"), "GET /auth/whoami pinned to Dispatch A").toEqual(signedIn);
  expect(await whoami("b"), "GET /auth/whoami pinned to Dispatch B").toEqual(signedIn);

  await context.close();
});
