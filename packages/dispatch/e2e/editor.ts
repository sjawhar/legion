import { expect, type Locator, type Page, type WebSocketRoute } from "@playwright/test";

export function documentEditor(page: Page): Locator {
  return page.getByRole("textbox", { name: "Document editor" });
}

export function actionBar(page: Page): Locator {
  return page.locator(".dispatch-action-bar");
}

export async function barAction(page: Page, label: "Comment" | "Suggest" | "Ask"): Promise<void> {
  await actionBar(page).getByRole("button", { exact: true, name: label }).click();
}

export function markSpan(page: Page, markId: string): Locator {
  return documentEditor(page).locator(`[data-id="${markId}"]`);
}

export function cursorLabel(page: Page, name: string): Locator {
  return page.locator(".proof-collab-cursor__label", { hasText: name });
}

/** Selects the first occurrence of `quote` inside the focused ProseMirror node the way a drag
 * does: a DOM Range plus the selectionchange ProseMirror's DOMObserver listens to. */
export async function selectEditorText(page: Page, quote: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate((root, quote) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const index = node.textContent?.indexOf(quote) ?? -1;
      if (index < 0) continue;
      const range = document.createRange();
      range.setStart(node, index);
      range.setEnd(node, index + quote.length);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
      return;
    }
    throw new Error(`quote is not in the editor: ${quote}`);
  }, quote);
  await actionBar(page).waitFor({ state: "visible" });
}

export async function deleteEditorText(page: Page, quote: string): Promise<void> {
  await selectEditorText(page, quote);
  await page.keyboard.press("Backspace");
}

export async function typeAtEnd(page: Page, text: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.click();
  await page.keyboard.press("Control+End");
  await page.keyboard.press("Enter");
  await page.keyboard.type(text);
}

export function marginCard(page: Page, id: string): Locator {
  return page.locator(`[data-margin-item="${id}"]`);
}

/** The thread card rendered inside a Conversation turn (the margin renders the same thread as
 *  its own card, so a bare `[data-margin-item]` lookup would match both). */
export function threadCard(page: Page, rootId: string): Locator {
  return page.locator(`[data-turn="comment:${rootId}"]`).locator(`[data-margin-item="${rootId}"]`);
}

export async function replyInThread(page: Page, rootId: string, body: string): Promise<void> {
  const card = threadCard(page, rootId);
  if ((await card.getAttribute("aria-expanded")) !== "true") {
    await card.getByRole("button").click();
  }
  const phoneThread = page.getByRole("dialog", { name: "Thread" });
  const thread = (await phoneThread.count()) === 0 ? card : phoneThread;
  const composer = thread.getByRole("form", { name: "Comment composer" });
  const reply = composer.getByRole("textbox", { name: "Reply" });
  await reply.fill(body);
  await reply.press("Control+Enter");
  await expect(composer.getByRole("textbox", { name: "Reply" })).toHaveValue("");
}
export function countDocumentSockets(page: Page): () => number {
  let count = 0;
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname.startsWith("/ws/doc/")) count += 1;
  });
  return () => count;
}

/** Document sockets the tab still holds open: one per live document, never one per visit. */
export function openDocumentSockets(page: Page): () => number {
  let open = 0;
  page.on("websocket", (socket) => {
    if (!new URL(socket.url()).pathname.startsWith("/ws/doc/")) return;
    open += 1;
    socket.on("close", () => {
      open -= 1;
    });
  });
  return () => open;
}

/**
 * Proxies the document transport so a test can drop it the way a network blip or a server
 * restart does: `sever()` closes the live document connections, leaving the client to reconnect.
 * Install it before navigating.
 *
 * This and `heldDocumentTransport` both register `page.routeWebSocket` on the same pattern, and
 * Playwright has no fallthrough for WebSocket routes: a test that installs both gets whichever
 * one it registered last, silently. Use one per test.
 */
export async function severableDocumentTransport(
  page: Page
): Promise<{ sever: () => Promise<void> }> {
  const live: WebSocketRoute[] = [];
  await page.routeWebSocket(/\/ws\/doc\//u, (route) => {
    route.connectToServer();
    live.push(route);
  });
  return {
    sever: async () => {
      for (const route of live.splice(0)) {
        await route.close({ code: 1012, reason: "transport blip" });
      }
    },
  };
}

/**
 * Holds the document transport so a test owns the landing window instead of racing it. While it
 * is held the editor never syncs, so the open document reports no layout and every anchored card
 * is still stacked at the top of the margin - the state the link's hold exists for. `release()`
 * connects what is held and anything that arrives afterwards; `hold()` starts holding again, for
 * a test whose first document has to load before the landing it is about. Install it before the
 * page's first navigation: only sockets opened afterwards are routed.
 *
 * Do not pair it with `severableDocumentTransport`: they register `page.routeWebSocket` on the
 * same pattern, Playwright has no fallthrough for WebSocket routes, and the second registration
 * silently wins.
 */
export async function heldDocumentTransport(
  page: Page,
  holding = true
): Promise<{ hold: () => void; release: () => Promise<void> }> {
  const connects: (() => void)[] = [];
  let releasing = !holding;
  await page.routeWebSocket(/\/ws\/doc\//u, (route) => {
    if (releasing) {
      route.connectToServer();
      return;
    }
    // The page's sync messages are buffered rather than dropped: the provider sends its first
    // one the moment the socket opens, and a connection that misses it never syncs at all.
    let server: WebSocketRoute | undefined;
    const pending: (string | Buffer)[] = [];
    route.onMessage((message) => {
      if (server === undefined) {
        pending.push(message);
        return;
      }
      server.send(message);
    });
    connects.push(() => {
      server = route.connectToServer();
      for (const message of pending.splice(0)) {
        server.send(message);
      }
    });
  });
  return {
    hold: () => {
      releasing = false;
    },
    release: async () => {
      releasing = true;
      for (const connect of connects.splice(0)) {
        connect();
      }
    },
  };
}
