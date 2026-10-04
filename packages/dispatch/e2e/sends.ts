import type { Locator, Page } from "@playwright/test";

/** In-app navigation, as a link does: a reload would drop a send with the page. */
export async function navigateInApp(page: Page, path: string): Promise<void> {
  await page.evaluate((to) => {
    window.history.pushState(null, "", to);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, path);
}

/** The answer `status` to a `POST` to `path` (the end of its URL), once the page has it. */
export function answeredPost(page: Page, path: string, status: number) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(path) &&
      response.status() === status
  );
}

/** Holds every `POST` to `pattern` until `release`, as a slow server would, and counts them. */
export async function holdPosts(
  page: Page,
  pattern: string
): Promise<{ posts: () => number; release: () => void }> {
  const { promise: held, resolve: release } = Promise.withResolvers<void>();
  let posts = 0;
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    posts += 1;
    await held;
    return route.fallback();
  });
  return { posts: () => posts, release };
}

/** Holds every `POST` to `pattern` until the returned call, then refuses it, as a server that is
 *  down: 503 with a reason the composer shows. */
export async function refusePosts(page: Page, pattern: string): Promise<() => void> {
  const { promise: held, resolve: refuse } = Promise.withResolvers<void>();
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    await held;
    return route.fulfill({
      body: JSON.stringify({ code: "UNAVAILABLE", error: "the server is down" }),
      contentType: "application/json",
      status: 503,
    });
  });
  return refuse;
}

/** Pastes a text file into `field`, as the clipboard hands one to it. Firefox ignores a
 *  programmatic `ClipboardEvent`'s initializer, so the test event carries the `DataTransfer`
 *  explicitly, which every browser exposes to React's paste handler. */
export async function pasteFile(field: Locator, name: string, text: string): Promise<void> {
  await field.evaluate(
    (node, file) => {
      const data = new DataTransfer();
      data.items.add(new File([file.text], file.name, { type: "text/markdown" }));
      const paste = new Event("paste", { bubbles: true, cancelable: true });
      Object.defineProperty(paste, "clipboardData", { value: data });
      node.dispatchEvent(paste);
    },
    { name, text }
  );
}
