// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Proof Editor — standalone library entry point.
 *
 * Constructs the Milkdown editor + Proof's mark/collab plugin stack WITHOUT
 * the app shell singleton in src/editor/index.ts (ProofEditorImpl) and
 * without any of its ../bridge, ../agent, ../analytics, ../ui imports for
 * auth/websocket/dialogs/analytics.
 *
 * The host owns the Y.Doc and its transport (e.g. HocuspocusProvider); this
 * factory only binds to what it is given via `opts.ydoc` / `opts.awareness`.
 * Mark-affecting user actions (Comment/Suggest/Ask on the selection bar,
 * Reply/Resolve/Accept/Reject on the mark popover) are reported through
 * `opts.onMarkAction` instead of being applied unconditionally — see
 * ./dispatch-action-bar.ts and ./dispatch-popover-hook.ts for the two
 * distinct interception mechanisms this required.
 *
 * Construction and collab-wiring below is adapted from src/editor/index.ts's
 * `ProofEditorImpl.init()` (plugin `.use()` chain) and `initFromShare()`'s
 * collab activation + `installCollabCursorsWhenReady()` — copied, not
 * imported, so this file has zero dependency on the ProofEditorImpl class or
 * any app-shell singleton.
 */

import { BLOCK_ID_DOM_ATTR, blockIdOf, blockIdPlugins } from './editor/schema/block-ids';
import { blockSchemaPlugins, type BlockSchema, type HostBlockRenderer } from './block-schema';
import { createTypedBlockCommands, type TypedBlockCommands } from './typed-block-commands';
import {
  Editor,
  rootCtx,
  defaultValueCtx,
  editorViewCtx,
  parserCtx,
  remarkCtx,
  schemaCtx,
  serializerCtx,
  remarkStringifyOptionsCtx,
  prosePluginsCtx,
} from '@milkdown/core';
import { ParserState } from '@milkdown/transformer';
import { commonmark } from '@milkdown/preset-commonmark';
import { gfm } from '@milkdown/preset-gfm';
import { history } from '@milkdown/plugin-history';
import { collab, collabServiceCtx, type CollabService } from '@milkdown/plugin-collab';
import { listener } from '@milkdown/plugin-listener';
import { cursor } from '@milkdown/plugin-cursor';
import { clipboard } from '@milkdown/plugin-clipboard';
import { nord } from '@milkdown/theme-nord';
import { yCursorPlugin, yCursorPluginKey, ySyncPluginKey } from 'y-prosemirror';
import type { Awareness } from 'y-protocols/awareness';
import type * as Y from 'yjs';
import type { EditorView } from '@milkdown/kit/prose/view';
import type { Ctx } from '@milkdown/ctx';

import { proofMarkPlugins } from 'proof-sdk-upstream/src/editor/schema/proof-marks';
import { codeBlockExtPlugins } from 'proof-sdk-upstream/src/editor/schema/code-block-ext';
import { frontmatterSchema } from 'proof-sdk-upstream/src/editor/schema/frontmatter';
import { libraryRemarkFrontmatterPlugin } from './lib-remark-frontmatter-plugin';
import { libraryRemarkDirectivePlugin, typedBlockRemarkPlugin } from './lib-remark-directive-plugin';
import { remarkProofMarksPlugin } from 'proof-sdk-upstream/src/editor/schema/remark-proof-marks-plugin';
import { proofMarkHandler } from 'proof-sdk-upstream/src/formats/remark-proof-marks';

import { authoredTrackerPlugin } from 'proof-sdk-upstream/src/editor/plugins/authored-tracker';
import { heatmapPlugin, heatmapCtx } from 'proof-sdk-upstream/src/editor/plugins/heatmap-decorations';
import type { HeatMapMode } from './upstream-types';
import { agentCursorPlugin, agentCursorCtx } from 'proof-sdk-upstream/src/editor/plugins/agent-cursor';
import { setCurrentActor } from 'proof-sdk-upstream/src/editor/actor';
import { suggestionsPlugins } from 'proof-sdk-upstream/src/editor/plugins/suggestions';
import { markPopoverPlugin } from 'proof-sdk-upstream/src/editor/plugins/mark-popover';
import { arrowCommentPlugin } from 'proof-sdk-upstream/src/editor/plugins/arrow-comment';
import { findHighlightsPlugin } from 'proof-sdk-upstream/src/editor/plugins/find-highlights';
import { markdownLinkClickPlugin } from 'proof-sdk-upstream/src/editor/plugins/markdown-link-click';
import { mermaidDiagramsPlugin } from 'proof-sdk-upstream/src/editor/plugins/mermaid-diagrams';
import { taskCheckboxesPlugin } from 'proof-sdk-upstream/src/editor/plugins/task-checkboxes';
import { tableKeyboardPlugin } from 'proof-sdk-upstream/src/editor/plugins/table-keyboard';
import { placeholderPlugin } from 'proof-sdk-upstream/src/editor/plugins/placeholder';
import { collabCursorBuilder, collabSelectionBuilder } from 'proof-sdk-upstream/src/editor/plugins/collab-cursors';
import {
  marksPlugins,
  setDefaultMarkdownParser,
  applyRemoteMarks as applyRemoteMarksMutation,
  deleteMark,
  type StoredMark,
} from 'proof-sdk-upstream/src/editor/plugins/marks';

import { dispatchMarkPlugins, remarkDispatchMarksPlugin, dispatchMarkHandler, removeAskMark } from './dispatch-marks';
import type { MarkAction } from './dispatch-marks';
import { dispatchActionBarPlugin } from './dispatch-action-bar';
import { registerPopoverHookInstance } from './dispatch-popover-hook';
import { dispatchMarkEventsPlugin } from './dispatch-mark-events';
import { remarkSoftBreakAsSpace } from './dispatch-soft-breaks';
import { configureDispatchLinks } from './dispatch-links';

export type { MarkAction, SelectionBarActionKind, PopoverActionKind } from './dispatch-marks';
export type {
  BlockAttributeKind,
  BlockAttributeSchema,
  BlockSchema,
  BlockTypeSchema,
  HostBlockRenderer,
} from './block-schema';
export type { StoredMark } from './upstream-types';
export { BLOCK_ID_ATTR, BLOCK_ID_DOM_ATTR, blockIdOf, isIdentifiedBlock, setBlockIdGenerator } from './editor/schema/block-ids';
export { createTypedBlockCommands } from './typed-block-commands';
export type {
  TypedBlockAttributeValue,
  TypedBlockAttributes,
  TypedBlockCommandContext,
  TypedBlockCommands,
  TypedBlockMenuItem,
} from './typed-block-commands';

export interface ProofEditorUser {
  name: string;
  color: string;
}

export interface CreateProofEditorOptions {
  /** Host-owned Yjs document. Must contain (or will lazily grow) an XmlFragment
   *  named 'prosemirror' — see CollabService#bindDoc, which calls
   *  `doc.getXmlFragment('prosemirror')` for us. */
  ydoc: Y.Doc;
  /** Host-owned awareness instance (e.g. from HocuspocusProvider#awareness).
   *  Pass null/undefined to run without collaborative cursors. */
  awareness?: Awareness | null;
  /** Local user identity for cursor labels. */
  user: ProofEditorUser;
  /** Render the document read-only. Defaults to false. */
  readOnly?: boolean;
  /** Fired for every mark-affecting user action: Comment/Suggest/Ask on the
   *  selection bar (the editor has already applied the mark locally with a
   *  fresh markId over [from,to]; a rejected promise removes it), and
   *  Reply/Resolve/Unresolve/Accept/Reject/Delete on the mark popover (no
   *  local mutation is applied for these — the host's server is expected to
   *  mutate and the change arrives back through the shared Y.Doc). */
  onMarkAction?: (action: MarkAction) => void | Promise<void>;
  /** Margin mode: report clicks and hover on mark spans and register neither
   *  the mark popover nor the arrow-comment plugin; the host renders threads
   *  itself. */
  onMarkClick?: (markId: string) => void;
  onMarkHover?: (markId: string | null) => void;
  /** Heatmap rendering mode. Defaults to 'background'. */
  heatMapMode?: HeatMapMode;
  /** Typed block schema fetched from the document service before editor construction. */
  blockSchema?: BlockSchema;
  /** Renders a host-owned typed block node when the schema declares `render: "host"`. */
  renderBlock?: HostBlockRenderer;
}

export interface ProofEditorHandle extends TypedBlockCommands {
  view: EditorView;
  /** Serialize the current document to markdown via the configured serializer. */
  getMarkdown(): string;
  /** Replace the current document with parsed markdown. */
  setMarkdown(markdown: string): void;
  /** Top offset of each mark's first span relative to the library root. */
  markOffsets(): Map<string, number>;
  /** Toggle editability (e.g. for a readOnly flag change post-construction). */
  setReadOnly(readOnly: boolean): void;
  /** Apply mark metadata received from a peer, re-anchoring by quote. Unified
   *  Proof marks (comment/suggestion/flagged/approved) only — see
   *  ./dispatch-marks.ts's module doc for why dispatchAsk marks are outside
   *  this system. */
  applyRemoteMarks(metadata: Record<string, StoredMark>, options?: { hydrateAnchors?: boolean }): void;
  /** Removes a mark by id, trying the unified marks system first and falling
   *  back to a dispatchAsk mark. */
  removeMark(markId: string): void;
  /** Scrolls a mark's anchor into view and pulses it. Works for both the
   *  unified marks system and dispatchAsk marks: both render `data-id`. */
  focusMark(markId: string): void;
  /** Scrolls a block into view by its stable `blockId` and pulses it. */
  focusBlock(blockId: string): void;
  /** The stable id of the block containing the selection head, if any. */
  blockIdAtSelection(): string | null;
  /** Tear down the editor and any collab bindings. */
  destroy(): void;
}

function installCollabCursorsWhenReady(
  view: EditorView,
  ctx: Ctx,
  collabService: CollabService,
  awareness: Awareness,
  isAlive: () => boolean,
): void {
  const hasCursorPlugin = () => view.state.plugins.some((plugin) => plugin.spec.key === yCursorPluginKey);

  const maxAttempts = 120;
  const attemptInstall = (attempt: number) => {
    if (!isAlive() || hasCursorPlugin()) return;

    let mappingReady = false;
    try {
      // y-prosemirror types ySyncPluginKey as PluginKey<any>; its state carries
      // a ProsemirrorBinding whose `mapping` is a Map<Y type, PM node>.
      const ystate = ySyncPluginKey.getState(view.state);
      const mapping = ystate?.binding?.mapping;
      if (mapping && typeof mapping.size === 'number' && mapping.size > 0) mappingReady = true;
    } catch {
      // ignore and retry
    }

    if (!mappingReady) {
      if (attempt < maxAttempts) requestAnimationFrame(() => attemptInstall(attempt + 1));
      return;
    }

    try {
      collabService.setAwareness(awareness);
    } catch {
      // ignore; cursor plugin can still work with direct awareness reference
    }

    try {
      const cursorPlugin = yCursorPlugin(
        awareness,
        { cursorBuilder: collabCursorBuilder, selectionBuilder: collabSelectionBuilder },
        undefined,
      );
      const nextPlugins = view.state.plugins.concat(cursorPlugin);
      ctx.set(prosePluginsCtx, nextPlugins);
      view.updateState(view.state.reconfigure({ plugins: nextPlugins }));
    } catch (error) {
      console.warn('[@sjawhar/proof-editor] failed to install yCursor plugin', error);
    }
  };

  attemptInstall(0);
}

const PULSE_CLASS = 'dispatch-mark-pulse';
const PULSE_DURATION_MS = 1200;

function cssEscapeAttrValue(value: string): string {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(value);
  return value.replace(/["\\]/g, '\\$&');
}

export async function createProofEditor(
  root: HTMLElement,
  opts: CreateProofEditorOptions,
): Promise<ProofEditorHandle> {
  root.classList.add('proof-editor');
  setCurrentActor(`human:${opts.user.name}`);
  let alive = true;
  const heatMapMode: HeatMapMode = opts.heatMapMode ?? 'background';

  const builder = Editor.make()
    .config((ctx) => {
      ctx.set(rootCtx, root);
      ctx.set(defaultValueCtx, '');
      configureDispatchLinks(ctx);
    })
    .config(nord)
    .use(commonmark)
    .use(gfm)
    // Frontmatter and typed directives register before their node schemas claim their AST nodes.
    .use(libraryRemarkFrontmatterPlugin)
    .use(libraryRemarkDirectivePlugin)
    .use(opts.blockSchema ? typedBlockRemarkPlugin(opts.blockSchema) : [])
    .use(frontmatterSchema)
    .use(codeBlockExtPlugins)
    // Every block carries a stable blockId (see ./editor/schema/block-ids.ts).
    .use(opts.blockSchema ? blockSchemaPlugins(opts.blockSchema, opts.renderBlock) : [])
    .use(blockIdPlugins)
    .use(history)
    .use(listener)
    .use(collab)
    .use(cursor)
    .use(clipboard)
    // Register proof mark schemas, plus this library's own dispatchAsk mark
    .use(proofMarkPlugins)
    .use(dispatchMarkPlugins)
    // Register remark plugins for proof marks and dispatchAsk parsing
    .use(remarkProofMarksPlugin)
    .use(remarkDispatchMarksPlugin)
    // Register contexts
    .use(heatmapCtx)
    .use(agentCursorCtx)
    // Register plugins
    .use(authoredTrackerPlugin)
    .use(heatmapPlugin)
    .use(agentCursorPlugin)
    .use(suggestionsPlugins);

  const marginMode = Boolean(opts.onMarkClick || opts.onMarkHover);
  if (marginMode) {
    builder.use(dispatchMarkEventsPlugin({ onMarkClick: opts.onMarkClick, onMarkHover: opts.onMarkHover }));
  } else {
    builder.use(markPopoverPlugin).use(arrowCommentPlugin);
  }

  const editor = await builder
    // This library's own Comment/Suggest/Ask bar replaces upstream's
    // markSelectionBarPlugin, which has no extension point for Ask or a hook
    // for Comment/Suggest.
    .use(dispatchActionBarPlugin({ by: opts.user.name, onMarkAction: opts.onMarkAction }))
    .use(findHighlightsPlugin)
    .use(taskCheckboxesPlugin)
    .use(mermaidDiagramsPlugin)
    .use(markdownLinkClickPlugin)
    // Register unified marks plugin
    .use(marksPlugins)
    // Allow Backspace to delete empty table rows
    .use(tableKeyboardPlugin)
    .use(placeholderPlugin)
    .config((ctx) => {
      ctx.update(remarkStringifyOptionsCtx, (prev) => ({
        ...prev,
        handlers: {
          ...(prev.handlers ?? {}),
          proofMark: proofMarkHandler,
          dispatchMark: dispatchMarkHandler,
        },
      }));
      ctx.set(heatmapCtx.key, { mode: heatMapMode });
    })
    .create();

  editor.action((ctx) => {
    setDefaultMarkdownParser(ctx.get(parserCtx));
  });

  const view = editor.ctx.get(editorViewCtx);

  let currentReadOnly = Boolean(opts.readOnly);
  view.setProps({ editable: () => !currentReadOnly });
  const setReadOnly = (readOnly: boolean) => {
    currentReadOnly = readOnly;
    view.setProps({ editable: () => !currentReadOnly });
  };

  // --- Collab binding -------------------------------------------------
  // Adapted from ProofEditorImpl's initFromShare() collab-activation block.
  // The awareness/yCursor plugin is intentionally installed *after* first
  // connecting without it: y-prosemirror's cursor plugin can throw (nodeSize
  // on undefined) if it evaluates awareness state before the ySync plugin's
  // mapping is populated.
  editor.action((ctx) => {
    const collabService = ctx.get(collabServiceCtx);
    collabService.bindDoc(opts.ydoc);
    collabService.mergeOptions({
      yCursorOpts: { cursorBuilder: collabCursorBuilder, selectionBuilder: collabSelectionBuilder },
    });
    collabService.connect();

    if (opts.awareness) {
      // Mirrors CollabClient#setLocalUser: yCursorPlugin's cursor builder reads
      // `awareness.getStates().get(clientId).user`.
      opts.awareness.setLocalStateField('user', opts.user);
      installCollabCursorsWhenReady(view, ctx, collabService, opts.awareness, () => alive);
    }
  });

  const unregisterPopoverHook = marginMode
    ? () => {}
    : registerPopoverHookInstance(view, opts.onMarkAction);

  const typedBlockCommands = createTypedBlockCommands({
    blockSchema: opts.blockSchema,
    getState: () => view.state,
    dispatch: (transaction) => {
      view.dispatch(transaction);
    },
  });

  return {
    view,
    ...typedBlockCommands,
    getMarkdown(): string {
      const serializer = editor.ctx.get(serializerCtx);
      return serializer(view.state.doc);
    },
    setMarkdown(markdown: string): void {
      // Markdown import only: soft line breaks become spaces here and in the headless parser,
      // never in the shared parserCtx, which the clipboard plugin also runs text/plain pastes
      // through - a pasted "alpha\nbeta" must keep its line break.
      // remarkCtx holds a frozen processor; calling it yields an unfrozen copy to extend.
      const importParser = ParserState.create(
        editor.ctx.get(schemaCtx),
        editor.ctx.get(remarkCtx)().use(remarkSoftBreakAsSpace),
      );
      const parsed = importParser(markdown);
      const { state } = view;
      view.dispatch(state.tr.replaceWith(0, state.doc.content.size, parsed.content));
    },
    markOffsets(): Map<string, number> {
      const offsets = new Map<string, number>();
      const rootTop = root.getBoundingClientRect().top;
      for (const span of view.dom.querySelectorAll<HTMLElement>('[data-id]')) {
        const id = span.dataset.id;
        if (id && !offsets.has(id)) offsets.set(id, span.getBoundingClientRect().top - rootTop);
      }
      return offsets;
    },
    setReadOnly,
    applyRemoteMarks(metadata: Record<string, StoredMark>, options?: { hydrateAnchors?: boolean }): void {
      applyRemoteMarksMutation(view, metadata, options);
    },
    removeMark(markId: string): void {
      if (deleteMark(view, markId)) return;
      removeAskMark(view, markId);
    },
    focusMark(markId: string): void {
      const escaped = cssEscapeAttrValue(markId);
      const elements = view.dom.querySelectorAll<HTMLElement>(`[data-id="${escaped}"]`);
      if (elements.length === 0) return;
      elements[0].scrollIntoView({ behavior: 'smooth', block: 'center' });
      for (const element of elements) {
        element.classList.add(PULSE_CLASS);
        window.setTimeout(() => element.classList.remove(PULSE_CLASS), PULSE_DURATION_MS);
      }
    },
    focusBlock(blockId: string): void {
      const escaped = cssEscapeAttrValue(blockId);
      const element = view.dom.querySelector<HTMLElement>(`[${BLOCK_ID_DOM_ATTR}="${escaped}"]`);
      if (element === null) return;
      element.scrollIntoView({ behavior: 'smooth', block: 'center' });
      element.classList.add(PULSE_CLASS);
      window.setTimeout(() => element.classList.remove(PULSE_CLASS), PULSE_DURATION_MS);
    },
    blockIdAtSelection(): string | null {
      const $head = view.state.selection.$head;
      for (let depth = $head.depth; depth > 0; depth -= 1) {
        const id = blockIdOf($head.node(depth));
        if (id !== null) return id;
      }
      return null;
    },
    destroy(): void {
      alive = false;
      unregisterPopoverHook();
      editor.action((ctx) => {
        const collabService = ctx.get(collabServiceCtx);
        collabService.disconnect();
      });
      void editor.destroy();
    },
  };
}

export default createProofEditor;
