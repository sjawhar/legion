import type { Browser, BrowserContext, BrowserContextOptions } from "@playwright/test";

import { devSignInError, devSignInPath } from "./api";

/** Signs `context` in as `login` at the harness server's dev sign-in route. The cookie it sets is
 *  the one a sign-in-pool sign-in issues; the context's request client shares the context's cookie jar,
 *  and a relative URL resolves against the config's baseURL. The route answers a sign-in with a
 *  302, which is not followed: the redirect is the dashboard, which this does not need. The cookie
 *  names a generation in `user_sessions`, which `resetDatabase` truncates, so sign a context in
 *  after the reset. */
export async function signIn(context: BrowserContext, login: string): Promise<void> {
  const response = await context.request.get(devSignInPath(login), { maxRedirects: 0 });
  if (response.status() !== 302) {
    throw devSignInError(login, response.status(), await response.text());
  }
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

/** `asUser` for the `chromium-plain-http` project: a context signed in as `login` by opening the
 *  dev sign-in route in a page rather than through `context.request`. The project's host name
 *  resolves only inside Chromium (its `--host-resolver-rules`), while the request client resolves
 *  names in Node, which answers `ENOTFOUND`. A sign-in the route refuses is not redirected, so the
 *  page's own answer, with its body, is the refusal. */
export async function asPlainHttpUser(browser: Browser, login: string): Promise<BrowserContext> {
  const context = await browser.newContext();
  const page = await context.newPage();
  const landed = await page.goto(devSignInPath(login));
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
