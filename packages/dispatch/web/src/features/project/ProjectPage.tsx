import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { type TabDefinition, Tabs } from "../../components/Tabs";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DocumentList } from "./DocumentList";
import { IssueList } from "./IssueList";

type ProjectTab = "issues" | "documents";

const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const route = parseProjectPath(location.pathname, location.search);
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => api.listProjects(),
  });
  const project = projects.data?.find((candidate) => candidate.key === route?.project);
  useDocumentTitle(
    project === undefined || route === undefined
      ? "Not found · Dispatch"
      : `${route.project} · ${project.name} · Dispatch`
  );
  if (route === undefined || route.kind === "document") {
    return <NotFoundPage />;
  }
  if (projects.isPending) {
    return <p className={textMutedOnCanvas}>Loading project…</p>;
  }
  if (projects.isError) {
    return <p className={dangerText}>Could not load this project.</p>;
  }
  if (project === undefined) {
    return <NotFoundPage />;
  }

  const activeTab: ProjectTab = route.kind === "documents" ? "documents" : "issues";
  const selectTab = (tab: ProjectTab) => {
    navigate(
      buildProjectPath(
        tab === "issues"
          ? { kind: "project", project: route.project }
          : { kind: "documents", project: route.project }
      )
    );
  };

  return (
    <section>
      <header className="mb-6">
        <Link
          className={`text-sm font-semibold ${linkText} ${linkHoverText}`}
          to={buildProjectPath({ kind: "project", project: route.project })}
        >
          {route.project}
        </Link>
        <h1 className={`mt-2 text-2xl font-semibold ${textPrimaryOnCanvas}`}>{project.name}</h1>
      </header>
      <Tabs
        activeTab={activeTab}
        ariaLabel="Project"
        idPrefix="project"
        onSelect={selectTab}
        tabs={tabs}
      />
      <div
        aria-hidden={activeTab !== "issues"}
        aria-labelledby="project-issues-tab"
        hidden={activeTab !== "issues"}
        id="project-issues-panel"
        role="tabpanel"
      >
        {activeTab === "issues" ? <IssueList project={route.project} /> : null}
      </div>
      <div
        aria-hidden={activeTab !== "documents"}
        aria-labelledby="project-documents-tab"
        hidden={activeTab !== "documents"}
        id="project-documents-panel"
        role="tabpanel"
      >
        {activeTab === "documents" ? <DocumentList project={route.project} /> : null}
      </div>
    </section>
  );
}
