import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { MachineLogin } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { credentialGrantsQuery } from "./grants";
import { machineLoginsQuery } from "./machineLogins";
import { type RevocableColumn, RevocableList } from "./RevocableList";

const columns: readonly RevocableColumn<MachineLogin>[] = [
  {
    cell: (login) => (
      <>
        {login.service ? `${login.service} on ${login.host}` : login.host}
        {login.expired ? (
          <span className={`block text-xs font-normal ${textSecondaryOnSurface}`}>
            expired, sessions still running
          </span>
        ) : null}
      </>
    ),
    header: "Machine",
  },
  { cell: (login) => <Timestamp at={login.issued_at} />, header: "Issued" },
  { cell: (login) => <Timestamp at={login.expires_at} />, header: "Expires" },
];

/**
 * The machine logins the viewer approved that can still reach a secret: one row per machine logged
 * in as them, and one per service whose login they approved (the Legion daemon's, labelled
 * `legion-daemon on <host>`), with when it was issued and when it expires. A login's sessions
 * outlive its expiry, since each renews with its own key, so an expired login stays listed, marked
 * `expired, sessions still running`, until its last session ends. Revoke ends a login, expired or
 * not: it enrolls no more sessions, and every session it enrolled (a service's worker pods) loses
 * the broker at once, its grants with it, so Live grants is refreshed too. Dispatch names the
 * viewer as the person revoking, and the broker refuses anyone but the login's approver. Rendered
 * on the machine-login page.
 */
export function MachineLoginsSection(): ReactNode {
  const queryClient = useQueryClient();
  const logins = useQuery({ ...machineLoginsQuery(), select: (data) => data.credentials });

  return (
    <RevocableList
      className="pt-4"
      columns={columns}
      confirm={(login) =>
        login.service
          ? `Revoke the ${login.service} login on ${login.host}? Every session it started, its worker pods included, ends at once, and ${login.service} needs a new login approval before it starts any more.`
          : `Revoke the machine login for ${login.host}? Every agent session it started loses its secrets at once, and the machine needs a new login.`
      }
      description="Every machine logged in as you, and every service whose login you allowed, while its login is unexpired or a session it started still runs: a session outlives its login's expiry. Revoking one ends it now: it starts no more sessions, and every session it started loses its secrets."
      empty="No live machine logins."
      heading="Your machine logins"
      headingId="machine-logins-heading"
      noun="machine logins"
      onRevoked={() => {
        void queryClient.invalidateQueries({ queryKey: machineLoginsQuery().queryKey });
        void queryClient.invalidateQueries({ queryKey: credentialGrantsQuery().queryKey });
      }}
      query={logins}
      revoke={(login) => api.revokeMachineLogin(login.credential_id)}
      revokeError="Could not revoke this machine login."
      rowKey={(login) => login.credential_id}
    />
  );
}
