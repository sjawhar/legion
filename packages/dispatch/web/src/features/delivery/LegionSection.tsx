// The Delivery page's first section (INFRA-1077's spec, "The Delivery page"): Legion, issue to
// merge. The page gives it the window and the comparison it gives the Pipeline section and the
// delivery timeline, so every chart on the page follows one window control and one compare range.
//
// Its body is the four Legion metrics - work time to merge, phase time by worker type, rework,
// and splitting and follow-ups - with a project picker and a toggle for all issues, roots or
// children, each chart drawn by `Chart` (`Chart.tsx`) so it has the page's graph-or-table and
// compare controls. The metrics come from `GET /api/v1/legion/metrics`, read for `window` and
// again for `compare` once the reader names one, as `PipelineSection` reads its own route twice.
// Shown for a project with a Legion setting; with none, it says so rather than drawing empty
// charts.
import type { ReactNode } from "react";

import { textPrimaryOnCanvas } from "../../theme/classes";
import type { ChartRange } from "./Chart";
import type { WindowChoice } from "./lib/window";

/** What the Delivery page gives its Legion section: its one window and its one compare range. */
export interface LegionSectionProps {
  /** The range the page reads beside its window, or null while nothing is compared. */
  readonly compare: ChartRange | null;
  /** The range a chart's compare control named; null turns the comparison off. */
  readonly onCompare: (next: ChartRange | null) => void;
  /** The page's window, which every section follows. */
  readonly window: WindowChoice;
}

/** The Delivery page's `#legion` section: the four Legion metrics over the page's window. */
export function LegionSection(_props: LegionSectionProps): ReactNode {
  return (
    <section aria-labelledby="legion-heading" className="flex flex-col gap-3" id="legion">
      <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`} id="legion-heading">
        Legion: issue to merge
      </h2>
    </section>
  );
}
