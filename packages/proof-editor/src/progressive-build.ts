/**
 * Building a document's editor a batch of blocks at a time, so that no one task of the build
 * freezes the page (dispatch://INFRA-341, R11).
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
 * What the batches are not allowed to change. Every block ends in the real DOM, so this is
 * neither a virtual DOM nor a virtualized list. Nothing is written to the Yjs document: the
 * sync plugin is not installed while the batches run, so no transaction of theirs can reach it,
 * and each batch carries `ySyncPluginKey`'s `isChangeOrigin` exactly as `_forceRerender`'s own
 * transaction does, so every plugin that tells a remote change from a reader's - the block-id
 * stamp (`./editor/schema/block-ids.ts`) and the ask content rule (`./ask-content.ts`) - reads
 * the build as what it is. An edit another person makes while the batches run lands in the Yjs
 * document as ever, and `repair` below puts it in the editor before the binding adopts it.
 */
import { Fragment, type Node as ProseMirrorNode, type Schema } from "@milkdown/kit/prose/model";
import type { EditorView } from "@milkdown/kit/prose/view";
import { initProseMirrorDoc, ySyncPluginKey } from "y-prosemirror";
import type * as Y from "yjs";

/** What `ySyncPlugin` adopts, and what `initProseMirrorDoc` answers: each Yjs type of the
 *  document against the ProseMirror node, or run of nodes, built from it. y-prosemirror names
 *  this `ProsemirrorMapping` but exports the name from no entry point of its own. The key is
 *  its own `Y.AbstractType<any>`: a document's mapping holds every kind of Yjs type at once,
 *  and `AbstractType` is invariant in the event it emits, so no narrower type names them all. */
// biome-ignore lint/suspicious/noExplicitAny: y-prosemirror's own key type, for the reason above.
export type ProsemirrorMapping = Map<Y.AbstractType<any>, ProseMirrorNode | ProseMirrorNode[]>;

/** A top-level child of the document's fragment: one block of the document. */
export type Block = Y.XmlElement | Y.XmlText | Y.XmlHook;

/** What one batch aims to cost. Well under the 500 ms a reader feels as a freeze, so that a
 *  batch whose blocks turn out four times heavier than the batch that sized it is still inside
 *  the target. */
const BATCH_BUDGET_MS = 120;

/** How many blocks the first batch converts, before any batch has been timed. Small enough that
 *  a page of heavy blocks - a wide table, a long code block - cannot overrun on the first try. */
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
function windowOnto(fragment: Y.XmlFragment, blocks: Block[]): Y.XmlFragment {
  const window: Y.XmlFragment = Object.create(fragment);
  window.toArray = () => blocks;
  return window;
}

/**
 * Converts `blocks` and adds what it learned to `mapping`, answering the nodes it built.
 *
 * The batches' blocks are disjoint, so no batch overwrites another's entries; `repair` converts
 * a block again on purpose, and its entries replace the stale ones.
 */
function convert(
  mapping: ProsemirrorMapping,
  fragment: Y.XmlFragment,
  blocks: Block[],
  schema: Schema
): Fragment {
  const built = initProseMirrorDoc(windowOnto(fragment, blocks), schema);
  for (const [type, node] of built.mapping) mapping.set(type, node);
  return built.doc.content;
}

/** How many blocks the next batch takes, from how long `blocks` of them cost. */
function nextBatch(blocks: number, costMs: number): number {
  const scaled = costMs <= 0 ? blocks * MAX_BATCH_GROWTH : (blocks * BATCH_BUDGET_MS) / costMs;
  return Math.max(1, Math.min(Math.round(scaled), blocks * MAX_BATCH_GROWTH, MAX_BATCH_BLOCKS));
}

/** The scheduler, where the browsing context has one. Chromium 129 and later give it a
 *  `yield()`; TypeScript's DOM library does not declare it, so it is read through one cast of
 *  the global rather than one per call. */
const theScheduler: { yield?: () => Promise<void> } | undefined = (
  globalThis as typeof globalThis & { scheduler?: { yield?: () => Promise<void> } }
).scheduler;

/**
 * The end of this task, so what runs next is a task of its own and the browser has the thread
 * back in between: each batch of the build, the binding's adoption after the last of them, and
 * the host's swap after that (`createProofEditor`).
 *
 * `scheduler.yield()` where it exists, because what the build must not do is wait: it resumes
 * in the next task, ahead of the page's own timers, where a `setTimeout` waits behind every
 * task already queued. Measured on this pod, `iphone` at 4x CPU throttling, the 406,319-character
 * document's build is 4.2 s of work in 33 batches; through `setTimeout` those batches took
 * 58 s of wall clock, each one queued behind a slice of the document's own draw painting
 * (`DocumentPreview.tsx` in Dispatch), while the reader waited for an editor.
 */
export function endOfTask(): Promise<void> {
  if (theScheduler?.yield !== undefined) return theScheduler.yield();
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 0);
  return promise;
}

/** Puts `content` in the editor: the first batch replaces the empty document Milkdown created,
 *  and every batch after it is appended at the end. Marked as `_forceRerender`'s own
 *  transaction is, so no plugin reads the build as a reader's edit and none of it is undoable. */
function place(view: EditorView, content: Fragment, first: boolean): void {
  const tr = view.state.tr;
  if (first) {
    tr.replaceWith(0, tr.doc.content.size, content);
  } else {
    tr.insert(tr.doc.content.size, content);
  }
  view.dispatch(
    tr.setMeta("addToHistory", false).setMeta(ySyncPluginKey, { isChangeOrigin: true })
  );
}

/**
 * Puts the document the fragment holds now in the editor, for a fragment that changed while the
 * batches ran: another person's edit, or this document's own settlement arriving.
 *
 * A block the change touched is converted again, a block it added is converted, a block it
 * removed is left out, and every other block is the node that batch already built - the same
 * object, so ProseMirror keeps its node views and the DOM it has. The caller runs this in the
 * same task as the binding it hands the mapping to, so nothing can change between the two.
 */
function repair(
  view: EditorView,
  fragment: Y.XmlFragment,
  schema: Schema,
  mapping: ProsemirrorMapping,
  changed: Set<Block>
): void {
  const blocks = fragment.toArray();
  const stale = blocks.filter((block) => changed.has(block) || !mapping.has(block));
  if (stale.length > 0) convert(mapping, fragment, stale, schema);
  const nodes: ProseMirrorNode[] = [];
  for (const block of blocks) {
    const node = mapping.get(block);
    // A block `createNodeFromYElement` refused answers no node, and it deleted the Yjs type as
    // it refused; the document it writes is the one `_forceRerender` would have written too.
    if (node !== undefined && !Array.isArray(node)) nodes.push(node);
  }
  const tr = view.state.tr.replaceWith(0, view.state.doc.content.size, Fragment.fromArray(nodes));
  view.dispatch(
    tr.setMeta("addToHistory", false).setMeta(ySyncPluginKey, { isChangeOrigin: true })
  );
}

export interface ProgressiveBuild {
  /** The document's own fragment, the one the binding will adopt. */
  fragment: Y.XmlFragment;
  schema: Schema;
  view: EditorView;
  /**
   * Hands the mapping the batches made to `ySyncPlugin`
   * (`collabService.mergeOptions({ ySyncOpts: { mapping } })`, then `connect()`), in a task of
   * its own after the last batch's.
   *
   * The binding adopts in this call rather than after this function resolves, because the
   * repair below and the adoption have to fall in one task: an edit that arrived between them
   * would be in the Yjs document, absent from the editor, and no business of a binding that
   * only draws what arrives after it. Not called at all for a view destroyed while the batches
   * ran, which belongs to nothing and must not be connected.
   */
  adopt(mapping: ProsemirrorMapping): void;
}

/**
 * Builds `view`'s document from `fragment` a batch of blocks at a time, and has `adopt` hand
 * the binding the mapping those batches made. Every block of the fragment is in the editor's
 * document, and in the real DOM, when this resolves.
 *
 * The caller keeps `view`'s root hidden for the whole of this and until it swaps the editor in
 * (`createProofEditor`'s `reveal`). The reader has the document's own draw in front of them
 * while an editor is built for them (`DocumentPreview.tsx` in Dispatch), and a build that
 * painted its batches would put a second, growing copy of the document under that draw;
 * hidden, the batches cost no layout either, which is the one part of the old single task that
 * was already cheap.
 *
 * The last batch, the adoption and the swap are three tasks, not one (dispatch://INFRA-341,
 * R11): together they were 1,370 ms on a phone at 4x CPU throttling, over the 500 ms a reader
 * feels as a freeze. The task between the last batch and the adoption is this function's;
 * the one between the adoption and the swap is `createProofEditor`'s, which answers its handle
 * a task after the connect so the host's swap never joins it.
 */
export async function buildTheDocumentInBatches({
  adopt,
  fragment,
  schema,
  view,
}: ProgressiveBuild): Promise<void> {
  const mapping: ProsemirrorMapping = new Map();
  const changed = new Set<Block>();
  let changedAtAll = false;
  // Which blocks changed while the batches ran. A deep event's `path` is read from the fragment,
  // so its first step is the index of the block the change is in; an event on the fragment
  // itself is a block added or removed, which `repair` reads off the fragment as it stands.
  const note = (events: Y.YEvent<Y.AbstractType<unknown>>[]): void => {
    changedAtAll = true;
    for (const event of events) {
      const [index] = event.path;
      if (typeof index !== "number") continue;
      const block = fragment.get(index);
      if (block !== undefined) changed.add(block as Block);
    }
  };
  fragment.observeDeep(note);
  try {
    const blocks = fragment.toArray();
    let built = 0;
    let take = FIRST_BATCH_BLOCKS;
    // At least one pass, so a fragment holding nothing still replaces the empty document
    // Milkdown created with the empty document the fragment says, exactly as `_forceRerender`
    // would have: otherwise the binding would adopt a document holding a paragraph the
    // fragment does not, and write that paragraph to the Yjs document at the reader's first
    // keystroke.
    do {
      if (view.isDestroyed) return;
      const began = performance.now();
      const batch = blocks.slice(built, built + take);
      place(view, convert(mapping, fragment, batch, schema), built === 0);
      built += batch.length;
      take = nextBatch(batch.length, performance.now() - began);
      if (built < blocks.length) await endOfTask();
    } while (built < blocks.length);
    // The last batch ends here, and the adoption is a task of its own; from the repair to the
    // binding is one task, so no edit can arrive between the two, and anything that arrives
    // afterwards is the binding's, which is what it is for.
    await endOfTask();
    if (view.isDestroyed) return;
    if (changedAtAll) repair(view, fragment, schema, mapping, changed);
    adopt(mapping);
  } finally {
    fragment.unobserveDeep(note);
  }
}
