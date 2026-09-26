// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
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
import type { Node as ProseMirrorNode, NodeSpec, DOMOutputSpec, ParseRule } from '@milkdown/kit/prose/model';
import type { Transaction } from '@milkdown/kit/prose/state';
import { Mapping } from '@milkdown/kit/prose/transform';
import { $prose } from '@milkdown/kit/utils';
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
import { ySyncPluginKey } from 'y-prosemirror';

export const BLOCK_ID_ATTR = 'blockId';
export const BLOCK_ID_DOM_ATTR = 'data-block-id';

export type BlockIdGenerator = () => string;

const defaultGenerator: BlockIdGenerator = () => crypto.randomUUID();
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

function isAttrsObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) && !('nodeType' in value);
}

/** Adds `data-block-id` to a DOMOutputSpec without disturbing the rest of it. */
function withDomBlockId(spec: DOMOutputSpec, blockId: string | null): DOMOutputSpec {
  if (blockId === null) return spec;
  if (Array.isArray(spec)) {
    const [tag, second, ...rest] = spec as unknown[];
    if (isAttrsObject(second)) {
      return [tag, { ...second, [BLOCK_ID_DOM_ATTR]: blockId }, ...rest] as unknown as DOMOutputSpec;
    }
    return [tag, { [BLOCK_ID_DOM_ATTR]: blockId }, second, ...rest].filter(
      (part) => part !== undefined,
    ) as unknown as DOMOutputSpec;
  }
  if (typeof spec === 'object' && spec !== null && 'dom' in spec) {
    (spec.dom as Element).setAttribute?.(BLOCK_ID_DOM_ATTR, blockId);
    return spec;
  }
  if (typeof spec === 'object' && spec !== null && 'setAttribute' in spec) {
    (spec as Element).setAttribute(BLOCK_ID_DOM_ATTR, blockId);
  }
  return spec;
}

function withParsedBlockId(rule: ParseRule): ParseRule {
  if (!('tag' in rule)) return rule;
  const { getAttrs, attrs: staticAttrs, ...rest } = rule as ParseRule & {
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
  } as ParseRule;
}

/** Extends a block NodeSpec with the `blockId` attribute and its DOM round-trip. */
export function withBlockIdSpec(spec: NodeSpec): NodeSpec {
  const toDOM = spec.toDOM;
  return {
    ...spec,
    attrs: { ...(spec.attrs ?? {}), [BLOCK_ID_ATTR]: { default: null } },
    parseDOM: spec.parseDOM?.map(withParsedBlockId),
    toDOM: toDOM ? (node) => withDomBlockId(toDOM(node), blockIdOf(node)) : undefined,
  };
}

type SchemaFactory = (ctx: Ctx) => NodeSpec;

function extend(schema: { extendSchema: (handler: (prev: SchemaFactory) => SchemaFactory) => unknown }) {
  return schema.extendSchema((prev) => (ctx) => withBlockIdSpec(prev(ctx)));
}

/**
 * The preset block schemas re-registered with `blockId`. `code_block` and
 * `frontmatter` are the fork's own schemas and extend themselves in their modules.
 */
export const blockIdSchemas = [
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

export const blockIdsPluginKey = new PluginKey('proof-block-ids');

interface MissingBlock {
  pos: number;
  node: ProseMirrorNode;
}

/**
 * Blocks with no id, or with an id another block of the document keeps. Of the blocks
 * sharing one id, the one `keeper` names keeps it; any other id stays with its first
 * block in document order, which is where a split leaves it (the first half keeps the
 * attrs both halves were copied from).
 */
function blocksNeedingIds(doc: ProseMirrorNode, keeper: (id: string) => number | undefined): MissingBlock[] {
  const blocks: { pos: number; node: ProseMirrorNode; id: string | null }[] = [];
  const count = new Map<string, number>();
  doc.descendants((node, pos) => {
    if (!isIdentifiedBlock(node)) return;
    const id = blockIdOf(node);
    blocks.push({ pos, node, id });
    if (id !== null) count.set(id, (count.get(id) ?? 0) + 1);
  });
  const kept = new Map<string, number>();
  for (const { pos, id } of blocks) {
    if (id === null || count.get(id) === 1 || kept.has(id)) continue;
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
 * inserted right at its start, so a copy inserted above it does not take its place.
 */
function holderAfter(before: ProseMirrorNode, transactions: readonly Transaction[]): (id: string) => number | undefined {
  const mapping = new Mapping();
  for (const tr of transactions) mapping.appendMapping(tr.mapping);
  let held: Map<string, number> | undefined;
  return (id) => {
    if (held === undefined) {
      const holders = new Map<string, number>();
      before.descendants((node, pos) => {
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
 */
export function createBlockIdsPlugin(): Plugin {
  return new Plugin({
    key: blockIdsPluginKey,
    appendTransaction(transactions, oldState, newState) {
      if (!transactions.some((tr) => tr.docChanged)) return null;
      if (transactions.some((tr) => isRemote(tr) || tr.getMeta('document-load'))) return null;
      const missing = blocksNeedingIds(newState.doc, holderAfter(oldState.doc, transactions));
      if (missing.length === 0) return null;
      let tr = newState.tr;
      for (const { pos, node } of missing) {
        tr = tr.setNodeMarkup(pos, undefined, { ...node.attrs, [BLOCK_ID_ATTR]: mintBlockId() }, node.marks);
      }
      return tr.setMeta('addToHistory', false).setMeta(blockIdsPluginKey, { stamped: missing.length });
    },
  });
}

export const blockIdsPlugin = $prose(() => createBlockIdsPlugin());

export const blockIdPlugins = [...blockIdSchemas, blockIdsPlugin].flat();
