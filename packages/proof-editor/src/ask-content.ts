import type { Fragment, ResolvedPos, Schema } from "@milkdown/prose/model";
import type { EditorView } from "@milkdown/prose/view";

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
 * text (INFRA-624). Only `validContent` changes, which is what `createChecked`, a step's own
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
 * is never prevented and the browser applies its own edit to the document's DOM. Installed as a
 * direct view prop, which ProseMirror consults before every plugin's, it offers the keystroke to
 * every other `handleKeyDown` in their order and declines it (handled: no edit, default prevented)
 * when one throws. Outside such an ask it does nothing.
 */
export function guardBrokenAskKeys(view: EditorView, event: KeyboardEvent): boolean {
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
