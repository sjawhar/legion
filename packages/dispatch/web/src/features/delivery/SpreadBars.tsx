// The prototype's web/src/components/SpreadBars.tsx, ported as it is; its fills are the
// `chartBar` tokens, since no component writes a colour utility of its own.
import { chartBar } from "../../theme/classes";
import { barScale } from "./lib/barScale";

export interface SpreadBarDay {
  day: string; // YYYY-MM-DD, the tooltip's prefix
  n: number;
  median_minutes: number | null;
  p90_minutes: number | null;
  partial: boolean;
}

interface Props {
  days: SpreadBarDay[];
  // Omitted where no target exists: no target line, and bars in one neutral colour.
  target?: number;
  noun: string; // what `n` counts, "runs"
  format: (minutes: number) => string;
}

const BAR = 12;
const HEIGHT = 36;

/** One bar per day: the p90 drawn faint behind the median. With a target, a dashed line marks it
 * and a bar is green under it, red at or over it. Today's partial day is faded. Plain SVG, like
 * `Sparkbars`. */
export function SpreadBars({ days, target, noun, format }: Props) {
  const y = barScale(
    1,
    target,
    days.map((d) => d.p90_minutes),
    HEIGHT
  );
  return (
    <svg
      viewBox={`0 0 ${days.length * BAR} ${HEIGHT}`}
      preserveAspectRatio="none"
      className="block h-9 w-full"
      role="img"
      aria-label="per-day median and p90"
    >
      {days.map((d, i) => (
        <g key={d.day} opacity={d.partial ? 0.55 : 1}>
          <title>
            {`${d.day}${d.partial ? " (today so far)" : ""}: ${
              d.median_minutes === null || d.p90_minutes === null
                ? `no ${noun}`
                : `median ${format(d.median_minutes)}, p90 ${format(d.p90_minutes)}, ${d.n} ${noun}`
            }`}
          </title>
          <rect x={i * BAR} y={0} width={BAR} height={HEIGHT} fill="transparent" />
          {d.median_minutes === null || d.p90_minutes === null ? (
            <rect
              x={i * BAR + 1}
              y={HEIGHT - 1}
              width={BAR - 2}
              height={1}
              className={chartBar.empty}
            />
          ) : (
            <>
              <rect
                x={i * BAR + 1}
                y={y(d.p90_minutes)}
                width={BAR - 2}
                height={Math.max(HEIGHT - y(d.p90_minutes), 1)}
                className={
                  target === undefined
                    ? chartBar.neutralFaint
                    : d.p90_minutes < target
                      ? chartBar.metFaint
                      : chartBar.missedFaint
                }
              />
              <rect
                x={i * BAR + 1}
                y={y(d.median_minutes)}
                width={BAR - 2}
                height={Math.max(HEIGHT - y(d.median_minutes), 1)}
                className={
                  target === undefined
                    ? chartBar.neutral
                    : d.median_minutes < target
                      ? chartBar.met
                      : chartBar.missed
                }
              />
            </>
          )}
        </g>
      ))}
      {target !== undefined && (
        <line
          x1={0}
          x2={days.length * BAR}
          y1={y(target)}
          y2={y(target)}
          className={chartBar.targetLine}
          strokeDasharray="3 2"
          vectorEffect="non-scaling-stroke"
        />
      )}
    </svg>
  );
}
