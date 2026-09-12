import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useEffect, useState } from "react";

import { Link, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { type TabDefinition, Tabs } from "../../components/Tabs";
import {
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { BlockedOnYou } from "../inbox/BlockedOnYou";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DocumentList } from "./DocumentList";
import { IssueBoard } from "./IssueBoard";
import { IssueList } from "./IssueList";

type ProjectTab = "issues" | "documents";

const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

type IssueView = "list" | "board";

function issueViewStorageKey(login: string): string {
  return `dispatch.project.issue-view:${login}`;
}

export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const whoAmI = useQuery({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });
  const login = whoAmI.data?.login;
  const [issueView, setIssueView] = useState<IssueView>(() =>
    login !== undefined && window.localStorage.getItem(issueViewStorageKey(login)) === "board"
      ? "board"
      : "list"
  );
  useEffect(() => {
    if (login === undefined) {
      return;
    }
    setIssueView(
      window.localStorage.getItem(issueViewStorageKey(login)) === "board" ? "board" : "list"
    );
  }, [login]);

  const route = parseProjectPath(location.pathname, location.search);
  const projectKey = route?.project;
  const inbox = useQuery({
    queryKey: ["inbox", projectKey],
    queryFn: () => api.getInbox(projectKey),
    enabled: projectKey !== undefined,
  });
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
      <BlockedOnYou asks={inbox.data ?? []} />
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
        {activeTab === "issues" ? (
          <>
            <fieldset className={`mb-4 inline-flex rounded-xl border p-1 ${borderDefault}`}>
              <legend className="sr-only">Issue view</legend>
              {(["list", "board"] as const).map((view) => (
                <button
                  aria-pressed={issueView === view}
                  className={`min-h-11 rounded-lg px-3 text-sm font-medium ${
                    issueView === view ? surfaceMutedStrongBg : surfaceMutedBg
                  } ${textSecondaryOnCanvas}`}
                  key={view}
                  onClick={() => {
                    setIssueView(view);
                    if (login !== undefined) {
                      window.localStorage.setItem(issueViewStorageKey(login), view);
                    }
                  }}
                  type="button"
                >
                  {view === "list" ? "List" : "Board"}
                </button>
              ))}
            </fieldset>
            {issueView === "list" ? (
              <IssueList project={route.project} />
            ) : (
              <IssueBoard project={route.project} />
            )}
          </>
        ) : null}
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
