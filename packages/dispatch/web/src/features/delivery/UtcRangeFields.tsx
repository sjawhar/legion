// The two date fields a reader names a custom range of whole UTC days with, and the Apply button
// and refusal that go with them: the Delivery page's window control (`WindowControl.tsx`) and
// every chart's compare control (`Chart.tsx`) offer the same range picker, so the fields, their
// labels, the `(UTC)` note and the refusal live here once.
//
// Each host keeps its own state - the window control holds `from` and `to` in two `useState`s,
// the compare control derives them from a draft - and passes its own class strings, since the
// two sit at different sizes: the control's fields are `px-2 py-1 text-sm` beside the chart's
// `px-1.5 py-0.5 text-xs`. A class a host does not pass is left off the element, as it is where
// the host writes the markup itself.
import type { ReactNode } from "react";

import { secondaryButtonCompact } from "../../theme/classes";

export function UtcRangeFields({
  idPrefix,
  fieldClass,
  labelClass,
  noteClass,
  from,
  to,
  onFrom,
  onTo,
  onApply,
  refused,
  refusalClass,
}: {
  /** What each field's `id` starts with, so its label's `htmlFor` names one field on the page. */
  readonly idPrefix: string;
  readonly fieldClass: string;
  readonly labelClass?: string;
  readonly noteClass?: string;
  /** The From field's value, a `YYYY-MM-DD` date or empty. */
  readonly from: string;
  /** The To field's value, the range's last whole day. */
  readonly to: string;
  readonly onFrom: (next: string) => void;
  readonly onTo: (next: string) => void;
  readonly onApply: () => void;
  /** Why the range the reader named was refused, or null while none was. */
  readonly refused: string | null;
  readonly refusalClass: string;
}): ReactNode {
  return (
    <>
      <label className={labelClass} htmlFor={`${idPrefix}-from`}>
        From
      </label>
      <input
        className={fieldClass}
        id={`${idPrefix}-from`}
        onChange={(event) => onFrom(event.target.value)}
        type="date"
        value={from}
      />
      <label className={labelClass} htmlFor={`${idPrefix}-to`}>
        To
      </label>
      <input
        className={fieldClass}
        id={`${idPrefix}-to`}
        onChange={(event) => onTo(event.target.value)}
        type="date"
        value={to}
      />
      <span className={noteClass}>(UTC)</span>
      <button className={secondaryButtonCompact} onClick={onApply} type="button">
        Apply
      </button>
      {refused === null ? null : (
        <p className={refusalClass} role="alert">
          {refused}
        </p>
      )}
    </>
  );
}
