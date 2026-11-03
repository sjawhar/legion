import { useQuery } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";

/** How many of the Inbox's thread rows are unread: the `total` of
 *  `GET /api/v1/me/replies?kind=reply,agent&limit=1`, which the sidebar's and the compact
 *  header's one Inbox number adds to the work waiting on the reader. One request per page load,
 *  never the Inbox's own unpaged read. The route is human-only, so no other principal asks. Its
 *  own module, so the shell reads it without loading the Inbox. */
export function useUnreadThreadCount(): number {
  return useQuery({ ...myRepliesCountQuery(), enabled: useIsHuman() }).data ?? 0;
}
