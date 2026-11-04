// What `createProofEditor` does with a document of many blocks: it builds the editor a batch of
// blocks at a time, each batch its own task, and the collab binding adopts what the batches made
// (`src/progressive-build.ts`, dispatch://INFRA-341 R11). These rows read the three things that
// makes true of the editor a reader is handed: every block of the document is in its DOM, an
// edit another person makes while the batches run is in it too, and the end of the build is
// three tasks - the last batch, the binding's adoption, and the host's swap - rather than the
// one task of 1,370 ms the three were on a phone at 4x CPU throttling.
import { describe, expect, test } from "bun:test";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { createProofEditor } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withDomWindow } from "./dom-window";

/** Blocks enough that the build takes several batches: the first takes eight and each one after
 *  it is sized by what the one before cost, growing fourfold at most. */
const BLOCKS = 160;

/** A document of `blocks` paragraphs, each with words of its own so a test can name one. */
function paragraphs(blocks: number): string {
  return `${Array.from({ length: blocks }, (_, index) => `Paragraph ${index} of the ledger.`).join(
    "\n\n"
  )}\n`;
}

/** The blocks the editor's root holds, at whatever depth. */
function blocksBuilt(root: HTMLElement): number {
  return root.querySelectorAll("[data-block-id]").length;
}

/**
 * Reads how far the build has got, once per task, for as long as the build runs, and calls `act`
 * the first time it finds the editor holding part of the document and not all of it - which is a
 * batch that has landed while others have not, the only moment an edit can cross a build.
 *
 * The watcher is a real `setTimeout(0)` chain, not a fake clock: it waits for nothing, it reads
 * the editor once per task, and what it watches for is the build's own yield between batches,
 * which a fake clock would drive rather than observe.
 */
function watchTheBuild(
  root: HTMLElement,
  act: () => void
): { readonly counts: number[]; stop: () => void } {
  const counts: number[] = [];
  let acted = false;
  let watching = true;
  const look = () => {
    if (!watching) return;
    const built = blocksBuilt(root);
    counts.push(built);
    if (!acted && built > 0 && built < BLOCKS) {
      acted = true;
      act();
    }
    setTimeout(look, 0);
  };
  setTimeout(look, 0);
  return {
    counts,
    stop: () => {
      watching = false;
    },
  };
}

/** A second client's copy of `ydoc`, for an edit made while the editor is being built. */
function peerOf(ydoc: Y.Doc): Y.Doc {
  const peer = new Y.Doc();
  Y.applyUpdate(peer, Y.encodeStateAsUpdate(ydoc));
  return peer;
}

/** Sends everything `peer` has that `ydoc` has not, as a collaborator's socket does. */
function sendToTheEditor(peer: Y.Doc, ydoc: Y.Doc): void {
  Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(peer, Y.encodeStateVector(ydoc)));
}

/**
 * Builds the editor over a Y.Doc holding `markdown`, with `act` run the first time the build is
 * found partway through, and answers what was in the editor's root at each look, what the root's
 * own `display` was when the handle arrived and once the host revealed it, and the text the
 * editor ended with.
 */
async function build(
  markdown: string,
  act: (ydoc: Y.Doc) => void
): Promise<{
  counts: number[];
  blocks: number;
  displayWhenHandled: string;
  displayWhenRevealed: string;
  text: string;
  markdown: string;
}> {
  const { parseMarkdown } = await createHeadlessProof({});
  const ydoc = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(markdown), ydoc.getXmlFragment("prosemirror"));
  return withDomWindow(async (window) => {
    const root = window.document.body.appendChild(
      window.document.createElement("div")
    ) as unknown as HTMLElement;
    const watch = watchTheBuild(root, () => act(ydoc));
    const handle = await createProofEditor(root, {
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
    });
    watch.stop();
    const displayWhenHandled = root.style.display;
    handle.reveal();
    const displayWhenRevealed = root.style.display;
    const { view } = handle;
    try {
      return {
        blocks: blocksBuilt(root),
        counts: watch.counts,
        displayWhenHandled,
        displayWhenRevealed,
        markdown: handle.getMarkdown(),
        text: view.dom.textContent ?? "",
      };
    } finally {
      handle.destroy();
      while (!view.isDestroyed) await Promise.resolve();
    }
  });
}

describe("a document built in batches", () => {
  test("ends with every one of its blocks in the DOM, having held part of it on the way", async () => {
    const built = await build(paragraphs(BLOCKS), () => {});

    expect(built.blocks).toBe(BLOCKS);
    // The build was in batches: at least one look found the editor holding some of the document
    // and not all of it, which a build done in one task never shows.
    expect(built.counts.filter((count) => count > 0 && count < BLOCKS).length).toBeGreaterThan(0);
    expect(built.text).toContain("Paragraph 0 of the ledger.");
    expect(built.text).toContain(`Paragraph ${BLOCKS - 1} of the ledger.`);
  });

  // The last batch, the binding's adoption and the host's swap were one task, and on a phone at
  // 4x CPU throttling that task was 1,370 ms - a freeze a reader sees. They are three tasks
  // now, so the look between them runs twice with the whole document built and the editor not
  // yet handed over: once while the adoption is still owed, and once while the swap is.
  test("ends in three tasks: the last batch, the binding's adoption and the host's swap", async () => {
    const built = await build(paragraphs(BLOCKS), () => {});

    expect(built.blocks).toBe(BLOCKS);
    expect(
      built.counts.filter((count) => count === BLOCKS).length,
      "the whole document was built, the binding adopted it and the handle was answered in one " +
        "task: nothing else could run between them, which is the freeze this build exists to end"
    ).toBeGreaterThanOrEqual(2);
  });

  // The root is hidden from before the first batch until the host shows it, so the batches cost
  // no layout, no frame holds a half-built document, and the host's swap - its own draw of the
  // document going, this root coming - is one change it makes in one task of its own.
  test("hands the host a hidden root, which `reveal` shows", async () => {
    const built = await build(paragraphs(BLOCKS), () => {});

    expect(built.displayWhenHandled).toBe("none");
    expect(built.displayWhenRevealed).toBe("");
  });

  test("keeps an edit another person makes to a block the batches already built", async () => {
    const built = await build(paragraphs(BLOCKS), (ydoc) => {
      const peer = peerOf(ydoc);
      const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
      (first.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      sendToTheEditor(peer, ydoc);
    });

    expect(built.blocks).toBe(BLOCKS);
    expect(built.markdown).toContain("Pangolin. Paragraph 0 of the ledger.");
    expect(built.text).toContain(`Paragraph ${BLOCKS - 1} of the ledger.`);
  });

  test("keeps a block another person adds while the batches run", async () => {
    const built = await build(paragraphs(BLOCKS), (ydoc) => {
      const peer = peerOf(ydoc);
      const paragraph = new Y.XmlElement("paragraph");
      paragraph.setAttribute("blockId", "written-while-building");
      const text = new Y.XmlText();
      text.insert(0, "Pangolin.");
      paragraph.insert(0, [text]);
      peer.getXmlFragment("prosemirror").insert(1, [paragraph]);
      sendToTheEditor(peer, ydoc);
    });

    expect(built.blocks).toBe(BLOCKS + 1);
    expect(built.markdown).toContain("Pangolin.");
    expect(built.text).toContain(`Paragraph ${BLOCKS - 1} of the ledger.`);
  });
});

// A host's node views have to be on the view before the build, not after it: `view.setProps`
// with a new `nodeViews` map re-runs ProseMirror's view update over the whole document, which
// destroys every node view the batches made and creates the host's in its place. For Dispatch's
// typed blocks over the 406,319-character document that was one task of 1,218 ms on a phone at
// 4x CPU throttling, longer than any batch of the build it followed (dispatch://INFRA-341, R11).
describe("the host's own node views", () => {
  test("are installed before the first batch, and built a batch at a time with it", async () => {
    const { parseMarkdown } = await createHeadlessProof({});
    const ydoc = new Y.Doc();
    prosemirrorToYXmlFragment(
      parseMarkdown(paragraphs(BLOCKS)),
      ydoc.getXmlFragment("prosemirror")
    );
    await withDomWindow(async (window) => {
      const root = window.document.body.appendChild(
        window.document.createElement("div")
      ) as unknown as HTMLElement;
      /** How many blocks the fragment had put in the editor when the host was called. */
      let whenCalled = -1;
      let created = 0;
      // How many of the host's node views existed at each look, one look per task, which is
      // what says they were created across the batches rather than in one task after them. A
      // real `setTimeout(0)` chain, as `watchTheBuild` is and for the same reason: what it
      // watches for is the build's own yield between batches, which a fake clock would drive
      // rather than observe.
      const seen: number[] = [];
      let watching = true;
      const look = () => {
        if (!watching) return;
        seen.push(created);
        setTimeout(look, 0);
      };
      setTimeout(look, 0);
      const handle = await createProofEditor(root, {
        beforeBuild: (view) => {
          whenCalled = blocksBuilt(root);
          view.setProps({
            nodeViews: {
              ...view.props.nodeViews,
              paragraph: () => {
                created += 1;
                const dom = window.document.createElement("p") as unknown as HTMLElement;
                return { contentDOM: dom, dom };
              },
            },
          });
        },
        heatMapMode: "hidden",
        onMarkClick: () => {},
        onMarkHover: () => {},
        user: { color: "#0369a1", name: "alice" },
        ydoc,
      });
      watching = false;
      const { view } = handle;
      try {
        // Called over the empty document Milkdown created, before a block of the fragment was
        // in it: a node view installed here is the one each batch builds with.
        expect(whenCalled).toBe(0);
        expect(view.state.doc.childCount).toBe(BLOCKS);
        expect(created).toBeGreaterThanOrEqual(BLOCKS);
        expect(
          seen.filter((count) => count > 0 && count < BLOCKS).length,
          "every one of the host's node views was created in one task: a host that installs " +
            "them after the build pays for the whole document a second time"
        ).toBeGreaterThan(0);
      } finally {
        handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });
});
