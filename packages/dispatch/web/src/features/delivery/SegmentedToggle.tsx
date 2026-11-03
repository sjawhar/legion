// The Delivery page's segmented toggle, extracted from its own `Timeline | List` control
// (`DeliveryPage.tsx`'s `ModeToggle`) so every chart's `Graph | Table` is that one shape rather
// than a second one: one bordered row of buttons, the chosen one pressed and filled.
import { Fragment, type ReactNode } from "react";

import {
  borderStrong,
  dragHandleBg,
  surfaceMutedBg,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";

/** One segment: the value it chooses and what it reads. */
export interface SegmentedOption<T extends string> {
  readonly value: T;
  readonly label: string;
}

/** A segmented toggle naming `label`, with one button per option and the chosen one pressed.
 *  `className` is the host's own placement, nothing more. */
export function SegmentedToggle<T extends string>({
  className = "",
  label,
  onChange,
  options,
  value,
}: {
  className?: string;
  label: string;
  onChange: (next: T) => void;
  options: readonly SegmentedOption<T>[];
  value: T;
}): ReactNode {
  return (
    <fieldset aria-label={label} className={`${className} flex rounded-md border ${borderStrong}`}>
      {options.map((option, index) => (
        <Fragment key={option.value}>
          {index === 0 ? null : <span aria-hidden="true" className={`w-px ${dragHandleBg}`} />}
          <button
            aria-pressed={value === option.value}
            className={`h-8 px-3 text-sm font-medium first:rounded-l-md last:rounded-r-md ${
              value === option.value
                ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                : textSecondaryOnCanvas
            }`}
            onClick={() => onChange(option.value)}
            type="button"
          >
            {option.label}
          </button>
        </Fragment>
      ))}
    </fieldset>
  );
}
