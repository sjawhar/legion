/**
 * Stable block identity.
 *
 * Every block node carries a `blockId` attribute: a uuid minted when the block is
 * created and kept through edits, moves, splits (the first half keeps the id, the
 * second half gets a new one) and merges. The id renders to the DOM as
 * `data-block-id` and travels through collaboration as an ordinary Yjs element
 * attribute; it is never written to markdown. Hosts use it to point at a block -
 * deep links, anchors, references - without quoting its text.
 *
 * The attribute is named `blockId`, not `id`: Milkdown's heading already has an
 * `id` attribute (its anchor slug, regenerated from the heading text).
 *
 * Minting happens in two places. The browser editor stamps blocks created by
 * local transactions in an `appendTransaction` plugin, and when a local edit leaves
 * one id on two blocks (a paste, a drop or an insert of a copy) the block that held
 * the id before the edit keeps it and the other gets a new one; remote (y-prosemirror)
 * transactions are left alone, since the peer that created the block stamped it,
 * and a document loaded from the server carries whatever ids the server gave it.
 * The headless engine stamps everything `parseMarkdown` produces (`withBlockIds`).
 */
import type { Ctx } from '@milkdown/kit/ctx';
import { Plugin, PluginKey } from '@milkdown/kit/prose/state';
import type { Node as ProseMirrorNode, DOMOutputSpec, TagParseRule } from '@milkdown/kit/prose/model';
import type { Transaction } from '@milkdown/kit/prose/state';
import { AttrStep, Mapping } from '@milkdown/kit/prose/transform';
import type { Step } from '@milkdown/kit/prose/transform';
import type { NodeSchema } from '@milkdown/kit/transformer';
import { $prose } from '@milkdown/kit/utils';
import type { $NodeSchema } from '@milkdown/kit/utils';
import { withDomAttributes } from './dom-attributes';
import { uuidV4 } from './uuid';
import {
  blockquoteSchema,
  bulletListSchema,
  headingSchema,
  hrSchema,
  htmlSchema,
  orderedListSchema,
  paragraphSchema,
} from '@milkdown/preset-commonmark';
import {
  extendListItemSchemaForTask,
  footnoteDefinitionSchema,
  tableCellSchema,
  tableHeaderRowSchema,
  tableHeaderSchema,
  tableRowSchema,
  tableSchema,
} from '@milkdown/preset-gfm';
import { codeBlockSchemaExt } from 'proof-sdk-upstream/src/editor/schema/code-block-ext';
import { frontmatterSchema } from 'proof-sdk-upstream/src/editor/schema/frontmatter';
import { ySyncPluginKey } from 'y-prosemirror';

export const BLOCK_ID_ATTR = 'blockId';
export const BLOCK_ID_DOM_ATTR = 'data-block-id';

export type BlockIdGenerator = () => string;

const defaultGenerator: BlockIdGenerator = uuidV4;
let generator: BlockIdGenerator = defaultGenerator;

/** Replaces the id generator - tests and fixture generators use a deterministic one. */
export function setBlockIdGenerator(next: BlockIdGenerator | null): void {
  generator = next ?? defaultGenerator;
}

export function mintBlockId(): string {
  return generator();
}

/** True for a node that carries a block id: every block node except the document root. */
export function isIdentifiedBlock(node: ProseMirrorNode): boolean {
  return node.isBlock && node.type.name !== 'doc' && BLOCK_ID_ATTR in node.type.spec.attrs!;
}

export function blockIdOf(node: ProseMirrorNode): string | null {
  const value = node.attrs[BLOCK_ID_ATTR];
  return typeof value === 'string' && value !== '' ? value : null;
}

function readBlockId(dom: unknown): string | null {
  if (typeof dom !== 'object' || dom === null || !('getAttribute' in dom)) return null;
  const value = (dom as Element).getAttribute(BLOCK_ID_DOM_ATTR);
  return value === null || value === '' ? null : value;
}


function withDomBlockId(spec: DOMOutputSpec, blockId: string | null): DOMOutputSpec {
  return blockId === null ? spec : withDomAttributes(spec, { [BLOCK_ID_DOM_ATTR]: blockId });
}

function withParsedBlockId(rule: TagParseRule): TagParseRule {
  if (!('tag' in rule)) return rule;
  const { getAttrs, attrs: staticAttrs, ...rest } = rule as TagParseRule & {
    getAttrs?: (dom: HTMLElement) => Record<string, unknown> | false | null;
    attrs?: Record<string, unknown>;
  };
  return {
    ...rest,
    getAttrs: (dom: HTMLElement) => {
      const base = getAttrs ? getAttrs(dom) : (staticAttrs ?? null);
      if (base === false) return false;
      return { ...(base ?? {}), [BLOCK_ID_ATTR]: readBlockId(dom) };
    },
  } as TagParseRule;
}

/** Extends a block NodeSchema with the `blockId` attribute and its DOM round-trip. */
export function withBlockIdSpec(spec: NodeSchema): NodeSchema {
  const toDOM = spec.toDOM;
  return {
    ...spec,
    attrs: { ...(spec.attrs ?? {}), [BLOCK_ID_ATTR]: { default: null } },
    parseDOM: spec.parseDOM?.map(withParsedBlockId),
    toDOM: toDOM ? (node) => withDomBlockId(toDOM(node), blockIdOf(node)) : undefined,
  };
}

function extend<T extends string>(schema: $NodeSchema<T>): $NodeSchema<T> {
  return schema.extendSchema((prev) => (ctx: Ctx) => withBlockIdSpec(prev(ctx)));
}

/**
 * Every block schema is re-registered with `blockId`, including the upstream
 * code-block and frontmatter schemas the consumer imports.
 */
export const blockIdSchemas = [
  codeBlockSchemaExt,
  frontmatterSchema,
  paragraphSchema,
  headingSchema,
  blockquoteSchema,
  bulletListSchema,
  orderedListSchema,
  extendListItemSchemaForTask,
  hrSchema,
  htmlSchema,
  tableSchema,
  tableHeaderRowSchema,
  tableRowSchema,
  tableCellSchema,
  tableHeaderSchema,
  footnoteDefinitionSchema,
].map(extend);

export const blockIdsPluginKey = new PluginKey<BlockIdCounts>('proof-block-ids');

/** Receives the number of nodes a stamping pass read - see `createBlockIdsPlugin`. */
export type BlockIdVisitCounter = (read: number) => void;

let visitCounter: BlockIdVisitCounter | null = null;

/**
 * Observes what each stamping pass reads, including the passes that stamp nothing and so
 * append no transaction to carry the count. Tests measure the plugin's cost with it; it
 * follows `setBlockIdGenerator`, this file's existing test seam.
 */
export function setBlockIdVisitCounter(next: BlockIdVisitCounter | null): void {
  visitCounter = next;
}

/** How many nodes a pass has visited so far; shared by every walk of one pass. */
interface VisitCount {
  read: number;
}

/** A span of one document, in that document's own positions, as `nodesBetween` takes it. */
interface Span {
  from: number;
  to: number;
}

/** The spans a batch changed: `before` in the old document, `after` in the new one. */
interface ChangedSpans {
  before: Span[];
  after: Span[];
}

/** `spans` clamped to a document of `size`, sorted, and joined where they meet or overlap. */
function mergeSpans(spans: Span[], size: number): Span[] {
  const clamp = (pos: number): number => Math.min(Math.max(pos, 0), size);
  const sorted = spans
    .map(({ from, to }) => ({ from: clamp(Math.min(from, to)), to: clamp(Math.max(from, to)) }))
    .sort((a, b) => a.from - b.from);
  const merged: Span[] = [];
  for (const span of sorted) {
    const last = merged[merged.length - 1];
    if (last !== undefined && span.from <= last.to) last.to = Math.max(last.to, span.to);
    else merged.push({ ...span });
  }
  return merged;
}

/**
 * Every node `spans` touch and every ancestor of one, each handed to `f` once however many
 * spans reach it. Every visit is counted, the repeats included: that is the work done.
 */
function forEachInSpans(
  doc: ProseMirrorNode,
  spans: readonly Span[],
  visits: VisitCount,
  f: (node: ProseMirrorNode, pos: number) => void,
): void {
  const seen = new Set<number>();
  for (const { from, to } of spans) {
    doc.nodesBetween(from, to, (node, pos) => {
      visits.read += 1;
      if (seen.has(pos)) return;
      seen.add(pos);
      f(node, pos);
    });
  }
}

/**
 * Where two documents differ, for a batch y-prosemirror applied: it writes a remote change as
 * one replace whose step map is of no finer grain than the region it covers, so the documents'
 * own content diff names the spans instead.
 */
function diffSpans(before: ProseMirrorNode, after: ProseMirrorNode): ChangedSpans {
  const start = before.content.findDiffStart(after.content);
  const end = start === null ? null : before.content.findDiffEnd(after.content);
  if (start === null || end === null) return { before: [], after: [] };
  // The two ends may run back past the shared start; the overlap is pushed onto both.
  const overlap = start - Math.min(end.a, end.b);
  const grow = overlap > 0 ? overlap : 0;
  return {
    before: [{ from: start, to: end.a + grow }],
    after: [{ from: start, to: end.b + grow }],
  };
}

/**
 * The spans `transactions` changed, each expressed against the one old and the one new
 * document the pass compares: every `[oldStart, oldEnd, newStart, newEnd]` a step's own map
 * reports, the old end mapped back through the steps before it and the new end forward
 * through the steps after it.
 */
function changedSpans(
  before: ProseMirrorNode,
  after: ProseMirrorNode,
  transactions: readonly Transaction[],
): ChangedSpans {
  if (transactions.some(isRemote)) return diffSpans(before, after);
  const steps: Step[] = [];
  for (const tr of transactions) steps.push(...tr.steps);
  const mapping = new Mapping(steps.map((step) => step.getMap()));
  const oldSpans: Span[] = [];
  const newSpans: Span[] = [];
  steps.forEach((step, index) => {
    const back = mapping.slice(0, index).invert();
    const forward = mapping.slice(index + 1);
    let moved = false;
    mapping.maps[index].forEach((oldStart, oldEnd, newStart, newEnd) => {
      moved = true;
      oldSpans.push({ from: back.map(oldStart, -1), to: back.map(oldEnd, 1) });
      newSpans.push({ from: forward.map(newStart, -1), to: forward.map(newEnd, 1) });
    });
    // An `AttrStep` moves nothing, so its map reports no span: the node it changes is the
    // span. A `DocAttrStep` changes the root, which carries no block id, and needs none.
    if (!moved && step instanceof AttrStep) {
      oldSpans.push({ from: back.map(step.pos, -1), to: back.map(step.pos + 1, 1) });
      newSpans.push({ from: forward.map(step.pos, -1), to: forward.map(step.pos + 1, 1) });
    }
  });
  return {
    before: mergeSpans(oldSpans, before.content.size),
    after: mergeSpans(newSpans, after.content.size),
  };
}

/**
 * How many blocks of the document carry each id, with what the plugin has read to keep it.
 *
 * `counts` is mutated from one value into the next rather than copied: a copy of every id of
 * the document on every transaction would cost what this plugin's walk no longer does. The
 * value that gave its map away is `spent`, and a second transaction applied from the same
 * state - a branch the editor almost never takes - rebuilds the counts rather than read a map
 * another document now describes.
 */
interface BlockIdCounts {
  counts: Map<string, number>;
  /** Nodes visited since the state was created; a pass reads its own cost as the difference. */
  read: number;
  spent: boolean;
}

function adjustCount(counts: Map<string, number>, node: ProseMirrorNode, by: number): void {
  if (!isIdentifiedBlock(node)) return;
  const id = blockIdOf(node);
  if (id === null) return;
  const next = (counts.get(id) ?? 0) + by;
  if (next > 0) counts.set(id, next);
  else counts.delete(id);
}

/** The blocks-per-id of a whole document: the load's walk, and the rebuild of a spent value. */
function countEveryBlock(doc: ProseMirrorNode, visits: VisitCount): Map<string, number> {
  const counts = new Map<string, number>();
  doc.descendants((node) => {
    visits.read += 1;
    adjustCount(counts, node, 1);
  });
  return counts;
}

interface MissingBlock {
  pos: number;
  node: ProseMirrorNode;
}

/** No position of a document: the id's holder is a block outside every changed span. */
const OUTSIDE = -1;

/**
 * Blocks of the changed spans with no id, or with an id `counts` shows on another block too.
 * Of the blocks sharing one id, the one `keeper` names keeps it; an id whose count reaches
 * past the spans is held by a block the edit did not touch, so every copy in the spans gets a
 * new one; otherwise the id stays with its first block in document order, which is where a
 * split leaves it (the first half keeps the attrs both halves were copied from). A block
 * outside every span is never stamped: the edit did not put its id on two blocks.
 */
function blocksNeedingIds(
  doc: ProseMirrorNode,
  spans: readonly Span[],
  counts: ReadonlyMap<string, number>,
  keeper: (id: string) => number | undefined,
  visits: VisitCount,
): MissingBlock[] {
  const blocks: { pos: number; node: ProseMirrorNode; id: string | null }[] = [];
  const here = new Map<string, number>();
  forEachInSpans(doc, spans, visits, (node, pos) => {
    if (!isIdentifiedBlock(node)) return;
    const id = blockIdOf(node);
    blocks.push({ pos, node, id });
    if (id !== null) here.set(id, (here.get(id) ?? 0) + 1);
  });
  const kept = new Map<string, number>();
  for (const { pos, id } of blocks) {
    if (id === null || (counts.get(id) ?? 0) < 2 || kept.has(id)) continue;
    if ((counts.get(id) ?? 0) > (here.get(id) ?? 0)) {
      kept.set(id, OUTSIDE);
      continue;
    }
    const held = keeper(id);
    const holderHere = held !== undefined && blocks.some((block) => block.id === id && block.pos === held);
    kept.set(id, holderHere ? held : pos);
  }
  return blocks
    .filter(({ pos, id }) => id === null || (kept.has(id) && kept.get(id) !== pos))
    .map(({ pos, node }) => ({ pos, node }));
}

/**
 * Where, in the document `transactions` produced, the block that held `id` in `before`
 * now starts: its old position mapped through every step, leaning past anything
 * inserted right at its start, so a copy inserted above it does not take its place. Only
 * the changed spans of `before` are read, and only when a duplicated id needs them: a block
 * outside them reaches the new document unchanged, where its copy is found instead.
 */
function holderAfter(
  before: ProseMirrorNode,
  spans: readonly Span[],
  transactions: readonly Transaction[],
  visits: VisitCount,
): (id: string) => number | undefined {
  const mapping = new Mapping();
  for (const tr of transactions) mapping.appendMapping(tr.mapping);
  let held: Map<string, number> | undefined;
  return (id) => {
    if (held === undefined) {
      const holders = new Map<string, number>();
      forEachInSpans(before, spans, visits, (node, pos) => {
        if (!isIdentifiedBlock(node)) return;
        const blockId = blockIdOf(node);
        if (blockId !== null && !holders.has(blockId)) holders.set(blockId, pos);
      });
      held = holders;
    }
    const pos = held.get(id);
    return pos === undefined ? undefined : mapping.map(pos, 1);
  };
}

/** A copy of `doc` with every block identified; the input is returned unchanged when nothing is missing. */
export function withBlockIds(doc: ProseMirrorNode, mint: BlockIdGenerator = mintBlockId): ProseMirrorNode {
  const seen = new Set<string>();
  let changed = false;
  const visit = (node: ProseMirrorNode): ProseMirrorNode => {
    let attrs = node.attrs;
    if (isIdentifiedBlock(node)) {
      const id = blockIdOf(node);
      if (id === null || seen.has(id)) {
        attrs = { ...attrs, [BLOCK_ID_ATTR]: mint() };
        changed = true;
      }
      seen.add(attrs[BLOCK_ID_ATTR] as string);
    }
    if (node.isLeaf) {
      return attrs === node.attrs ? node : node.type.create(attrs, null, node.marks);
    }
    const children: ProseMirrorNode[] = [];
    let childChanged = false;
    node.forEach((child) => {
      const next = visit(child);
      if (next !== child) childChanged = true;
      children.push(next);
    });
    if (attrs === node.attrs && !childChanged) return node;
    return node.type.create(attrs, children, node.marks);
  };
  const result = visit(doc);
  return changed ? result : doc;
}

function isRemote(tr: Transaction): boolean {
  const meta = tr.getMeta(ySyncPluginKey) as { isChangeOrigin?: boolean } | undefined;
  return meta?.isChangeOrigin === true;
}

/**
 * Stamps ids on blocks created by local transactions. Remote transactions and the
 * initial document load are left to their origin (the peer or the server).
 *
 * The plugin reads only what an edit changed. Its state holds how many blocks of the
 * document carry each id: `init` counts them over the whole document, which is the load and
 * happens once, and `apply` lowers the counts for the blocks of the changed spans of the old
 * document and raises them for those of the new one. A pass then stamps within those spans
 * alone - a block with no id, and a copy of an id the counts show on another block, which
 * the old holder keeps (`holderAfter`). A block outside them is left as it is, a peer's
 * block with no id included: no local edit created it, and the server stamps it.
 *
 * The pass reports what it costs: `read` is the number of nodes it visited, its state's
 * walks for this batch included. Nodes visited, not blocks found: a walk that reads less is
 * the point. The count reaches a stamping pass's transaction as the meta `{ stamped, read }`
 * under `blockIdsPluginKey`, and every pass, stamping or not, through
 * `setBlockIdVisitCounter`.
 */
export function createBlockIdsPlugin(): Plugin<BlockIdCounts> {
  return new Plugin<BlockIdCounts>({
    key: blockIdsPluginKey,
    state: {
      init(_config, state) {
        const visits: VisitCount = { read: 0 };
        return { counts: countEveryBlock(state.doc, visits), read: visits.read, spent: false };
      },
      apply(tr, value) {
        if (!tr.docChanged) return value;
        const visits: VisitCount = { read: 0 };
        if (value.spent) {
          const counts = countEveryBlock(tr.doc, visits);
          return { counts, read: value.read + visits.read, spent: false };
        }
        const spans = changedSpans(tr.before, tr.doc, [tr]);
        forEachInSpans(tr.before, spans.before, visits, (node) => adjustCount(value.counts, node, -1));
        forEachInSpans(tr.doc, spans.after, visits, (node) => adjustCount(value.counts, node, 1));
        value.spent = true;
        return { counts: value.counts, read: value.read + visits.read, spent: false };
      },
    },
    appendTransaction(transactions, oldState, newState) {
      if (!transactions.some((tr) => tr.docChanged)) return null;
      const before = blockIdsPluginKey.getState(oldState);
      const after = blockIdsPluginKey.getState(newState);
      // This plugin's own state, so both are there; the key's type does not know it.
      if (before === undefined || after === undefined) return null;
      // What the state's walks have already read for this batch.
      const visits: VisitCount = { read: after.read - before.read };
      if (transactions.some((tr) => isRemote(tr) || tr.getMeta('document-load'))) {
        visitCounter?.(visits.read);
        return null;
      }
      const spans = changedSpans(oldState.doc, newState.doc, transactions);
      const keeper = holderAfter(oldState.doc, spans.before, transactions, visits);
      const missing = blocksNeedingIds(newState.doc, spans.after, after.counts, keeper, visits);
      visitCounter?.(visits.read);
      if (missing.length === 0) return null;
      let tr = newState.tr;
      for (const { pos, node } of missing) {
        tr = tr.setNodeMarkup(pos, undefined, { ...node.attrs, [BLOCK_ID_ATTR]: mintBlockId() }, node.marks);
      }
      return tr
        .setMeta('addToHistory', false)
        .setMeta(blockIdsPluginKey, { stamped: missing.length, read: visits.read });
    },
  });
}

export const blockIdsPlugin = $prose(() => createBlockIdsPlugin());

export const blockIdPlugins = [...blockIdSchemas, blockIdsPlugin].flat();
