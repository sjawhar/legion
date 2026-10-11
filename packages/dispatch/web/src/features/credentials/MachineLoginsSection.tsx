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
  {
    cell: (login) => <span className="font-mono text-xs">{login.credential_id}</span>,
    header: "Credential",
  },
  {
    // `== null`: a broker older than this Dispatch sends no `subject` at all.
    cell: (login) => (login.subject == null ? login.approved_by : `No one: ${login.subject}`),
    header: "Approved by",
  },
  { cell: (login) => <Timestamp at={login.issued_at} />, header: "Issued" },
  { cell: (login) => <Timestamp at={login.expires_at} />, header: "Expires" },
];

/**
 * The machine logins the viewer may revoke that can still reach a secret: one row per machine
 * logged in as them, and one per service's login (the Legion daemon's, labelled
 * `legion-daemon on <host>`), whoever approved it, since anyone signed in lists and revokes a
 * service's login. Each row says which credential it is (the broker's `credential_id`, which every
 * enrollment the login made names, while its machine name is the machine's own claim), who approved
 * it — no one, for a service's token login, whose row names the Kubernetes ServiceAccount that
 * logged in — when it was issued and when it expires. A login's sessions outlive its expiry, since
 * each renews with its own key, so an expired login stays listed, marked `expired, sessions still
 * running`, until its last session ends.
 * Revoke ends a login, expired or not: it enrolls no more sessions, and every session it enrolled
 * (a service's worker pods) loses the broker at once, its grants with it, so Live grants is
 * refreshed too. A service logging in with its token logs in again under a new login, which
 * enrolls the pods it launches next; a pod already running stays without secrets until it is
 * replaced. Dispatch names the viewer as the person revoking, and the broker refuses anyone but the
 * approver of a person's machine login. Rendered on the machine-login page.
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
            ? login.subject == null
              ? `Every session it started, its worker pods included, ends at once.`
              : `Every session it started, its worker pods included, ends at once, and a running pod stays without secrets until it is replaced; ${login.service} logs in again with its Kubernetes token, for the pods it launches next, unless ${login.subject} is removed from the broker's BROKER_SERVICE_LOGINS.`
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
