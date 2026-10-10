import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { MachineLogin } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { credentialGrantsQuery } from "./grants";
import { machineLoginsQuery, machineName } from "./machineLogins";
import { type RevocableColumn, RevocableList } from "./RevocableList";

const columns: readonly RevocableColumn<MachineLogin>[] = [
  {
    cell: (login) => (
      <>
        {machineName(login)}
        {login.expired ? (
          <span className={`block text-xs font-normal ${textSecondaryOnSurface}`}>
            expired, sessions still running
          </span>
        ) : null}
      </>
    ),
    header: "Machine",
  },
  { cell: (login) => login.approved_by, header: "Approved by" },
  { cell: (login) => <Timestamp at={login.issued_at} />, header: "Issued" },
  { cell: (login) => <Timestamp at={login.expires_at} />, header: "Expires" },
];

/**
 * The machine logins the viewer may revoke that can still reach a secret: one row per machine
 * logged in as them, and one per service's login (the Legion daemon's, labelled
 * `legion-daemon on <host>`), whoever approved it, since anyone signed in approves, lists and
 * revokes a service's login. Each row says who approved it, when it was issued and when it
 * expires. A login's sessions outlive its expiry, since each renews with its own key, so an expired
 * login stays listed, marked `expired, sessions still running`, until its last session ends.
 * Revoke ends a login, expired or not: it enrolls no more sessions, and every session it enrolled
 * (a service's worker pods) loses the broker at once, its grants with it, so Live grants is
 * refreshed too. Dispatch names the viewer as the person revoking, and the broker refuses anyone
 * but the approver of a person's machine login. Rendered on the machine-login page.
 */
export function MachineLoginsSection(): ReactNode {
  const queryClient = useQueryClient();
  const logins = useQuery({ ...machineLoginsQuery(), select: (data) => data.credentials });

  return (
    <RevocableList
      className="pt-4"
      columns={columns}
      confirm={(login) =>
        `Revoke the machine login for ${machineName(login)}? ${
          login.service
            ? `Every session it started, its worker pods included, ends at once, and ${login.service} needs a new login approval before it starts any more.`
            : "Every agent session it started loses its secrets at once, and the machine needs a new login."
        }`
      }
      description="Every machine logged in as you, and every service's login, which anyone signed in may revoke, while its login is unexpired or a session it started still runs: a session outlives its login's expiry. Revoking one ends it now: it starts no more sessions, and every session it started loses its secrets."
      empty="No live machine logins."
      heading="Machine logins"
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
