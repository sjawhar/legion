import type { ReactNode } from "react";

import type { ArtifactApproval } from "../api/types";
import {
  approvalPill,
  statusPill,
  statusPillDot,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../theme/classes";

export type PillTone = "label" | "selected-label" | "status";

const pillTones: Record<PillTone, string> = {
  label: `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`,
  "selected-label": `${surfaceMutedStrongBg} ${textSecondaryOnSurface}`,
  status: `${statusPill.bg} ${statusPill.text}`,
};

export function Pill({
  children,
  className,
  tone = "label",
}: {
  children: ReactNode;
  className?: string;
  tone?: PillTone;
}): ReactNode {
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap ${pillTones[tone]} ${className ?? ""}`}
    >
      {children}
    </span>
  );
}

export function LabelPill({
  children,
  selected = false,
}: {
  children: ReactNode;
  selected?: boolean;
}): ReactNode {
  return <Pill tone={selected ? "selected-label" : "label"}>{children}</Pill>;
}

export function StatusPill({ children }: { children: ReactNode }): ReactNode {
  return (
    <Pill className="gap-1.5 font-semibold tracking-wide uppercase" tone="status">
      <span aria-hidden="true" className={`size-1.5 rounded-full ${statusPillDot}`} />
      {children}
    </Pill>
  );
}

/** The approval pill's full class string, for hosts whose chip must stay an interactive
 * element (ApprovalChip's button opens the review history) while looking identical. */
export function approvalPillClassName(state: ArtifactApproval["state"]): string {
  return `inline-flex shrink-0 items-center rounded-full border px-2 py-1 text-xs font-medium whitespace-nowrap ${approvalPill[state]}`;
}
