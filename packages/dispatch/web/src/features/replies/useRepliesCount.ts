import { useQuery } from "@tanstack/react-query";

import { myRepliesCountQuery, whoAmIQuery } from "../../api/queries";

/** The count the compact header's and the sidebar's `Replies N` show: `GET /api/v1/me/replies`'s
 *  `total`. The route is human-only, so no other principal asks. Its own module, so the shell
 *  reads it without loading the lazy Replies page. */
export function useRepliesCount(): number {
  const human = useQuery(whoAmIQuery()).data?.kind === "user";
  return useQuery({ ...myRepliesCountQuery(), enabled: human }).data ?? 0;
}
