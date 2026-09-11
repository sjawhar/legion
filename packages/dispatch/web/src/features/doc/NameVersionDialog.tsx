import { type KeyboardEvent, type ReactNode, useRef, useState } from "react";

import {
  backdrop40,
  card,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";

export interface NameVersionDialogProps {
  error: boolean;
  onClose: () => void;
  onSave: (summary: string) => void;
  open: boolean;
  saving: boolean;
}

/**
 * The "Name version" toolbar action's confirmation dialog — an in-app replacement for the
 * `window.prompt` it used to call, matching the app's own focus-trap/Escape/backdrop dialog
 * convention (see ReferencePicker for the same shape). Unmounts entirely while closed so
 * `useDialog`'s `open` effect (focus trap, Escape, scroll lock) only ever runs while a caller
 * actually wants it showing.
 */
export function NameVersionDialog({
  error,
  onClose,
  onSave,
  open,
  saving,
}: NameVersionDialogProps): ReactNode {
  const [summary, setSummary] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open });

  if (!open) {
    return null;
  }

  const trimmed = summary.trim();

  const commit = (): void => {
    if (trimmed === "" || saving) {
      return;
    }
    onSave(trimmed);
  };

  const handleInputKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key !== "Enter" || event.nativeEvent.isComposing) {
      return;
    }
    event.preventDefault();
    commit();
  };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Name this version"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Name this version</h2>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            What changed?
            <input
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${inputClasses(false)}`}
              disabled={saving}
              onChange={(event) => setSummary(event.target.value)}
              onKeyDown={handleInputKeyDown}
              ref={inputRef}
              type="text"
              value={summary}
            />
          </label>
          {error ? (
            <p className={`text-sm ${dangerText}`} role="alert">
              Could not name this version. Try again.
            </p>
          ) : null}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={trimmed === "" || saving}
              onClick={commit}
              type="button"
            >
              {saving ? "Saving…" : "Save"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}
