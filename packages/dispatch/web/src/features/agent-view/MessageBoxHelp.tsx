import { type ReactNode, useEffect, useRef } from "react";

import {
  card,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";

/** A prefix a person types at the start of a message, as the panel shows it. */
function Prefix({ children }: { children: string }): ReactNode {
  return <span className={`font-mono font-semibold ${linkText}`}>{children}</span>;
}

export interface MessageBoxHelpProps {
  /** Closing and opening the panel, so the host can keep one panel of its row open at a time. */
  readonly onOpenChange: (open: boolean) => void;
  readonly open: boolean;
}

/**
 * The one question mark under the message box, and the panel it opens (INFRA-812, R6): what `!`
 * does, where a command runs, and what this view keeps of the conversation. The page carries no
 * rule text of its own, so this panel is where a person reads all three; the message box keeps
 * only its own hint and its own refusal, which are a control's words about the next press.
 *
 * The host owns `open`, as the context ring's is owned, so one panel of the row stands open at a
 * time. Escape closes the panel wherever the focus is and puts it back on the question mark, and
 * so does a pointer press outside it.
 */
export function MessageBoxHelp({ onOpenChange, open }: MessageBoxHelpProps): ReactNode {
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      // The panel is what Escape acts on while it stands open, not the view behind it.
      event.stopPropagation();
      onOpenChange(false);
      triggerRef.current?.focus();
    };
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      onOpenChange(false);
    };
    document.addEventListener("keydown", closeOnEscape);
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => {
      document.removeEventListener("keydown", closeOnEscape);
      document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
    };
  }, [onOpenChange, open]);

  return (
    <span className="relative shrink-0">
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label="What the message box does"
        className={`flex min-h-8 min-w-8 shrink-0 items-center justify-center rounded-lg border text-xs font-semibold ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        data-testid="agent-help"
        onClick={() => onOpenChange(!open)}
        ref={triggerRef}
        title="What the message box does"
        type="button"
      >
        ?
      </button>
      {open ? (
        <div
          aria-label="What the message box does"
          className={`absolute right-0 bottom-full z-10 mb-2 w-80 max-w-[calc(100vw-1rem)] rounded-lg border p-3 shadow-lg ${card}`}
          data-testid="agent-help-popover"
          ref={panelRef}
          role="dialog"
        >
          <p className={`text-sm font-semibold ${textPrimaryOnSurface}`}>The message box</p>
          <ul className={`mt-1 space-y-1 text-xs ${textSecondaryOnSurface}`}>
            <li>
              <Prefix>!</Prefix> at the start of a message runs the rest as a shell command in this
              session, and its output lands in the conversation. A BTW runs nothing: the session
              answers it.
            </li>
            <li>
              <Prefix>/</Prefix> at the start of a message runs one of the session's own commands,
              at the session itself. A command runs nowhere else.
            </li>
            <li>
              Live from the session: its turns are relayed while this page is open and are not
              stored. Messages sent to it through Dispatch, and its Dispatch replies, are kept.
            </li>
          </ul>
        </div>
      ) : null}
    </span>
  );
}
