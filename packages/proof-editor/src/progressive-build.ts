/**
 * Building a document's editor a batch of blocks at a time, in front of the reader, so that no
 * one task of the build freezes the page and the blocks it has built are a document they can
 * read and write before the rest of it arrives (dispatch://INFRA-341, R11).
 *
 * What the one task was. `collabService.connect()` installs y-prosemirror's sync plugin, whose
 * plugin view calls `ProsemirrorBinding._forceRerender()`
 * (`y-prosemirror/src/plugins/sync-plugin.js`): it maps the whole `Y.XmlFragment` through
 * `createNodeFromYElement`, replaces the editor's whole document with the result, and
 * ProseMirror builds every node view of it - all in one continuous call. On a phone at 4x CPU
 * throttling that was 1,600 ms for an 81,294-character document and 4,800 ms for a
 * 406,319-character one, against a target of 500 ms. The trace has no seam to yield at: the
 * work is prosemirror-model, native DOM and prosemirror-view, with almost no layout in it.
 *
 * Why the binding can be fed instead. `ySyncPlugin` takes a prebuilt `mapping`, and skips
 * `_forceRerender` entirely when it is given one (`sync-plugin.js`: `if (mapping == null)`).
 * That option exists for `initProseMirrorDoc` (`y-prosemirror/src/lib.js`), the helper that
 * builds a document and its mapping from a fragment before the editor is created, so that the
 * binding adopts a document already standing rather than drawing one. This module is that
 * helper run in batches: `initProseMirrorDoc` reads the fragment through `toArray()` alone, so
 * a window onto the fragment that answers `toArray()` with a slice of its blocks converts just
 * those blocks, and the mappings of the batches are one mapping for the binding to adopt. Each
 * batch's nodes go into the editor in a transaction of their own, so ProseMirror builds that
 * batch's node views and no more, and each batch is its own task.
 *
 * Why the reader is given each batch. A build hidden until its last batch hands the reader a
 * document eight to thirteen seconds after they asked for it, and what they read until then -
 * the host's own draw of the same blocks - takes no edits. So the editor's root is shown in the
 * task its first batch lands in, the host takes its own copies of those blocks off the page in
 * that same task (`onBatch`), and from that moment the blocks on the screen are the editor's
 * own: a reader can type in them, split one, join two or paste across several, and every one of
 * those edits reaches the Yjs document before the last batch lands.
 *
 * How an edit reaches the Yjs document before the binding exists. The sync plugin is not
 * installed while the batches run, so there is no binding to write through. The batches build
 * each block against its own element of the real fragment, so the editor's document is the
 * fragment's first `built` blocks, in order, and y-prosemirror's own `updateYFragment` - marked
 * `@private @unstable`, which is why `package.json` pins the version exactly and
 * `tests/progressive-build.test.ts` covers every shape of edit through it - writes the
 * difference between a ProseMirror node and a `Y.XmlFragment`. It is given a window onto the
 * blocks already built: `toArray()` answers the fragment's first `built` children, read at the
 * moment of the write, and `insert` and `delete` pass through to the real fragment at the same
 * index, because that window is a prefix of it. The window may not hold the writes itself - a
 * Yjs insert through an `Object.create` view makes the view the inserted item's parent
 * (`Item.js`), and encoding the update then throws in `findRootTypeKey` (`ID.js`) - and the
 * indices may not be remembered, because another person's insert shifts them while the build
 * runs, which is why they are read from the fragment at write time. No element of a block the
 * batches have not built yet is read or removed: the window ends at `built`.
 *
 * What the batches are not allowed to change. Every block ends in the real DOM, so this is
 * neither a virtual DOM nor a virtualized list. Each batch carries `ySyncPluginKey`'s
 * `isChangeOrigin` exactly as `_forceRerender`'s own transaction does, so every plugin that
 * tells a remote change from a reader's - the block-id stamp (`./editor/schema/block-ids.ts`)
 * and the ask content rule (`./ask-content.ts`) - reads the build as what it is, none of it is
 * undoable, and none of it is written back to the Yjs document. A change another person makes
 * while the batches run is applied to the editor in the task it arrives in, for the block it
 * touched, with the reader's cursor kept, so their next keystroke neither writes over it nor
 * writes at a stale index.
 */
import type { Fragment, Node as ProseMirrorNode, Schema } from "@milkdown/kit/prose/model";
import type { Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { initProseMirrorDoc, updateYFragment, ySyncPluginKey } from "y-prosemirror";
import type * as Y from "yjs";

import { BLOCK_ID_ATTR } from "./editor/schema/block-ids";

/** What `ySyncPlugin` adopts, and what `initProseMirrorDoc` answers: each Yjs type of the
 *  document against the ProseMirror node, or run of nodes, built from it. y-prosemirror names
 *  this `ProsemirrorMapping` but exports the name from no entry point of its own. The key is
 *  its own `Y.AbstractType<any>`: a document's mapping holds every kind of Yjs type at once,
 *  and `AbstractType` is invariant in the event it emits, so no narrower type names them all. */
// biome-ignore lint/suspicious/noExplicitAny: y-prosemirror's own key type, for the reason above.
export type ProsemirrorMapping = Map<Y.AbstractType<any>, ProseMirrorNode | ProseMirrorNode[]>;

/** What y-prosemirror carries between the calls that read and write one document: the mapping,
 *  and its cache of which mark types may overlap. `BindingMetadata` is the name in
 *  `sync-plugin.js`, which the package exports from no entry point either, so `updateYFragment`
 *  takes this shape structurally. */
interface BindingMetadata {
  // biome-ignore lint/suspicious/noExplicitAny: y-prosemirror's own value type for this cache.
  isOMark: Map<any, boolean>;
  mapping: ProsemirrorMapping;
}

/** A top-level child of the document's fragment: one block of the document. */
export type Block = Y.XmlElement | Y.XmlText | Y.XmlHook;

/** What one batch aims to cost. Well under the 500 ms a reader feels as a freeze, so that a
 *  batch whose blocks turn out four times heavier than the batch that sized it is still inside
 *  the target. */
const BATCH_BUDGET_MS = 120;

/** How many blocks the first batch converts, before any batch has been timed. Small enough that
 *  a page of heavy blocks - a wide table, a long code block - cannot overrun on the first try,
 *  and more than a phone's screen holds, so the first batch is a screenful to read. */
const FIRST_BATCH_BLOCKS = 8;

/** The most blocks one batch converts, however cheap the last one was. A batch far larger than
 *  the budget's estimate is the one that overruns when the blocks stop being alike. */
const MAX_BATCH_BLOCKS = 512;

/** The most a batch may grow over the one before it, so one unusually cheap batch - a run of
 *  empty paragraphs - cannot size the next one off a number that says nothing. */
const MAX_BATCH_GROWTH = 4;

/**
 * A read-only window onto `fragment` that holds `blocks` and nothing else.
 *
 * `initProseMirrorDoc` reads the fragment it converts through `toArray()` and nothing else
 * (`y-prosemirror/src/lib.js`), and converts each block through the block itself, so a window
 * whose `toArray()` answers a slice converts that slice of the real fragment - the real blocks,
 * whose types are the keys the binding's mapping needs. Everything else about the window is the
 * fragment's own, through the prototype chain.
 */
function windowOnto(fragment: Y.XmlFragment, blocks: readonly Block[]): Y.XmlFragment {
  const window: Y.XmlFragment = Object.create(fragment);
  window.toArray = () => [...blocks];
  return window;
}

/**
 * A window onto the blocks of `fragment` that the batches have already built - its first
 * `built()` children - which `updateYFragment` may write through.
 *
 * `toArray()` is read at the moment of the write rather than remembered, because another
 * person's insert or delete moves those blocks while the build runs. `insert` and `delete` are
 * the real fragment's, at the same index, since the window is a prefix of it: a Yjs insert
 * through the window itself would make the window the inserted item's parent (`Item.js`) and
 * encoding the update would then throw in `findRootTypeKey` (`ID.js`), and a delete through it
 * would write the new length onto the window rather than onto the fragment. Everything
 * `updateYFragment` reads - `get`, `slice`, the document, the item - is the fragment's own
 * through the prototype chain, and reaches no block past `built()`, because the only indices it
 * is given come from `toArray()`.
 */
function writableWindowOnto(fragment: Y.XmlFragment, built: () => number): Y.XmlFragment {
  const window: Y.XmlFragment = Object.create(fragment);
  window.toArray = () => fragment.toArray().slice(0, built());
  window.insert = (index, content) => {
    fragment.insert(index, content);
  };
  window.delete = (index, length) => {
    fragment.delete(index, length);
  };
  return window;
}

/** The ids of `blocks`, as `data-block-id` spells them in the DOM: what the host takes its own
 *  copies of this batch's blocks off the page by. A block the markdown named - the server's
 *  draw, and every decision block - carries the document's own id here. */
function idsOf(blocks: readonly Block[]): string[] {
  const ids: string[] = [];
  for (const block of blocks) {
    const id = "getAttribute" in block ? block.getAttribute(BLOCK_ID_ATTR) : undefined;
    if (typeof id === "string" && id !== "") ids.push(id);
  }
  return ids;
}

/** How many blocks the next batch takes, from how long `blocks` of them cost. */
function nextBatch(blocks: number, costMs: number): number {
  const scaled = costMs <= 0 ? blocks * MAX_BATCH_GROWTH : (blocks * BATCH_BUDGET_MS) / costMs;
  return Math.max(1, Math.min(Math.round(scaled), blocks * MAX_BATCH_GROWTH, MAX_BATCH_BLOCKS));
}

/**
 * The end of this task, so what runs next is a task of its own and the browser has the thread
 * back in between: each batch of the build, the binding's adoption after the last of them, and
 * the host's swap after that (`createProofEditor`).
 *
 * A **timer**, so the continuation queues behind everything the page is already waiting on,
 * which is the whole point of handing the thread back. Two other ways were measured on this
 * pod, `iphone` at 4x CPU throttling, over the 406,319-character document of
 * `e2e/large-documents.ts` in Dispatch (dispatch://INFRA-341, R11), and each of them starves
 * the page:
 *
 * - `scheduler.yield()` stood here. The browser serves its continuation ahead of every posted
 *   task, so for as long as a build lasted nothing else on the page ran at all: no socket
 *   message, so another person's edit and a reconnect waited; no fetch callback; no timer, so
 *   React, whose scheduler posts one, drew nothing. A `page.evaluate` poll was answered twice
 *   and then not again for 209 s, while the page's own animation frames kept running
 *   throughout.
 * - `scheduler.postTask()` at its default `user-visible` priority lets a socket message
 *   through - the worst one waited 7 ms across a build - and still starves the page's own
 *   clock: Chromium runs a web-scheduling task of that priority ahead of a timer, and the
 *   worst timer handed to the browser during a build waited 19,945 ms.
 *
 * A timer is the one hand-back behind both. Its cost is the 4 ms a nested timer is clamped
 * to, which over the hundred-odd batches of that document is a tenth of a second. What the
 * build does not lose by waiting is its throughput: the draw it used to queue behind - the
 * document's own, painting a slice at a time - stops when the build begins
 * (`DocumentPreview.tsx` in Dispatch), so what a batch now waits for is the page's own work.
 */
export function endOfTask(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 0);
  return promise;
}

/** Whether `tr` is one of this module's own writes into the editor, or the binding's: a
 *  transaction marked as `_forceRerender`'s own is, which no plugin and nothing here reads as a
 *  reader's edit. */
function isTheDocumentsOwn(tr: Transaction): boolean {
  const meta = tr.getMeta(ySyncPluginKey) as { isChangeOrigin?: boolean } | undefined;
  return meta?.isChangeOrigin === true;
}

/** Where the editor's top-level block `index` starts. */
function startOf(doc: ProseMirrorNode, index: number): number {
  let pos = 0;
  for (let at = 0; at < index; at += 1) pos += doc.child(at).nodeSize;
  return pos;
}

export interface ProgressiveBuild {
  /**
   * Hands the mapping the batches made to `ySyncPlugin`
   * (`collabService.mergeOptions({ ySyncOpts: { mapping } })`, then `connect()`), in a task of
   * its own after the last batch's.
   *
   * The binding adopts in this call rather than after this function resolves, because the live
   * write path above has to end in the same task the binding begins in: an edit that arrived
   * between them would be in neither. Not called at all for a view destroyed while the batches
   * ran, which belongs to nothing and must not be connected.
   */
  adopt(mapping: ProsemirrorMapping): void;
  /** The document's own fragment, the one the binding will adopt. */
  fragment: Y.XmlFragment;
  /**
   * The editor is the reader's from here: its first batch's blocks are in it, its root is to be
   * shown, and an edit in those blocks reaches the Yjs document. Called once, in the task that
   * first batch lands in, and not at all for a document of no blocks, which has nothing to show
   * and nothing to write.
   */
  live(): void;
  /**
   * Hands the host each batch as it lands, so its own draw of the document gives way block by
   * block: called in the batch's own task with the ids of the blocks it is about to build, and
   * what it answers is called in that same task once they are in the editor. Nothing is painted
   * between the two, so no frame holds one of those blocks twice and none holds it nowhere.
   */
  onBatch?: (building: readonly string[]) => (() => void) | undefined;
  schema: Schema;
  view: EditorView;
}

/**
 * Builds `view`'s document from `fragment` a batch of blocks at a time, showing each batch to
 * the reader as it lands and writing their edits in it back to `fragment`, and has `adopt` hand
 * the binding the mapping those batches made. Every block of the fragment is in the editor's
 * document, and in the real DOM, when this resolves.
 *
 * The last batch, the adoption and the host's swap are three tasks, not one
 * (dispatch://INFRA-341, R11): together they were 1,370 ms on a phone at 4x CPU throttling,
 * over the 500 ms a reader feels as a freeze. The task between the last batch and the adoption
 * is this function's; the one between the adoption and the swap is `createProofEditor`'s, which
 * answers its handle a task after the connect so the host's swap never joins it.
 */
export async function buildTheDocumentInBatches({
  adopt,
  fragment,
  live,
  onBatch,
  schema,
  view,
}: ProgressiveBuild): Promise<void> {
  const ydoc = fragment.doc;
  if (ydoc === null) throw new Error("the document's fragment belongs to no Y.Doc");
  const meta: BindingMetadata = { isOMark: new Map(), mapping: new Map() };
  /** How many of the fragment's leading blocks are in the editor, in order: the editor's
   *  document is those blocks and nothing else, which is what makes the window below a prefix
   *  of the fragment and its indices the fragment's own. */
  let built = 0;
  /** Whether a write of this module's own is running, so the transactions it causes are not
   *  read as the reader's and written again. */
  let writing = false;
  const liveWindow = writableWindowOnto(fragment, () => built);

  /** Converts `blocks`, adds what it learned to the mapping, and answers the nodes it built.
   *  The batches' blocks are disjoint; a block converted again on purpose - one another person
   *  changed - replaces the stale entries with its own. */
  const convert = (blocks: readonly Block[]): Fragment => {
    const made = initProseMirrorDoc(windowOnto(fragment, blocks), schema);
    for (const [type, node] of made.mapping) meta.mapping.set(type, node);
    return made.doc.content;
  };

  /** Puts a change of the document's own in the editor: never undoable, and marked as
   *  `_forceRerender`'s own transaction is, so no plugin reads it as a reader's edit and
   *  nothing writes it back to the Yjs document it came from. */
  const change = (edit: (tr: Transaction) => void): void => {
    const tr = view.state.tr;
    edit(tr);
    view.dispatch(
      tr.setMeta("addToHistory", false).setMeta(ySyncPluginKey, { isChangeOrigin: true })
    );
  };

  /** Puts `content` in the editor: the first batch replaces the empty document Milkdown
   *  created, and every batch after it is appended at the end. */
  const place = (content: Fragment, first: boolean): void => {
    change((tr) => {
      if (first) {
        tr.replaceWith(0, tr.doc.content.size, content);
      } else {
        tr.insert(tr.doc.content.size, content);
      }
    });
  };

  /** Writes the reader's edits in the built blocks into the fragment, through the window onto
   *  those blocks. The editor's document is that prefix, so what `updateYFragment` reads as the
   *  fragment's children are the blocks the batches built and what it writes lands at their
   *  real indices; the fragment itself is left out of the mapping the binding will adopt, since
   *  the window is not the type that binding draws. */
  const writeTheReadersEdits = (): void => {
    const { doc } = view.state;
    updateYFragment(ydoc, liveWindow, doc, meta);
    meta.mapping.delete(liveWindow);
    built = doc.childCount;
  };

  const dispatch = view.dispatch;
  /** Reads every transaction the editor applies, and writes the reader's own through the window
   *  in the task they made it. Wrapping `dispatch` rather than adding a plugin keeps the write
   *  path exactly as long as the build: Milkdown's collab service reconfigures the view from
   *  `prosePluginsCtx` when it connects, which a plugin added here is not in. */
  const installTheWritePath = (): void => {
    view.dispatch = (tr) => {
      dispatch(tr);
      if (writing || !tr.docChanged || isTheDocumentsOwn(tr)) return;
      writing = true;
      try {
        writeTheReadersEdits();
      } finally {
        writing = false;
      }
    };
  };

  /**
   * Draws the block at `index` again, for a block another person changed.
   *
   * What goes into the editor is the smallest range of that block that differs
   * (`Fragment.findDiffStart` and `findDiffEnd`, ProseMirror's own), never the whole block, so
   * the reader's cursor is carried over that range by the transaction's own mapping: a caret
   * after another person's insert moves along with their words, and one before it does not
   * move at all. A block whose node type has changed under them is the one case with nothing
   * to map through, and it is replaced whole.
   */
  const redraw = (index: number): void => {
    const block = fragment.get(index) as Block | undefined;
    if (block === undefined || index >= view.state.doc.childCount) return;
    const drawn = convert([block]).firstChild;
    // A block `createNodeFromYElement` refused answers no node, and it deleted the Yjs type as
    // it refused; the fragment's own observer brings that deletion back here.
    if (drawn === null) return;
    change((tr) => {
      const from = startOf(tr.doc, index);
      const current = tr.doc.child(index);
      if (current.type !== drawn.type) {
        tr.replaceWith(from, from + current.nodeSize, drawn);
        return;
      }
      if (!current.sameMarkup(drawn)) tr.setNodeMarkup(from, drawn.type, drawn.attrs, drawn.marks);
      const start = current.content.findDiffStart(drawn.content);
      const ends = current.content.findDiffEnd(drawn.content);
      if (start === null || ends === null) return;
      // Where the two ends overlap - a run repeated on both sides of the change - the diff is
      // widened to the start, as ProseMirror's own document comparison does.
      const overlap = Math.max(start - Math.min(ends.a, ends.b), 0);
      const inside = from + 1;
      tr.replaceWith(
        inside + start,
        inside + ends.a + overlap,
        drawn.content.cut(start, ends.b + overlap)
      );
    });
  };

  /** Applies another person's insert or delete of a whole block inside the built run. The delta
   *  is walked over the fragment as it now stands, so `at` is the index each change lands at;
   *  a block added at or past the frontier is left to the batch that reaches it. */
  const restructure = (delta: Y.YEvent<Y.XmlFragment>["changes"]["delta"]): void => {
    let at = 0;
    for (const op of delta) {
      if (op.retain !== undefined) {
        at += op.retain;
      } else if (op.insert !== undefined) {
        const added = op.insert as Block[];
        if (at < built) {
          const content = convert(added);
          change((tr) => tr.insert(startOf(tr.doc, at), content));
          built += added.length;
        }
        at += added.length;
      } else if (op.delete !== undefined) {
        const gone = Math.min(op.delete, Math.max(built - at, 0));
        if (gone === 0) continue;
        change((tr) => {
          const from = startOf(tr.doc, at);
          let to = from;
          for (let which = at; which < at + gone; which += 1) to += tr.doc.child(which).nodeSize;
          tr.delete(from, to);
        });
        built -= gone;
      }
    }
  };

  /** Everything another person changed while the batches run, in the task it arrives in: the
   *  blocks they added or removed inside the built run first, so the indices the changed blocks
   *  are read at are the ones the fragment now holds. A deep event's `path` is read from the
   *  fragment, so its first step is the index of the block the change is in. */
  const applyTheirChange = (
    events: Y.YEvent<Y.AbstractType<unknown>>[],
    transaction: Y.Transaction
  ): void => {
    // The window's own writes come back through here; they are already in the editor.
    if (transaction.origin === ySyncPluginKey || view.isDestroyed) return;
    for (const event of events) {
      if (event.target === fragment) {
        restructure((event as Y.YEvent<Y.XmlFragment>).changes.delta);
      }
    }
    const changed = new Set<number>();
    for (const event of events) {
      const [index] = event.path;
      if (typeof index === "number" && index < built) changed.add(index);
    }
    for (const index of [...changed].sort((left, right) => left - right)) redraw(index);
  };

  /** The live path - the fragment's observer and the write wrapper - taken down once, in the
   *  task the binding goes up in, or when the build gives up on a destroyed view. */
  let watching = true;
  const stopWatching = (): void => {
    if (!watching) return;
    watching = false;
    fragment.unobserveDeep(applyTheirChange);
    view.dispatch = dispatch;
  };

  fragment.observeDeep(applyTheirChange);
  try {
    let take = FIRST_BATCH_BLOCKS;
    let replaced = false;
    /** Whether the reader has the editor: its first batch is in it and its blocks take edits
     *  that reach the Yjs document. */
    let open = false;
    // At least one pass, so a fragment holding nothing still replaces the empty document
    // Milkdown created with the empty document the fragment says, exactly as `_forceRerender`
    // would have: otherwise the binding would adopt a document holding a paragraph the
    // fragment does not, and write that paragraph to the Yjs document at the reader's first
    // keystroke.
    do {
      if (view.isDestroyed) return;
      const began = performance.now();
      const batch = fragment.toArray().slice(built, built + take) as Block[];
      const landed = onBatch?.(idsOf(batch));
      const content = convert(batch);
      // The frontier moves before the dispatch that puts the batch on the screen, never after
      // it. `place` is where the editor's own plugins first see those blocks, and one of them
      // writes in that same call: Milkdown's `syncHeadingIdPlugin` is a plugin view, so
      // ProseMirror runs its `update` inside `place`'s `view.dispatch` and it dispatches an
      // `id` for each heading the batch brought (`@milkdown/preset-commonmark`'s
      // `sync-heading-id-plugin`). That dispatch is a reader's edit as far as the write path
      // above can tell, and the window it writes through is `built` blocks wide
      // (`writableWindowOnto`), so a frontier still naming the batch before meant the window
      // and the editor disagreed by exactly this batch: `updateYFragment` wrote the
      // difference into the Yjs document, inserting the batch's blocks a second time, and
      // `writeTheReadersEdits` then left `built` at the editor's own count for this loop to
      // add the batch to a second time, which took the window past the editor on the next
      // batch and had the same call delete the blocks the batches had still to reach. The
      // document the reader opened came out of it with blocks of its own missing and others
      // under ids nothing had indexed (dispatch://INFRA-341, R11).
      //
      // It counts what the conversion made rather than what the batch held, which is the same
      // number except for a block `initProseMirrorDoc` refused: it deletes such a block from
      // the fragment as it refuses it, so the editor holds this many of the fragment's
      // leading blocks either way and the window stays the editor's own run of them.
      built += content.childCount;
      place(content, !replaced);
      replaced = true;
      if (batch.length > 0 && !open) {
        open = true;
        installTheWritePath();
        live();
      }
      landed?.();
      take = nextBatch(batch.length, performance.now() - began);
      if (built < fragment.length) await endOfTask();
    } while (built < fragment.length);
    // The last batch ends here, and the adoption is a task of its own; the live write path ends
    // in the task the binding starts in, so no edit falls between the two and anything that
    // arrives afterwards is the binding's, which is what it is for.
    await endOfTask();
    if (view.isDestroyed) return;
    stopWatching();
    adopt(meta.mapping);
  } finally {
    stopWatching();
  }
}
