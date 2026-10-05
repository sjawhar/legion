import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { focusVisibleRing, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import type { CredentialSessionLine as Line } from "./session";

const SOURCE_PREFIX: Record<"enrollment" | "request", string> = {
  enrollment: "The session that enrolled: ",
  request: "The session the request names: ",
};

/** Renders one `CredentialSessionLine`: the Inbox row and the record page both call this for each
 *  line `useCredentialSessionLines` returns, so a running, not-running or couldn't-check session
 *  always reads identically in both places (LEGION-587). */
export function CredentialSessionLineText({ line }: { line: Line }): ReactNode {
  const prefix = line.source === null ? "" : SOURCE_PREFIX[line.source];
  switch (line.status.kind) {
    case "running":
      return (
        <span className={textMutedOnCanvas}>
          {prefix}
          <Link
            className={`rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`}
            to={`/agents/${encodeURIComponent(line.status.id)}/live`}
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

/** "No session named.": the words both call sites show for a request whose `session` names
 *  neither id (including an older broker's response, which carries no `session` field at all). A
 *  module-level element, not a function, since it takes no props and never changes. */
export const noSessionNamed: ReactNode = (
  <span className={textMutedOnCanvas}>No session named.</span>
);
