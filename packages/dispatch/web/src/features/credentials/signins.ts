import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** The viewer's own sign-ins on their machines (`agent-secrets login`), as `SignInsSection` lists
 *  them; its key is the one revoking a sign-in invalidates. */
export const signInsQuery = () =>
  queryOptions({
    queryKey: ["signins"],
    queryFn: () => api.getSignIns(),
  });

/** How many characters of a box key's thumbprint a row shows. An RFC 7638 thumbprint is 43
 *  base64url characters, which no table reads across; the first twelve are more than enough to
 *  tell one of a person's boxes from another, and the full value is in the row's title. */
const THUMBPRINT_SHOWN = 12;

/** A box key's thumbprint as a row shows it: its first characters, with an ellipsis for the rest.
 *  It is the one fact that tells two sign-ins on one machine account apart, so it is shown rather
 *  than hidden behind the id. */
export function shortThumbprint(thumbprint: string): string {
  return thumbprint.length > THUMBPRINT_SHOWN
    ? `${thumbprint.slice(0, THUMBPRINT_SHOWN)}…`
    : thumbprint;
}
