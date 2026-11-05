import type { Browser, BrowserContext, BrowserContextOptions, Page } from "@playwright/test";

import { devSignInError, devSignInPath, reusableSessionCookie } from "./api";
import { measured } from "./measure";

/** Signs `context` in as `login`, with the cookie an issuer sign-in issues.
 *
 *  The first sign-in of alice or bob since the last reset carries the cookie e2e/api.ts already
 *  holds for that login into the context (`reusableSessionCookie`), rather than asking the
 *  harness server for one: the reset re-created the `user_sessions` row that cookie names
 *  (e2e/seed.ts), so it authenticates again, and one cache serves both the row's API seeds and
 *  its browser contexts, which therefore act as the same session. The row still starts clean: the
 *  cookie names a login and a generation the emptied database holds again, and no row, document
 *  or fake state of the row before reaches this one.
 *
 *  Every other sign-in mints at the harness server's dev sign-in route, as every sign-in did: the
 *  context's request client shares the context's cookie jar, and a relative URL resolves against
 *  the config's baseURL. The route answers a sign-in with a 302, which is not followed: the
 *  redirect is the dashboard, which this does not need. */
export async function signIn(context: BrowserContext, login: string): Promise<void> {
  await measured("sign-in", async () => {
    const reusable = reusableSessionCookie(login);
    if (reusable !== undefined) {
      await context.addCookies([await reusable]);
      return;
    }
    const response = await context.request.get(devSignInPath(login), { maxRedirects: 0 });
    if (response.status() !== 302) {
      throw devSignInError(login, response.status(), await response.text());
    }
  });
}

export async function asUser(
  browser: Browser,
  login: string,
  options: BrowserContextOptions = {}
): Promise<BrowserContext> {
  const context = await browser.newContext(options);
  await signIn(context, login);
  return context;
}

/** Opens a page signed in as alice for `check`, and closes her context once it returns. */
export async function withAlice(
  browser: Browser,
  check: (page: Page) => Promise<void>
): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

/** `asUser` for the `chromium-plain-http` project: a context signed in as `login` by opening the
 *  dev sign-in route in a page rather than through `context.request`. The project's host name
 *  resolves only inside Chromium (its `--host-resolver-rules`), while the request client resolves
 *  names in Node, which answers `ENOTFOUND`. A sign-in the route refuses is not redirected, so the
 *  page's own answer, with its body, is the refusal. */
export async function asPlainHttpUser(browser: Browser, login: string): Promise<BrowserContext> {
  const context = await browser.newContext();
  const page = await context.newPage();
  const landed = await measured("sign-in", () => page.goto(devSignInPath(login)));
  const signInResponse = (await landed?.request().redirectedFrom()?.response()) ?? landed;
  if (signInResponse?.status() !== 302) {
    throw devSignInError(
      login,
      signInResponse?.status() ?? 0,
      signInResponse ? await signInResponse.text() : ""
    );
  }
  await page.close();
  return context;
}
