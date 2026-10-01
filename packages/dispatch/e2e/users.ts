import type { Browser, BrowserContext, BrowserContextOptions } from "@playwright/test";

/** Signs `context` in as `login` at the harness server's dev sign-in route. The cookie it sets is
 *  the one a GitHub sign-in issues; the context's request client shares the context's cookie jar,
 *  and a relative URL resolves against the config's baseURL. The route answers a sign-in with a
 *  302, which is not followed: the redirect is the dashboard, which this does not need. The cookie
 *  names a generation in `user_sessions`, which `resetDatabase` truncates, so sign a context in
 *  after the reset. */
export async function signIn(context: BrowserContext, login: string): Promise<void> {
  const response = await context.request.get(
    `/auth/_dev/signin?login=${encodeURIComponent(login)}`,
    { maxRedirects: 0 }
  );
  if (response.status() !== 302) {
    throw new Error(
      `dev sign-in as ${login} failed: ${response.status()} ${await response.text()}`
    );
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
