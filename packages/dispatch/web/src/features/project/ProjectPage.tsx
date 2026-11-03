import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import {
  legionSettingsQuery,
  projectArchitectureSourcesQuery,
  projectsQuery,
  whoAmIQuery,
} from "../../api/queries";
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
import { LegionView } from "../legion/LegionView";
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

/** Which view of a project's issues is on screen. `legion` is offered only for a project that
 *  names a Legion daemon (`GET /api/v1/settings/legion`); a stored preference naming it on a
 *  project without one falls back to the list rather than showing an empty view. */
type IssueView = "list" | "board" | "legion";

/** The order `v` cycles the views in, which is the order the selector draws them. */
const issueViews = ["list", "board", "legion"] as const;

const issueViewLabels: Record<IssueView, string> = {
  list: "List",
  board: "Board",
  legion: "Legion",
};

export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const [storedView, setIssueView] = useUserPreference<IssueView>(
    "project.issue-view",
    (stored) => (stored === "board" || stored === "legion" ? stored : "list"),
    (view) => view
  );
  const [showEdges, setShowEdges] = useUserPreference(
    "project.board-edges",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
  const [lanes, setLanes] = useUserPreference(
    "project.board-lanes",
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
  // Whose questions the pill counts, read as every other surface that counts work waiting on
  // the reader reads it.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const projects = useQuery(projectsQuery());
  const project = projects.data?.find((candidate) => candidate.key === route?.project);
  // Whether the project has an architecture source decides where the bare project path opens
  // and whether the Architecture tab exists at all: one read of its source list.
  const sources = useQuery({
    ...projectArchitectureSourcesQuery(projectKey ?? ""),
    enabled: projectKey !== undefined,
  });
  const hasSource = (sources.data?.length ?? 0) > 0;
  // Which projects name a Legion daemon: one deployment-wide read, shared with the Delivery
  // page's Legion section. A project that names none has no Legion view at all, so a stored
  // preference naming it falls back to the list.
  const legionSettings = useQuery(legionSettingsQuery());
  const hasLegion =
    projectKey !== undefined &&
    (legionSettings.data ?? []).some((setting) => setting.project === projectKey);
  const views: readonly IssueView[] = hasLegion
    ? issueViews
    : issueViews.filter((view) => view !== "legion");
  const issueView: IssueView = storedView === "legion" && !hasLegion ? "list" : storedView;
  useKeymapScope("project");
  useKeymap("project", [
    {
      id: "toggle-view",
      keys: "v",
      label: hasLegion ? "Toggle List / Board / Legion" : "Toggle List / Board",
      run: () => setIssueView(views[(views.indexOf(issueView) + 1) % views.length] ?? "list"),
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
      message="Could not load this project's architecture sources."
      onRetry={() => void sources.refetch()}
      retrying={sources.isFetching}
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
    // without (the list is empty) opens on Issues, the filter parameters forwarded. A failed
    // read is shown, never quietly turned into Issues. `replace` keeps Back sane; sidebar links
    // stay on the bare path.
    if (sources.isPending) {
      return null;
    }
    if (sources.isError) {
      return sourceError;
    }
    if (hasSource) {
      return (
        <Navigate replace to={buildProjectPath({ kind: "architecture", project: route.project })} />
      );
    }
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
  if (route.kind === "architecture") {
    // Like the bare path: nothing until the source lookup settles, so the tab strip never
    // renders without its active tab.
    if (sources.isPending) {
      return null;
    }
    if (sources.isError) {
      return sourceError;
    }
    if (!hasSource) {
      // A direct link to /architecture on a project without a source: nothing to show there.
      return <NotFoundPage />;
    }
  }

  const activeTab: ProjectTab = route.kind;
  const selectTab = (tab: ProjectTab) => {
    navigate(buildProjectPath({ kind: tab, project: route.project }));
  };

  return (
    <section>
      {/* One strip at `md` and above. Every control is `shrink-0`, so something has to give
          when they outgrow the row. The title gives, truncating, which is what a long project
          name should do; otherwise the last control - the blocked-on-you pill - would be pushed
          off the right edge and unreachable. */}
      <header className="flex flex-wrap items-center gap-2 md:min-h-12 md:flex-nowrap">
        {/* `flex-initial` at `md`: the title takes the width its name needs and no more, so
            the project key sits beside a short name instead of 380px away, and only a long
            name truncates - carrying the whole name as its `title`. */}
        <h1
          className={`order-1 min-w-0 flex-1 truncate text-[22px] font-semibold tracking-tight md:flex-initial ${textPrimaryOnCanvas}`}
          title={project.name}
        >
          {project.name}
        </h1>
        <Link
          className={`order-2 inline-flex min-h-11 shrink-0 items-center rounded-full px-3 text-xs font-semibold md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
          to={buildProjectPath({ kind: "issues", project: route.project })}
        >
          {route.project}
        </Link>
        {/* The reader's own questions on this project, and no credential request: the pending
            list is global (a request belongs to no project), so counting one here would make
            this project's number partly about another page. */}
        <BlockedOnYou
          asks={inbox.data ?? []}
          className="order-3 shrink-0 md:order-5"
          variant="pill"
          viewer={viewer}
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
            {views.map((view) => (
              <button
                aria-pressed={issueView === view}
                className={`min-h-11 rounded-lg px-3 text-sm font-medium md:min-h-9 md:px-2 ${
                  issueView === view ? surfaceMutedStrongBg : surfaceMutedBg
                } ${textSecondaryOnCanvas}`}
                key={view}
                onClick={() => setIssueView(view)}
                type="button"
              >
                {issueViewLabels[view]}
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
        {activeTab === "issues" && issueView === "board" ? (
          <button
            aria-pressed={lanes}
            className={`order-8 min-h-11 shrink-0 rounded-xl border px-3 text-sm font-medium md:order-4 md:min-h-9 md:px-2 ${borderDefault} ${
              lanes ? surfaceMutedStrongBg : surfaceMutedBg
            } ${textSecondaryOnCanvas}`}
            onClick={() => setLanes(!lanes)}
            type="button"
          >
            {lanes ? "Hide lanes" : "Show lanes"}
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
          issueView === "legion" ? (
            // The Legion view has its own columns and its own header; the filter strip's chips
            // belong to Dispatch's own list and board, and none of them reaches the daemon.
            <LegionView project={route.project} />
          ) : (
            <>
              <IssueFilters
                key={route.project}
                project={route.project}
                showStatus={issueView === "list"}
              />
              {issueView === "list" ? (
                <IssueList project={route.project} />
              ) : (
                <IssueBoard lanes={lanes} project={route.project} showEdges={showEdges} />
              )}
            </>
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
