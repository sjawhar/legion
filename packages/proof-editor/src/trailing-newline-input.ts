import { Plugin } from "@milkdown/kit/prose/state";
import { $prose } from "@milkdown/kit/utils";

/**
 * Types over a selection that would leave its text block ending in a newline, instead of leaving
 * that to the browser.
 *
 * Replacing the whole last line of a code block leaves the block's text ending in "\n" until the
 * typed text lands. Firefox's native editing puts that text before the newline instead of after
 * it ("const x = 1;hello\n" for "const x = 1;\nhello"), and does so in a plain contenteditable
 * without ProseMirror (LEGION-289). A paragraph's hard break reads as that newline too, and there
 * Firefox deletes the break along with the selection ("first lineX"). Chromium and WebKit get
 * both right. For that one shape the text is inserted through ProseMirror's own typing path — the
 * handleTextInput props first, so input rules still run, then a plain insertText — and the
 * browser's edit is prevented.
 */
export const trailingNewlineInputPlugin = $prose(
  () =>
    new Plugin({
      props: {
        handleDOMEvents: {
          beforeinput(view, event) {
            if (event.inputType !== "insertText" || !event.data || view.composing) return false;
            const { selection } = view.state;
            const { $from, $to, from, to } = selection;
            if (selection.empty || !$from.sameParent($to) || !$from.parent.isTextblock)
              return false;
            if ($to.parentOffset !== $to.parent.content.size) return false;
            if (!$from.parent.textBetween(0, $from.parentOffset).endsWith("\n")) return false;
            const text = event.data;
            const insert = () => view.state.tr.insertText(text, from, to);
            if (
              !view.someProp("handleTextInput", (handle) => handle(view, from, to, text, insert))
            ) {
              view.dispatch(insert().scrollIntoView());
            }
            event.preventDefault();
            return true;
          },
        },
      },
    })
);
