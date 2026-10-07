import type { ReactNode } from "react";
import { generatePath, Link } from "react-router-dom";

import type { Agent, CredentialSession } from "../../api/types";
import { focusVisibleRing, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { AGENT_LIVE_PATH } from "../refs/routes";
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

function CredentialSessionLineText({ line }: { line: CredentialSessionLine }): ReactNode {
  const prefix = line.source === null ? "" : SOURCE_PREFIX[line.source];
  switch (line.status.kind) {
    case "running":
      return (
        <span className={textMutedOnCanvas}>
          {prefix}
          <Link
            className={`rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`}
            to={generatePath(AGENT_LIVE_PATH, { sessionId: encodeURIComponent(line.status.id) })}
          >
            {line.status.label}
          </Link>
          {` · ${line.status.dir} · ${line.status.machineId}`}
        </span>
      );
    case "not-running":
      return (
        <span className={textMutedOnCanvas}>{`${prefix}${line.status.id} isn't running.`}</span>
      );
    case "unknown":
      return (
        <span className={textMutedOnCanvas}>
          {`${prefix}Couldn't check whether ${line.status.id} is running.`}
        </span>
      );
  }
}

/**
 * The credential request's session, resolved against the live Agents list: the Inbox row and the
 * record page both render this one component (LEGION-587), so a running, not-running, couldn't-
 * check or unnamed session always reads identically in both places. Takes the agents list (and
 * its loading/error state) as props rather than calling `useAgents` itself: a page that renders
 * several of these (the Inbox's pending-requests list) polls the shared Agents list once from
 * its own page-level `useAgents` call and passes the result down, so N rows cost one 15s poll,
 * not N independently-phased ones (TanStack dedupes only a fetch already in flight, which a
 * second per-row `useQuery` observer mounting later than the first is not). Renders bare
 * text/links, no wrapping element: each call site supplies its own layout.
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
        <CredentialSessionLineText key={line.source ?? line.status.id} line={line} />
      ))}
    </>
  );
}
