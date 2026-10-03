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
import { Timestamp } from "../refs/Timestamp";
import { useCredentialRequests } from "./pending";

/** A `launcher_credential` (machine) record is decided only through the code-lookup route (contract
 *  v9 ruling 13): its inbox row links to the code-entry page rather than trying to deep-link the
 *  specific pending request. */
function pendingRowPath(row: CredentialPendingRow): string {
  return row.kind === "launcher_credential"
    ? "/credentials/machine"
    : `/credentials/${row.record_id}`;
}

/** The Inbox's supplementary credential-requests section, mounted above the ask sections. A
 *  broker that isn't configured for this deployment (404 FEATURE_OFF) lists none, so the whole
 *  section hides silently - the one deliberate quiet path; any other failure is surfaced instead. */
export function CredentialRequestsSection(): ReactNode {
  const { requests, status } = useCredentialRequests();

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
          </li>
        ))}
      </ul>
    </section>
  );
}
