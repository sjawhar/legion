// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Dispatch "ask" mark — a Proof span anchor introduced by the @sjawhar/proof-editor
 * library, additional to (not part of) Proof's own unified marks system in
 * ./editor/plugins/marks.ts and ./editor/schema/proof-marks.ts.
 *
 * Deliberately namespaced under `data-dispatch` (not `data-proof`) and its own
 * `dispatchMark` mdast node type (not `proofMark`) so this file never has to
 * modify any upstream file: it is a fully independent ProseMirror mark schema,
 * remark parse/stringify pair, and mutation helper set that plugs into the
 * same Editor.use() chain and the same headless schema builder as a sibling,
 * not a patch.
 *
 * Because it lives outside the unified marks system, `getMarks()` /
 * `applyRemoteMarks()` / the marks decorations layer in ./editor/plugins/marks.ts
 * do not know about it — it is a plain ProseMirror mark, added/removed via a
 * direct transaction and rendered via its own toDOM/CSS.
 */

import { $markSchema, $markAttr, $remark } from '@milkdown/kit/utils';
import type { Attrs, Mark as ProseMirrorMark, Node as ProseMirrorNode } from '@milkdown/kit/prose/model';
import type { EditorView } from '@milkdown/kit/prose/view';

import { generateMarkId } from 'proof-sdk-upstream/src/formats/marks.js';
import { proofMarkHandler } from 'proof-sdk-upstream/src/formats/remark-proof-marks.js';
import type { MarkRange } from 'proof-sdk-upstream/src/editor/plugins/marks';

// ============================================================================
// Public action-hook types (the CreateProofEditorOptions.onMarkAction contract)
// ============================================================================

export type SelectionBarActionKind = 'comment' | 'ask' | 'suggest';
export type PopoverActionKind = 'reply' | 'resolve' | 'unresolve' | 'accept' | 'reject' | 'delete';

export type MarkAction =
  | { kind: SelectionBarActionKind; markId: string; quote: string; from: number; to: number }
  | { kind: 'reply'; markId: string; text: string }
  | { kind: Exclude<PopoverActionKind, 'reply'>; markId: string };

// ============================================================================
// ProseMirror mark schema
// ============================================================================

function parseCommonAttrs(dom: HTMLElement): { id: string | null; by: string } {
  return {
    id: dom.getAttribute('data-id'),
    by: dom.getAttribute('data-by') || 'unknown',
  };
}

type DispatchMarkNode = {
  type?: string;
  dispatch?: string;
  attrs?: Record<string, string | null | undefined>;
  children?: unknown[];
};

export const dispatchAskAttr = $markAttr('dispatchAsk', () => ({}));

export const dispatchAskSchema = $markSchema('dispatchAsk', (ctx) => ({
  attrs: {
    id: { default: null },
    by: { default: 'unknown' },
  },
  inclusive: false,
  spanning: true,
  parseDOM: [
    {
      tag: 'span[data-dispatch="ask"]',
      getAttrs: (dom: HTMLElement): Attrs => parseCommonAttrs(dom),
    },
  ],
  toDOM: (mark) => {
    const extra = ctx.get(dispatchAskAttr.key)(mark);
    const domAttrs: Record<string, string> = {
      'data-dispatch': 'ask',
      ...extra,
    };
    if (mark.attrs.id) domAttrs['data-id'] = String(mark.attrs.id);
    if (mark.attrs.by) domAttrs['data-by'] = String(mark.attrs.by);
    return ['span', domAttrs, 0];
  },
  parseMarkdown: {
    match: (node) => (node as DispatchMarkNode).type === 'dispatchMark' && (node as DispatchMarkNode).dispatch === 'ask',
    runner: (state, node, markType) => {
      const n = node as DispatchMarkNode;
      const attrs = n.attrs || {};
      state.openMark(markType, {
        id: attrs.id ?? null,
        by: attrs.by ?? 'unknown',
      });
      state.next((n.children as never[]) || []);
      state.closeMark(markType);
    },
  },
  toMarkdown: {
    match: (mark) => mark.type.name === 'dispatchAsk',
    runner: (state, mark) => {
      state.withMark(mark, 'dispatchMark', undefined, {
        dispatch: 'ask',
        attrs: { id: mark.attrs.id ?? null, by: mark.attrs.by ?? null },
      });
    },
  },
}));

/** Pass to `.use()` in the same place `proofMarkPlugins` is used (browser and headless). */
export const dispatchMarkPlugins = [dispatchAskAttr, dispatchAskSchema];

// ============================================================================
// remark parse/stringify for <span data-dispatch="ask" data-id="…" data-by="…">
//
// Ported from ./formats/remark-proof-marks.ts's HTML-span parser, renamed and
// scoped to the `data-dispatch` attribute instead of `data-proof`, and without
// the legacy <code>/<strong>/<em>/<del> HTML-formatting migration path that
// exists there only to read pre-existing `data-proof` content written before
// a historical serialization fix — there is no pre-existing dispatchAsk
// content to migrate, so that path is intentionally not ported.
// ============================================================================

type MdastNode = {
  type: string;
  value?: string;
  children?: MdastNode[];
  [key: string]: unknown;
};
type MdastParent = { children: MdastNode[] };

function isDispatchHtml(value: string): boolean {
  return value.includes('<span') && value.includes('data-dispatch');
}

function parseAttributes(input: string): Record<string, string> | null {
  const attrs: Record<string, string> = {};
  let i = 0;

  while (i < input.length) {
    while (i < input.length && /\s/.test(input[i])) i++;
    if (i >= input.length) break;

    let name = '';
    while (i < input.length && /[^\s=]/.test(input[i])) {
      name += input[i];
      i++;
    }
    if (!name) return null;

    while (i < input.length && /\s/.test(input[i])) i++;

    let value = '';
    if (input[i] === '=') {
      i++;
      while (i < input.length && /\s/.test(input[i])) i++;
      const quote = input[i];
      if (quote === '"' || quote === '\'') {
        i++;
        while (i < input.length && input[i] !== quote) {
          value += input[i];
          i++;
        }
        if (input[i] !== quote) return null;
        i++;
      } else {
        while (i < input.length && /[^\s]/.test(input[i])) {
          value += input[i];
          i++;
        }
      }
    }

    attrs[name] = value;
  }

  return attrs;
}

function parseDispatchHtml(value: string): MdastNode[] | null {
  const root: MdastNode[] = [];
  const stack: (MdastNode & DispatchMarkNode)[] = [];

  const pushNode = (node: MdastNode) => {
    const parent = stack[stack.length - 1];
    if (parent) {
      parent.children = parent.children ?? [];
      parent.children.push(node);
    } else {
      root.push(node);
    }
  };

  let i = 0;
  while (i < value.length) {
    const nextLt = value.indexOf('<', i);
    if (nextLt === -1) {
      const text = value.slice(i);
      if (text) pushNode({ type: 'text', value: text });
      break;
    }

    if (nextLt > i) {
      const text = value.slice(i, nextLt);
      if (text) pushNode({ type: 'text', value: text });
      i = nextLt;
    }

    if (value.startsWith('</span', i)) {
      const end = value.indexOf('>', i);
      if (end === -1) return null;
      if (stack.length === 0) return null;
      stack.pop();
      i = end + 1;
      continue;
    }

    if (value.startsWith('<span', i)) {
      const end = value.indexOf('>', i);
      if (end === -1) return null;
      const attrSource = value.slice(i + 5, end).trim();
      const attrs = parseAttributes(attrSource);
      if (!attrs) return null;
      const dispatch = attrs['data-dispatch'];
      if (!dispatch) return null;

      const node: MdastNode & DispatchMarkNode = {
        type: 'dispatchMark',
        dispatch,
        attrs: { id: attrs['data-id'], by: attrs['data-by'] },
        children: [],
      };

      pushNode(node);
      stack.push(node);
      i = end + 1;
      continue;
    }

    return null;
  }

  if (stack.length > 0) return null;
  return root;
}

type DispatchSpanToken =
  | { type: 'open'; dispatch: string; attrs: Record<string, string | null | undefined> }
  | { type: 'close' };

function parseDispatchSpanToken(value: string): DispatchSpanToken | null {
  const trimmed = value.trim();
  if (/^<\/span\s*>$/i.test(trimmed)) {
    return { type: 'close' };
  }

  const openMatch = trimmed.match(/^<span\b([^>]*)>$/i);
  if (!openMatch) return null;

  const attrs = parseAttributes(openMatch[1].trim());
  if (!attrs) return null;
  const dispatch = attrs['data-dispatch'];
  if (!dispatch) return null;

  return {
    type: 'open',
    dispatch,
    attrs: { id: attrs['data-id'], by: attrs['data-by'] },
  };
}

function normalizeSplitDispatchSpans(parent: MdastParent): void {
  const { children } = parent;
  const stack: (MdastNode & DispatchMarkNode)[] = [];
  let i = 0;

  while (i < children.length) {
    const child = children[i];
    if (child.type === 'html' && typeof child.value === 'string') {
      const token = parseDispatchSpanToken(child.value);
      if (token?.type === 'open') {
        const node: MdastNode & DispatchMarkNode = {
          type: 'dispatchMark',
          dispatch: token.dispatch,
          attrs: token.attrs,
          children: [],
        };

        const current = stack[stack.length - 1];
        if (current) {
          current.children = current.children ?? [];
          current.children.push(node);
          children.splice(i, 1);
        } else {
          children.splice(i, 1, node);
          i += 1;
        }

        stack.push(node);
        continue;
      }

      if (token?.type === 'close') {
        if (stack.length > 0) {
          stack.pop();
          children.splice(i, 1);
          continue;
        }
      }
    }

    if (stack.length > 0) {
      const current = stack[stack.length - 1];
      current.children = current.children ?? [];
      current.children.push(child);
      children.splice(i, 1);
      continue;
    }
    i += 1;
  }
}

function visit(node: MdastNode): void {
  if (!node.children) return;
  normalizeSplitDispatchSpans(node as MdastParent);
  const children = node.children;

  for (let i = 0; i < children.length; i++) {
    const child = children[i];
    if (child.type === 'html' && typeof child.value === 'string' && isDispatchHtml(child.value)) {
      const parsed = parseDispatchHtml(child.value);
      if (parsed) {
        children.splice(i, 1, ...parsed);
        i += parsed.length - 1;
        continue;
      }
    }

    if (child.children) {
      visit(child);
    }
  }
}

/** Raw remark transformer factory — use directly in a `unified()` pipeline (headless). */
export function remarkDispatchMarks() {
  return (tree: MdastNode) => {
    visit(tree);
  };
}

/** Milkdown-wrapped remark plugin — use in `.use()` alongside `remarkProofMarksPlugin` (browser). */
export const remarkDispatchMarksPlugin = $remark('remarkDispatchMarks', () => () => remarkDispatchMarks());

function escapeAttr(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

function renderInlineNodes(nodes?: MdastNode[]): string {
  if (!nodes || nodes.length === 0) return '';
  return nodes.map(renderInlineNode).join('');
}

function renderInlineNode(node: MdastNode): string {
  switch (node.type) {
    case 'text':
      return node.value ?? '';
    case 'strong':
      return `**${renderInlineNodes(node.children)}**`;
    case 'emphasis':
      return `*${renderInlineNodes(node.children)}*`;
    case 'delete':
      return `~~${renderInlineNodes(node.children)}~~`;
    case 'inlineCode': {
      const val = node.value ?? '';
      if (val.includes('`')) return `\`\` ${val} \`\``;
      return `\`${val}\``;
    }
    case 'link': {
      const href = String((node as MdastNode & { url?: string }).url ?? '');
      const text = renderInlineNodes(node.children);
      return `[${text}](${href})`;
    }
    case 'image': {
      const src = String((node as MdastNode & { url?: string }).url ?? '');
      const alt = String((node as MdastNode & { alt?: string }).alt ?? '');
      return `![${alt}](${src})`;
    }
    case 'break':
      return `\\\n`;
    case 'html':
      return typeof node.value === 'string' ? node.value : '';
    case 'dispatchMark':
      return renderDispatchMarkNode(node as MdastNode & DispatchMarkNode);
    // A dispatchMark can nest inside a proofMark (e.g. an Ask anchored fully
    // within a Comment's range). The reverse — a proofMark nested inside a
    // dispatchMark's children — is handled here by delegating to upstream's
    // exported `proofMarkHandler`, which is a public API, not an edit to
    // remark-proof-marks.ts. The other direction (dispatchMark nested inside
    // a proofMark's own inline renderer) cannot be fixed without editing that
    // upstream file's switch statement; a dispatchAsk mark that is fully
    // contained within a proofComment/proofSuggestion/etc. mark's range will
    // lose its <span data-dispatch> wrapper on markdown serialization only
    // (the ProseMirror doc and DOM rendering are unaffected). See the PR
    // description for this known, documented limitation.
    case 'proofMark':
      return proofMarkHandler.call({}, node as never);
    default:
      if (node.children && node.children.length > 0) {
        return renderInlineNodes(node.children);
      }
      return node.value ?? '';
  }
}

function renderDispatchMarkNode(node: MdastNode & DispatchMarkNode): string {
  const dispatch = node.dispatch || 'ask';
  const attrs = node.attrs ?? {};
  const parts: string[] = [`data-dispatch="${escapeAttr(dispatch)}"`];

  if (attrs.id) parts.push(`data-id="${escapeAttr(String(attrs.id))}"`);
  if (attrs.by) parts.push(`data-by="${escapeAttr(String(attrs.by))}"`);

  const content = renderInlineNodes(node.children as MdastNode[] | undefined);
  return `<span ${parts.join(' ')}>${content}</span>`;
}

/** Register on `remarkStringifyOptionsCtx.handlers.dispatchMark` alongside `proofMark: proofMarkHandler`. */
export function dispatchMarkHandler(
  this: unknown,
  node: MdastNode & DispatchMarkNode,
  _parent?: unknown,
  _state?: unknown,
  _info?: unknown,
): string {
  return renderDispatchMarkNode(node);
}

// ============================================================================
// Mutation helpers — plain ProseMirror mark add/remove/find, independent of
// the unified marks plugin's metadata/decoration system.
// ============================================================================

export interface AskMarkResult {
  id: string;
  from: number;
  to: number;
}

/** Anchors a fresh dispatchAsk mark over `range` with a newly generated id. Returns null if the
 *  range is empty or the schema doesn't have the mark registered. */
export function createAskMark(view: EditorView, range: MarkRange, by: string): AskMarkResult | null {
  const { state } = view;
  const markType = state.schema.marks.dispatchAsk;
  if (!markType) return null;
  if (range.from >= range.to || range.to > state.doc.content.size) return null;

  const id = generateMarkId();
  const tr = state.tr.addMark(range.from, range.to, markType.create({ id, by }));
  view.dispatch(tr);
  return { id, from: range.from, to: range.to };
}

function collectAskMarkRanges(doc: ProseMirrorNode, markId: string): MarkRange[] {
  const ranges: MarkRange[] = [];
  doc.descendants((node, pos) => {
    if (!node.isText) return true;
    for (const mark of node.marks as ProseMirrorMark[]) {
      if (mark.type.name === 'dispatchAsk' && mark.attrs.id === markId) {
        ranges.push({ from: pos, to: pos + node.nodeSize });
      }
    }
    return true;
  });
  return ranges;
}

/** Resolves the current anchor range for a dispatchAsk mark id, or null if not found. */
export function findAskMarkRange(doc: ProseMirrorNode, markId: string): MarkRange | null {
  const ranges = collectAskMarkRanges(doc, markId);
  if (ranges.length === 0) return null;
  return { from: ranges[0].from, to: ranges[ranges.length - 1].to };
}

/** Removes every span of a dispatchAsk mark id. Returns false if none were found. */
export function removeAskMark(view: EditorView, markId: string): boolean {
  const { state } = view;
  const markType = state.schema.marks.dispatchAsk;
  if (!markType) return false;

  const ranges = collectAskMarkRanges(state.doc, markId);
  if (ranges.length === 0) return false;

  let tr = state.tr;
  for (const range of ranges.slice().reverse()) {
    tr = tr.removeMark(range.from, range.to, markType);
  }
  view.dispatch(tr);
  return true;
}
