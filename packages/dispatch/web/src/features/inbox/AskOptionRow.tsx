import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";

const ROW_CLASS = `flex min-h-11 items-start gap-3 rounded-lg border px-3 py-2 text-sm ${borderDefault}`;

/** The label/description column is a flex item; `min-w-0` overrides its `min-width: auto` so it
 *  shrinks to the row and `MarkdownBody`'s own `break-words` folds a long word. The row's parent
 *  `<fieldset>` needs `min-w-0` too: its UA default `min-inline-size: min-content` would let the
 *  longest unbroken word widen the fieldset past a narrow card before anything here applies. */
function OptionText({ option }: { option: AskOption }): ReactNode {
  return (
    <span className="min-w-0">
      <span className={`font-medium ${textPrimaryOnSurface}`}>
        <MarkdownBody markdown={option.label} variant="inline" />
      </span>
      {option.description === undefined ? null : (
        <span className={`mt-0.5 block text-sm ${textOptionDescription}`}>
          <MarkdownBody markdown={option.description} variant="inline" />
        </span>
      )}
    </span>
  );
}

/** A choosable option: a radio or checkbox row. The `name` groups radios per ask. `hotkey` marks
 * the input `data-ask-option` so the Inbox's number keys 1–9 pick it (the real options of the
 * focused ask, not Other). */
export function AskChoiceRow({
  checked,
  className = "",
  disabled = false,
  hotkey = false,
  multiple,
  name,
  onChange,
  option,
}: {
  checked: boolean;
  className?: string;
  disabled?: boolean;
  hotkey?: boolean;
  multiple: boolean;
  name: string;
  onChange: () => void;
  option: AskOption;
}): ReactNode {
  return (
    <label className={`${ROW_CLASS} cursor-pointer ${cardHoverBorder} ${className}`}>
      <input
        checked={checked}
        data-ask-option={hotkey ? "" : undefined}
        disabled={disabled}
        name={name}
        onChange={onChange}
        type={multiple ? "checkbox" : "radio"}
      />
      <OptionText option={option} />
    </label>
  );
}

/** A recorded option: ticked and tinted when it was chosen. */
export function AskRecordRow({
  className = "",
  option,
  selected,
}: {
  className?: string;
  option: AskOption;
  selected: boolean;
}): ReactNode {
  return (
    <li
      className={`${ROW_CLASS} ${selected ? `${selectedCardBorder} ${selectedCardBg}` : className}`}
      data-selected={selected}
    >
      {selected ? (
        <span aria-label="Selected" className={`font-semibold ${linkText}`} role="img">
          ✓
        </span>
      ) : (
        <span aria-hidden className="w-3" />
      )}
      <OptionText option={option} />
    </li>
  );
}
