import type { ReactNode } from "react";

import type { Agent, CredentialSession } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import {
  type CredentialSessionLine,
  type CredentialSessionSource,
  credentialSessionLines,
} from "./session";

// Only the enrollment line carries a preamble: when both ids are shown, it says which one the
// broker verified. The request's line is the request's session as written, with no preamble.
const SOURCE_PREFIX: Record<CredentialSessionSource, string> = {
  enrollment: "The session that enrolled: ",
  request: "",
};

/** One id: the agent, with its running state and its live link, then, while the agents list
 *  lists it, where it runs. */
function CredentialSessionLineText({ line }: { line: CredentialSessionLine }): ReactNode {
  return (
    <span className={textMutedOnCanvas}>
      {line.source === null ? "" : SOURCE_PREFIX[line.source]}
      <AgentName sessionId={line.id} storedTitle={undefined} />
      {line.place === null ? "" : ` · ${line.place.dir} · ${line.place.machineId}`}
    </span>
  );
}

/**
 * The credential request's session: the Inbox row and the record page both render this one
 * component (LEGION-587), so a running, not-running, couldn't-check or unnamed session always
 * reads identically in both places. Each id is an `AgentName`, whose mark says whether it is
 * running, not running, or could not be checked. Takes the agents list (and its loading/error
 * state) as props rather than calling `useAgents` itself: a page that renders several of these
 * (the Inbox's pending-requests list) reads the list once from its own page-level `useAgents`
 * call for every line's directory and machine. Renders bare text/links, no wrapping element:
 * each call site supplies its own layout.
 */
export function CredentialSessionLines({
  agents,
  isError,
  isPending,
  session,
}: {
  agents: readonly Agent[];
  isError: boolean;
  isPending: boolean;
  session: CredentialSession | undefined;
}): ReactNode {
  const lines = credentialSessionLines(session, agents, isError || isPending);
  if (lines.length === 0) {
    return <span className={textMutedOnCanvas}>No session named.</span>;
  }
  return (
    <>
      {lines.map((line) => (
        <CredentialSessionLineText key={line.source ?? line.id} line={line} />
      ))}
    </>
  );
}
