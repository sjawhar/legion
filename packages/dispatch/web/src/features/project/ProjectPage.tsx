import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useEffect, useState } from "react";

import { Link, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { type TabDefinition, Tabs } from "../../components/Tabs";
import {
  borderDefault,
  dangerText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { BlockedOnYou } from "../inbox/BlockedOnYou";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { DocumentList } from "./DocumentList";
import { IssueBoard } from "./IssueBoard";
import { IssueList } from "./IssueList";

type ProjectTab = "issues" | "documents";

const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

type IssueView = "list" | "board";

export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const whoAmI = useQuery({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });
  const login = whoAmI.data?.login;
  const [issueView, setIssueView] = useState<IssueView>(() =>
    login !== undefined &&
    window.localStorage.getItem(userPreferenceStorageKey(login, "project.issue-view")) === "board"
      ? "board"
      : "list"
  );
  useEffect(() => {
    if (login === undefined) {
      return;
    }
    setIssueView(
      window.localStorage.getItem(userPreferenceStorageKey(login, "project.issue-view")) === "board"
        ? "board"
        : "list"
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
      <header className="flex flex-wrap items-center gap-2 md:min-h-12 md:flex-nowrap">
        <h1
          className={`order-1 min-w-0 flex-1 truncate text-[22px] font-semibold tracking-tight md:flex-none ${textPrimaryOnCanvas}`}
        >
          {project.name}
        </h1>
        <Link
          className={`order-2 inline-flex min-h-11 shrink-0 items-center rounded-full px-3 text-xs font-semibold md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
          to={buildProjectPath({ kind: "project", project: route.project })}
        >
          {route.project}
        </Link>
        <BlockedOnYou
          asks={inbox.data ?? []}
          className="order-3 shrink-0 md:order-5"
          variant="pill"
        />
        <div aria-hidden className="order-4 basis-full md:hidden" />
        <div className="order-5 shrink-0 md:order-3">
          <Tabs
            activeTab={activeTab}
            ariaLabel="Project"
            compact
            idPrefix="project"
            onSelect={selectTab}
            tabs={tabs}
          />
        </div>
        {activeTab === "issues" ? (
          <fieldset
            className={`order-6 inline-flex shrink-0 rounded-xl border p-1 md:order-4 md:ml-auto ${borderDefault}`}
          >
            <legend className="sr-only">Issue view</legend>
            {(["list", "board"] as const).map((view) => (
              <button
                aria-pressed={issueView === view}
                className={`min-h-11 rounded-lg px-3 text-sm font-medium md:min-h-9 md:px-2 ${
                  issueView === view ? surfaceMutedStrongBg : surfaceMutedBg
                } ${textSecondaryOnCanvas}`}
                key={view}
                onClick={() => {
                  setIssueView(view);
                  if (login !== undefined) {
                    window.localStorage.setItem(
                      userPreferenceStorageKey(login, "project.issue-view"),
                      view
                    );
                  }
                }}
                type="button"
              >
                {view === "list" ? "List" : "Board"}
              </button>
            ))}
          </fieldset>
        ) : null}
      </header>
      <div
        aria-hidden={activeTab !== "issues"}
        aria-labelledby="project-issues-tab"
        className="mt-3"
        hidden={activeTab !== "issues"}
        id="project-issues-panel"
        role="tabpanel"
      >
        {activeTab === "issues" ? (
          issueView === "list" ? (
            <IssueList login={login} project={route.project} />
          ) : (
            <IssueBoard project={route.project} />
          )
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
