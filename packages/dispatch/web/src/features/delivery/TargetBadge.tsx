import type { ReactNode } from "react";

import { approvalPill } from "../../theme/classes";

/** The met / missed / no-data pill a delivery card wears against its target: the measures panel's
 *  KPI cards and the Pipeline page's two lead-time cards. `met` is null when there is nothing to
 *  judge; each surface names the met and missed states its own way, as its prototype page does,
 *  and `data-target-met` carries the verdict for tests and the e2e rows. */
export function TargetBadge({
  met,
  metLabel,
  missedLabel,
}: {
  met: boolean | null;
  metLabel: string;
  missedLabel: string;
}): ReactNode {
  const [label, state] =
    met === null
      ? ["no data", "draft" as const]
      : met
        ? [metLabel, "approved" as const]
        : [missedLabel, "changes_requested" as const];
  return (
    <span
      className={`shrink-0 rounded border px-1.5 py-0.5 text-[10px] font-medium whitespace-nowrap ${approvalPill[state]}`}
      data-target-met={met === null ? "none" : String(met)}
    >
      {label}
    </span>
  );
}
