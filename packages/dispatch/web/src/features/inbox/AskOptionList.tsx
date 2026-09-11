import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import {
  borderDefault,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";

export function AskOptionList({
  options,
  selected,
}: {
  options: AskOption[];
  selected: readonly string[];
}): ReactNode {
  if (options.length === 0) {
    return null;
  }
  const selections = new Set(selected);
  return (
    <ul aria-label="Options" className="mt-2 space-y-1">
      {options.map((option) => {
        const isSelected = selections.has(option.label);
        return (
          <li
            className={`flex items-start gap-2 rounded-lg border px-3 py-2 text-sm ${borderDefault} ${isSelected ? `${selectedCardBorder} ${selectedCardBg}` : ""}`}
            data-selected={isSelected}
            key={option.label}
          >
            {isSelected ? (
              <span aria-label="Selected" className={`font-semibold ${linkText}`} role="img">
                ✓
              </span>
            ) : (
              <span aria-hidden className="w-3" />
            )}
            <span>
              <span className={`font-medium ${textPrimaryOnSurface}`}>
                <MarkdownBody markdown={option.label} variant="inline" />
              </span>
              {option.description === undefined ? null : (
                <span className={`mt-0.5 block text-sm ${textOptionDescription}`}>
                  <MarkdownBody markdown={option.description} variant="inline" />
                </span>
              )}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
