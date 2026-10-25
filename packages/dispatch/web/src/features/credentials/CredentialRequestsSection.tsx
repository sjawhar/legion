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

/** A secret request's page on the secrets broker, where a person signed in to the broker decides
 *  it: the row's `approve_url` when it is an absolute `http:` or `https:` URL, else null. Anything
 *  else, such as a `javascript:` URL or a relative path, is treated as absent, since the row would
 *  otherwise run or open whatever the relayed body named. */
function brokerPage(row: CredentialPendingRow): string | null {
  if (row.approve_url === undefined) {
    return null;
  }
  try {
    const url = new URL(row.approve_url);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}

const rowLinkClass = `flex flex-wrap items-center gap-2 rounded-lg text-sm outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

/** One pending request's line. A machine (`launcher_credential`) record is decided only through
 *  the code-lookup route, so its line links to the code-entry page rather than trying to deep-link
 *  the specific pending request. A secret request is decided on the secrets broker's own page, so
 *  its line is a plain link there, leaving Dispatch in the same tab; from a broker that names no
 *  page (one older than it), the line links nowhere and says where the request is decided. */
function PendingRowLine({ row }: { row: CredentialPendingRow }): ReactNode {
  const summary = (
    <>
      <LabelPill>
        {row.kind === "launcher_credential" ? "Machine login" : "Secret request"}
      </LabelPill>
      <span>{row.identifiers.join(", ")}</span>
      <Timestamp at={row.requested_at} className={textMutedOnCanvas} />
    </>
  );
  if (row.kind === "launcher_credential") {
    return (
      <Link className={rowLinkClass} to="/credentials/machine">
        {summary}
      </Link>
    );
  }
  const page = brokerPage(row);
  if (page === null) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-sm">
        {summary}
        <span className={textMutedOnCanvas}>Decided on the secrets broker.</span>
      </div>
    );
  }
  return (
    <a className={rowLinkClass} href={page}>
      {summary}
    </a>
  );
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
            <PendingRowLine row={row} />
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
