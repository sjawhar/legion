import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { CredentialPendingRow } from "../../api/types";
import { LabelPill } from "../../components/Pill";
import {
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { Timestamp } from "../refs/Timestamp";
import { CredentialSessionLines } from "./CredentialSessionLines";
import type { CredentialRequests } from "./pending";
import { credentialSessionNamesAnyone } from "./session";

/** A `launcher_credential` (machine) record is decided only through the code-lookup route: its
 *  inbox row links to the code-entry page rather than trying to deep-link the
 *  specific pending request. */
function pendingRowPath(row: CredentialPendingRow): string {
  return row.kind === "launcher_credential"
    ? "/credentials/machine"
    : `/credentials/${row.record_id}`;
}

/** The Inbox's supplementary credential-requests section, mounted above the ask sections, from the
 *  Inbox's own reading of the list (`useCredentialRequests`), which its banner and empty state read
 *  too. A Dispatch with no secrets broker lists none, so the whole section hides silently - the one
 *  deliberate quiet path; a failure is surfaced only when no list has ever loaded (a later poll
 *  failing keeps showing the last list it held, `useCredentialRequests`'s `status` stays `listed`).
 *  Calls `useAgents` once for every row in the list (before either early return, so the hook always
 *  runs), rather than once per `CredentialSessionLines` instance: N pending rows then share one 15s
 *  poll of the Agents list instead of each phasing its own from whenever it mounted (LEGION-587's
 *  review, round 3). */
export function CredentialRequestsSection({
  credentials: { requests, status },
}: {
  credentials: CredentialRequests;
}): ReactNode {
  const { agents, isError, isPending } = useAgents(
    requests.some((row) => credentialSessionNamesAnyone(row.session)),
    true
  );
  if (status === "failed") {
    return <p className={dangerText}>Couldn't load credential requests.</p>;
  }
  if (requests.length === 0) {
    return null;
  }

  return (
    <section aria-labelledby="credential-requests-heading" className="mb-6">
      <h2
        className={`text-base font-semibold ${textMutedOnCanvas}`}
        id="credential-requests-heading"
      >
        Credential requests
      </h2>
      <ul className="mt-2 space-y-2">
        {requests.map((row) => (
          <li key={row.record_id}>
            <Link
              className={`flex flex-wrap items-center gap-2 rounded-lg text-sm outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`}
              to={pendingRowPath(row)}
            >
              <LabelPill>
                {row.kind === "launcher_credential" ? "Machine login" : "Secret request"}
              </LabelPill>
              <span>{row.identifiers.join(", ")}</span>
              <Timestamp at={row.requested_at} className={textMutedOnCanvas} />
            </Link>
            <div className="mt-0.5 flex flex-col gap-0.5 text-xs">
              <CredentialSessionLines
                agents={agents}
                isError={isError}
                isPending={isPending}
                session={row.session}
              />
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
