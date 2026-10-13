import { useQuery } from "@tanstack/react-query";

import { whoAmIQuery } from "../api/queries";

/** Whether the signed-in principal is a person (`/auth/whoami`'s `kind: "user"`), which a
 *  human-only route needs before a surface asks it: false while the principal is still loading
 *  and for a bearer, so no other principal ever cycles on a 403. */
export function useIsHuman(): boolean {
  return useQuery(whoAmIQuery()).data?.kind === "user";
}
