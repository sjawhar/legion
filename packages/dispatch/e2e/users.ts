import type { Browser, BrowserContext } from "@playwright/test";

export function asUser(browser: Browser, login: string): Promise<BrowserContext> {
  return browser.newContext({
    extraHTTPHeaders: { "X-Dispatch-User": login },
  });
}
