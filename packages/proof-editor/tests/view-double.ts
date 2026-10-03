import type { EditorState, Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";

export interface ViewDouble {
  readonly view: EditorView;
  /** Every transaction dispatched to the view, in order. */
  readonly transactions: readonly Transaction[];
}

/** A view with the two members mark commands read, the state and dispatch applying to it, for
 *  commands that need no DOM. */
export function viewDouble(state: EditorState): ViewDouble {
  const transactions: Transaction[] = [];
  const double = {
    state,
    dispatch(tr: Transaction) {
      transactions.push(tr);
      double.state = double.state.apply(tr);
    },
  };
  return { view: double as unknown as EditorView, transactions };
}
