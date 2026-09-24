import { expect, test } from "bun:test";
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

const settle = () => new Promise((resolve) => setTimeout(resolve, 50));

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
    user: { color: "#f59e0b", name: "alice" },
    ydoc: browser,
  });
  let unbind = () => {};
  try {
    // The document holds a pending suggestion anchored on "brown", as the server leaves it.
    handle.setMarkdown("The quick brown fox");
    handle.applyRemoteMarks({ "suggestion-1": replaceSuggestion("pending") });
    server.getMap("marks").set("suggestion-1", replaceSuggestion("pending"));
    await settle();
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
    await settle();

    expect(handle.view.state.doc.textContent).toBe("The quick red fox");
    expect(text.toString()).toBe("The quick red fox");
  } finally {
    unbind();
    handle.destroy();
    root.remove();
  }
});
