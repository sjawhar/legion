// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Popover mutation boundary.
 *
 * ./editor/plugins/mark-popover.ts's Reply/Resolve/Accept/Reject buttons check
 * a global `window.proof` object before mutating the document directly:
 *
 *   const proof = getProofEditorApi();
 *   const created = proof?.markReply ? proof.markReply(...) : replyToComment(this.view, ...);
 *
 * That object is normally the app shell's `ProofEditorImpl` singleton
 * (src/editor/index.ts, `window.proof = new ProofEditorImpl()`), which this
 * library deliberately never imports. Its optional-method check is still a
 * real, exported extension seam: mark-popover.ts's own code, unmodified,
 * skips its local mutation whenever `window.proof.markReply` /
 * `markResolve` / `markAccept` / `markReject` exists and is truthy — and
 * falls back to the local mutation call (the exact behavior with no hook at
 * all) when the property is absent. This module supplies that object
 * ourselves, without depending on ProofEditorImpl, so `onMarkAction` — not a
 * local doc edit — is what fires when a host requests the host-mediated
 * flow, and the original in-editor mutation still runs unchanged for any
 * `createProofEditor()` instance that didn't ask for it.
 *
 * `window.proof` is a single page-global, but Dispatch may render more than
 * one editor on a page, so each `createProofEditor()` call registers itself
 * here; the popover only ever passes a `markId`, so the dispatcher resolves
 * the owning instance by scanning registered instances for one whose current
 * marks contain that id. Every popover mutation reports through `onMarkAction`
 * when it is provided; otherwise its established local mutation remains the
 * fallback for standalone hosts.
 */

import type { EditorView } from '@milkdown/kit/prose/view';

import {
  accept as acceptMutation,
  deleteMark,
  getMarks,
  reject as rejectMutation,
  reply as replyMutation,
  resolve as resolveMutation,
  unresolve as unresolveMutation,
} from 'proof-sdk-upstream/src/editor/plugins/marks';
import type { MarkAction, PopoverActionKind } from './dispatch-marks';

interface RegisteredInstance {
  view: EditorView;
  onMarkAction?: (action: MarkAction) => void | Promise<void>;
}

const instances = new Set<RegisteredInstance>();
const HOOK_MARKER = '__isSjawharProofEditorHook';
function findInstanceForMark(markId: string): RegisteredInstance | undefined {
  let match: RegisteredInstance | undefined;
  for (const instance of instances) {
    if (getMarks(instance.view.state).some((mark) => mark.id === markId)) match = instance;
  }
  return match;
}

function reportRejection(kind: PopoverHookKind, error: unknown): void {
  console.warn(`[@sjawhar/proof-editor] onMarkAction(${kind}) rejected`, error);
}

type PopoverHookKind = PopoverActionKind;

function fireHookOrFallBack(
  kind: Exclude<PopoverHookKind, 'reply'>,
  markId: string,
  localMutation: (view: EditorView, markId: string) => boolean,
): boolean {
  const instance = findInstanceForMark(markId);
  if (!instance) return false;
  if (instance.onMarkAction) {
    void Promise.resolve(instance.onMarkAction({ kind, markId })).catch((error) => reportRejection(kind, error));
    return true;
  }
  return localMutation(instance.view, markId);
}

function installWindowProofHook(): void {
  if (typeof window === 'undefined') return;
  const globalWindow = window as unknown as Record<string, Record<string, unknown> | undefined>;
  if ((globalWindow.proof as Record<string, unknown> | undefined)?.[HOOK_MARKER]) return;

  if (globalWindow.proof) {
    console.warn(
      '[@sjawhar/proof-editor] window.proof is already set (likely the full Proof app shell). ' +
        'Installing the @sjawhar/proof-editor popover hook over it; running this library alongside ' +
        'the app shell on the same page is not supported.',
    );
  }

  globalWindow.proof = {
    [HOOK_MARKER]: true,
    markReply(markId: string, by: string, text: string) {
      const instance = findInstanceForMark(markId);
      if (!instance) return null;
      if (instance.onMarkAction) {
        void Promise.resolve(instance.onMarkAction({ kind: 'reply', markId, text })).catch((error) =>
          reportRejection('reply', error),
        );
        return true;
      }
      return replyMutation(instance.view, markId, by, text);
    },
    markResolve(markId: string) {
      return fireHookOrFallBack('resolve', markId, (view, id) => resolveMutation(view, id));
    },
    markUnresolve(markId: string) {
      return fireHookOrFallBack('unresolve', markId, (view, id) => unresolveMutation(view, id));
    },
    markAccept(markId: string) {
      return fireHookOrFallBack('accept', markId, (view, id) => acceptMutation(view, id));
    },
    markReject(markId: string) {
      return fireHookOrFallBack('reject', markId, (view, id) => rejectMutation(view, id));
    },
    markDeleteThread(markId: string) {
      return fireHookOrFallBack('delete', markId, (view, id) => deleteMark(view, id));
    },
  };
}

/** Call once per `createProofEditor()` instance. Returns an unregister function for `destroy()`. */
export function registerPopoverHookInstance(
  view: EditorView,
  onMarkAction: ((action: MarkAction) => void | Promise<void>) | undefined,
): () => void {
  installWindowProofHook();
  const entry: RegisteredInstance = { view, onMarkAction };
  instances.add(entry);
  return () => {
    instances.delete(entry);
  };
}
