import { type EditorState, Plugin } from "@milkdown/kit/prose/state";
import type { DecorationSet } from "@milkdown/kit/prose/view";
import {
  collabCursorBuilder,
  collabSelectionBuilder,
} from "proof-sdk-upstream/src/editor/plugins/collab-cursors";
import { yCursorPlugin, yCursorPluginKey } from "y-prosemirror";
import type { Awareness } from "y-protocols/awareness";

/**
 * y-prosemirror's peer-cursor plugin, except that a peer's caret is not drawn while it sits exactly
 * where the local caret is.
 *
 * A peer's caret is a non-editable widget. When the local caret shares its position,
 * prosemirror-view puts the DOM caret directly beside that widget, and Chromium and WebKit then
 * drop or misplace what the local user types: a new line at the end of a code block, the start of
 * a code line, an empty code block. Firefox is unaffected. The local caret already marks that
 * spot, so the peer's caret reappears as soon as the two part. Everything else — the awareness
 * updates, the peer's selection highlight, the builders — is y-prosemirror's own plugin.
 */
export function collabCursorPlugin(awareness: Awareness): Plugin {
  const peerCursors: Plugin = yCursorPlugin(awareness, {
    cursorBuilder: collabCursorBuilder,
    selectionBuilder: collabSelectionBuilder,
  });
  return new Plugin({
    ...peerCursors.spec,
    props: { decorations: peerDecorationsAwayFromCaret },
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
