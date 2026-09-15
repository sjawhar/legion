import { type MouseEvent, type ReactNode, useEffect, useState } from "react";

import { copyText } from "../lib/clipboard";
import { dangerText, linkHoverText, linkText, successText } from "../theme/classes";

type CopyStatus = "idle" | "copied" | { failed: string };

/** Copies `value` to the clipboard and confirms inline for 1.5 s ("Copied") or reports the
 *  failure until the next attempt. With `children` the button shows that label before the
 *  icon (e.g. the tmux target itself); without, it is an icon-only button whose 44 px tap
 *  target shrinks to 32 px on desktop like the issue pin. On failure the hint says to select
 *  the text when the button already shows the value, and otherwise prints the value in a
 *  selectable `<code>` so a manual copy is still one step away.
 *
 *  `value` may be a function of the click, for a button whose modifier keys choose what it
 *  copies; such a button names itself through `label`, since the default `Copy <what> <value>`
 *  needs one value to print. */
export function CopyButton({
  children,
  className,
  label,
  value,
  what,
}: {
  children?: ReactNode;
  className?: string;
  label?: string;
  value: string | ((event: MouseEvent<HTMLButtonElement>) => string);
  what: string;
}): ReactNode {
  const [status, setStatus] = useState<CopyStatus>("idle");
  useEffect(() => {
    if (status !== "copied") {
      return;
    }
    const timeout = window.setTimeout(() => setStatus("idle"), 1500);
    return () => window.clearTimeout(timeout);
  }, [status]);
  const name = label ?? (typeof value === "string" ? `Copy ${what} ${value}` : `Copy ${what}`);
  const shape =
    children === undefined
      ? "min-w-11 justify-center md:min-w-8"
      : "max-w-full gap-1 px-1 font-mono text-xs font-medium select-text";

  return (
    <>
      <button
        aria-label={name}
        className={`inline-flex min-h-11 shrink-0 items-center rounded-lg md:min-h-8 ${shape} ${linkText} ${linkHoverText} ${className ?? ""}`}
        onClick={(event) => {
          const text = typeof value === "string" ? value : value(event);
          void copyText(text).then((copied) => setStatus(copied ? "copied" : { failed: text }));
        }}
        title={name}
        type="button"
      >
        {children === undefined ? null : <span>{children}</span>}
        <svg
          aria-hidden="true"
          className={children === undefined ? "size-4 shrink-0" : "size-3 shrink-0"}
          fill="none"
          viewBox="0 0 20 20"
        >
          <rect height="10" rx="1" stroke="currentColor" strokeWidth="1.5" width="8" x="7" y="7" />
          <path
            d="M5 13H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h8a1 1 0 0 1 1 1v1"
            stroke="currentColor"
            strokeWidth="1.5"
          />
        </svg>
      </button>
      {status === "idle" ? null : (
        <span
          aria-live="polite"
          className={`ml-1 text-xs font-medium ${status === "copied" ? successText : dangerText}`}
        >
          {status === "copied" ? (
            "Copied"
          ) : children === status.failed ? (
            "Copy failed - select the text"
          ) : (
            <>
              Copy failed - <code className="select-all font-mono">{status.failed}</code>
            </>
          )}
        </span>
      )}
    </>
  );
}
