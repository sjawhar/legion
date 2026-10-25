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

/** What a route answers a `POST` it refuses: the status, and the code and reason the composer
 *  shows. */
export interface Refusal {
  readonly code: string;
  readonly error: string;
  readonly status: number;
}

/** Dispatch's verdict refusing a write. Every write the page makes tries a 5xx, a 429 or a lost
 *  answer again under its key until Dispatch answers, so only a verdict - a 4xx of Dispatch's
 *  own - ends one refused. */
export const WRITE_REFUSED: Refusal = {
  code: "INPUT_REFUSED",
  error: "Dispatch refused the write",
  status: 400,
};

/** What a composer says once Dispatch refuses its write with `WRITE_REFUSED`. */
export const WRITE_REFUSED_TEXT = `Couldn't send — ${WRITE_REFUSED.error}`;

/** Dispatch's verdict on a message to one session, with the code that route answers. */
export const MESSAGE_REFUSED: Refusal = {
  code: "MESSAGE_INPUT",
  error: "Dispatch refused the message",
  status: 400,
};

/** Holds every `POST` to `pattern` until the returned call, then refuses it with `refusal`,
 *  `WRITE_REFUSED` by default. A 5xx or a 429 is no refusal: the page tries it again under the
 *  write's key, so a row that would refuse with one is refused here, naming `WRITE_REFUSED`,
 *  rather than left waiting on a write that never gives up. */
export async function refusePosts(
  page: Page,
  pattern: string,
  refusal: Refusal = WRITE_REFUSED
): Promise<() => void> {
  if (refusal.status >= 500 || refusal.status === 429) {
    throw new Error(
      `refusePosts: a ${refusal.status} to ${pattern} is no refusal - the page tries it again under the write's key; refuse it with WRITE_REFUSED`
    );
  }
  const { promise: held, resolve: refuse } = Promise.withResolvers<void>();
  await page.route(pattern, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    await held;
    return route.fulfill({
      body: JSON.stringify({ code: refusal.code, error: refusal.error }),
      contentType: "application/json",
      status: refusal.status,
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

/** A 1x1 PNG, the smallest picture a browser draws. */
const ONE_PIXEL_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==";

/** Pastes a PNG named `name` into `field`, as the clipboard hands a screenshot to it (the same
 *  explicit `DataTransfer` as `pasteFile`). */
export async function pastePicture(field: Locator, name: string): Promise<void> {
  await field.evaluate(
    (node, file) => {
      const bytes = Uint8Array.from(atob(file.base64), (char) => char.charCodeAt(0));
      const data = new DataTransfer();
      data.items.add(new File([bytes], file.name, { type: "image/png" }));
      const paste = new Event("paste", { bubbles: true, cancelable: true });
      Object.defineProperty(paste, "clipboardData", { value: data });
      node.dispatchEvent(paste);
    },
    { base64: ONE_PIXEL_PNG, name }
  );
}
