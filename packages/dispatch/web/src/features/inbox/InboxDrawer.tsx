import { type ReactNode, useRef, useState } from "react";

import {
  backdrop50,
  borderDefault,
  card,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { InboxRoute } from "../refs/routes";
import { DIALOG_SCOPE } from "../shell/keymap";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { Inbox } from "./Inbox";

/**
 * The Inbox as a peek over whatever page is open (LEGION-547): the same list, cards and actions
 * as the Inbox page, mounted once in the app shell as a sibling of `<main>` so a route change
 * underneath never unmounts the page beneath it - the drawer itself closes instead
 * (`useCloseOnNavigation`), which is also what "open the item" from inside it does.
 *
 * Its assignee filter is its own `useState`, never the URL: `Inbox`'s `filter`/`onAssigneesChange`
 * exist exactly so a peek embedded on, say, the Agents page does not navigate it to `/` the
 * moment someone picks a person. `keymapScope={DIALOG_SCOPE}` puts its j/k/Enter/Escape
 * bindings on the scope a dialog anywhere on the stack exclusively consults, instead of the
 * standalone page's `inbox` scope, which a dialog's keydown handling never reaches. `pane={false}`
 * is the third: a peek is a strip beside the page it covers, not a two-column reader, so it keeps
 * the card in the row at every width - the Inbox the page itself shows below 1280 px.
 */
export function InboxDrawer({ onClose, open }: { onClose: () => void; open: boolean }): ReactNode {
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  // `undefined` is the reader alone, the same default the page reads from an address without
  // `?assignee=`.
  const [assignees, setAssignees] = useState<readonly string[] | undefined>(undefined);
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open });
  useCloseOnNavigation(open, onClose);

  if (!open) {
    return null;
  }

  const filter: InboxRoute = { assignees };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-30 ${backdrop50}`} onClick={onClose} />
      <aside
        aria-label="Inbox"
        aria-modal="true"
        className={`fixed inset-y-0 right-0 z-40 flex w-full flex-col overflow-y-auto border-l p-4 shadow-2xl md:max-w-md ${card} ${borderDefault}`}
        data-testid="inbox-drawer"
        ref={dialog.containerRef}
        role="dialog"
      >
        <div className={`mb-4 flex items-center justify-between border-b pb-3 ${borderDefault}`}>
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>Inbox</h2>
          <button
            className={`min-h-11 rounded-lg px-3 text-sm font-medium ${textMutedOnSurface}`}
            onClick={onClose}
            ref={closeButtonRef}
            type="button"
          >
            Close
          </button>
        </div>
        <Inbox
          filter={filter}
          keymapScope={DIALOG_SCOPE}
          onAssigneesChange={setAssignees}
          pane={false}
        />
      </aside>
    </>
  );
}
