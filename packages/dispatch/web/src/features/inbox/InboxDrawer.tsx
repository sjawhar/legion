import { type ReactNode, useRef, useState } from "react";

import {
  backdrop50,
  borderDefault,
  card,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { InboxRoute, InboxView } from "../refs/routes";
import { DIALOG_SCOPE } from "../shell/keymap";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { Inbox } from "./Inbox";

/**
 * The Inbox as a peek over whatever page is open (LEGION-547): the same list, cards and actions
 * as the Inbox page, mounted once in the app shell as a sibling of `<main>` so a route change
 * underneath never unmounts the page beneath it - the drawer itself closes instead
 * (`useCloseOnNavigation`), which is also what "open the item" from inside it does.
 *
 * Its Mine/Everyone choice is its own `useState`, never the URL: `Inbox`'s `filter`/`onViewChange`
 * exist exactly so a peek embedded on, say, the Agents page does not navigate it to `/` the
 * moment someone toggles the view. `keymapScope={DIALOG_SCOPE}` puts its j/k/Enter/Escape
 * bindings on the scope a dialog anywhere on the stack exclusively consults, instead of the
 * standalone page's `inbox` scope, which a dialog's keydown handling never reaches.
 */
export function InboxDrawer({ onClose, open }: { onClose: () => void; open: boolean }): ReactNode {
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const [view, setView] = useState<InboxView>("mine");
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open });
  useCloseOnNavigation(open, onClose);

  if (!open) {
    return null;
  }

  const filter: InboxRoute = { view };

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
        <Inbox filter={filter} keymapScope={DIALOG_SCOPE} onViewChange={setView} />
      </aside>
    </>
  );
}
