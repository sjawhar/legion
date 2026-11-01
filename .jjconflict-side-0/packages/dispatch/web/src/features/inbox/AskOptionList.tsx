import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import { AskRecordRow } from "./AskOptionRow";

export function AskOptionList({
  options,
  rowClassName,
  selected,
}: {
  options: AskOption[];
  /** Extra classes for each unchosen row, e.g. a surface fill on a tinted host. */
  rowClassName?: string;
  selected: readonly string[];
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
        />
      ))}
    </ul>
  );
}
