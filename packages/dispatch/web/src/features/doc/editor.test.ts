import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import type { Window as HappyDOMWindow } from "happy-dom";
import * as Y from "yjs";

import { bindRemoteMarks, createEditor, editorAttributes, type StoredMark } from "./editor";
import { blockSchemaCache } from "./schema";

test("editorAttributes expose the editor as a multiline textbox", () => {
  expect(editorAttributes).toEqual({
    "aria-label": "Document editor",
    "aria-multiline": "true",
    role: "textbox",
  });
});

// Two documents relaying every update to each other, as the room relays one browser's.
function linkedDocs(): { browser: Y.Doc; server: Y.Doc } {
  const browser = new Y.Doc();
  const server = new Y.Doc();
  for (const [from, to] of [
    [server, browser],
    [browser, server],
  ] as const) {
    from.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== "relay") {
        Y.applyUpdate(to, update, "relay");
      }
    });
  }
  return { browser, server };
}

function replaceSuggestion(status: "pending" | "accepted"): StoredMark {
  return {
    by: "human:bob",
    content: "red",
    createdAt: "2026-09-24T00:00:00.000Z",
    kind: "replace",
    quote: "brown",
    status,
  };
}

// A server write that accepts a suggestion can reach the browser as one update: the text change
// and the marks map entry that closes the suggestion together. The editor must end on the
// accepted text, and must not write the replaced text back to the server.
test("bindRemoteMarks keeps an accepted suggestion's text when its projection arrives in the same update", async () => {
  const { browser, server } = linkedDocs();
  const root = document.body.appendChild(document.createElement("div"));
  const blockSchema = await blockSchemaCache.load(() => Promise.reject(new Error("no schema")));
  const handle = await createEditor(root, {
    awareness: null,
    blockSchema,
    // This row reads the marks binding, never the document's decision blocks.
    onAskBlockHosts: () => {},
    // No picture in this row's document, so nothing ever asks for a latest version, and no
    // fallback link is drawn for these to open.
    navigate: () => {},
    queryClient: new QueryClient(),
    watchPicture: () => () => {},
    user: { color: "#f59e0b", name: "alice" },
    ydoc: browser,
  });
  let unbind = () => {};
  try {
    // The document holds a pending suggestion anchored on "brown", as the server leaves it.
    handle.setMarkdown("The quick brown fox");
    handle.applyRemoteMarks({ "suggestion-1": replaceSuggestion("pending") });
    server.getMap("marks").set("suggestion-1", replaceSuggestion("pending"));
    unbind = bindRemoteMarks(browser, handle);
    const text = (server.getXmlFragment("prosemirror").get(0) as Y.XmlElement).get(0) as Y.XmlText;
    const delta = text.toDelta() as { attributes?: Record<string, unknown>; insert: string }[];
    const anchor = Object.keys(delta.find((op) => op.insert === "brown")?.attributes ?? {});
    expect(anchor).not.toHaveLength(0);

    server.transact(() => {
      text.delete("The quick ".length, "brown".length);
      text.insert(
        "The quick ".length,
        "red",
        Object.fromEntries(anchor.map((attribute) => [attribute, null]))
      );
      server.getMap("marks").set("suggestion-1", replaceSuggestion("accepted"));
    });

    expect(handle.view.state.doc.textContent).toBe("The quick red fox");
    expect(text.toString()).toBe("The quick red fox");
  } finally {
    unbind();
    handle.destroy();
    root.remove();
  }
});

// R8: one card on a picture's fallback link at any moment - the editor library's own in an
// editable view, Dispatch's (`data-dispatch-href`) in a read-only one. A mounted editor is
// turned read-only and back in place (`ProofDocument.tsx`, on a closed issue or a change of
// admission) and ProseMirror rebuilds no node view for it, so the attribute must follow the
// live state for a link drawn before the toggle as well as one drawn after it.
test("a picture's fallback link carries Dispatch's card only while the mounted editor is read-only", async () => {
  const pageBefore = window.location.href;
  (window as unknown as HappyDOMWindow).happyDOM.setURL("https://dispatch.test/issues/CORE-1/spec");
  const root = document.body.appendChild(document.createElement("div"));
  const blockSchema = await blockSchemaCache.load(() => Promise.reject(new Error("no schema")));
  const handle = await createEditor(root, {
    awareness: null,
    blockSchema,
    onAskBlockHosts: () => {},
    navigate: () => {},
    queryClient: new QueryClient(),
    // Both sources pin a version, so neither waits on a read to draw.
    watchPicture: () => () => {},
    user: { color: "#f59e0b", name: "alice" },
    ydoc: new Y.Doc(),
  });
  /** Every picture's fallback link, in document order: a picture the browser drew is no link. */
  const links = () => [...root.querySelectorAll<HTMLElement>("[data-dispatch-doc-picture] > a")];
  /** The picture still drawn, turned into its fallback link by a load the browser refused. */
  const failToDraw = () => root.querySelector("img")?.dispatchEvent(new Event("error"));
  try {
    handle.setMarkdown(
      "![first.md](dispatch://CORE-1/artifact/first-md@v1) " +
        "![second.md](dispatch://CORE-1/artifact/second-md@v2)"
    );
    // The first link is drawn while the editor takes edits: the library's card is the one there.
    failToDraw();
    expect(links()).toHaveLength(1);
    expect(links()[0]?.hasAttribute("data-dispatch-href")).toBe(false);

    // Closing the issue turns the mounted editor read-only: the link already drawn takes
    // Dispatch's card, and the one drawn after the toggle is drawn with it.
    handle.setReadOnly(true);
    expect(links()[0]?.getAttribute("data-dispatch-href")).toBe(
      "dispatch://CORE-1/artifact/first-md@v1"
    );
    failToDraw();
    expect(links().map((link) => link.getAttribute("data-dispatch-href"))).toEqual([
      "dispatch://CORE-1/artifact/first-md@v1",
      "dispatch://CORE-1/artifact/second-md@v2",
    ]);

    // Reopened: both links give the card back, so neither shows two.
    handle.setReadOnly(false);
    expect(links()).toHaveLength(2);
    expect(links().map((link) => link.hasAttribute("data-dispatch-href"))).toEqual([false, false]);
  } finally {
    handle.destroy();
    root.remove();
    (window as unknown as HappyDOMWindow).happyDOM.setURL(pageBefore);
  }
});
