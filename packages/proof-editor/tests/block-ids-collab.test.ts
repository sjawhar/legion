/**
 * Block ids across two browsers and the server, now that the block-id plugin stamps only
 * inside the ranges a local edit changed. One editor's id for a block it created is the id
 * every replica ends on; two editors that split the same paragraph at once still leave every
 * block an id of its own; and a block that arrives without one - a peer's, or the server's
 * before settlement wrote it - is left alone by both editors until the server's id arrives.
 *
 * The harness is the two closest ones joined: marks-editor.ts's editor over a Y.Doc, twice,
 * and editor.test.ts's `linkedDocs` relay, with a third Y.Doc for the server. The relay holds
 * every update until `sync`, so two browsers can edit at the same time before either sees the
 * other, which is the whole point of the second case.
 */

import { expect, test } from "bun:test";
import { splitBlock } from "@milkdown/kit/prose/commands";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";
import { createProofEditor, type ProofEditorHandle } from "../src/lib";
import {
  BLOCK_ID_ATTR,
  blockIdOf,
  createHeadlessProof,
  isIdentifiedBlock,
} from "../src/lib-headless.js";
import { withDomWindow } from "./dom-window";

/** The origin an update carries when the relay, not an editor or the server, wrote it. */
const RELAY = "relay";

interface Room {
  /** The two browsers, each bound to its own Y.Doc. */
  alice: EditorView;
  bob: EditorView;
  /** The document the server holds, where settlement writes a block's id. */
  server: Y.Doc;
  /** Delivers every update the three have made since the last call, to the other two. */
  sync(): void;
}

/**
 * Runs `run` against two editors of the real stack, each on its own Y.Doc seeded with
 * `markdown`, and the server's Y.Doc, the three relaying their updates to one another when
 * `sync` is called.
 */
async function withTwoEditors(
  markdown: string,
  run: (room: Room) => void | Promise<void>
): Promise<void> {
  const { parseMarkdown } = await createHeadlessProof();
  const server = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(markdown), server.getXmlFragment("prosemirror"));
  const browsers = [new Y.Doc(), new Y.Doc()];
  const docs = [...browsers, server];
  const pending: { from: Y.Doc; update: Uint8Array }[] = [];
  for (const doc of browsers) Y.applyUpdate(doc, Y.encodeStateAsUpdate(server), RELAY);
  for (const doc of docs) {
    doc.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== RELAY) pending.push({ from: doc, update });
    });
  }
  const sync = (): void => {
    for (let next = pending.shift(); next !== undefined; next = pending.shift()) {
      for (const doc of docs) {
        if (doc !== next.from) Y.applyUpdate(doc, next.update, RELAY);
      }
    }
  };
  await withDomWindow(async (window) => {
    const handles: ProofEditorHandle[] = [];
    try {
      for (const [index, name] of ["alice", "bob"].entries()) {
        const root = window.document.body.appendChild(window.document.createElement("div"));
        handles.push(
          await createProofEditor(root as unknown as HTMLElement, {
            heatMapMode: "hidden",
            onMarkClick: () => {},
            onMarkHover: () => {},
            user: { color: "#0369a1", name },
            ydoc: browsers[index],
          })
        );
      }
      await run({ alice: handles[0].view, bob: handles[1].view, server, sync });
    } finally {
      for (const handle of handles) {
        handle.destroy();
        // The editor tears its view down asynchronously; its plugins' teardown needs the DOM.
        while (!handle.view.isDestroyed) await Promise.resolve();
      }
    }
  });
}

/** Every identified block's id, in document order, `null` where a block carries none. */
function blockIds(doc: ProseMirrorNode): (string | null)[] {
  const ids: (string | null)[] = [];
  doc.descendants((node) => {
    if (isIdentifiedBlock(node)) ids.push(blockIdOf(node));
  });
  return ids;
}

/** The position just inside the end of the paragraph at `index`. */
function endOfParagraph(doc: ProseMirrorNode, index: number): number {
  let pos = 0;
  for (let i = 0; i < index; i += 1) pos += doc.child(i).nodeSize;
  return pos + doc.child(index).nodeSize - 1;
}

test("a block one browser adds reaches the other with the id that browser minted", async () => {
  await withTwoEditors("one\n", ({ alice, bob, sync }) => {
    const { schema } = alice.state;
    alice.dispatch(
      alice.state.tr.insert(
        alice.state.doc.content.size,
        schema.nodes.paragraph.create(null, schema.text("two"))
      )
    );
    const minted = blockIds(alice.state.doc);
    expect(minted).toHaveLength(2);
    expect(typeof minted[1]).toBe("string");
    expect(minted[1]).not.toBe(minted[0]);

    sync();
    expect(bob.state.doc.textContent).toContain("two");
    expect(blockIds(bob.state.doc)).toEqual(minted);
  });
});

test("two browsers splitting the same paragraph leave every block an id no other block holds", async () => {
  await withTwoEditors("alpha beta\n", ({ alice, bob, sync }) => {
    // Neither browser has seen the other's split when it makes its own.
    for (const view of [alice, bob]) {
      const at = view.state.doc.child(0).textContent.indexOf(" ") + 1;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, at + 1)));
      splitBlock(view.state, view.dispatch);
      expect(view.state.doc.childCount).toBe(2);
    }

    sync();
    // Both splits survive the merge: the paragraph they cut, and a second half each.
    expect(alice.state.doc.childCount).toBe(3);
    for (const view of [alice, bob]) {
      const ids = blockIds(view.state.doc);
      expect(ids.filter((id) => typeof id === "string" && id !== "")).toHaveLength(ids.length);
      expect(new Set(ids).size).toBe(ids.length);
    }
    expect(blockIds(alice.state.doc)).toEqual(blockIds(bob.state.doc));
  });
});

test("a block that arrives without an id keeps none until the server's id arrives", async () => {
  await withTwoEditors("one\n", ({ alice, bob, server, sync }) => {
    const fragment = server.getXmlFragment("prosemirror");
    const paragraph = new Y.XmlElement("paragraph");
    paragraph.insert(0, [new Y.XmlText("from the server")]);
    fragment.insert(fragment.length, [paragraph]);
    sync();
    for (const view of [alice, bob]) {
      expect(view.state.doc.textContent).toContain("from the server");
      expect(blockIds(view.state.doc)[1]).toBeNull();
    }

    // A local edit elsewhere - in the first paragraph - stamps nothing: the unidentified block
    // is outside every range the edit changed, and is the server's to name.
    for (const view of [alice, bob]) {
      view.dispatch(view.state.tr.insertText("!", endOfParagraph(view.state.doc, 0)));
    }
    sync();
    for (const view of [alice, bob]) {
      expect(view.state.doc.child(0).textContent).toBe("one!!");
      expect(blockIds(view.state.doc)[1]).toBeNull();
    }

    // Settlement names it, as it names every block a browser did not create.
    const settled = crypto.randomUUID();
    paragraph.setAttribute(BLOCK_ID_ATTR, settled);
    sync();
    for (const view of [alice, bob]) expect(blockIds(view.state.doc)[1]).toBe(settled);
  });
});
