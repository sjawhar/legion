import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { inboxQuery, projectsQuery, userAgentStateQuery } from "../../api/queries";
import type { AuthenticatedUser } from "../../api/types";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { PinButton } from "../../components/PinButton";
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
import { useLiveViewState } from "../agent-view/live-views";
import { pinnedAgentSessions, useAgentPinWrite } from "../agents/pins";
import { totalUnreadReplies, unreadRepliesLabel } from "../agents/unread";
import { useNeedsYouCount } from "../inbox/BlockedOnYou";
import { AgentName, useAgentText } from "../refs/AgentName";
import {
  buildIssuePath,
  buildProjectPath,
  liveViewSessionOf,
  parseIssuePath,
  parseProjectPath,
  REPLIES_PATH,
  routeIsInbox,
} from "../refs/routes";
import { useRepliesCount } from "../replies/useRepliesCount";

/** The rows the Agents heading folds, named so the heading's chevron can say what it controls. */
const PINNED_AGENTS_ID = "rail-pinned-agents";

/**
 * One pinned agent in the rail (INFRA-812, R1): its running state and name, both `AgentName`'s,
 * its unread replies, what its live view is doing (`live`, `live 30s` or `asleep`, the word the
 * registry computes), and the control that unpins it.
 *
 * The name is the link into the session's live view, and it `replace`s the address when the
 * reader is already in one: a move between pinned agents from the rail is one step of their
 * reading, so Back walks the pages they came from rather than every agent they looked at. Every
 * other link into a live view - `AgentName`'s elsewhere, the Inbox's, a pasted address - pushes.
 */
function PinnedAgentRow({
  onNavigate,
  onUnpin,
  replace,
  selected,
  sessionId,
  unread,
}: {
  onNavigate: (() => void) | undefined;
  onUnpin: () => void;
  replace: boolean;
  selected: boolean;
  sessionId: string;
  unread: number;
}): ReactNode {
  const name = useAgentText().sessionText(sessionId, undefined);
  const viewState = useLiveViewState(sessionId);
  return (
    <li
      aria-current={selected ? "page" : undefined}
      className={`flex items-center gap-2 rounded px-2 text-sm ${selected ? railActiveBg : ""}`}
    >
      <AgentName
        className="min-w-0 flex-1"
        onNavigate={onNavigate}
        replace={replace}
        sessionId={sessionId}
        storedTitle={undefined}
      />
      {unread === 0 ? null : (
        <span
          className={`shrink-0 rounded-full px-1.5 py-0.5 text-xs ${railBadgeBg} ${railBadgeText}`}
          title={unreadRepliesLabel(unread)}
        >
          {unread}
        </span>
      )}
      {/* What the session's view is doing, which only the registry knows: the rail renders the
          word and computes none of it (`live-views.ts`). */}
      <span className={`shrink-0 text-xs ${railSecondaryText}`}>{viewState}</span>
      <PinButton label={`Unpin ${name} from the sidebar`} onClick={onUnpin} pinned quiet />
    </li>
  );
}

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
  onOpenInboxDrawer,
  user,
}: {
  onHide?: () => void;
  onNavigate?: () => void;
  onOpenInboxDrawer?: () => void;
  user: AuthenticatedUser;
}): ReactNode {
  const location = useLocation();
  const inbox = useQuery(inboxQuery());
  const pinned = useQuery({
    queryKey: ["issues", "pinned"],
    queryFn: () => api.listIssues({ pinned: true }),
  });
  const projects = useQuery(projectsQuery());
  const needsYouCount = useNeedsYouCount();
  const agentState = useQuery(userAgentStateQuery()).data;
  const unreadReplies = totalUnreadReplies(agentState);
  // The rail's own list: the person's pinned agents, oldest pin first, and the control that
  // unpins one from here. The registry is told the whole set in the shell (`app.tsx`), not
  // here, because this rail is a sheet behind the Menu control below 1280 pixels and is not on
  // the page at all while the sidebar is collapsed.
  const pinnedAgents = pinnedAgentSessions(agentState);
  const writePin = useAgentPinWrite();
  const [agentsOpen, setAgentsOpen] = useState(true);
  // The live view the reader is in, which its row marks as the page and which makes a move to
  // another pinned agent a replacement rather than a step of its own.
  const currentLiveView = liveViewSessionOf(location.pathname);
  const unreadThreads = useRepliesCount();
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

  return (
    <>
      {hideControl}
      <nav aria-label="Navigation" className="mt-8 space-y-5">
        <section className="flex items-center gap-1">
          <Link
            className={`mt-1 flex flex-1 items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
            onClick={onNavigate}
            to="/"
          >
            <span>Inbox</span>
            {needsYouCount === 0 ? null : (
              <span
                className={`rounded-full px-1.5 py-0.5 text-xs ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
              >
                Needs you {needsYouCount}
              </span>
            )}
          </Link>
          {/* The Inbox page is the Inbox already, so there is nothing to peek at from it. */}
          {onOpenInboxDrawer === undefined || routeIsInbox(location.pathname) ? null : (
            <button
              aria-label="Peek at the inbox without leaving this page"
              className={`mt-1 shrink-0 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
              onClick={onOpenInboxDrawer}
              type="button"
            >
              Peek
            </button>
          )}
        </section>
        <section>
          <div className="flex items-center gap-1">
            <Link
              aria-current={location.pathname === "/agents" ? "page" : undefined}
              className={
                location.pathname === "/agents"
                  ? `mt-1 flex min-w-0 flex-1 items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railActiveBg} ${railAccentText}`
                  : `mt-1 flex min-w-0 flex-1 items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`
              }
              onClick={onNavigate}
              to="/agents"
            >
              <span>Agents</span>
              {unreadReplies === 0 ? null : (
                <span
                  className={`rounded-full px-1.5 py-0.5 text-xs ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
                >
                  {unreadRepliesLabel(unreadReplies)}
                </span>
              )}
            </Link>
            {/* The heading stays the link to the Agents page, so the fold is a control of its
                own beside it rather than the heading itself. */}
            <button
              aria-controls={PINNED_AGENTS_ID}
              aria-expanded={agentsOpen}
              aria-label={agentsOpen ? "Hide pinned agents" : "Show pinned agents"}
              className={`mt-1 flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
              onClick={() => setAgentsOpen(!agentsOpen)}
              type="button"
            >
              <ChevronIcon expanded={agentsOpen} />
            </button>
          </div>
          {agentsOpen ? (
            pinnedAgents.length === 0 ? (
              <p className={`mt-1 px-2 text-sm ${railMutedText}`} id={PINNED_AGENTS_ID}>
                Pin an agent on the Agents page, or in its live view, to keep it here.
              </p>
            ) : (
              <ul className="mt-1 space-y-0.5" id={PINNED_AGENTS_ID}>
                {pinnedAgents.map((sessionId) => (
                  <PinnedAgentRow
                    key={sessionId}
                    onNavigate={onNavigate}
                    onUnpin={() => writePin(sessionId, false)}
                    replace={currentLiveView !== undefined}
                    selected={sessionId === currentLiveView}
                    sessionId={sessionId}
                    unread={agentState?.[sessionId]?.unread_replies ?? 0}
                  />
                ))}
              </ul>
            )
          ) : null}
        </section>
        {/* Listed at 0 too, so the page is always one click away; the count shows only above 0,
            and the link then reads `Replies N`, as the compact header's does. */}
        <section>
          <Link
            aria-current={location.pathname === REPLIES_PATH ? "page" : undefined}
            className={
              location.pathname === REPLIES_PATH
                ? `mt-1 flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railActiveBg} ${railAccentText}`
                : `mt-1 flex items-baseline gap-2 rounded px-2 py-1.5 text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`
            }
            onClick={onNavigate}
            to={REPLIES_PATH}
          >
            <span>Replies</span>
            {unreadThreads === 0 ? null : (
              <span
                className={`rounded-full px-1.5 py-0.5 text-xs ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
              >
                {unreadThreads}
              </span>
            )}
          </Link>
        </section>
        <section>
          <ul className="mt-1 space-y-0.5">
            <RailRow
              badge={undefined}
              detail="Deploys, merges, and pipeline failures"
              label="Delivery"
              onNavigate={onNavigate}
              selected={location.pathname === "/delivery"}
              to="/delivery"
            />
            <RailRow
              badge={undefined}
              detail="CI and deploy step timings"
              label="Pipeline"
              onNavigate={onNavigate}
              selected={location.pathname === "/pipeline"}
              to="/pipeline"
            />
          </ul>
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
