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
