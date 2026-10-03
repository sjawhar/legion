import {
  type Browser,
  expect,
  type Locator,
  type Page,
  type WebSocketRoute,
} from "@playwright/test";

import { createIssue, createProject, getArtifactText } from "./api";
import { asUser } from "./users";

export function documentEditor(page: Page): Locator {
  return page.getByRole("textbox", { name: "Document editor" });
}

/** The document's connection dot once it is connected. Its accessible name is its `title`
 * (ConnectionDot.tsx), which reads "connected" only in that state, so this matches nothing while
 * the document connects and none of the shell skeleton's loading statuses. */
export function connectedDot(scope: Page | Locator): Locator {
  return scope.getByRole("status", { name: "connected" });
}

/** Opens an issue's spec in page and waits for the update its editor makes on its own as it opens
 * the document: an id for each heading an agent wrote, which changes the document's full-state
 * token and none of its text. The spec needs a heading. Returns the token after that update. */
export async function openSpecAndAwaitHeadingIds(
  page: Page,
  issueKey: string,
  artifactId: string,
  text: string
): Promise<string | undefined> {
  const before = (await getArtifactText(artifactId)).token;
  await page.goto(`/issues/${issueKey}/spec`);
  await expect(documentEditor(page)).toContainText(text);
  let after = before;
  await expect
    .poll(async () => {
      after = (await getArtifactText(artifactId)).token;
      return after;
    })
    .not.toBe(before);
  return after;
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

/** The text of each mark among `spans`, keyed by mark id: a mark another mark nests inside renders
 *  as more than one span, and an outer span's text includes the spans inside it, so a mark's text
 *  is its own spans' text joined in document order. */
export function markTexts(spans: Locator): Promise<Record<string, string>> {
  return spans.evaluateAll((elements) => {
    const texts: Record<string, string> = {};
    for (const element of elements) {
      const id = element.getAttribute("data-id") ?? "";
      texts[id] = (texts[id] ?? "") + (element.textContent ?? "");
    }
    return texts;
  });
}

/** Waits until `page`'s editor shows the mark `markId` over exactly `quote`, whether it renders as
 *  one span or, with another mark nested inside it, as several. */
export async function expectMark(page: Page, markId: string, quote: string): Promise<void> {
  const spans = markSpan(page, markId);
  await expect(spans.first()).toBeVisible();
  await expect.poll(async () => (await markTexts(spans))[markId] ?? "").toBe(quote);
}

export function cursorLabel(page: Page, name: string): Locator {
  return page.locator(".proof-collab-cursor__label", { hasText: name });
}

/** Selects the first occurrence of `quote` inside the focused ProseMirror node the way a drag
 * does: a DOM Range plus the selectionchange ProseMirror's DOMObserver listens to. */
export async function selectEditorText(page: Page, quote: string): Promise<void> {
  await setEditorRange(page, { extent: "whole", quote });
  await actionBar(page).waitFor({ state: "visible" });
}

/** Puts the caret directly before or after the first occurrence of `quote`, the way a click
 * there does. */
export function placeCaret(page: Page, edge: "before" | "after", quote: string): Promise<void> {
  return setEditorRange(page, { extent: edge, quote });
}

/** Over, before or after the first occurrence of `quote`, or the end of the last text a caret can
 * enter: a collaborator's cursor label and an atom's text sit in `contenteditable="false"`. */
type EditorRange = { extent: "whole" | "before" | "after"; quote: string } | { extent: "end" };

/** Sets the page's selection and dispatches the selectionchange ProseMirror's DOMObserver reads, so
 * the editor takes the selection before this returns. Without that read, ProseMirror's focus
 * handler (prosemirror-view `handlers.focus`) writes the editor's own selection back to the page
 * 20 ms after focus and undoes a caret placed in between. */
async function setEditorRange(page: Page, target: EditorRange): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate((root, target) => {
    const range = document.createRange();
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    if (target.extent === "end") {
      let last: Node | null = null;
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        if (node.parentElement?.isContentEditable === true) last = node;
      }
      if (last === null) throw new Error("the editor holds no text a caret can enter");
      range.setStart(last, last.textContent?.length ?? 0);
    } else {
      const { extent, quote } = target;
      let found = false;
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const index = node.textContent?.indexOf(quote) ?? -1;
        if (index < 0) continue;
        range.setStart(node, extent === "after" ? index + quote.length : index);
        range.setEnd(node, extent === "before" ? index : index + quote.length);
        found = true;
        break;
      }
      if (!found) throw new Error(`quote is not in the editor: ${quote}`);
    }
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
  }, target);
}

export async function deleteEditorText(page: Page, quote: string): Promise<void> {
  await selectEditorText(page, quote);
  await page.keyboard.press("Backspace");
}

/** Presses Enter at the end of the document's last text, then types `text`: after a heading or a
 * paragraph, that is a paragraph of its own. */
export async function typeAtEnd(page: Page, text: string): Promise<void> {
  await setEditorRange(page, { extent: "end" });
  await page.keyboard.press("Enter");
  await page.keyboard.type(text);
}

export function marginCard(page: Page, id: string): Locator {
  return page.locator(`[data-margin-item="${id}"]`);
}

/** The card of a margin thread the reader opened: in the Thread dialog on the phone layout, in
 *  the margin beside the document everywhere else. */
export function openedThreadCard(page: Page, project: string, rootId: string): Locator {
  return project === "iphone"
    ? page.getByRole("dialog", { name: "Thread" }).getByTestId(`margin-comment-${rootId}`)
    : marginCard(page, rootId);
}

// On the phone layout the margin is a bottom sheet over the document. Acting on a selection
// opens it; close it again before selecting another range, as a person would.
export async function setSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const sheet = page.getByTestId("margin-sheet");
  if ((await sheet.getAttribute("data-expanded")) !== String(open)) {
    if (open) {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    } else {
      await page.mouse.click(1, 1);
    }
  }
  await expect(sheet).toHaveAttribute("data-expanded", String(open));
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
 * Proxies the document transport so a test owns its connections. `sever()` closes the ones it
 * made, the way a network blip or a server restart does, and the client reconnects. With
 * `holding`, or after `hold()`, new connections are held rather than connected, so the editor
 * does not sync: the open document reports no layout and every anchored card is still stacked at
 * the top of the margin, which is the state the link's hold exists for, and a reconnect after
 * `sever()` waits. `release()` connects what is held and anything that arrives afterwards.
 * Install it before the page's first navigation: only sockets opened afterwards are routed, and
 * Playwright has no fallthrough for WebSocket routes, so a test installs one.
 */
export async function documentTransport(
  page: Page,
  { holding = false }: { holding?: boolean } = {}
): Promise<{ hold: () => void; release: () => Promise<void>; sever: () => Promise<void> }> {
  const connects: (() => void)[] = [];
  const live: WebSocketRoute[] = [];
  let releasing = !holding;
  await page.routeWebSocket(/\/ws\/doc\//u, (route) => {
    if (releasing) {
      route.connectToServer();
      live.push(route);
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
      live.push(route);
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
    sever: async () => {
      for (const route of live.splice(0)) {
        await route.close({ code: 1012, reason: "transport blip" });
      }
    },
  };
}

export interface Clipboard {
  html: string;
  text: string;
}

/** Copies the selection through the editor's own copy handler: ProseMirror serializes it into the
 * copy event's clipboardData, which is what a browser's clipboard receives. */
export async function copy(page: Page): Promise<Clipboard> {
  return documentEditor(page).evaluate((root) => {
    const data = new DataTransfer();
    root.dispatchEvent(
      new ClipboardEvent("copy", { bubbles: true, cancelable: true, clipboardData: data })
    );
    return { html: data.getData("text/html"), text: data.getData("text/plain") };
  });
}

/** Pastes clipboard contents at the caret, through the editor's own paste handler. */
export async function paste(page: Page, clipboard: Clipboard): Promise<void> {
  await documentEditor(page).evaluate((root, { html, text }) => {
    const data = new DataTransfer();
    data.setData("text/html", html);
    data.setData("text/plain", text);
    root.dispatchEvent(
      new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: data })
    );
  }, clipboard);
}

/** Creates an issue whose spec is `spec` and opens it as alice. */
export async function openIssue(browser: Browser, title: string, spec: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}`);
  return { alice, issue, page };
}

/** Opens `spec` as alice, with the caret collapsed at the start or the end of the text `quote`:
 * the selection bar is gone once the selection collapses, and a paste before that replaces the
 * selected text. */
export async function openWithCaret(
  browser: Browser,
  title: string,
  spec: string,
  quote: string,
  caret: "start" | "end"
) {
  const { alice, issue, page } = await openIssue(browser, title, spec);
  await selectEditorText(page, quote);
  await page.keyboard.press(caret === "start" ? "ArrowLeft" : "ArrowRight");
  await expect(actionBar(page)).toBeHidden();
  return { alice, issue, page };
}
