// What `createProofEditor` does with a document of many blocks: it builds the editor a batch of
// blocks at a time, each batch its own task, shows the reader each batch as it lands, takes
// their edits in the blocks it has built, and has the collab binding adopt what the batches made
// (`src/progressive-build.ts`, dispatch://INFRA-341 R11). These rows read what that makes true of
// the editor a reader is handed: every block of the document is in its DOM, the root is shown
// with part of the document in it, the host is told which blocks each batch built, an edit the
// reader makes in a built block is in the Yjs document before the last batch, another person's
// change to a built block is in the editor in the task it arrives in, and the end of the build is
// three tasks - the last batch, the binding's adoption, and the host's swap - rather than the one
// task of 1,370 ms the three were on a phone at 4x CPU throttling. The last rows read how the
// thread goes back between batches, which decides whether anything else on the page runs while
// the build lasts.
import { describe, expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { createProofEditor } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { endOfTask } from "../src/progressive-build";
import { withDomWindow } from "./dom-window";
import { pastePlainText, withHTMLElementGlobal } from "./paste";

/** Blocks enough that the build takes several batches: the first takes eight and each one after
 *  it is sized by what the one before cost, growing fourfold at most. */
const BLOCKS = 160;

/** The words the paragraph at `index` of the generated document is made of. */
function paragraph(index: number): string {
  return `Paragraph ${index} of the ledger.`;
}

/** A document of `blocks` paragraphs, each with words of its own so a test can name one. */
function paragraphs(blocks: number): string {
  return `${Array.from({ length: blocks }, (_, index) => paragraph(index)).join("\n\n")}\n`;
}

/** A document of `count` sections, each a heading of its own and a paragraph under it, which is
 *  `count * 2` top-level blocks. A heading in every batch is what brings Milkdown's
 *  `syncHeadingIdPlugin` into the build: it is a plugin view, so ProseMirror runs its `update`
 *  inside the `view.dispatch` each batch is placed with, and it dispatches an `id` for every
 *  heading the batch brought (`@milkdown/preset-commonmark`). */
function sections(count: number): string {
  return `${Array.from(
    { length: count },
    (_, index) => `## Section ${index}\n\n${paragraph(index)}`
  ).join("\n\n")}\n`;
}

/** The blocks the editor's root holds, at whatever depth. */
function blocksBuilt(root: HTMLElement): number {
  return root.querySelectorAll("[data-block-id]").length;
}

/** What one batch told the host: the ids it was about to build, how many blocks the editor held
 *  when it said so, and how many it held once they were in. */
interface Batch {
  readonly after: number;
  readonly before: number;
  readonly ids: readonly string[];
}

/** What one look at the build found: how many blocks the editor's root held, and whether the
 *  reader could see it. */
interface Look {
  readonly blocks: number;
  readonly shown: boolean;
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
): { readonly looks: Look[]; stop: () => void } {
  const looks: Look[] = [];
  let acted = false;
  let watching = true;
  const look = () => {
    if (!watching) return;
    const blocks = blocksBuilt(root);
    looks.push({ blocks, shown: root.style.display !== "none" });
    if (!acted && blocks > 0 && blocks < BLOCKS) {
      acted = true;
      act();
    }
    setTimeout(look, 0);
  };
  setTimeout(look, 0);
  return {
    looks,
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

/** A paragraph block, as a collaborator's client builds one. */
function paragraphBlock(id: string, text: string): Y.XmlElement {
  const block = new Y.XmlElement("paragraph");
  block.setAttribute("blockId", id);
  const content = new Y.XmlText();
  content.insert(0, text);
  block.insert(0, [content]);
  return block;
}

/** Where the editor's top-level block `index` starts. */
function startOf(doc: ProseMirrorNode, index: number): number {
  let pos = 0;
  for (let at = 0; at < index; at += 1) pos += doc.child(at).nodeSize;
  return pos;
}

/** Puts the caret `offset` characters into the editor's top-level block `index`. */
function caretIn(view: EditorView, index: number, offset: number): void {
  const at = startOf(view.state.doc, index) + 1 + offset;
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, at)));
}

/** The text of the Yjs document's top-level block `index`, which is what a reader's edit has to
 *  reach: read off the fragment itself, so nothing of the editor's own state is in the answer. */
function blockText(fragment: Y.XmlFragment, index: number): string {
  const block = fragment.get(index);
  return block === undefined ? "" : block.toString().replace(/<[^>]*>/g, "");
}

/** The text of every top-level block of the Yjs document. */
function everyBlockText(fragment: Y.XmlFragment): string[] {
  return fragment.toArray().map((_, index) => blockText(fragment, index));
}

/** Every top-level block of the Yjs document, by the id it carries and the text it holds: what
 *  a build of a document nobody is editing has to leave exactly as it found it. */
function everyBlock(fragment: Y.XmlFragment): string[] {
  return fragment
    .toArray()
    .map(
      (block, index) =>
        `${block instanceof Y.XmlElement ? (block.getAttribute("blockId") ?? "-") : "-"}:` +
        blockText(fragment, index)
    );
}

/** What the build left behind, for a row to read. */
interface Built {
  readonly batches: Batch[];
  readonly blocks: number;
  readonly displayWhenHandled: string;
  readonly displayWhenRevealed: string;
  /** The Yjs document's own blocks as the build found them, and as it left them. */
  readonly documentAfter: string[];
  readonly documentBefore: string[];
  readonly looks: Look[];
  readonly markdown: string;
  readonly text: string;
  readonly writePath: string | undefined;
}

/** What a row is handed the first time it finds the build partway through. */
interface MidBuild {
  readonly fragment: Y.XmlFragment;
  readonly root: HTMLElement;
  readonly view: EditorView;
  readonly ydoc: Y.Doc;
}

/**
 * Builds the editor over a Y.Doc holding `markdown`, with `act` run the first time the build is
 * found partway through, and answers what the build left behind.
 */
async function build(markdown: string, act: (what: MidBuild) => void = () => {}): Promise<Built> {
  const { parseMarkdown } = await createHeadlessProof({});
  const ydoc = new Y.Doc();
  const fragment = ydoc.getXmlFragment("prosemirror");
  prosemirrorToYXmlFragment(parseMarkdown(markdown), fragment);
  const documentBefore = everyBlock(fragment);
  return withDomWindow(async (window) => {
    const root = window.document.body.appendChild(
      window.document.createElement("div")
    ) as unknown as HTMLElement;
    const batches: Batch[] = [];
    let view: EditorView | undefined;
    const watch = watchTheBuild(root, () => {
      if (view === undefined) throw new Error("the build never handed its view over");
      act({ fragment, root, view, ydoc });
    });
    const handle = await createProofEditor(root, {
      beforeBuild: (built) => {
        view = built;
      },
      heatMapMode: "hidden",
      onBatch: (ids) => {
        const before = blocksBuilt(root);
        return () => {
          batches.push({ after: blocksBuilt(root), before, ids });
        };
      },
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
    });
    watch.stop();
    const displayWhenHandled = root.style.display;
    const writePath = root.dataset.writePath;
    handle.reveal();
    const displayWhenRevealed = root.style.display;
    try {
      return {
        batches,
        blocks: blocksBuilt(root),
        displayWhenHandled,
        displayWhenRevealed,
        documentAfter: everyBlock(fragment),
        documentBefore,
        looks: watch.looks,
        markdown: handle.getMarkdown(),
        text: handle.view.dom.textContent ?? "",
        writePath,
      };
    } finally {
      const { view: built } = handle;
      handle.destroy();
      while (!built.isDestroyed) await Promise.resolve();
    }
  });
}

/** Every look that found the editor holding part of the document and not all of it. */
function partway(built: Built): Look[] {
  return built.looks.filter((look) => look.blocks > 0 && look.blocks < BLOCKS);
}

describe("a document built in batches", () => {
  test("ends with every one of its blocks in the DOM, having held part of it on the way", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.blocks).toBe(BLOCKS);
    // The build was in batches: at least one look found the editor holding some of the document
    // and not all of it, which a build done in one task never shows.
    expect(partway(built).length).toBeGreaterThan(0);
    expect(built.text).toContain(paragraph(0));
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  // The last batch, the binding's adoption and the host's swap were one task, and on a phone at
  // 4x CPU throttling that task was 1,370 ms - a freeze a reader sees. They are three tasks
  // now, so the look between them runs twice with the whole document built and the editor not
  // yet handed over: once while the adoption is still owed, and once while the swap is.
  test("ends in three tasks: the last batch, the binding's adoption and the host's swap", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.blocks).toBe(BLOCKS);
    expect(
      built.looks.filter((look) => look.blocks === BLOCKS).length,
      "the whole document was built, the binding adopted it and the handle was answered in one " +
        "task: nothing else could run between them, which is the freeze this build exists to end"
    ).toBeGreaterThanOrEqual(2);
  });

  // The reader waited eight to thirteen seconds for a document whose first batch was ready in
  // half of one. The root is hidden until that first batch and shown in the task it lands in, so
  // what a reader has from then on is the editor's own blocks, growing.
  test("shows the reader the editor with part of the document in it", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(
      partway(built).filter((look) => !look.shown),
      "the editor was built out of sight again: the reader waits for the last batch to see any " +
        "of it"
    ).toEqual([]);
    expect(built.displayWhenHandled).toBe("");
    expect(built.displayWhenRevealed).toBe("");
    // And the root says those blocks take edits that reach the Yjs document, which is what the
    // measurement reads the editor's first usable block by (`e2e/large-documents-page.ts`).
    expect(built.writePath).toBe("live");
  });

  test("hands the host each batch's blocks, before and after they are in the editor", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.batches.length).toBeGreaterThan(1);
    expect(built.batches[0]?.ids.length).toBe(8);
    // Every block of the document, once, in document order: what the host takes its own draw of
    // off the page as each batch lands.
    expect(built.batches.flatMap((batch) => batch.ids).length).toBe(BLOCKS);
    expect(new Set(built.batches.flatMap((batch) => batch.ids)).size).toBe(BLOCKS);
    // Each batch's blocks were in the editor between the call and what it answered, and nothing
    // of the batch was in it before.
    let standing = 0;
    for (const batch of built.batches) {
      expect({ after: batch.after, before: batch.before }).toEqual({
        after: standing + batch.ids.length,
        before: standing,
      });
      standing = batch.after;
    }
  });

  // Nobody edited this document, so the build has to leave it block for block as it found it.
  // It did not: Milkdown's `syncHeadingIdPlugin` dispatches an `id` for each heading a batch
  // brought, inside the very `view.dispatch` that batch is placed with, and the write path
  // reads that as a reader's edit and writes the editor's blocks through a window as many
  // blocks wide as the frontier says. The frontier used to move after that dispatch, so the
  // window was a batch short of the editor and `updateYFragment` wrote the difference into the
  // Yjs document - the batch's blocks a second time - and then, a batch later, with the
  // frontier counting that batch twice, deleted blocks the batches had still to reach. What
  // the reader was left with was a document missing blocks of its own, and blocks under ids
  // nothing had indexed (dispatch://INFRA-341, R11).
  test("leaves the Yjs document's blocks as it found them, under the ids it was given", async () => {
    const built = await build(sections(BLOCKS / 2));

    expect(built.documentBefore.length).toBe(BLOCKS);
    expect(built.documentAfter).toEqual(built.documentBefore);
    expect(built.blocks).toBe(BLOCKS);
    expect(partway(built).length).toBeGreaterThan(0);
  });
});

describe("a reader's edit in a block the batches have built", () => {
  test("is in the Yjs document before the last batch lands", async () => {
    let written = "";
    let untouched: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      caretIn(view, 0, 0);
      view.dispatch(view.state.tr.insertText("Pangolin. "));
      written = blockText(fragment, 0);
      untouched = everyBlockText(fragment).slice(1);
    });

    expect(written).toBe(`Pangolin. ${paragraph(0)}`);
    // Every other block of the document is as it was: the window the edit is written through
    // reaches the built blocks and no further.
    expect(untouched).toEqual(
      Array.from({ length: BLOCKS - 1 }, (_, index) => paragraph(index + 1))
    );
    expect(built.markdown).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("splits a block at Enter and joins two at Backspace, in the Yjs document", async () => {
    let afterSplit: string[] = [];
    let afterJoin: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      // Enter in the middle of the first block: two blocks where there was one.
      caretIn(view, 0, "Paragraph 0".length);
      view.dispatch(view.state.tr.split(view.state.selection.head));
      afterSplit = everyBlockText(fragment).slice(0, 3);
      // Backspace at the start of the second half joins it back onto the first.
      caretIn(view, 1, 0);
      view.dispatch(view.state.tr.join(startOf(view.state.doc, 1)));
      afterJoin = everyBlockText(fragment).slice(0, 2);
    });

    expect(afterSplit).toEqual(["Paragraph 0", " of the ledger.", paragraph(1)]);
    expect(afterJoin).toEqual([paragraph(0), paragraph(1)]);
    expect(built.markdown).toContain(paragraph(0));
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
  });

  test("writes a paste that spans several blocks to the Yjs document", async () => {
    let pasted: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      // The reader selects from inside the first block to inside the third and pastes three
      // blocks over them, which is a delete and an insert across the built run at once.
      const from = startOf(view.state.doc, 0) + 1;
      const to = startOf(view.state.doc, 2) + 1 + "Paragraph 2".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
      withHTMLElementGlobal(() => pastePlainText(view, "Otter one\n\nOtter two\n\nOtter three"));
      pasted = everyBlockText(fragment).slice(0, 4);
    });

    expect(pasted).toEqual(["Otter one", "Otter two", "Otter three of the ledger.", paragraph(3)]);
    expect(built.markdown).toContain("Otter two");
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
  });
});

describe("another person's change while the batches run", () => {
  test("is in the editor in the task it arrives in, for the block it touched", async () => {
    let atOnce = "";
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
      (first.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      sendToTheEditor(peer, ydoc);
      atOnce = root.textContent ?? "";
    });

    expect(atOnce).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.blocks).toBe(BLOCKS);
    expect(built.markdown).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("keeps the reader's cursor in the block they are writing in", async () => {
    let where = -1;
    let inside = "";
    const built = await build(paragraphs(BLOCKS), ({ view, ydoc }) => {
      caretIn(view, 1, "Paragraph 1".length);
      const peer = peerOf(ydoc);
      const second = peer.getXmlFragment("prosemirror").get(1) as Y.XmlElement;
      (second.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      sendToTheEditor(peer, ydoc);
      where = view.state.selection.head - startOf(view.state.doc, 1) - 1;
      // The reader writes on without looking, and their word lands where their cursor was
      // rather than at the top of the block another person has just rewritten.
      view.dispatch(view.state.tr.insertText("!"));
      inside = blockText(ydoc.getXmlFragment("prosemirror"), 1);
    });

    // The caret stayed on the reader's own words, which another person's ten characters have
    // pushed along: it is where "Paragraph 1" ends, not where it ended before they wrote.
    expect(where).toBe("Pangolin. Paragraph 1".length);
    expect(inside).toBe("Pangolin. Paragraph 1! of the ledger.");
    expect(built.markdown).toContain("Pangolin. Paragraph 1! of the ledger.");
  });

  test("keeps a block they add inside the built run, at the index they added it", async () => {
    let around: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      peer
        .getXmlFragment("prosemirror")
        .insert(1, [paragraphBlock("written-while-building", "Pangolin.")]);
      sendToTheEditor(peer, ydoc);
      around = Array.from(
        root.querySelectorAll('[data-block-id="written-while-building"]'),
        (block) => block.textContent ?? ""
      );
    });

    expect(around).toEqual(["Pangolin."]);
    expect(built.blocks).toBe(BLOCKS + 1);
    expect(built.markdown).toContain("Pangolin.");
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("takes a block they remove from the built run out of the editor", async () => {
    let afterwards = "";
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      peer.getXmlFragment("prosemirror").delete(1, 1);
      sendToTheEditor(peer, ydoc);
      afterwards = root.textContent ?? "";
    });

    expect(afterwards).not.toContain(paragraph(1));
    expect(built.blocks).toBe(BLOCKS - 1);
    expect(built.markdown).not.toContain(paragraph(1));
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("leaves the reader's own edit of a built block standing beside it", async () => {
    let both: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view, ydoc }) => {
      const peer = peerOf(ydoc);
      const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
      (first.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      peer
        .getXmlFragment("prosemirror")
        .insert(1, [paragraphBlock("written-while-building", "Otter.")]);
      peer.getXmlFragment("prosemirror").delete(3, 1);
      sendToTheEditor(peer, ydoc);
      caretIn(view, 2, 0);
      view.dispatch(view.state.tr.insertText("Beaver. "));
      both = everyBlockText(fragment).slice(0, 4);
    });

    expect(both).toEqual([
      `Pangolin. ${paragraph(0)}`,
      "Otter.",
      `Beaver. ${paragraph(1)}`,
      paragraph(3),
    ]);
    expect(built.markdown).toContain(`Beaver. ${paragraph(1)}`);
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
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

/** Runs `act` with `scheduler` standing in for the browsing context's own, and puts back
 *  whatever was there. `undefined` runs it in a context that has none. */
async function withScheduler(scheduler: unknown, act: () => Promise<void>): Promise<void> {
  const had = Object.getOwnPropertyDescriptor(globalThis, "scheduler");
  if (scheduler === undefined) {
    Reflect.deleteProperty(globalThis, "scheduler");
  } else {
    Object.defineProperty(globalThis, "scheduler", {
      configurable: true,
      value: scheduler,
      writable: true,
    });
  }
  try {
    await act();
  } finally {
    Reflect.deleteProperty(globalThis, "scheduler");
    if (had !== undefined) Object.defineProperty(globalThis, "scheduler", had);
  }
}

/** A real 0 ms timer, not a fake clock: what these rows read is the order the browsing
 *  context's own queue serves two tasks in, which a fake clock would drive rather than
 *  observe. Neither waits for anything. */
function thePageSetsATimer(order: string[]): void {
  setTimeout(() => order.push("the page's own timer"), 0);
}

// How the thread goes back between batches is the whole of whether anything else on the page
// runs while a build lasts. `scheduler.yield()` stood here, and the browser serves its
// continuation ahead of every posted task: for as long as a build lasted - about 35 s over one
// screenful of the 406,319-character document and minutes over a whole drawn one - no socket
// message, no fetch callback and no timer ran. `scheduler.postTask()` at its default priority
// left the socket message served in 7 ms and a timer waiting 19,945 ms, because Chromium runs
// a web-scheduling task of that priority ahead of a timer (dispatch://INFRA-341, R11).
describe("the thread a batch hands back", () => {
  test("is handed back behind a task the page is already waiting on", async () => {
    const order: string[] = [];

    await withScheduler(undefined, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });

  test("is a timer even where the browsing context has a scheduler of its own", async () => {
    const order: string[] = [];
    const taken: string[] = [];
    const scheduler = {
      postTask: (work: () => void) => {
        taken.push("postTask");
        work();
        return Promise.resolve();
      },
      yield: () => {
        taken.push("yield");
        return Promise.resolve();
      },
    };

    await withScheduler(scheduler, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(taken).toEqual([]);
    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });
});
