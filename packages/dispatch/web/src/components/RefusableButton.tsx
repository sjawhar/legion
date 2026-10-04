import { type ReactNode, useId } from "react";

import { primaryButton, secondaryButtonCompact } from "../theme/classes";

/** Each look whole, refused or not: the `refused:` variant (`styles.css`) paints `disabled` and
 *  `aria-disabled` alike, so a look states its cannot-press state once. */
const LOOKS = {
  primary: primaryButton,
  "secondary-compact": secondaryButtonCompact,
} as const;

/**
 * A button that says why it cannot be pressed, in one of `LOOKS`. A refusal is `aria-disabled`
 * rather than `disabled`, so the button stays in the tab order and its reason reaches a keyboard
 * and a screen reader: on `title`, and through `aria-describedby` on the line that shows it
 * (`refusalShownBy`), or, where no line on screen does, on a hidden one of the button's own. A
 * click on a refused button does nothing, a submit button's included, so the refusal holds
 * whatever the host's handler checks; a form's other ways to submit (Ctrl/Cmd+Enter) remain the
 * host's to refuse. Only `busy` - a press already in flight, which its label names - disables it
 * outright.
 */
export function RefusableButton({
  busy,
  children,
  className = "",
  describedBy,
  look = "primary",
  onPress,
  refusal,
  refusalShownBy,
  type = "button",
}: {
  /** The label while a press is in flight; the one state that renders `disabled`. */
  readonly busy?: string;
  readonly children: ReactNode;
  /** Placement only: the button's look, refused or not, is this component's. */
  readonly className?: string;
  /** The id of a line on screen that bears on the press, read after the reason. */
  readonly describedBy?: string;
  readonly look?: keyof typeof LOOKS;
  readonly onPress?: () => void;
  /** Why the button cannot be pressed, or null while it can. */
  readonly refusal: string | null;
  /** The id of the line on screen that already says `refusal`. */
  readonly refusalShownBy?: string;
  readonly type?: "button" | "submit";
}): ReactNode {
  const ownReasonId = useId();
  const reasonId = refusal === null ? undefined : (refusalShownBy ?? ownReasonId);
  const description = [reasonId, describedBy].filter((id) => id !== undefined).join(" ");
  return (
    <>
      <button
        aria-describedby={description === "" ? undefined : description}
        aria-disabled={refusal === null ? undefined : true}
        className={`${LOOKS[look]} ${className}`}
        disabled={busy !== undefined}
        onClick={(event) => {
          if (refusal !== null) {
            event.preventDefault();
            return;
          }
          onPress?.();
        }}
        title={refusal ?? undefined}
        type={type}
      >
        {busy ?? children}
      </button>
      {refusal === null || refusalShownBy !== undefined ? null : (
        <span className="sr-only" id={ownReasonId}>
          {refusal}
        </span>
      )}
    </>
  );
}
