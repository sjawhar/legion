// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Standalone selection-action bar: Comment / Suggest / Ask.
 *
 * ./editor/plugins/mark-selection-bar.ts has no extension point — its button
 * set (Comment/Flag/Suggest) is built by a private `buildButtons()` method
 * with no registration hook, and its Comment/Suggest buttons call
 * `openCommentComposer`/`suggestReplace` directly and unconditionally, with
 * no `window.proof`-style seam of the kind mark-popover.ts has. Wrapping that
 * file's exported plugin cannot add an Ask button to it, and cannot make its
 * existing buttons fire `onMarkAction` after creating a fresh mark without
 * either editing it or fighting its internal DOM/state to detect "this specific
 * click just created this specific mark".
 *
 * This module is a full replacement, registered instead of (not alongside)
 * `markSelectionBarPlugin`. On fine pointers it centers the bar on the
 * selection instead of docking it in the editor's gutter; on touch it waits
 * for a stable selection and docks at the bottom of the viewport so the bar
 * does not compete with the native selection menu. It
 * implements exactly the three MarkAction kinds the contract defines for the
 * selection bar: Comment, Suggest, and Ask (dropping the upstream bar's Flag
 * button, which has no corresponding MarkAction kind). Each button applies
 * the underlying mark locally with a freshly generated id — a comment mark
 * with empty body text, a replace-suggestion mark with empty replacement
 * content, or a fresh dispatchAsk mark — then calls `onMarkAction`; a Dispatch
 * host is expected to collect the actual comment/suggestion body through its
 * own UI, correlated by `markId`. If the hook's promise rejects, the local
 * mark is removed.
 */

import { $prose } from '@milkdown/kit/utils';
import { Plugin, PluginKey } from '@milkdown/kit/prose/state';
import type { EditorView } from '@milkdown/kit/prose/view';

import { comment, suggestReplace, deleteMark } from 'proof-sdk-upstream/src/editor/plugins/marks';
import type { MarkRange } from 'proof-sdk-upstream/src/editor/plugins/marks';
import { createAskMark, removeAskMark } from './dispatch-marks';
import type { MarkAction, SelectionBarActionKind } from './dispatch-marks';

const actionBarKey = new PluginKey('dispatch-action-bar');
const MARGIN = 12;
const TOUCH_SELECTION_SETTLE_DELAY = 400;

export interface ActionBarOptions {
  by: string;
  onMarkAction?: (action: MarkAction) => void | Promise<void>;
}

function getSelectionRange(view: EditorView): MarkRange | null {
  const { from, to } = view.state.selection;
  if (from === to) return null;
  return { from, to };
}

function isRangeValid(view: EditorView, range: MarkRange | null): range is MarkRange {
  if (!range) return false;
  return range.from >= 0 && range.to > range.from && range.to <= view.state.doc.content.size;
}

function getAnchorBox(view: EditorView, range: MarkRange) {
  const from = view.coordsAtPos(range.from);
  const to = view.coordsAtPos(range.to);
  return {
    top: Math.min(from.top, to.top),
    bottom: Math.max(from.bottom, to.bottom),
    left: Math.min(from.left, to.left),
    right: Math.max(from.right, to.right),
  };
}

/** Centers the bar horizontally on the selection's anchor box, clamped to the
 *  viewport by MARGIN, and places it above the selection when there's room,
 *  else below, else clamped — always near the selection, never docked in the
 *  editor's gutter. "Room above" is measured against the editor's own top edge,
 *  not the viewport's: the bar is fixed-positioned, so a selection on the
 *  document's first line would otherwise put it over whatever the host renders
 *  above the editor (tabs, a toolbar) and swallow clicks meant for those. */
export function positionBar(bar: HTMLElement, view: EditorView, range: MarkRange): void {
  try {
    const anchorBox = getAnchorBox(view, range);
    if (typeof bar.getBoundingClientRect !== 'function') return;
    if (typeof view.dom.getBoundingClientRect !== 'function') return;
    const barRect = bar.getBoundingClientRect();
    const editorRect = view.dom.getBoundingClientRect();
    const viewportW = window.innerWidth;
    const viewportH = window.innerHeight;
    const minTop = Math.max(MARGIN, editorRect.top);
    const maxTop = Math.max(minTop, viewportH - barRect.height - MARGIN);

    const aboveTop = anchorBox.top - barRect.height - MARGIN;
    const belowTop = anchorBox.bottom + MARGIN;
    const hasRoomAbove = aboveTop >= minTop;
    const hasRoomBelow = belowTop + barRect.height <= viewportH - MARGIN;
    const top = hasRoomAbove ? aboveTop : hasRoomBelow ? belowTop : Math.max(minTop, Math.min(anchorBox.top, maxTop));
    const center = (anchorBox.left + anchorBox.right) / 2;
    const left = Math.max(MARGIN, Math.min(center - barRect.width / 2, viewportW - barRect.width - MARGIN));
    bar.style.left = `${left}px`;
    bar.style.top = `${top}px`;
  } catch {
    // Ignore positioning errors for invalid positions (e.g. detached view mid-teardown).
  }
}

class ActionBarController {
  private view: EditorView;
  private readonly bar: HTMLDivElement;
  private lastRange: MarkRange | null = null;
  private readonly options: ActionBarOptions;
  private lastPointerType: string | null = null;
  private pointerDown = false;
  private editorLostFocus = false;
  private destroyed = false;
  private selectionChangedAt: number | null = null;
  private selectionSettleTimer: ReturnType<typeof setTimeout> | null = null;

  private readonly handleSelectionChange = () => {
    const range = getSelectionRange(this.view);
    if (!this.isTouchSelection()) {
      if (range) this.lastRange = range;
      return;
    }

    if (!range) {
      this.lastRange = null;
      this.hideBar();
      return;
    }

    this.lastRange = range;
    this.selectionChangedAt = Date.now();
    this.hideBar();
    this.scheduleTouchBar();
  };

  private readonly handlePointerDown = (event: PointerEvent) => {
    this.lastPointerType = event.pointerType;
    this.pointerDown = true;
    if (this.isTouchSelection()) this.hideBar();
  };

  private readonly handleTouchStart = () => {
    this.lastPointerType = 'touch';
    this.pointerDown = true;
    this.hideBar();
  };

  private readonly handlePointerUp = () => {
    if (!this.pointerDown) return;
    this.pointerDown = false;
    if (this.isTouchSelection()) this.scheduleTouchBar();
  };

  private readonly handleEditorFocusIn = () => {
    if (this.isTouchSelection()) this.editorLostFocus = false;
  };

  private readonly handleEditorFocusOut = (event: FocusEvent) => {
    if (!this.isTouchSelection()) return;
    const nextTarget = event.relatedTarget;
    if (nextTarget && this.bar.contains(nextTarget as Node)) return;
    this.editorLostFocus = true;
    this.hideBar();
  };

  private readonly handleScroll = () => {
    if (this.lastRange && !this.isTouchSelection()) positionBar(this.bar, this.view, this.lastRange);
  };

  constructor(view: EditorView, options: ActionBarOptions) {
    this.view = view;
    this.options = options;
    this.bar = document.createElement('div');
    this.bar.className = 'dispatch-action-bar';
    this.bar.style.display = 'none';
    this.bar.addEventListener('pointerdown', (event) => {
      event.preventDefault();
      event.stopPropagation();
    });

    const container = view.dom.parentElement ?? document.body;
    container.appendChild(this.bar);
    this.buildButtons();

    document.addEventListener('selectionchange', this.handleSelectionChange);
    document.addEventListener('pointerup', this.handlePointerUp);
    document.addEventListener('touchend', this.handlePointerUp);
    document.addEventListener('touchcancel', this.handlePointerUp);
    view.dom.addEventListener('pointerdown', this.handlePointerDown);
    view.dom.addEventListener('touchstart', this.handleTouchStart);
    view.dom.addEventListener('focusin', this.handleEditorFocusIn);
    view.dom.addEventListener('focusout', this.handleEditorFocusOut);
    window.addEventListener('scroll', this.handleScroll, true);
    window.addEventListener('resize', this.handleScroll);
  }

  destroy(): void {
    this.destroyed = true;
    document.removeEventListener('selectionchange', this.handleSelectionChange);
    document.removeEventListener('pointerup', this.handlePointerUp);
    document.removeEventListener('touchend', this.handlePointerUp);
    document.removeEventListener('touchcancel', this.handlePointerUp);
    this.view.dom.removeEventListener('pointerdown', this.handlePointerDown);
    this.view.dom.removeEventListener('touchstart', this.handleTouchStart);
    this.view.dom.removeEventListener('focusin', this.handleEditorFocusIn);
    this.view.dom.removeEventListener('focusout', this.handleEditorFocusOut);
    window.removeEventListener('scroll', this.handleScroll, true);
    window.removeEventListener('resize', this.handleScroll);
    this.clearSelectionSettleTimer();
    this.bar.remove();
  }

  update(view: EditorView): void {
    this.view = view;
    const range = getSelectionRange(view);
    if (!range || this.editorLostFocus) {
      this.lastRange = range;
      this.hideBar();
      return;
    }

    this.lastRange = range;
    if (this.isTouchSelection()) {
      this.bar.dataset.touch = 'true';
      if (this.selectionChangedAt === null) this.selectionChangedAt = Date.now();
      this.scheduleTouchBar();
      return;
    }

    this.clearSelectionSettleTimer();
    delete this.bar.dataset.touch;
    this.bar.style.display = 'flex';
    positionBar(this.bar, view, range);
  }

  private isTouchSelection(): boolean {
    if (this.lastPointerType === 'touch') return true;
    if (this.lastPointerType === 'mouse' || this.lastPointerType === 'pen') return false;
    try {
      return typeof window !== 'undefined' && window.matchMedia('(pointer: coarse)').matches;
    } catch {
      return false;
    }
  }

  private scheduleTouchBar(): void {
    this.clearSelectionSettleTimer();
    if (this.pointerDown || !this.lastRange || this.editorLostFocus) return;

    const elapsed = this.selectionChangedAt === null ? 0 : Date.now() - this.selectionChangedAt;
    this.selectionSettleTimer = setTimeout(() => {
      if (this.destroyed) return;
      this.selectionSettleTimer = null;
      if (
        this.pointerDown ||
        this.editorLostFocus ||
        !this.isTouchSelection() ||
        !isRangeValid(this.view, this.lastRange)
      ) {
        return;
      }
      this.bar.dataset.touch = 'true';
      this.bar.style.left = '';
      this.bar.style.top = '';
      this.bar.style.display = 'flex';
    }, Math.max(0, TOUCH_SELECTION_SETTLE_DELAY - elapsed));
  }

  private clearSelectionSettleTimer(): void {
    if (this.selectionSettleTimer === null) return;
    clearTimeout(this.selectionSettleTimer);
    this.selectionSettleTimer = null;
  }

  private hideBar(): void {
    this.clearSelectionSettleTimer();
    this.bar.style.display = 'none';
  }

  private async runAction(kind: SelectionBarActionKind, range: MarkRange): Promise<void> {
    const quote = this.view.state.doc.textBetween(range.from, range.to, '\n', '\n');
    let markId: string;

    if (kind === 'comment') {
      markId = comment(this.view, quote, this.options.by, '', range).id;
    } else if (kind === 'suggest') {
      const mark = suggestReplace(this.view, quote, this.options.by, '', range);
      if (!mark) return;
      markId = mark.id;
    } else {
      const created = createAskMark(this.view, range, this.options.by);
      if (!created) return;
      markId = created.id;
    }

    const onMarkAction = this.options.onMarkAction;
    if (!onMarkAction) return;

    const action: MarkAction = { kind, markId, quote, from: range.from, to: range.to };
    try {
      await onMarkAction(action);
    } catch (error) {
      if (kind === 'ask') removeAskMark(this.view, markId);
      else deleteMark(this.view, markId);
      console.warn(`[@sjawhar/proof-editor] onMarkAction(${kind}) rejected; removed the local mark`, error);
    }
  }

  private buildButtons(): void {
    this.bar.innerHTML = '';

    const makeButton = (label: string, kind: SelectionBarActionKind) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = label;
      button.addEventListener('pointerdown', (event) => {
        event.preventDefault();
        event.stopPropagation();
      });
      button.addEventListener('click', () => {
        const range = isRangeValid(this.view, this.lastRange) ? this.lastRange : getSelectionRange(this.view);
        if (!range) return;
        void this.runAction(kind, range);
      });
      return button;
    };

    this.bar.appendChild(makeButton('Comment', 'comment'));
    this.bar.appendChild(makeButton('Suggest', 'suggest'));
    this.bar.appendChild(makeButton('Ask', 'ask'));
  }
}

export const dispatchActionBarPlugin = (options: ActionBarOptions) =>
  $prose(() => {
    return new Plugin({
      key: actionBarKey,
      view(view) {
        return new ActionBarController(view, options);
      },
    });
  });
