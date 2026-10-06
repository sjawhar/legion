import type { ReactNode } from "react";
import { generatePath, Link } from "react-router-dom";

import type { CredentialSession } from "../../api/types";
import { focusVisibleRing, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { AGENT_LIVE_PATH } from "../refs/routes";
import {
  type CredentialSessionLine,
  type CredentialSessionSource,
  credentialSessionLines,
  credentialSessionNamesAnyone,
} from "./session";

const SOURCE_PREFIX: Record<CredentialSessionSource, string> = {
  enrollment: "The session that enrolled: ",
  request: "The session the request says it came from: ",
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
            to={generatePath(AGENT_LIVE_PATH, { sessionId: line.status.id })}
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
 * check or unnamed session always reads identically in both places. Enables `useAgents` only when
 * there is an id to resolve, and keeps refreshing it while mounted (as the Agents page does), so
 * a session that ends while the page is open stops reading "running" without a remount. Renders
 * bare text/links, no wrapping element: each call site supplies its own layout.
 */
export function CredentialSessionLines({
  session,
}: {
  session: CredentialSession | undefined;
}): ReactNode {
  const { agents, isError, isPending } = useAgents(credentialSessionNamesAnyone(session), true);
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
