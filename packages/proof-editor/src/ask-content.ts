import { $prose } from "@milkdown/kit/utils";
import { isHistoryTransaction } from "@milkdown/prose/history";
import type { Fragment, ResolvedPos, Schema } from "@milkdown/prose/model";
import { type EditorState, Plugin, TextSelection, type Transaction } from "@milkdown/prose/state";
import { dropPoint } from "@milkdown/prose/transform";
import type { EditorProps, EditorView } from "@milkdown/prose/view";
import { ySyncPluginKey } from "y-prosemirror";

/**
 * Makes `schema`'s `ask` node (when it has one) accept, as a node's content, what the document
 * service accepts in it: at least one child of the schema's `block` group (the server's block
 * node types, pmdoc's `isBlockNodeType`, and the block schema's typed blocks), and no mark the
 * ask disallows.
 *
 * An ask's content rule is `paragraph+ bullet_list?`, but two collaborators' concurrent edits
 * (two lists, a paragraph after the list) can leave a shared document holding an ask that rule
 * refuses. y-prosemirror builds each node through `createChecked`, and when that throws it deletes
 * the whole element from the shared Yjs document, so every editor and the server lose the ask's
 * text (sjawhar/legion#2433). Only `validContent` changes, which is what `createChecked`, a step's own
 * check and `Node.check` read; content matching (`contentMatch`, `canReplace`, `createAndFill`)
 * keeps the rule, so every command, input rule, paste and markdown parse still holds a local edit
 * to it, and the typed-block drawing's fit check is untouched.
 */
export function relaxAskContentCheck(schema: Schema): void {
  const ask = schema.nodes.ask;
  if (ask === undefined) return;
  ask.validContent = (content: Fragment): boolean => {
    if (content.childCount === 0) return false;
    for (let index = 0; index < content.childCount; index++) {
      const child = content.child(index);
      if (!child.type.isInGroup("block")) return false;
      if (!ask.allowsMarks(child.marks)) return false;
    }
    return true;
  };
}

/** Whether `$pos` lies inside an ask whose content its own content rule refuses. */
function inBrokenAsk($pos: ResolvedPos): boolean {
  for (let depth = $pos.depth; depth > 0; depth--) {
    const node = $pos.node(depth);
    if (node.type.name === "ask" && !node.type.contentMatch.matchFragment(node.content)?.validEnd)
      return true;
  }
  return false;
}

/**
 * A `handleKeyDown` view prop that keeps a key command from throwing out of a keystroke inside
 * an ask its content rule refuses (see `relaxAskContentCheck`). Commands such as Enter on an empty
 * list item or Backspace across blocks read the ask's content rule (`contentMatchAt`, `canReplace`)
 * and throw on such an ask. ProseMirror's keydown listener does not catch, so the event's default
 * is never prevented and the browser applies its own edit to the document's DOM. It offers the
 * keystroke to every other `handleKeyDown` in their order and declines it (handled: no edit,
 * default prevented) when one throws. Outside such an ask it does nothing.
 */
function guardBrokenAskKeys(view: EditorView, event: KeyboardEvent): boolean {
  const { $from, $to } = view.state.selection;
  if (!inBrokenAsk($from) && !inBrokenAsk($to)) return false;
  try {
    return (
      view.someProp("handleKeyDown", (handleKeyDown) =>
        handleKeyDown === guardBrokenAskKeys ? undefined : handleKeyDown(view, event)
      ) ?? false
    );
  } catch {
    return true;
  }
}

/**
 * A `handleTextInput` view prop that keeps typed text from throwing out of its input event
 * inside an ask its content rule refuses. Typing over a selection that runs from one block into
 * another has no key command: ProseMirror's keypress listener builds `insertText` over the
 * selection, which reads the ask's content rule and throws there before the event's default is
 * prevented, so the browser's own edit lands. It offers the text to every other `handleTextInput`
 * (input rules among them) in their order and, when none takes it, dispatches ProseMirror's
 * default edit itself. When any of that throws (an edit, or a plugin's appended transaction such
 * as a block id set on a node the rule refuses, which fails before the view's state changes), it
 * declines the text (handled: no edit, default prevented). Outside such an ask it does nothing.
 */
function guardBrokenAskTextInput(
  view: EditorView,
  from: number,
  to: number,
  text: string,
  deflt: () => Transaction
): boolean {
  const { doc } = view.state;
  if (!inBrokenAsk(doc.resolve(from)) && !inBrokenAsk(doc.resolve(to))) return false;
  try {
    const handled = view.someProp("handleTextInput", (handleTextInput) =>
      handleTextInput === guardBrokenAskTextInput
        ? undefined
        : handleTextInput(view, from, to, text, deflt)
    );
    if (!handled) view.dispatch(deflt());
  } catch {
    // Declined: nothing applied.
  }
  return true;
}

/**
 * Whether deleting the selection, with the transactions plugins append to that, throws: applied
 * to the state without dispatching it, so nothing changes either way.
 */
function deletionThrows(view: EditorView): boolean {
  try {
    view.state.apply(view.state.tr.deleteSelection());
    return false;
  } catch {
    return true;
  }
}

/**
 * The view props that keep user input inside an ask its content rule refuses from throwing, and
 * from letting the browser apply its own edit: `guardBrokenAskKeys` and `guardBrokenAskTextInput`
 * above, and the handlers below. Installed as direct view props (`view.setProps`), which
 * ProseMirror consults before every plugin's; the editor sets no other direct `handleKeyDown`,
 * `handleTextInput`, `handleDrop` or `handleDOMEvents`, so none is replaced.
 *
 * - `cut`: ProseMirror's cut handler fills the clipboard, then dispatches the selection's
 *   deletion, which across blocks of such an ask throws; when it would, the cut is cancelled
 *   (nothing copied, nothing deleted) before that handler runs.
 * - `beforeinput`: input that comes with no keypress (an input method's commit, dictation, mobile
 *   keyboards) is the browser's own edit, which ProseMirror reads back from the DOM; over a
 *   selection across blocks of such an ask that read-back replace drops the blocks after the
 *   selection (the second option list among them). Over such a selection, as ProseMirror's
 *   keypress handler does for any selection across blocks, the browser's edit is prevented and
 *   ProseMirror's own is dispatched instead (typed text through `handleTextInput`, a deletion as
 *   `deleteSelection`), or declined when it throws; any other insertion is declined.
 * - `compositionstart`: ProseMirror deletes a selection across blocks when an input method starts
 *   composing over it, and that deletion throws in such an ask; when it would, the selection is
 *   collapsed to its start first, so the composed text lands there and nothing is deleted.
 * - `handleDrop`: ProseMirror's drop handler prevents the browser's drop, then finds the drop
 *   point (`dropPoint` reads each ancestor's content rule) and deletes a moved selection, either
 *   of which throws in such an ask; when its edit would, the drop is declined.
 */
export const brokenAskInputGuards: EditorProps = {
  handleKeyDown: guardBrokenAskKeys,
  handleTextInput: guardBrokenAskTextInput,
  handleDrop(view, event, slice, moved) {
    const target = view.posAtCoords({ left: event.clientX, top: event.clientY });
    if (target === null) return false;
    const { doc, selection } = view.state;
    const $target = doc.resolve(target.pos);
    const movesFromBrokenAsk =
      moved && (inBrokenAsk(selection.$from) || inBrokenAsk(selection.$to));
    if (!inBrokenAsk($target) && !movesFromBrokenAsk) return false;
    // ProseMirror's own drop edit, applied to the state without dispatching it.
    try {
      const tr = view.state.tr;
      if (moved) tr.deleteSelection();
      const at = tr.mapping.map(dropPoint(doc, $target.pos, slice) ?? $target.pos);
      view.state.apply(tr.replaceRange(at, at, slice));
      return false;
    } catch {
      return true;
    }
  },
  handleDOMEvents: {
    cut(view, event) {
      const { $from, $to } = view.state.selection;
      if ((!inBrokenAsk($from) && !inBrokenAsk($to)) || !deletionThrows(view)) return false;
      event.preventDefault();
      return true;
    },
    beforeinput(view, event) {
      const { $from, $to, from, to } = view.state.selection;
      if (!event.cancelable || $from.sameParent($to)) return false;
      if (!inBrokenAsk($from) && !inBrokenAsk($to)) return false;
      const { data, inputType } = event;
      if (!inputType.startsWith("insert") && !inputType.startsWith("delete")) return false;
      event.preventDefault();
      if (inputType === "insertText" && data) {
        guardBrokenAskTextInput(view, from, to, data, () =>
          view.state.tr.insertText(data).scrollIntoView()
        );
      } else if (inputType.startsWith("delete")) {
        try {
          view.dispatch(view.state.tr.deleteSelection().scrollIntoView());
        } catch {
          // Declined: nothing applied.
        }
      }
      return true;
    },
    compositionstart(view) {
      const { $from, $to } = view.state.selection;
      if ($from.sameParent($to) || (!inBrokenAsk($from) && !inBrokenAsk($to))) return false;
      if (!deletionThrows(view)) return false;
      view.dispatch(view.state.tr.setSelection(TextSelection.between($from, $from)));
      return false;
    },
  },
};

/**
 * Whether `tr` drops, from the ask at `askPos` in `state.doc` that its content rule refuses, a
 * child block that lies wholly outside the selection: every step of `tr` replaces a range inside
 * the ask's content, and some step deletes a leaf of the block, one character of its text or a
 * leaf node such as an image or a hard break, or the block itself when it is a leaf (a divider),
 * so a block holding only an image, or a divider, is kept as one holding text is. Inserting into
 * the block deletes none of its leaves, nor does deleting only node boundaries, as a Backspace
 * joining the block to the one before it or a node's attributes being set does, and neither drops
 * it. A non-leaf block with no leaf in it (an empty paragraph) holds nothing a collaborator wrote,
 * so a Backspace that removes it applies.
 */
function dropsUnselectedAskBlock(tr: Transaction, state: EditorState, askPos: number): boolean {
  const ask = state.doc.nodeAt(askPos);
  if (ask === null || ask.type.contentMatch.matchFragment(ask.content)?.validEnd) return false;
  let contentFrom = askPos + 1;
  let contentTo = askPos + ask.nodeSize - 1;
  for (const step of tr.steps) {
    const map = step.getMap();
    let inside = true;
    map.forEach((oldStart, oldEnd) => {
      if (oldStart < contentFrom || oldEnd > contentTo) inside = false;
    });
    if (!inside) return false;
    contentFrom = map.map(contentFrom, -1);
    contentTo = map.map(contentTo, 1);
  }
  const { from, to } = state.selection;
  let dropped = false;
  ask.forEach((block, offset) => {
    const start = askPos + 1 + offset;
    if (dropped || (start + block.nodeSize > from && start < to)) return;
    state.doc.nodesBetween(start, start + block.nodeSize, (node, pos) => {
      if (dropped || !node.isLeaf) return !dropped;
      // A leaf unit `[at, at + 1]` is deleted when a step's deleted range ends at or spans its
      // end (`deletedBefore`, the end mapped to the left); an insertion at either side, which
      // `deletedAfter` also reports at the insertion point, deletes nothing.
      for (let at = pos; at < pos + node.nodeSize && !dropped; at++) {
        dropped = tr.mapping.mapResult(at + 1, -1).deletedBefore;
      }
      return false;
    });
  });
  return dropped;
}

/**
 * Refuses a local edit that drops a block of an ask its content rule refuses (see
 * `relaxAskContentCheck`) which the selection does not touch.
 *
 * An edit the browser makes itself, with no `beforeinput` for `brokenAskInputGuards` to prevent
 * (`document.execCommand`, which grammar and writing extensions use to replace selected text),
 * is read back from the DOM by ProseMirror, which parses the changed blocks through the ask's
 * content rule: over a selection from the question into the first of two option lists, the
 * read-back replace drops the second list too, which was never selected. Such a transaction is
 * refused, so the state keeps every block; ProseMirror marked the DOM it read as dirty, and since
 * the state's document does not change, the DOM observer's flush redraws those nodes from the
 * kept state (`view.updateState(view.state)` when `docView.dirty`), writing nothing to Yjs.
 *
 * Only transactions every step of which replaces a range inside one such ask's content are read:
 * edits elsewhere, whole-document replacements, the shared document's own changes (y-prosemirror's
 * sync, undo and redo among them) and prosemirror-history's undo and redo, which revert or redo
 * the person's own edit wherever the caret now is, are not. Whether a block is kept is read from
 * the transaction's steps, not from the text it leaves, so a block whose text another block also
 * holds is still seen as dropped. An edit that deletes none of an unselected block's leaves
 * (typing, inserting text into an unselected option, Backspace joining the lists, a block id set
 * on a node) applies.
 */
export const keepUnselectedAskBlocks = $prose(
  () =>
    new Plugin({
      filterTransaction(tr, state) {
        if (!tr.docChanged || isHistoryTransaction(tr)) return true;
        if (tr.getMeta(ySyncPluginKey)?.isChangeOrigin) return true;
        let refused = false;
        state.doc.descendants((node, pos) => {
          if (refused) return false;
          if (node.type.name !== "ask") return true;
          refused = dropsUnselectedAskBlock(tr, state, pos);
          return false;
        });
        return !refused;
      },
    })
);
