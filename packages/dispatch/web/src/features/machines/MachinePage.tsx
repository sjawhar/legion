import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { Link, useLocation } from "react-router-dom";

import { inboxQuery, userAgentStateQuery, whoAmIQuery } from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { useCappedSearchParam } from "../../lib/query-param";
import {
  borderDefault,
  dangerText,
  inlineWarningText,
  linkHoverText,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { tallyBySession } from "../agents/ask-tally";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { buildAgentsMachinePath, parseMachinePath } from "../refs/routes";
import { relativeTimeLabel } from "../refs/Timestamp";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import {
  chosenWindowId,
  type MachineTmuxServer,
  type MachineWindow,
  machineDrawingAge,
  machineGrouping,
  serverKey,
  sessionCountSentence,
  windowChoice,
} from "./machine-model";
import { type PaneCounts, WindowDrawing } from "./WindowDrawing";

/**
 * One machine's tmux layout, drawn from the agent registry alone: one part per tmux server, each
 * with a strip of its windows and one of them drawn. The page says what it cannot know, because
 * each session reports only its own pane and its own window (dispatch://INFRA-883's spec): a
 * machine with no session in tmux, a server's windows that hold no agent, how old the reports
 * are, and what a machine name is.
 */

/** What the page says about what it does not know. Each sentence is one line of the page, named
 *  by the `data-testid` it is drawn under, so a browser check reads it where it is said. */
const NO_SESSION_REPORTS_IT = "No live session reports this machine, so there is nothing to draw.";
const NONE_IN_TMUX = "No session on this machine is in tmux, so there is no layout to draw.";
const WINDOWS_WITH_AGENTS =
  "This server shows only the windows that hold an agent: a window with no agent reports nothing.";
const DRAWING_STALE = "This drawing is stale: its oldest report is over three minutes old.";
const SHARED_NAME =
  "This machine name is one a deployment chose, not one host: its sessions report more than one directory.";

/** One window of a server's strip: the tmux session holding it, and the window itself. */
interface StripWindow {
  readonly sessionName: string;
  readonly window: MachineWindow;
}

/** Every window of a server, in tmux-session order and then window-index order, as the model
 *  sorted them. */
function stripWindows(server: MachineTmuxServer): StripWindow[] {
  return server.sessions.flatMap((session) =>
    session.windows.map((window) => ({ sessionName: session.name, window }))
  );
}

export function MachinePage(): ReactNode {
  const { pathname } = useLocation();
  const host = parseMachinePath(pathname);
  if (host === undefined) {
    return <NotFoundPage />;
  }
  return <MachineView host={host} />;
}

function MachineView({ host }: { host: string }): ReactNode {
  useDocumentTitle(`${host} · Machines · Dispatch`);
  const { agents, error, isError, isPending } = useAgents(true, true);
  // The picked window rides the address (`?window=<socket>:<pid>:<window id>`), so the picker
  // is a link, the back button means something across a window change, and one window is
  // deep-linkable. The server is in the value because tmux numbers windows per server, so a
  // bare `@1` would name a window on every server of the machine at once (`windowChoice`).
  const [picked] = useCappedSearchParam("window");
  // What the viewer is owed, read once for the page from the two queries the shell already
  // makes - never once per pane - so a pane that needs them reads `needs-you`, the first of
  // the four states, and the page adds no request of its own.
  const inbox = useQuery(inboxQuery());
  // Until `/auth/whoami` answers, no question is counted as the reader's.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const agentState = useQuery(userAgentStateQuery()).data;
  const counts = useMemo<PaneCounts>(
    () => ({
      asksForYou: tallyBySession(waitingOnYou(inbox.data ?? [], viewer)),
      unreadReplies: new Map(
        Object.entries(agentState ?? {}).map(([sessionId, state]) => [
          sessionId,
          state.unread_replies ?? 0,
        ])
      ),
    }),
    [agentState, inbox.data, viewer]
  );
  // The grouping is the registry's alone, so it is rebuilt only when the rows change; the
  // drawing's age is this render's clock, so it goes on ageing while the page sits open.
  const model = useMemo(() => machineGrouping(agents, host), [agents, host]);
  const now = Date.now();
  const age = machineDrawingAge(model, now);

  if (isPending) return <LoadingSkeleton label="Loading machine" />;
  if (isError && agents.length === 0) {
    return <p className={dangerText}>Could not load agents: {error}</p>;
  }

  const oldest =
    age === undefined
      ? undefined
      : (relativeTimeLabel(new Date(age.oldestSeen), now) ?? "just now");

  return (
    <section aria-label={`Machine ${host}`} data-machine={host} data-testid="machine-page">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/machines">
          ← Machines
        </Link>
        <h1
          className={`mt-1 min-w-0 break-words text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}
        >
          {host}
        </h1>
        <p className={`mt-1 flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
          <span>{sessionCountSentence(model.summary)}</span>
          <Link className={`${linkText} ${linkHoverText}`} to={buildAgentsMachinePath(host)}>
            Agents on this machine
          </Link>
        </p>
      </header>
      {model.sharedName ? (
        <p className={`mb-3 text-sm ${textSecondaryOnCanvas}`} data-testid="machine-shared-name">
          {SHARED_NAME}
        </p>
      ) : null}
      {age === undefined ? null : (
        <p className={`mb-3 text-sm ${textMutedOnCanvas}`} data-testid="machine-drawing-age">
          Oldest report drawn: {oldest}.
        </p>
      )}
      {age?.stale === true ? (
        <p className={`mb-3 text-sm ${inlineWarningText}`} data-testid="machine-drawing-stale">
          {DRAWING_STALE}
        </p>
      ) : null}
      {model.summary.sessions === 0 ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`} data-testid="machine-unknown">
          {NO_SESSION_REPORTS_IT}
        </p>
      ) : model.noneInTmux ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`} data-testid="machine-none-in-tmux">
          {NONE_IN_TMUX}
        </p>
      ) : (
        <div className="space-y-5">
          {model.servers.map((server) => (
            <ServerPart
              counts={counts}
              key={serverKey(server.socket, server.serverPid)}
              now={now}
              picked={picked}
              server={server}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function ServerPart({
  counts,
  now,
  picked,
  server,
}: {
  counts: PaneCounts;
  now: number;
  picked: string;
  server: MachineTmuxServer;
}): ReactNode {
  const windows = stripWindows(server);
  // Each part reads only the choice that names its own server: an absent one, or one naming
  // another server or a window this server does not hold, draws this server's first window, so
  // every server draws one whichever window the address names.
  const chosen = chosenWindowId(picked, server.socket, server.serverPid);
  const drawn = windows.find((entry) => entry.window.windowId === chosen) ?? windows[0];

  // No box around the drawing: a border or a padding of the server's own would take the
  // drawing's width off the page's, and R6 holds the drawing to the width of the page.
  return (
    <section
      aria-label={`tmux server ${server.socket}`}
      className={`border-t pt-3 ${borderDefault}`}
      data-server-pid={server.serverPid}
      data-socket={server.socket}
      data-testid="machine-server"
    >
      <h2 className={`min-w-0 break-words text-sm font-semibold ${textPrimaryOnCanvas}`}>
        {server.socket}
        {server.showPid ? ` · pid ${server.serverPid}` : ""}
      </h2>
      <p className={`mt-1 text-xs ${textMutedOnCanvas}`} data-testid="machine-windows-with-agents">
        {WINDOWS_WITH_AGENTS}
      </p>
      <ul
        className="mt-2 mb-3 flex flex-nowrap items-center gap-2 overflow-x-auto [scrollbar-width:thin]"
        data-testid="window-strip"
      >
        {windows.map((entry) => {
          const current = entry.window.windowId === drawn?.window.windowId;
          return (
            <li key={entry.window.windowId}>
              <Link
                className={`block rounded-lg border px-2 py-1 text-xs whitespace-nowrap ${
                  current
                    ? `${selectedCardBorder} ${selectedCardBg} ${textPrimaryOnCanvas}`
                    : `${borderDefault} ${textSecondaryOnCanvas}`
                }`}
                data-picked={current ? "true" : "false"}
                data-testid="window-pick"
                data-window={entry.window.windowId}
                to={`?${new URLSearchParams({
                  window: windowChoice(server.socket, server.serverPid, entry.window.windowId),
                })}`}
              >
                {entry.sessionName}:{entry.window.windowIndex} {entry.window.windowName}
              </Link>
            </li>
          );
        })}
      </ul>
      {drawn === undefined ? null : (
        <WindowDrawing counts={counts} now={now} window={drawn.window} />
      )}
    </section>
  );
}
