import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { CredentialGrant } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { credentialGrantsQuery } from "./grants";
import { type RevocableColumn, RevocableList } from "./RevocableList";

/** How a grant came to be, for its Live grants row: the policy's, or the person who approved it. */
function grantedBy(grant: CredentialGrant): string {
  return grant.granted === "automatic" ? "Automatically" : `Approved by ${grant.approver}`;
}

const columns: readonly RevocableColumn<CredentialGrant>[] = [
  {
    cell: (grant) => (
      <>
        {grant.enrollment.kind} · {grant.enrollment.runtime_id} · {grant.enrollment.operator || "—"}
        {grant.enrollment.slot ? (
          <span className={`block text-xs font-normal ${textSecondaryOnSurface}`}>
            slot {grant.enrollment.slot}
          </span>
        ) : null}
      </>
    ),
    header: "Enrollment",
  },
  { cell: (grant) => grant.names.join(", "), header: "Names" },
  { cell: grantedBy, header: "Granted" },
  { cell: (grant) => <Timestamp at={grant.created_at} />, header: "Created" },
  { cell: (grant) => <Timestamp at={grant.expires_at} />, header: "Expires" },
];

/**
 * The viewer's live credential grants: every grant of a session the viewer operates, whether the
 * policy gave it without asking or someone approved it, and every grant the viewer approved on
 * anyone's session. Each row names how it was granted and is revocable with one click: Dispatch
 * sends the broker the viewer's own login, and the broker allows the revoke only when that login
 * is the grant's approver or its enrollment's operator. Only the operator sees an automatic grant,
 * and revoking one also ends that session's other automatic grants of those secrets and makes it
 * ask before it gets them again. A pod enrollment's slot shows under its enrollment, so the grants
 * of two roles in one pod read apart. Rendered on the Settings page.
 */
export function GrantsSection(): ReactNode {
  const queryClient = useQueryClient();
  const grants = useQuery({ ...credentialGrantsQuery(), select: (data) => data.grants });

  return (
    <RevocableList
      className="mt-10"
      columns={columns}
      description="Every live grant of your sessions, given automatically or on approval, and the grants you approved. Revoking one ends its access immediately; after you revoke an automatic grant, that session asks before it gets those secrets again."
      empty="No live grants."
      heading="Live grants"
      headingId="credential-grants-heading"
      noun="grants"
      onRevoked={() =>
        void queryClient.invalidateQueries({ queryKey: credentialGrantsQuery().queryKey })
      }
      query={grants}
      revoke={(grant) => api.revokeCredentialGrant(grant.grant_id)}
      revokeError="Could not revoke this grant."
      rowKey={(grant) => grant.grant_id}
    />
  );
}
