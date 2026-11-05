import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { type TabDefinition, Tabs } from "../../components/Tabs";
import { buildIssuePath, type IssueTab } from "../refs/routes";

function tabs(artifactCount: number): readonly TabDefinition<IssueTab>[] {
  return [
    { id: "spec", label: "Spec" },
    { id: "verification", label: "Verification" },
    { id: "plan", label: "Plan" },
    { id: "conversation", label: "Conversation" },
    { id: "children", label: "Children" },
    artifactCount === 0
      ? { id: "artifacts", label: "Artifacts" }
      : { id: "artifacts", label: "Artifacts", count: artifactCount },
    { id: "activity", label: "Activity" },
    { id: "decisions", label: "Decisions" },
    { id: "collaborators", label: "Collaborators" },
  ];
}

/** The tablist's labels in order, for the loading skeleton that stands in for this tablist
 *  (`app.tsx`). It stood on a hand-written copy of this list until that copy had drifted by
 *  three tabs, so it reads the list itself; the count a label can carry is a number the skeleton
 *  has no issue to take, and it stands in for the plain label. */
export const ISSUE_TAB_LABELS: readonly string[] = tabs(0).map((tab) => tab.label);

/** The IssuePage tablist keeps its active view in the canonical issue route. `artifactCount` is
 *  how many rows the Artifacts tab lists - every artifact the issue has, its primary document
 *  included - so the badge and the list a reader checks it against say the same number. */
export function IssueTabs({
  activeTab,
  artifactCount,
  issueKey,
  onBeforeTabChange,
  toolbar,
}: {
  activeTab: IssueTab;
  artifactCount: number;
  issueKey: string;
  onBeforeTabChange: (current: IssueTab) => void;
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();

  return (
    <div data-testid="issue-tabs">
      <Tabs
        activeTab={activeTab}
        ariaLabel="Issue detail"
        compact
        idPrefix="issue"
        onBeforeTabChange={onBeforeTabChange}
        onSelect={(tab) => navigate(buildIssuePath({ key: issueKey, kind: tab }))}
        tabs={tabs(artifactCount)}
        trailing={toolbar}
      />
    </div>
  );
}
