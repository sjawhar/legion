import { useQuery } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";

/** The count the compact header's and the sidebar's `Replies N` show: `GET /api/v1/me/replies`'s
 *  `total`. The route needs the `person` scope, which a signed-in browser holds, so the count
 *  waits for the sign-in read before it asks. Its own module, so the shell reads it without
 *  loading the lazy Replies page. */
export function useRepliesCount(): number {
  return useQuery({ ...myRepliesCountQuery(), enabled: useIsHuman() }).data ?? 0;
}
