import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { AuthenticatedUser } from "../../api/types";
import {
  railAccentText,
  railActiveBg,
  railBadgeBg,
  railBadgeText,
  railBorder,
  railDangerText,
  railHoverBg,
  railHoverText,
  railMutedText,
  railNeedsYouBadgeBg,
  railNeedsYouBadgeText,
  railSecondaryText,
} from "../../theme/classes";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { buildIssuePath, buildProjectPath, parseIssuePath, parseProjectPath } from "../refs/routes";

/** One rail entry: a key, its title, and the open-ask count when there is one. */
function RailRow({
  badge,
  detail,
  label,
  onNavigate,
  selected,
  to,
}: {
  badge: number | undefined;
  detail: string;
  label: string;
  onNavigate: (() => void) | undefined;
  selected: boolean;
  to: string;
}): ReactNode {
  return (
    <li>
      <Link
        aria-current={selected ? "page" : undefined}
        className={
          selected
            ? `flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railActiveBg} ${railAccentText}`
            : `flex items-baseline gap-2 rounded px-2 py-1.5 text-sm ${railHoverBg}`
        }
        onClick={onNavigate}
        to={to}
      >
        <span className="shrink-0 font-medium whitespace-nowrap">{label}</span>
        <span className={`min-w-0 flex-1 ${railSecondaryText}`}>{detail}</span>
        {badge === undefined ? null : (
          <span
            className={`shrink-0 rounded-full px-1.5 py-0.5 text-xs ${railBadgeBg} ${railBadgeText}`}
          >
            {badge}
          </span>
        )}
      </Link>
    </li>
  );
}

export function Sidebar({
  onHide,
  onNavigate,
  user,
}: {
  onHide?: () => void;
  onNavigate?: () => void;
  user: AuthenticatedUser;
}): ReactNode {
  const location = useLocation();
  const inbox = useQuery(inboxQuery());
  const pinned = useQuery({
    queryKey: ["issues", "pinned"],
    queryFn: () => api.listIssues({ pinned: true }),
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.listProjects() });
  const currentIssue = parseIssuePath(location.pathname)?.key;
  const currentProject = parseProjectPath(location.pathname)?.project;

  const hideControl =
    onHide === undefined ? null : (
      <button
        aria-label="Hide sidebar"
        className={`absolute top-2 right-2 min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
        onClick={onHide}
        type="button"
      >
        <span aria-hidden="true">‹</span>
      </button>
    );

  if (inbox.isPending || pinned.isPending || projects.isPending) {
    return (
      <>
        {hideControl}
        <p className={`mt-8 text-sm ${railMutedText}`}>Loading navigation…</p>
      </>
    );
  }
  if (inbox.isError || pinned.isError || projects.isError) {
    return (
      <>
        {hideControl}
        <p className={`mt-8 text-sm ${railDangerText}`}>Could not load navigation.</p>
      </>
    );
  }
  const needsYou = waitingOnYou(inbox.data);

  return (
    <>
      {hideControl}
      <nav aria-label="Navigation" className="mt-8 space-y-5">
        <section>
          <Link
            className={`mt-1 flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
            onClick={onNavigate}
            to="/"
          >
            <span>Inbox</span>
            {needsYou.length === 0 ? null : (
              <span
                className={`rounded-full px-1.5 py-0.5 text-xs ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
              >
                Needs you {needsYou.length}
              </span>
            )}
          </Link>
        </section>
        <section>
          <Link
            aria-current={location.pathname === "/agents" ? "page" : undefined}
            className={
              location.pathname === "/agents"
                ? `mt-1 flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railActiveBg} ${railAccentText}`
                : `mt-1 flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`
            }
            onClick={onNavigate}
            to="/agents"
          >
            Agents
          </Link>
        </section>
        {pinned.data.length === 0 ? null : (
          <section>
            <h2 className={`px-2 text-xs font-semibold tracking-wide uppercase ${railMutedText}`}>
              Pinned
            </h2>
            <ul className="mt-1 space-y-0.5">
              {pinned.data.map((issue) => (
                <RailRow
                  badge={issue.open_asks === 0 ? undefined : issue.open_asks}
                  detail={issue.title}
                  key={issue.key}
                  label={issue.key}
                  onNavigate={onNavigate}
                  selected={issue.key === currentIssue}
                  to={buildIssuePath({ key: issue.key, kind: "issue" })}
                />
              ))}
            </ul>
          </section>
        )}
        <section>
          <h2 className={`px-2 text-xs font-semibold tracking-wide uppercase ${railMutedText}`}>
            Projects
          </h2>
          <ul className="mt-1 space-y-0.5">
            {projects.data.map((project) => (
              <RailRow
                badge={
                  project.open_asks === undefined || project.open_asks === 0
                    ? undefined
                    : project.open_asks
                }
                detail={project.name}
                key={project.key}
                label={project.key}
                onNavigate={onNavigate}
                selected={project.key === currentProject}
                to={buildProjectPath({ kind: "project", project: project.key })}
              />
            ))}
          </ul>
        </section>
        {user.kind === "user" ? (
          <div className={`border-t pt-4 ${railBorder}`}>
            <Link
              className={`block rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
              onClick={onNavigate}
              to="/settings"
            >
              Settings
            </Link>
          </div>
        ) : null}
      </nav>
    </>
  );
}
