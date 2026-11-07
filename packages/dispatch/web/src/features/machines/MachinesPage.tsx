import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { buildMachinePath } from "../refs/routes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { machineList, sessionCountSentence } from "./machine-model";

/**
 * Every machine a live session names, with how many sessions are on it and how many of those are
 * in a tmux pane: the second count says which machines have a layout to draw. The list is the
 * agent registry grouped by `host` and nothing else, so it adds no read to the page's one shared
 * poll.
 */
export function MachinesPage(): ReactNode {
  useDocumentTitle("Machines · Dispatch");
  const { agents, current, error, isError, isPending } = useAgents(true, true);
  const machines = useMemo(() => machineList(agents), [agents]);

  if (isPending) return <LoadingSkeleton label="Loading machines" />;
  // A refresh that failed keeps the rows it has; with no rows there is nothing to keep.
  if (isError && agents.length === 0) {
    return <p className={dangerText}>Could not load agents: {error}</p>;
  }

  return (
    <section aria-label="Machines" data-testid="machines-page">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Machines
        </h1>
        <p className={`mt-1 flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
          <span>Every machine a live session names, and how many of its sessions are in tmux.</span>
          <Link className={`${linkText} ${linkHoverText}`} to="/agents">
            Agents
          </Link>
        </p>
      </header>
      {machines.length === 0 ? (
        // Only a current list says that nobody is connected.
        current ? (
          <EmptyState
            label="Machines empty state"
            message="No agent is connected, so no machine is listed."
          />
        ) : null
      ) : (
        <ul className="space-y-2" data-testid="machine-list">
          {machines.map((machine) => (
            <li
              className={`rounded-xl border ${card} ${borderDefault}`}
              data-in-tmux={machine.inTmux}
              data-machine={machine.host}
              data-sessions={machine.sessions}
              data-testid="machine-row"
              key={machine.host}
            >
              <Link className="flex w-full flex-col px-3 py-2" to={buildMachinePath(machine.host)}>
                <span className={`min-w-0 break-words font-medium ${textPrimaryOnCanvas}`}>
                  {machine.host}
                </span>
                <span className={`text-xs ${textMutedOnCanvas}`}>
                  {sessionCountSentence(machine)}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
