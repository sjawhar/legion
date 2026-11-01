import { type ReactNode, useId, useState } from "react";

import {
  borderStrong,
  dangerText,
  secondaryButtonCompact,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import {
  customRange,
  lastUtcDate,
  PRESET_LABELS,
  utcDate,
  WINDOW_PRESETS,
  type WindowChoice,
} from "./lib/window";

const fieldClass = `rounded border px-2 py-1 text-sm ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;

/** The Delivery page's window: a rolling preset (the last 7, 14, 28 or 84 days) or a custom range
 *  of whole UTC days. A custom range is applied only once both dates are set and it spans at most
 *  84 days, the server's limit; a longer one is refused here with the limit stated. */
export function WindowControl({
  value,
  onChange,
}: {
  value: WindowChoice;
  onChange: (next: WindowChoice) => void;
}): ReactNode {
  const id = useId();
  const [custom, setCustom] = useState(value.kind === "custom");
  const [fromDate, setFromDate] = useState(value.kind === "custom" ? utcDate(value.from) : "");
  const [toDate, setToDate] = useState(value.kind === "custom" ? lastUtcDate(value.to) : "");
  const [refused, setRefused] = useState<string | null>(null);

  const apply = () => {
    const range = customRange(fromDate, toDate);
    if ("refused" in range) {
      setRefused(range.refused);
      return;
    }
    setRefused(null);
    onChange({ kind: "custom", ...range });
  };

  return (
    <div className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      <label className="flex items-center gap-2" htmlFor={`${id}-window`}>
        Window
      </label>
      <select
        className={fieldClass}
        id={`${id}-window`}
        onChange={(event) => {
          const next = event.target.value;
          const preset = WINDOW_PRESETS.find((candidate) => candidate === next);
          if (preset === undefined) {
            setCustom(true);
            return;
          }
          setCustom(false);
          setRefused(null);
          onChange({ kind: "preset", preset });
        }}
        value={custom ? "custom" : value.kind === "preset" ? value.preset : "custom"}
      >
        {WINDOW_PRESETS.map((preset) => (
          <option key={preset} value={preset}>
            {PRESET_LABELS[preset]}
          </option>
        ))}
        <option value="custom">Custom range…</option>
      </select>
      {custom ? (
        <>
          <label className="flex items-center gap-1" htmlFor={`${id}-from`}>
            From
          </label>
          <input
            className={fieldClass}
            id={`${id}-from`}
            onChange={(event) => setFromDate(event.target.value)}
            type="date"
            value={fromDate}
          />
          <label className="flex items-center gap-1" htmlFor={`${id}-to`}>
            To
          </label>
          <input
            className={fieldClass}
            id={`${id}-to`}
            onChange={(event) => setToDate(event.target.value)}
            type="date"
            value={toDate}
          />
          <span className="text-xs">(UTC)</span>
          <button className={secondaryButtonCompact} onClick={apply} type="button">
            Apply
          </button>
          {refused === null ? null : (
            <p className={`basis-full text-xs ${dangerText}`} role="alert">
              {refused}
            </p>
          )}
        </>
      ) : null}
    </div>
  );
}
