// The Delivery page's first section (INFRA-1077's spec, "The Delivery page"): Legion, issue to
// merge. The page gives it the window and the comparison it gives the Pipeline section and the
// delivery timeline (`DeliverySectionProps`), so every chart on the page follows one window
// control and one compare range.
//
// What it draws today is its heading and its `#legion` anchor, which is what the spec's section
// order and a `/delivery#legion` link need. Its body - the four Legion metrics (work time to
// merge, phase time by worker type, rework, and splitting and follow-ups) from
// `GET /api/v1/legion/metrics`, with a project picker and a toggle for all issues, roots or
// children, each chart drawn by `Chart` (`Chart.tsx`) - is not written yet, and neither is the
// read of that route: no web module names it.
import type { ReactNode } from "react";

import { textPrimaryOnCanvas } from "../../theme/classes";
import type { DeliverySectionProps } from "./Chart";

/** The Delivery page's `#legion` section: its heading and its anchor, and the page's window and
 *  comparison, which the metrics it will draw follow. */
export function LegionSection(_props: DeliverySectionProps): ReactNode {
  return (
    <section aria-labelledby="legion-heading" className="flex flex-col gap-3" id="legion">
      <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`} id="legion-heading">
        Legion: issue to merge
      </h2>
    </section>
  );
}
