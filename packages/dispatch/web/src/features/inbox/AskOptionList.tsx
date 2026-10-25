import type { ReactNode } from "react";

import type { ReferenceSource } from "../../api/client";
import type { AskOption } from "../../api/types";
import { AskRecordRow } from "./AskOptionRow";

export function AskOptionList({
  options,
  rowClassName,
  selected,
  source,
}: {
  options: AskOption[];
  /** Extra classes for each unchosen row, e.g. a surface fill on a tinted host. */
  rowClassName?: string;
  selected: readonly string[];
  /** The ask the options belong to, which a `#id` in an option resolves against. */
  source?: ReferenceSource;
}): ReactNode {
  if (options.length === 0) {
    return null;
  }
  return (
    <ul aria-label="Options" className="mt-2 space-y-1">
      {options.map((option) => (
        <AskRecordRow
          className={rowClassName}
          key={option.label}
          option={option}
          selected={selected.includes(option.label)}
          source={source}
        />
      ))}
    </ul>
  );
}
