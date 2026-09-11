import type { ReactNode } from "react";

import {
  backdrop40,
  card,
  dangerHoverBorder,
  hoverToDangerText,
  secondaryButtonBorder,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";

export interface UnsubscribeDialogProps {
  message: string;
  onCancel: () => void;
  onConfirm: () => void;
}

/**
 * Confirms removing a subscriber from an issue's or project document's "Subscribed
 * agents" list, shared between IssueHeader and DocumentPage. Escape, a backdrop click,
 * and Cancel all dismiss without action; Confirm is the only path that unsubscribes.
 */
export function UnsubscribeDialog({
  message,
  onCancel,
  onConfirm,
}: UnsubscribeDialogProps): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose: onCancel, open: true });

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onCancel} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-center justify-center p-4">
        <div
          aria-label="Unsubscribe"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-4 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <p className={`text-sm ${textPrimaryOnSurface}`}>{message}</p>
          <div className="flex justify-end gap-2">
            <button
              className={`rounded border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${textMutedHoverToSecondary}`}
              onClick={onCancel}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${hoverToDangerText} ${dangerHoverBorder}`}
              onClick={onConfirm}
              type="button"
            >
              Confirm
            </button>
          </div>
        </div>
      </div>
    </>
  );
}
