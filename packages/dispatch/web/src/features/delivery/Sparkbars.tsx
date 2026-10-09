// The prototype's web/src/components/Sparkbars.tsx, ported as it is; its fills are the `chartBar`
// tokens, since no component writes a colour utility of its own.
import { chartBar } from "../../theme/classes";
import { barScale } from "./lib/barScale";

export interface SparkbarPoint {
  label: string; // tooltip prefix, e.g. "2026-09-27"
  value: number | null; // null: nothing to measure that day
  partial: boolean;
}

interface Props {
  points: SparkbarPoint[];
  // Omitted where no target exists: no target line, and every bar the same neutral colour.
  target?: number;
  // Top of the scale; the target line always fits under it.
  max: number;
  met?: (value: number) => boolean;
  format: (value: number) => string;
}

const BAR = 10;
const HEIGHT = 28;

/** One bar per day with a dashed target line: green bars meet the target, red
 * bars miss it, faded bars are days the window only partly covers. Plain SVG,
 * no chart library, so it costs nothing on a filter change. */
export function Sparkbars({ points, target, max, met, format }: Props) {
  const y = barScale(
    max,
    target,
    points.map((p) => p.value),
    HEIGHT
  );
  return (
    <svg
      viewBox={`0 0 ${points.length * BAR} ${HEIGHT}`}
      preserveAspectRatio="none"
      className="w-full h-7 block"
      role="img"
      aria-label="per-day trend"
    >
      {points.map((p, i) => (
        <g key={p.label} opacity={p.partial ? 0.4 : 1}>
          <title>{`${p.label}${p.partial ? " (partial day)" : ""}: ${p.value === null ? "no runs" : format(p.value)}`}</title>
          <rect x={i * BAR} y={0} width={BAR} height={HEIGHT} fill="transparent" />
          {p.value === null ? (
            <rect
              x={i * BAR + 1}
              y={HEIGHT - 1}
              width={BAR - 2}
              height={1}
              className={chartBar.empty}
            />
          ) : (
            <rect
              x={i * BAR + 1}
              y={y(p.value)}
              width={BAR - 2}
              height={Math.max(HEIGHT - y(p.value), 1)}
              className={
                target === undefined || met === undefined
                  ? chartBar.neutral
                  : met(p.value)
                    ? chartBar.met
                    : chartBar.missed
              }
            />
          )}
        </g>
      ))}
      {target !== undefined && (
        <line
          x1={0}
          x2={points.length * BAR}
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
