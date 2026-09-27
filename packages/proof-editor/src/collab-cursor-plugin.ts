import { type EditorState, Plugin } from "@milkdown/kit/prose/state";
import type { DecorationSet, EditorView } from "@milkdown/kit/prose/view";
import {
  collabCursorBuilder,
  collabSelectionBuilder,
} from "proof-sdk-upstream/src/editor/plugins/collab-cursors";
import { yCursorPlugin, yCursorPluginKey } from "y-prosemirror";
import type { Awareness } from "y-protocols/awareness";

/**
 * y-prosemirror's peer-cursor plugin, except that a peer's caret is not drawn while it sits exactly
 * on the local caret of a focused editor.
 *
 * A peer's caret is a non-editable widget. When the local caret shares its position,
 * prosemirror-view puts the DOM caret directly beside that widget, and Chromium and WebKit then
 * drop or misplace what the local user types: a new line at the end of a code block, the start of
 * a code line, an empty code block. Firefox is unaffected. The local caret already marks that
 * spot, so the peer's caret reappears as soon as the two part. An unfocused editor has no DOM
 * caret, only a stale selection, so it draws every peer; y-prosemirror republishes the local
 * cursor on focusin and focusout, and that awareness change redraws the decorations. Everything
 * else — the awareness updates, the peer's selection highlight, the builders — is
 * y-prosemirror's own plugin.
 */
export function collabCursorPlugin(awareness: Awareness): Plugin {
  const peerCursors: Plugin = yCursorPlugin(awareness, {
    cursorBuilder: collabCursorBuilder,
    selectionBuilder: collabSelectionBuilder,
  });
  const peerCursorsView = peerCursors.spec.view;
  if (peerCursorsView === undefined) throw new Error("y-prosemirror's cursor plugin has no view");
  let editor: EditorView | undefined;
  return new Plugin({
    ...peerCursors.spec,
    props: {
      ...peerCursors.spec.props,
      decorations: (state) =>
        editor?.hasFocus() ? peerDecorationsAwayFromCaret(state) : yCursorPluginKey.getState(state),
    },
    view: (view) => {
      editor = view;
      return peerCursorsView(view);
    },
  });
}

function peerDecorationsAwayFromCaret(state: EditorState): DecorationSet | undefined {
  const peers: DecorationSet | undefined = yCursorPluginKey.getState(state);
  const { selection } = state;
  if (peers === undefined || !selection.empty) return peers;
  const { head } = selection;
  const atCaret = peers.find(head, head).filter(({ from, to }) => from === head && to === head);
  return atCaret.length === 0 ? peers : peers.remove(atCaret);
}
