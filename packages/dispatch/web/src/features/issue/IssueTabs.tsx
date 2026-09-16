import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { type TabDefinition, Tabs } from "../../components/Tabs";
import { buildIssuePath, type IssueTab } from "../refs/routes";

function tabs(artifactCount: number): readonly TabDefinition<IssueTab>[] {
  return [
    { id: "spec", label: "Spec" },
    { id: "conversation", label: "Conversation" },
    { id: "children", label: "Children" },
    artifactCount === 0
      ? { id: "artifacts", label: "Artifacts" }
      : { id: "artifacts", label: "Artifacts", count: artifactCount },
  ];
}

/** The IssuePage tablist keeps its active view in the canonical issue route. `artifactCount` is
 *  the number of artifacts uploaded to the issue besides its spec, shown on the Artifacts tab so a
 *  reader knows there is something to open without visiting it. */
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
