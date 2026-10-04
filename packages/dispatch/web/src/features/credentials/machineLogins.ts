import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** The machine logins the viewer approved that can still reach a secret, as `MachineLoginsSection`
 *  lists them; its key is the one approving a login on the machine-login page and revoking one
 *  both invalidate. */
export const machineLoginsQuery = () =>
  queryOptions({
    queryKey: ["machine-logins"],
    queryFn: () => api.getMachineLogins(),
  });

/** A machine login's machine as every credentials page names it: its host, or `<service> on
 *  <host>` for a service's login (the Legion daemon's). */
export function machineName({ host, service }: { host: string; service: string | null }): string {
  return service ? `${service} on ${host}` : host;
}
