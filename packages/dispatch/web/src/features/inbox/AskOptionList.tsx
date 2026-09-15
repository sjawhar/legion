import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import { AskRecordRow } from "./AskOptionRow";

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
  return (
    <ul aria-label="Options" className="mt-2 space-y-1">
      {options.map((option) => (
        <AskRecordRow
          key={option.label}
          option={option}
          selected={selected.includes(option.label)}
        />
      ))}
    </ul>
  );
}
