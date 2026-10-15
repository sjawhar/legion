// The Delivery page's window: a rolling preset (the URL keeps `window=7d`, so a shared link always
// shows the latest days) or a custom range of whole UTC days (the URL keeps `from` and `to`). A
// preset is subscribed by its days, so Dispatch keeps its end at the present; the page reads the
// range it resolved from the answer's `window`. The server refuses a window over 84 days
// (`maxDeliveryWindow`, api/delivery.go); a custom range longer than that is refused here first,
// with the limit stated. Pure: no React.
import type { DeliveryLiveWindow } from "../../../api/types";

export const WINDOW_PRESETS = ["7d", "14d", "28d", "84d"] as const;
export type WindowPreset = (typeof WINDOW_PRESETS)[number];

/** The preset the page shows when its URL names none: the prototype's 28 days. */
export const DEFAULT_WINDOW_PRESET: WindowPreset = "28d";

/** The longest window the server reads (`maxDeliveryWindow`). */
export const MAX_WINDOW_DAYS = 84;

export type WindowChoice =
  | { kind: "preset"; preset: WindowPreset }
  | { kind: "custom"; from: string; to: string };

const DAY_MS = 24 * 60 * 60 * 1000;

const PRESET_DAYS = { "7d": 7, "14d": 14, "28d": 28, "84d": 84 } as const satisfies Record<
  WindowPreset,
  number
>;

export const PRESET_LABELS: Record<WindowPreset, string> = {
  "7d": "Last 7 days",
  "14d": "Last 14 days",
  "28d": "Last 28 days",
  "84d": "Last 84 days",
};

function isPreset(value: string | null): value is WindowPreset {
  return WINDOW_PRESETS.some((preset) => preset === value);
}

function isCustomWindow(from: string, to: string): boolean {
  const fromMs = Date.parse(from);
  const toMs = Date.parse(to);
  return (
    Number.isFinite(fromMs) &&
    Number.isFinite(toMs) &&
    toMs > fromMs &&
    toMs - fromMs <= MAX_WINDOW_DAYS * DAY_MS
  );
}

/** Whether the address names a malformed window. A valid custom range wins over an accompanying
 *  preset for backward compatibility; partial, unparseable, inverted and overlong custom values
 *  are malformed, as is an unknown preset with no valid custom range. */
export function hasMalformedWindowParams(params: URLSearchParams): boolean {
  const from = params.get("from");
  const to = params.get("to");
  if (from !== null || to !== null) {
    return from === null || to === null || !isCustomWindow(from, to);
  }
  const preset = params.get("window");
  return preset !== null && !isPreset(preset);
}

/** The window the URL names: a valid `from` and `to` together are a custom range, else `window`
 *  names a preset, else (absent or malformed) the 28-day preset. */
export function readWindow(params: URLSearchParams): WindowChoice {
  const from = params.get("from");
  const to = params.get("to");
  if (from !== null && to !== null && isCustomWindow(from, to)) {
    return { kind: "custom", from, to };
  }
  const preset = params.get("window");
  return { kind: "preset", preset: isPreset(preset) ? preset : DEFAULT_WINDOW_PRESET };
}

/** The live socket's window for a choice: a preset by its days, whose end Dispatch keeps at the
 *  present, or a custom range by its exact ends. */
export function liveWindow(choice: WindowChoice): DeliveryLiveWindow {
  return choice.kind === "custom"
    ? { from: choice.from, to: choice.to }
    : { preset: PRESET_DAYS[choice.preset] };
}

/** A custom range from two `YYYY-MM-DD` dates read as whole UTC days: it starts at the first
 *  day's midnight and ends at the midnight after the last, so the last day is included. Refused,
 *  with the reason, when a date is missing, the end comes before the start, or it spans more
 *  than MAX_WINDOW_DAYS. */
export function customRange(
  fromDate: string,
  toDate: string
): { from: string; to: string } | { refused: string } {
  const fromMs = Date.parse(`${fromDate}T00:00:00Z`);
  const lastMs = Date.parse(`${toDate}T00:00:00Z`);
  if (Number.isNaN(fromMs) || Number.isNaN(lastMs)) {
    return { refused: "Choose both a start and an end date." };
  }
  if (lastMs < fromMs) return { refused: "The window's end must not come before its start." };
  const days = Math.round((lastMs - fromMs) / DAY_MS) + 1;
  if (days > MAX_WINDOW_DAYS) {
    return {
      refused: `The window can span at most ${MAX_WINDOW_DAYS} days; this range spans ${days}.`,
    };
  }
  return { from: new Date(fromMs).toISOString(), to: new Date(lastMs + DAY_MS).toISOString() };
}

/** A `YYYY-MM-DD` date input's value for an instant, in UTC. */
export function utcDate(iso: string): string {
  return new Date(iso).toISOString().slice(0, 10);
}

/** The last day a custom range's exclusive `to` includes, as a date input's value. */
export function lastUtcDate(toIso: string): string {
  return new Date(Date.parse(toIso) - DAY_MS).toISOString().slice(0, 10);
}
