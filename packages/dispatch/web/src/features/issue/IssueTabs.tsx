import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { type TabDefinition, Tabs } from "../../components/Tabs";
import { buildIssuePath, type IssueTab } from "../refs/routes";

const tabs: readonly TabDefinition<IssueTab>[] = [
  { id: "spec", label: "Spec" },
  { id: "conversation", label: "Conversation" },
  { id: "children", label: "Children" },
  { id: "artifacts", label: "Artifacts" },
];

/** The IssuePage tablist keeps its active view in the canonical issue route. */
export function IssueTabs({
  activeTab,
  issueKey,
  onBeforeTabChange,
}: {
  activeTab: IssueTab;
  issueKey: string;
  onBeforeTabChange: (current: IssueTab) => void;
}): ReactNode {
  const navigate = useNavigate();

  return (
    <Tabs
      activeTab={activeTab}
      ariaLabel="Issue detail"
      idPrefix="issue"
      onBeforeTabChange={onBeforeTabChange}
      onSelect={(tab) => navigate(buildIssuePath({ key: issueKey, kind: tab }))}
      tabs={tabs}
    />
  );
}
