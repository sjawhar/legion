import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";

export function AskOptionList({
  descriptionClass,
  labelClass,
  options,
  selected,
}: {
  descriptionClass: string;
  labelClass: string;
  options: AskOption[];
  selected: string[];
}): ReactNode {
  if (options.length === 0) {
    return null;
  }
  const selections = new Set(selected);
  return (
    <ul aria-label="Answer options" className="mt-3 space-y-2">
      {options.map((option) => {
        const isSelected = selections.has(option.label);
        return (
          <li className="flex items-start gap-2" key={option.label}>
            <span aria-hidden="true" className="w-4 shrink-0 font-semibold">
              {isSelected ? "✓" : ""}
            </span>
            <span>
              <span className={`${isSelected ? "font-semibold" : "font-medium"} ${labelClass}`}>
                {option.label}
              </span>
              {option.description === undefined ? null : (
                <span className={`mt-0.5 block text-sm ${descriptionClass}`}>
                  {option.description}
                </span>
              )}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
