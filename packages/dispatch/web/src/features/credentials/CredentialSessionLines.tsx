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

/**
 * One id: the agent, with its running state and its live link, then its full id, then where it
 * runs while the agents list lists it, or in words that it isn't running or couldn't be
 * checked. A request's id is an unsigned claim (`session.ts`), and an unlisted id's name is its
 * short form `session:<8>…`, which a forged id (or any session started in the same minute)
 * shares; so the person approving reads the whole id, and the state in words rather than only
 * the mark's ring. The full id breaks anywhere, so it never widens the record page's grid.
 */
function statusText(line: CredentialSessionLine): string {
  switch (line.status.kind) {
    case "running":
      return ` · ${line.id} · ${line.status.place.dir} · ${line.status.place.machineId}`;
    case "not-running":
      return ` · ${line.id} isn't running.`;
    case "unknown":
      return ` · couldn't check whether ${line.id} is running.`;
  }
}

function CredentialSessionLineText({ line }: { line: CredentialSessionLine }): ReactNode {
  return (
    <span className={`${textMutedOnCanvas} [overflow-wrap:anywhere]`}>
      {line.source === null ? "" : SOURCE_PREFIX[line.source]}
      <AgentName sessionId={line.id} storedTitle={undefined} />
      {statusText(line)}
    </span>
  );
}

/**
 * The credential request's session: the Inbox row and the record page both render this one
 * component (LEGION-587), so a running, not-running, couldn't-check or unnamed session always
 * reads identically in both places. Each id is an `AgentName` and its full id, and a line says
 * in words when its session isn't running or could not be checked. Takes the agents list (and
 * its loading/error state) as props rather than calling `useAgents` itself: a page that renders
 * several of these (the Inbox's pending-requests list) reads the list once from its own
 * page-level `useAgents` call, and that one read decides every line's status, so the words
 * follow the same `["agents"]` query as each name's mark. Renders bare text/links,
 * no wrapping element: each call site supplies its own layout.
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
