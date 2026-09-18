import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { api, isSourceNotFound } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
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
import { ArchitecturePage } from "../architecture/ArchitecturePage";
import { BlockedOnYou } from "../inbox/BlockedOnYou";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useUserPreference } from "../shell/userPreference";
import { DocumentList } from "./DocumentList";
import { IssueBoard } from "./IssueBoard";
import { IssueFilters } from "./IssueFilters";
import { IssueList } from "./IssueList";

type ProjectTab = "architecture" | "issues" | "documents";

const architectureTab: TabDefinition<ProjectTab> = { id: "architecture", label: "Architecture" };
const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

type IssueView = "list" | "board";

export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const [issueView, setIssueView] = useUserPreference<IssueView>(
    "project.issue-view",
    (stored) => (stored === "board" ? "board" : "list"),
    (view) => view
  );
  const [showEdges, setShowEdges] = useUserPreference(
    "project.board-edges",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );

  const route = parseProjectPath(location.pathname, location.search);
  const projectKey = route?.project;
  const inbox = useQuery({
    queryKey: ["inbox", projectKey],
    queryFn: () => api.getInbox(projectKey),
    enabled: projectKey !== undefined,
  });
  const projects = useQuery(projectsQuery());
  const project = projects.data?.find((candidate) => candidate.key === route?.project);
  // Whether the project has an architecture source decides where the bare project path opens
  // and whether the Architecture tab exists at all.
  const source = useQuery({
    queryKey: ["architecture-source", projectKey],
    queryFn: () => api.getArchitectureSource(projectKey as string),
    enabled: projectKey !== undefined,
  });
  const hasSource = source.isSuccess;
  useKeymapScope("project");
  useKeymap("project", [
    {
      id: "toggle-view",
      keys: "v",
      label: "Toggle List / Board",
      run: () => setIssueView(issueView === "list" ? "board" : "list"),
      when: () => route?.kind === "issues",
    },
  ]);
  useDocumentTitle(
    project === undefined || route === undefined
      ? "Not found · Dispatch"
      : `${route.project} · ${project.name} · Dispatch`
  );
  if (route === undefined || route.kind === "document") {
    return <NotFoundPage />;
  }
  const sourceError = (
    <QueryError
      message="Could not load this project's architecture source."
      onRetry={() => void source.refetch()}
      retrying={source.isFetching}
    />
  );
  if (projects.isPending) {
    return <p className={textMutedOnCanvas}>Loading project…</p>;
  }
  if (projects.isError) {
    return <p className={dangerText}>Could not load this project.</p>;
  }
  if (project === undefined) {
    return <NotFoundPage />;
  }
  if (route.kind === "project") {
    // The open-on rule: a project with an architecture source opens on its Architecture; one
    // without (404 SOURCE_NOT_FOUND, the only expected failure) opens on Issues, the filter
    // parameters forwarded. Any other failure is shown, never quietly turned into Issues.
    // `replace` keeps Back sane; sidebar links stay on the bare path.
    if (source.isPending) {
      return null;
    }
    if (source.isSuccess) {
      return (
        <Navigate replace to={buildProjectPath({ kind: "architecture", project: route.project })} />
      );
    }
    if (isSourceNotFound(source.error)) {
      return (
        <Navigate
          replace
          to={{
            pathname: buildProjectPath({ kind: "issues", project: route.project }),
            search: location.search,
          }}
        />
      );
    }
    return sourceError;
  }
  if (route.kind === "architecture") {
    // Like the bare path: nothing until the source lookup settles, so the tab strip never
    // renders without its active tab.
    if (source.isPending) {
      return null;
    }
    if (!hasSource) {
      // A direct link to /architecture on a project without a source: nothing to show there.
      return isSourceNotFound(source.error) ? <NotFoundPage /> : sourceError;
    }
  }

  const activeTab: ProjectTab = route.kind;
  const selectTab = (tab: ProjectTab) => {
    navigate(buildProjectPath({ kind: tab, project: route.project }));
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
          to={buildProjectPath({ kind: "issues", project: route.project })}
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
            tabs={hasSource ? [architectureTab, ...tabs] : tabs}
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
                onClick={() => setIssueView(view)}
                type="button"
              >
                {view === "list" ? "List" : "Board"}
              </button>
            ))}
          </fieldset>
        ) : null}
        {activeTab === "issues" && issueView === "board" ? (
          <button
            aria-pressed={showEdges}
            className={`order-7 min-h-11 shrink-0 rounded-xl border px-3 text-sm font-medium md:order-4 md:min-h-9 md:px-2 ${borderDefault} ${
              showEdges ? surfaceMutedStrongBg : surfaceMutedBg
            } ${textSecondaryOnCanvas}`}
            onClick={() => setShowEdges(!showEdges)}
            type="button"
          >
            {showEdges ? "Hide Icebox & Done" : "Show Icebox & Done"}
          </button>
        ) : null}
      </header>
      {hasSource ? (
        <div
          aria-hidden={activeTab !== "architecture"}
          aria-labelledby="project-architecture-tab"
          className="mt-3"
          hidden={activeTab !== "architecture"}
          id="project-architecture-panel"
          role="tabpanel"
        >
          {activeTab === "architecture" ? <ArchitecturePage project={route.project} /> : null}
        </div>
      ) : null}
      <div
        aria-hidden={activeTab !== "issues"}
        aria-labelledby="project-issues-tab"
        className="mt-3"
        hidden={activeTab !== "issues"}
        id="project-issues-panel"
        role="tabpanel"
      >
        {activeTab === "issues" ? (
          <>
            <IssueFilters project={route.project} showStatus={issueView === "list"} />
            {issueView === "list" ? (
              <IssueList project={route.project} />
            ) : (
              <IssueBoard project={route.project} showEdges={showEdges} />
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
